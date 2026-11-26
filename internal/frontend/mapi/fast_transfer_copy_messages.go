// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"

	"ewsgo/internal/core"
)

const ropFastTransferSourceCopyMessages byte = 0x4b

type fastTransferCopyMessagesRequest struct {
	mids               []uint64
	flags, sendOptions byte
}

func parseFastTransferCopyMessages(r *reader, op *Operation, handles int) error {
	header := r.take(3)
	if r.err != nil || int(header[0]) >= handles {
		return ErrMalformed
	}
	count := int(binary.LittleEndian.Uint16(header[1:]))
	raw := r.take(8 * count)
	options := r.take(2)
	if r.err != nil {
		return ErrMalformed
	}
	if count > core.MaxMailContentMessages {
		return ErrUnsupported
	}
	mids, err := decodeDistinctMessageIDs(raw)
	if err != nil {
		return err
	}
	op.Output = header[0]
	op.TransferMessages = fastTransferCopyMessagesRequest{mids: mids, flags: options[0], sendOptions: options[1]}
	return nil
}

func openFastTransferCopyMessages(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32, codePage uint32) []byte {
	reply := func(code uint32) []byte { return ropError(op.ID, op.Output, code) }
	folder, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return reply(code)
	}
	if folder.isLogon || folder.download != nil {
		return reply(NotSupported)
	}
	request := op.TransferMessages
	if len(request.mids) == 0 || request.flags & ^byte(0x37) != 0 {
		return reply(0x80070057)
	}
	string8, code := fastTransferCopyString8(request.sendOptions, codePage)
	if code != 0 {
		return reply(code)
	}
	box := session.Mailbox()
	if request.flags&1 != 0 {
		// Move is only a download hint. This source never mutates messages.
		_, trash := box.(core.MailMessagePlacementWriter)
		_, remove := box.(core.MailMessageSoftDeleter)
		if !trash && !remove {
			return reply(accessDenied)
		}
	}
	if !objects.hasDownloadCapacity() {
		return reply(NotSupported)
	}
	store := folder.binding.store
	store.mu.Lock()
	native := store.byFID[folder.fid].native
	store.mu.Unlock()
	if native == "" {
		return reply(NotSupported)
	}
	lease, err := store.reserveContentsPeak(ctx, 2*maxMailProjectionBytes+maxContentsFastTransferBytes)
	if err != nil {
		return reply(storeError(err))
	}
	defer func() { lease.release() }()
	snapshot, err := store.observeMailMessages(ctx, session, folder.fid)
	if err != nil {
		return reply(storeError(err))
	}
	selected := make(map[uint64]messageRow, len(request.mids))
	for _, row := range snapshot.messages {
		selected[row.mid] = row
	}
	for _, mid := range request.mids {
		row, found := selected[mid]
		if !found || row.object.Parent != native {
			return reply(PropertyNotFound)
		}
	}
	stream := &fastTransfer{string8CodePage: string8, limit: maxContentsFastTransferBytes}
	for _, mid := range request.mids {
		if err := ctx.Err(); err != nil {
			return reply(storeError(err))
		}
		if err := store.writeCopiedMessage(stream, folder, selected[mid], request.flags&0x20 != 0); err != nil {
			return reply(storeError(err))
		}
	}
	if err := ctx.Err(); err != nil {
		return reply(storeError(err))
	}
	if err := lease.shrink(cap(stream.data) + 16*cap(stream.atoms) + 512); err != nil {
		return reply(storeError(err))
	}
	handle, err := objects.addDownload(folder, &hierarchyDownload{copyFolder: native, copyLease: lease, stream: stream})
	if err != nil {
		return reply(storeError(err))
	}
	lease = nil
	handles[op.Output] = handle
	return reply(0)
}

func (s *ramStore) writeCopiedMessage(stream *fastTransfer, folder serverObject, row messageRow, sendEntryID bool) error {
	payload, err := s.projectContentsPayload(row)
	if err != nil {
		return err
	}
	if sendEntryID {
		if !row.changeKey.valid() || row.sourceKey == ([22]byte{}) || row.modified.IsZero() {
			return core.ErrConflict
		}
		entryID := marshalMessageEntryID(folder.binding.mailboxGUID, s.replica, folder.fid, row.mid)
		putContentsValue(payload.values, 0x3a120102, core.Binary(entryID))
		putContentsValue(payload.values, 0x65e00102, core.Binary(row.sourceKey[:]))
		putContentsValue(payload.values, 0x30080040, core.Timestamp(row.modified))
		putContentsValue(payload.values, 0x65e20102, core.Binary(row.changeKey))
		putContentsValue(payload.values, 0x65e30102, core.Binary(row.predecessors))
	}
	if row.associated {
		stream.word(0x40100003) // StartFAIMsg.
	} else {
		stream.word(0x400c0003) // StartMessage.
	}
	if err := s.writeFastTransferProperties(stream, payload.values, nil, nil); err != nil {
		return err
	}
	if err := s.writeFastTransferMessageChildren(stream, payload, func(uint32) bool { return true }); err != nil {
		return err
	}
	stream.word(0x400d0003) // EndMessage.
	return stream.err
}
