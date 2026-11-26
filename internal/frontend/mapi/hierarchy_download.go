// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"time"

	"ewsgo/internal/core"
)

const maxLiveTransfers = 32
const maxContextTransfers = 4

type hierarchyDownload struct {
	hierarchyCollector    bool // Hierarchy upload context, separate from download and FAI.
	collector             *faiCollector
	draft                 *faiDraft
	draftCollector        *faiCollector
	faiUpload             *faiUpload
	mailImport            *mailImportDraft
	mailUpload            *mailImportUpload
	contents              *contentsDownload
	state                 *hierarchyState // Nil for the immutable source of GetTransferState.
	config                synchronizationConfigure
	stream                *fastTransfer
	copySource            *openedMessage
	copyFolder            core.ID
	copyLease             *contentsLease
	cursor                int
	finalGiven, finalSeen idSet
	failure               uint32
}

func (*hierarchyDownload) String() string   { return "hierarchyDownload{redacted}" }
func (*hierarchyDownload) GoString() string { return "hierarchyDownload{redacted}" }

func (t *SessionTransport) EnableHierarchyDownloadSpike() { t.hierarchyDownloadSpike = true }

// ImportMessageChange returns a Message object. Its staging properties already
// reserve space in contentsMemory, separately from FastTransfer and ICS contexts.
func (d *hierarchyDownload) consumesTransfer() bool {
	return d != nil && d.draft == nil && d.mailImport == nil
}

func (s *mailboxObjects) hasDownloadCapacity() bool {
	count := 0
	for _, o := range s.objects {
		if o.download.consumesTransfer() {
			count++
		}
	}
	return count < maxContextTransfers && len(s.objects) < maxLiveStoreHandles &&
		s.allocated < maxContextHandleAllocations && s.allocator.transfers.Load() < maxLiveTransfers
}

func (s *mailboxObjects) addDownload(object serverObject, d *hierarchyDownload) (uint32, error) {
	if len(s.objects) >= maxLiveStoreHandles || s.allocated >= maxContextHandleAllocations {
		return 0, core.ErrUnsupported
	}
	transfer := d.consumesTransfer()
	if transfer {
		if !s.hasDownloadCapacity() {
			return 0, core.ErrUnsupported
		}
		for {
			n := s.allocator.transfers.Load()
			if n >= maxLiveTransfers {
				return 0, core.ErrUnsupported
			}
			if s.allocator.transfers.CompareAndSwap(n, n+1) {
				break
			}
		}
	}
	handle, err := s.allocator.handle()
	if err != nil {
		if transfer {
			s.allocator.transfers.Add(-1)
		}
		return 0, err
	}
	object.isLogon = false
	object.download = d
	s.objects[handle] = object
	s.allocated++
	return handle, nil
}

func configureHierarchy(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(0x70, op.Output, code) }
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if object.isLogon || object.download != nil {
		return fail(NotSupported)
	}
	if err := validateHierarchyConfigure(*op.Configure); err != nil {
		return fail(hierarchyError(err))
	}
	state, err := newHierarchyState(ctx, session, object.binding.store, object.fid)
	if err != nil {
		return fail(hierarchyError(err))
	}
	handle, err := objects.addDownload(object, &hierarchyDownload{state: state, config: *op.Configure})
	if err != nil {
		return fail(hierarchyError(err))
	}
	handles[op.Output] = handle
	return fail(0)
}

func uploadHierarchy(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) ([]byte, contentsUploadFailure) {
	fail := func(code uint32) ([]byte, contentsUploadFailure) {
		return ropError(op.ID, op.Input, code), contentsUploadFailure{}
	}
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if object.download != nil && object.download.collector != nil {
		err := object.download.collector.state.upload(ctx, session, *op.Upload)
		return ropError(op.ID, op.Input, faiError(err, false)), inspectContentsUploadFailure(err)
	}
	if object.download != nil && object.download.contents != nil {
		return uploadContents(ctx, session, object, op)
	}
	if object.download == nil || object.download.state == nil {
		return fail(NotSupported)
	}
	err := object.download.state.uploadMode(ctx, session, *op.Upload, object.download.hierarchyCollector)
	if err != nil {
		return fail(hierarchyError(err))
	}
	return fail(0)
}

func (d *hierarchyDownload) prepare(ctx context.Context, session core.Session, object serverObject) error {
	if d.stream != nil {
		return nil
	}
	if d.state == nil {
		return core.ErrInvalid
	}
	snapshot, err := object.binding.store.observeHierarchy(ctx, session, object.fid, time.Now())
	if err != nil {
		return err
	}
	delta, err := d.state.planHierarchy(ctx, session, snapshot)
	if err != nil {
		return err
	}
	stream, err := hierarchyFastTransfer(snapshot.replica, object.binding.mailboxGUID, d.config, delta)
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	d.stream = stream
	d.finalGiven, d.finalSeen = delta.finalGiven, delta.finalSeen
	return nil
}

func getHierarchyBuffer(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	response := func(code uint32, status uint16, data []byte, done, total int) []byte {
		out := ropError(0x4e, op.Input, code)
		out = binary.LittleEndian.AppendUint16(out, status)
		out = binary.LittleEndian.AppendUint16(out, uint16(min(done, 65535)))
		out = binary.LittleEndian.AppendUint16(out, uint16(min(total, 65535)))
		out = append(out, 0)
		out = binary.LittleEndian.AppendUint16(out, uint16(len(data)))
		return append(out, data...)
	}
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return response(code, 0, nil, 0, 0)
	}
	d := object.download
	if d != nil && d.contents != nil {
		return getContentsBuffer(ctx, session, object, op)
	}
	if d == nil || d.collector != nil || d.draft != nil || d.hierarchyCollector {
		return response(NotSupported, 0, nil, 0, 0)
	}
	if d.copySource != nil {
		if err := object.binding.store.authorizeAttachmentContent(ctx, session, object.fid, *d.copySource); err != nil {
			return response(attachmentError(err), 0, nil, 0, 0)
		}
	} else if d.copyFolder != "" {
		folder, err := object.binding.store.messageAnchor(ctx, session, object.fid, false)
		if err != nil {
			return response(storeError(err), 0, nil, 0, 0)
		}
		if folder.native != d.copyFolder {
			return response(PropertyNotFound, 0, nil, 0, 0)
		}
	} else if err := object.binding.store.authorizeHierarchyAnchor(ctx, session, object.fid); err != nil {
		d.failure = hierarchyError(err)
		return response(d.failure, 0, nil, 0, 0)
	}
	if d.failure != 0 {
		return response(d.failure, 0, nil, 0, 0)
	}
	if err := d.prepare(ctx, session, object); err != nil {
		// Failed serialization/upload phase cannot produce a fictitious Done.
		d.failure = hierarchyError(err)
		return response(d.failure, 0, nil, 0, 0)
	}
	end := d.stream.chunkEnd(d.cursor, int(op.TransferLimit))
	status := uint16(1)
	if end == len(d.stream.data) {
		status = 3
	} else if end == d.cursor {
		status = 2
	}
	if err := ctx.Err(); err != nil {
		return response(hierarchyError(err), 0, nil, 0, 0)
	}
	data := d.stream.data[d.cursor:end]
	total := max(1, (len(d.stream.data)+1023)/1024)
	done := end / 1024
	if status == 3 {
		done = total
	}
	out := response(0, status, data, done, total)
	// Execute preflight has reserved this complete response before any ROP.
	d.cursor = end
	return out
}

func getHierarchyTransferState(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32, failure *transferStateFailure) []byte {
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return failure.reject(op.Output, code, transferStateHandleLookup)
	}
	d := object.download
	if d == nil {
		return failure.reject(op.Output, NotSupported, transferStateWrongObject)
	}
	if d.collector != nil {
		failure.source = transferStateCollector
		return getFAITransferState(ctx, session, objects, object, op, handles, failure)
	}
	if d.contents != nil {
		failure.source = transferStateContents
		return getContentsTransferState(ctx, session, objects, object, op, handles, failure)
	}
	if d.state == nil {
		if d.stream != nil && d.copySource == nil && d.copyFolder == "" {
			return failure.reject(op.Output, NotSupported, transferStateExportedSource)
		}
		return failure.reject(op.Output, NotSupported, transferStateWrongObject)
	}
	failure.source = transferStateHierarchy
	if err := object.binding.store.authorizeHierarchyAnchor(ctx, session, object.fid); err != nil {
		return failure.reject(op.Output, hierarchyError(err), transferStateAuthorization)
	}
	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	if d.state.pending != nil {
		return failure.reject(op.Output, hierarchyError(core.ErrConflict), transferStatePending)
	}
	given, seen := d.state.given, d.state.seen
	if d.stream != nil && d.cursor == len(d.stream.data) {
		given, seen = d.finalGiven, d.finalSeen
	}
	var stream *fastTransfer
	var err error
	if d.hierarchyCollector {
		stream, err = hierarchyUploadStateFastTransfer(seen)
	} else {
		stream, err = stateFastTransfer(given, seen)
	}
	if err != nil {
		return failure.reject(op.Output, hierarchyError(err), transferStateEncoding)
	}
	if err := ctx.Err(); err != nil {
		return failure.reject(op.Output, hierarchyError(err), transferStateCanceled)
	}
	handle, err := objects.addDownload(object, &hierarchyDownload{stream: stream})
	if err != nil {
		return failure.reject(op.Output, hierarchyError(err), transferStateHandleCreate)
	}
	handles[op.Output] = handle
	return ropError(0x82, op.Output, 0)
}

func hierarchyError(err error) uint32 {
	if errors.Is(err, core.ErrResync) {
		return 0x80040805 // NotSynchronized; this RAM profile requires resync.
	}
	if errors.Is(err, ErrMalformed) {
		return 0x000004b6
	}
	if errors.Is(err, ErrUnsupported) {
		return NotSupported
	}
	if errors.Is(err, core.ErrConflict) {
		return 0x80040102
	}
	return storeError(err)
}
