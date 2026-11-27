// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"crypto/sha256"
	"encoding/binary"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const (
	embeddedResponseFields = 9 // Reserved byte and MessageId.
	embeddedMessageKey     = 0xfd
)

// The attachment numbers locate the nested MIME parts within one content
// revision of a native message. No MIME data is kept on the handle.
type embeddedMessageRef struct {
	rootMID uint64
	path    [maxMailPartDepth]uint32
	depth   int
}

func (m openedMessage) rootMessage() openedMessage {
	if m.embedded != nil {
		m.mid, m.embedded = m.embedded.rootMID, nil
	}
	return m
}

func (s *ramStore) bindEmbeddedMessage(ctx context.Context, ref embeddedMessageRef, revision core.Integer) (uint64, error) {
	var address [16 + 4*maxMailPartDepth]byte
	binary.LittleEndian.PutUint64(address[:8], ref.rootMID)
	binary.LittleEndian.PutUint64(address[8:16], uint64(revision))
	for i, number := range ref.path[:ref.depth] {
		binary.LittleEndian.PutUint32(address[16+4*i:], number)
	}
	key := sha256.Sum256(address[:16+4*ref.depth])
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if mid := s.embeddedMIDs[key]; mid != 0 {
		return mid, nil
	}
	if s.mappingCountLocked() >= s.mappingLimit() || s.next >= maxGlobalCount {
		return 0, core.ErrLimit
	}
	next := s.next + 1
	if s.durable.store != nil {
		binding := mapireplica.Binding{Namespace: mapireplica.Message, Key: append([]byte{embeddedMessageKey}, key[:]...), Counter: next}
		if err := s.commitReplica(ctx, mapireplica.Mutation{HighWater: next, Bindings: []mapireplica.Binding{binding}}); err != nil {
			return 0, err
		}
	}
	s.next = next
	if s.embeddedMIDs == nil {
		s.embeddedMIDs = make(map[[32]byte]uint64)
	}
	mid := folderID(next)
	s.embeddedMIDs[key] = mid
	return mid, nil
}

func (p projectedMessage) embeddedRow(mid uint64) messageRow {
	properties := core.Properties{
		core.Subject:        p.values[fixedContentsProperty(0x0037001f)],
		core.HasAttachments: core.Bool(len(p.attachments) != 0),
		core.IsRead:         core.Bool(false),
		core.MessageSize:    core.Integer(p.size),
	}
	return messageRow{mid: mid, object: core.Object{
		Kind: core.Message, Properties: properties, Recipients: p.embeddedRecipients,
	}}
}

func openEmbeddedMessage(ctx context.Context, session core.Session, state *contextState, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Output, code) }
	objects := state.mailbox
	input, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	attachment := input.attachment
	if attachment == nil {
		return fail(nullObject)
	}
	request := op.OpenMessage
	if request.mode > 2 {
		return fail(0x80070057)
	}
	cp := uint32(request.codePage)
	if cp == 0x0fff {
		cp = state.codePage
	}
	if _, ok := propertyString8("", cp); !ok {
		return fail(0x03ef)
	}
	if attachment.draft != nil && attachment.draft.calendar != nil {
		return openCalendarException(ctx, session, objects, input, op, handles, cp)
	}
	if request.mode != 0 {
		return fail(accessDenied)
	}
	message := attachment.message
	if attachment.draft != nil || message.mailDraft != nil || !message.contentVersioned {
		return fail(NotSupported)
	}
	if len(objects.objects) >= maxLiveStoreHandles || objects.allocated >= maxContextHandleAllocations {
		return fail(0x8007000e)
	}
	store := input.binding.store
	lease, err := store.reserveContentsPeak(ctx, 2*maxMailProjectionBytes)
	if err != nil {
		return fail(storeError(err))
	}
	defer lease.release()
	parent, err := store.readOpenedMailProjection(ctx, session, input.fid, message)
	if err != nil {
		return fail(attachmentError(err))
	}
	child, exists := parent.embedded[int(attachment.number)]
	if !exists {
		return fail(PropertyNotFound)
	}
	ref := embeddedMessageRef{rootMID: message.mid}
	if message.embedded != nil {
		ref = *message.embedded
	}
	if ref.depth == len(ref.path) {
		return fail(NotSupported)
	}
	ref.path[ref.depth] = attachment.number
	ref.depth++
	row := child.embeddedRow(0)
	reply, code := openMessageResponse(op.ID, op.Output, row, request.limit)
	if code != 0 {
		return fail(code)
	}
	handle, err := objects.allocator.handle()
	if err != nil {
		return fail(0x8007000e)
	}
	message.mid, err = store.bindEmbeddedMessage(ctx, ref, message.contentRevision)
	if err != nil {
		return fail(storeError(err))
	}
	message.embedded, message.codePage = &ref, cp
	message.recipientDigest = digestRecipients(row.object.Recipients)
	objects.objects[handle] = serverObject{logon: op.Logon, binding: input.binding, fid: input.fid, message: &message}
	objects.allocated++
	handles[op.Output] = handle
	response := binary.LittleEndian.AppendUint64(append(reply[:6:6], 0), message.mid)
	return append(response, reply[6:]...)
}
