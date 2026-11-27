// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"

	"ewsgo/internal/core"
)

const attachDataBinary uint32 = 0x37010102

const (
	ropWriteStream          byte = 0x2d
	ropSetStreamSize        byte = 0x2f
	ropCommitStream         byte = 0x5d
	ropCopyToStream         byte = 0x3a
	ropCloneStream          byte = 0x3b
	ropLockRegionStream     byte = 0x5b
	ropUnlockRegionStream   byte = 0x5c
	ropWriteAndCommitStream byte = 0x90
	copyToStreamMaxResponse      = 26
)

var errStreamReadCount = errors.New("invalid packed RopReadStream count")

type streamRequest struct {
	tag          uint32
	mode, origin byte
	offset       int64
	count, limit uint32
	size         uint64
	destination  byte
	data         []byte
	packed       bool
	region       streamRegion
}

// Immutable MIME bytes and their lease are shared by stream handles within
// one Execute context. The context serializes all handle access and cleanup.
type streamContent struct {
	data       []byte
	lease      *contentsLease
	references int
}

type propertyStream struct {
	message    openedMessage
	attachment *openedAttachment
	content    *streamContent
	position   int64
	draft      *draftAttachment
	fai        *faiDraft
	version    uint64
	writable   bool
	locks      *streamLocks
	lockKey    streamLockKey
	lockOwner  *streamLockSession
}

func (*propertyStream) String() string   { return "propertyStream{private}" }
func (*propertyStream) GoString() string { return "propertyStream{private}" }

func (s *propertyStream) close() {
	if s == nil || s.content == nil {
		return
	}
	if s.locks != nil {
		s.locks.release(s)
	}
	s.content.references--
	if s.content.references == 0 {
		s.content.lease.release()
	}
	s.content = nil
}

func parseAttachmentStream(r *reader, op *Operation, handles int) error {
	request := &op.Stream
	switch op.ID {
	case 0x2b:
		wire := r.take(6)
		if r.err != nil {
			return r.err
		}
		op.Output = wire[0]
		if int(op.Output) >= handles {
			return ErrMalformed
		}
		request.tag, request.mode = binary.LittleEndian.Uint32(wire[1:]), wire[5]
	case 0x2c:
		wire := r.take(2)
		if r.err != nil {
			return r.err
		}
		request.count = uint32(binary.LittleEndian.Uint16(wire))
		if request.count == 0xbabe {
			wire = r.take(4)
			if r.err != nil {
				return r.err
			}
			request.packed = true
			request.count = binary.LittleEndian.Uint32(wire)
			if request.count > 1<<31 {
				return errStreamReadCount
			}
		}
	case 0x2e:
		wire := r.take(9)
		if r.err != nil {
			return r.err
		}
		request.origin, request.offset = wire[0], int64(binary.LittleEndian.Uint64(wire[1:]))
	case ropWriteStream, ropWriteAndCommitStream:
		request.data = r.take(int(readRecipientUint16(r)))
	case ropSetStreamSize:
		wire := r.take(8)
		if r.err == nil {
			request.size = binary.LittleEndian.Uint64(wire)
		}
	case ropCopyToStream:
		wire := r.take(9)
		if r.err != nil || int(wire[0]) >= handles {
			return ErrMalformed
		}
		request.destination = wire[0]
		request.size = binary.LittleEndian.Uint64(wire[1:])
	case ropCloneStream:
		wire := r.take(1)
		if r.err != nil || int(wire[0]) >= handles {
			return ErrMalformed
		}
		op.Output = wire[0]
	case ropLockRegionStream, ropUnlockRegionStream:
		wire := r.take(20)
		if r.err == nil {
			request.region = streamRegion{
				offset: binary.LittleEndian.Uint64(wire), size: binary.LittleEndian.Uint64(wire[8:]),
				flags: binary.LittleEndian.Uint32(wire[16:]),
			}
		}
	}
	return r.err
}

func executePropertyStream(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) ([]byte, *contentsLease) {
	if op.ID == 0x2b {
		return openPropertyStream(ctx, session, objects, op, handles), nil
	}
	if op.ID == ropCopyToStream {
		return copyPropertyStream(ctx, session, objects, op, handles), nil
	}
	if op.ID == ropCloneStream {
		return clonePropertyStream(ctx, session, objects, op, handles), nil
	}
	fail := func(code uint32) ([]byte, *contentsLease) {
		response := ropError(op.ID, op.Input, code)
		if op.ID == 0x2c {
			response = append(response, 0, 0) // DataSize on error.
		}
		return response, nil
	}
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	stream := object.stream
	if stream == nil {
		return fail(NotSupported)
	}
	store := object.binding.store
	content, err := stream.authorizedContent(ctx, session, object)
	if err != nil {
		return fail(attachmentError(err))
	}

	size := int64(len(content))
	switch op.ID {
	case ropLockRegionStream, ropUnlockRegionStream:
		return fail(stream.adjustRegionLock(op.ID, op.Stream.region))
	case 0x5e:
		return append32(ropError(op.ID, op.Input, 0), uint32(size)), nil
	case 0x2e:
		var base int64
		switch op.Stream.origin {
		case 0:
		case 1:
			base = stream.position
		case 2:
			base = size
		default:
			return fail(0x80030057) // StreamInvalidParam.
		}
		offset := op.Stream.offset
		if offset < -base || offset > 1<<31-base {
			return fail(0x80030019) // StreamSeekError.
		}
		stream.position = base + offset
		return binary.LittleEndian.AppendUint64(ropError(op.ID, op.Input, 0), uint64(stream.position)), nil
	case 0x2c:
		remaining := max(0, size-stream.position)
		if remaining > 0 && op.Stream.count > 0 && op.Stream.limit == 0 {
			return fail(tableBufferTooSmall)
		}
		n := min(int64(op.Stream.limit), remaining)
		response := binary.LittleEndian.AppendUint16(ropError(op.ID, op.Input, 0), uint16(n))
		if n == 0 {
			return response, nil
		}
		if code := stream.checkReadLock(stream.position, n); code != 0 {
			return fail(code)
		}
		// Account the reply copies until the entire Execute is framed.
		lease, err := store.reserveContentsMemory(4*int(n) + 32)
		if err != nil {
			return fail(storeError(err))
		}
		next := stream.position + n
		response = append(response, content[stream.position:next]...)
		stream.position = next
		return response, lease
	case ropCommitStream:
		// Attachment and Message streams are committed with their parent.
		return fail(0)
	case ropWriteStream, ropWriteAndCommitStream, ropSetStreamSize:
		if !stream.writable {
			return fail(0x80030005) // StreamAccessDenied.
		}
		if stream.draft != nil {
			if err := stream.draft.editable(); err != nil {
				return fail(attachmentError(err))
			}
		}
		writing := op.ID != ropSetStreamSize
		if writing && len(op.Stream.data) == 0 {
			return binary.LittleEndian.AppendUint16(ropError(op.ID, op.Input, 0), 0), nil
		}
		newSize := op.Stream.size
		if writing {
			newSize = uint64(max(size, stream.position+int64(len(op.Stream.data))))
		}
		if newSize > uint64(stream.maxSize()) {
			return fail(0x80030070) // StreamMediumFull.
		}
		if stream.fai != nil {
			if err := stream.fai.editRuleStream(ctx, session, content, int(newSize), stream.position, op.Stream.data); err != nil {
				return fail(attachmentError(err))
			}
			if writing {
				stream.position += int64(len(op.Stream.data))
				return binary.LittleEndian.AppendUint16(ropError(op.ID, op.Input, 0), uint16(len(op.Stream.data))), nil
			}
			return fail(0)
		}
		if !writing {
			stream.draft.resize(int(newSize))
			if int64(newSize) > size {
				stream.draft.markUnwritten(int(size), int(newSize), true)
			}
			return fail(0)
		}
		// Writes update the opened attachment property directly. CommitStream
		// has no separate flush before SaveChangesAttachment and SaveChangesMessage.
		stream.write(op.Stream.data)
		return binary.LittleEndian.AppendUint16(ropError(op.ID, op.Input, 0), uint16(len(op.Stream.data))), nil
	}
	return fail(NotSupported)
}

func (s *propertyStream) authorizedContent(ctx context.Context, session core.Session, object serverObject) ([]byte, error) {
	if s.fai != nil {
		return s.fai.ruleStreamContent(ctx, session)
	}
	if s.draft == nil {
		err := object.binding.store.authorizeAttachmentContent(ctx, session, object.fid, s.message)
		return s.content.data, err
	}
	if err := s.draft.authorize(ctx, session); err != nil {
		return nil, err
	}
	if s.version != s.draft.generation {
		return nil, core.ErrConflict
	}
	return s.draft.data, nil
}

func clonePropertyStream(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	reply := func(code uint32) []byte { return ropError(op.ID, op.Output, code) }
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return reply(code)
	}
	if object.stream == nil {
		return reply(NotSupported)
	}
	if len(objects.objects) >= maxLiveStoreHandles || objects.allocated >= maxContextHandleAllocations {
		return reply(0x8007000e)
	}
	if _, err := object.stream.authorizedContent(ctx, session, object); err != nil {
		return reply(attachmentError(err))
	}
	handle, err := objects.allocator.handle()
	if err != nil {
		return reply(0x8007000e)
	}
	clone := *object.stream
	if clone.content != nil {
		clone.content.references++
	}
	object.stream = &clone
	objects.objects[handle] = object
	objects.allocated++
	handles[op.Output] = handle
	return reply(0)
}

// write is called after the stream and resulting size have been checked.
func (s *propertyStream) write(data []byte) {
	start := int(s.position)
	end := start + len(data)
	previous := len(s.draft.data)
	s.draft.resize(max(previous, end))
	if start > previous {
		s.draft.markUnwritten(previous, start, true)
	}
	copy(s.draft.data[start:end], data)
	s.draft.markUnwritten(start, end, false)
	s.position = int64(end)
}

func copyPropertyStream(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	reply := func(code uint32, copied uint64) []byte {
		out := ropError(op.ID, op.Input, code)
		if code == moveCopyNullDestination {
			out = append32(out, uint32(op.Stream.destination))
		}
		return binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint64(out, copied), copied)
	}
	source, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return reply(code, 0)
	}
	if source.stream == nil {
		return reply(NotSupported, 0)
	}
	target, code := objects.lookup(handles[op.Stream.destination], op.Logon)
	if code == nullObject {
		return reply(moveCopyNullDestination, 0)
	}
	if code != 0 {
		return reply(code, 0)
	}
	if target.stream == nil {
		return reply(NotSupported, 0)
	}
	if !target.stream.writable {
		return reply(accessDenied, 0)
	}
	content, err := source.stream.authorizedContent(ctx, session, source)
	if err != nil {
		return reply(attachmentError(err), 0)
	}
	targetContent, err := target.stream.authorizedContent(ctx, session, target)
	if err != nil {
		return reply(attachmentError(err), 0)
	}
	if target.stream.draft != nil {
		if err := target.stream.draft.editable(); err != nil {
			return reply(attachmentError(err), 0)
		}
	}
	start := source.stream.position
	available := max(int64(len(content))-start, 0)
	space := max(target.stream.maxSize()-target.stream.position, 0)
	count := int64(min(op.Stream.size, uint64(min(available, space))))
	if count == 0 {
		return reply(0, 0)
	}
	if code := source.stream.checkReadLock(start, count); code != 0 {
		return reply(code, 0)
	}
	// Growing an attachment reserves ranges before Outlook supplies the data.
	// A copy must not turn those unwritten ranges into a complete attachment.
	if draft := source.stream.draft; draft != nil && draft.hasUnwritten(int(start), int(start+count)) {
		return reply(0x8003001e, 0) // STG_E_READFAULT.
	}
	data := content[start : start+count]
	if target.stream.fai != nil {
		size := max(int64(len(targetContent)), target.stream.position+count)
		if err := target.stream.fai.editRuleStream(ctx, session, targetContent, int(size), target.stream.position, data); err != nil {
			return reply(attachmentError(err), 0)
		}
		target.stream.position += count
	} else {
		target.stream.write(data)
	}
	source.stream.position = start + count
	return reply(0, uint64(count))
}

func (a *draftAttachment) hasUnwritten(start, end int) bool {
	if a.unwritten == nil {
		return false
	}
	for start < end {
		next := min((start/64+1)*64, end)
		mask := (^uint64(0) >> (64 - (next - start))) << (start % 64)
		if a.unwritten[start/64]&mask != 0 {
			return true
		}
		start = next
	}
	return false
}

func openPropertyStream(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	result := func(code uint32) []byte { return ropError(op.ID, op.Output, code) }
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return result(code)
	}
	if object.attachment == nil && object.message == nil {
		return result(NotSupported)
	}
	if object.attachment != nil && object.attachment.draft != nil {
		if object.attachment.draft.calendar != nil {
			return result(NotSupported)
		}
		return openDraftAttachmentStream(ctx, session, objects, object, op, handles)
	}
	if object.message != nil && object.message.draft != nil && op.Stream.tag == rwRulesStreamTag {
		return openRuleStream(ctx, session, objects, object, op, handles)
	}
	switch op.Stream.mode {
	case 0:
	case 1, 2:
		return result(0x80030005) // StreamAccessDenied.
	default:
		return result(0x80070057)
	}
	tag := op.Stream.tag
	attachment := object.attachment
	if attachment != nil && tag != attachDataBinary {
		return result(PropertyNotFound)
	}
	if attachment == nil {
		if !object.message.contentVersioned {
			return result(NotSupported)
		}
		switch tag {
		case messageBodyID<<16 | 0x001e, messageBodyID<<16 | 0x001f, messageHTMLID<<16 | 0x0102, messageRTFTag:
		default:
			return result(PropertyNotFound)
		}
	}
	if len(objects.objects) >= maxLiveStoreHandles || objects.allocated >= maxContextHandleAllocations {
		return result(0x8007000e)
	}

	store := object.binding.store
	lease, err := store.reserveContentsPeak(ctx, 2*maxMailProjectionBytes)
	if err != nil {
		return result(storeError(err))
	}
	defer func() { lease.release() }()
	var message openedMessage
	var content []byte
	if attachment != nil {
		message = attachment.message
		rows, err := store.readOpenedAttachments(ctx, session, object.binding.mailboxGUID, object.fid, message)
		if err != nil {
			return result(attachmentError(err))
		}
		row, found := findAttachmentRow(rows, attachment.number)
		if !found {
			return result(PropertyNotFound)
		}
		payload, ok := row[fixedContentsProperty(tag)].(core.Binary)
		if !ok {
			return result(PropertyNotFound)
		}
		content = bytes.Clone(payload)
	} else {
		message = *object.message
		projection, err := store.readOpenedMailProjection(ctx, session, object.fid, message)
		if err != nil {
			return result(attachmentError(err))
		}
		var ok bool
		content, ok = messageBodyBytes(projection.values, tag, message.codePage)
		if !ok {
			return result(PropertyNotFound)
		}
	}
	if err = ctx.Err(); err != nil {
		return result(storeError(err))
	}
	handle, err := objects.allocator.handle()
	if err != nil {
		return result(0x8007000e)
	}
	if err = lease.shrink(cap(content) + 512); err != nil {
		return result(storeError(err))
	}
	stream := &propertyStream{message: message, attachment: attachment,
		content: &streamContent{data: content, lease: lease, references: 1}}
	if message.contentVersioned && message.embedded == nil && message.mailDraft == nil {
		stream.locks, stream.lockOwner = &store.streamLocks, &objects.streamLockSession
		stream.lockKey = streamLockKey{mid: message.mid, revision: message.contentRevision, tag: tag}
		if attachment != nil {
			stream.lockKey.attachment = attachment.number
		}
		if uint16(tag) == 0x001e {
			stream.lockKey.codePage = message.codePage
		}
	}
	objects.objects[handle] = serverObject{logon: op.Logon, binding: object.binding, fid: object.fid, stream: stream}
	objects.allocated++
	handles[op.Output] = handle
	lease = nil
	return append32(result(0), uint32(len(content)))
}

func openDraftAttachmentStream(ctx context.Context, session core.Session, objects *mailboxObjects, object serverObject, op Operation, handles []uint32) []byte {
	result := func(code uint32) []byte { return ropError(op.ID, op.Output, code) }
	if op.Stream.tag != attachDataBinary {
		return result(PropertyNotFound)
	}
	if op.Stream.mode > 2 {
		return result(0x80070057)
	}
	if len(objects.objects) >= maxLiveStoreHandles || objects.allocated >= maxContextHandleAllocations {
		return result(0x8007000e)
	}
	attachment := object.attachment.draft
	if err := attachment.authorize(ctx, session); err != nil {
		return result(attachmentError(err))
	}
	writable := op.Stream.mode != 0
	if writable {
		if err := attachment.editable(); err != nil {
			return result(attachmentError(err))
		}
	}
	if op.Stream.mode != 2 && !attachment.dataSet {
		return result(PropertyNotFound)
	}
	handle, err := objects.allocator.handle()
	if err != nil {
		return result(0x8007000e)
	}
	if op.Stream.mode == 2 {
		attachment.resize(0)
		attachment.generation++
	}
	objects.objects[handle] = serverObject{
		logon: op.Logon, binding: object.binding, fid: object.fid,
		stream: &propertyStream{attachment: object.attachment, draft: attachment, version: attachment.generation, writable: writable},
	}
	objects.allocated++
	handles[op.Output] = handle
	return append32(result(0), uint32(len(attachment.data)))
}

func (a *draftAttachment) resize(size int) {
	previous := len(a.data)
	if size > cap(a.data) {
		data := make([]byte, size, min(core.MaxMailContentMIMEBytes, size*2))
		copy(data, a.data)
		a.data = data
	} else {
		a.data = a.data[:size]
		clear(a.data[min(previous, size):])
	}
	if size < previous {
		a.markUnwritten(size, previous, false)
	}
	a.dataSet, a.dirty = true, true
}

// A stream resize declares space without supplying file data. Track the gaps
// separately so writes of actual zero bytes count and Seek can fill gaps later.
func (a *draftAttachment) markUnwritten(first, end int, unwritten bool) {
	if first == end {
		return
	}
	if a.unwritten == nil {
		if !unwritten {
			return
		}
		a.unwritten = make([]uint64, attachmentCoverageWords)
		if a.parent != nil {
			a.parent.incompleteAttachments++
		}
	}
	for i := first; i < end; i++ {
		word, mask := &a.unwritten[i/64], uint64(1)<<(i%64)
		if unwritten && *word&mask == 0 {
			*word |= mask
			a.unwrittenBytes++
		} else if !unwritten && *word&mask != 0 {
			*word &^= mask
			a.unwrittenBytes--
		}
	}
	if a.unwrittenBytes == 0 {
		a.clearUnwritten()
	}
}

func (a *draftAttachment) clearUnwritten() {
	if a.unwritten != nil {
		if a.parent != nil {
			a.parent.incompleteAttachments--
		}
		a.unwritten, a.unwrittenBytes = nil, 0
	}
}
