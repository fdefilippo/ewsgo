// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"

	"ewsgo/internal/core"
)

const (
	ropMoveCopyMessages         = 0x33
	moveCopyNullDestination     = 0x00000503
	moveCopyMessagesMaxResponse = 11
)

type moveCopyMessagesRequest struct {
	destination byte
	async       bool
	copy        bool
	mids        []uint64
}

func parseMoveCopyMessages(r *reader, request *moveCopyMessagesRequest, handleCount int) error {
	header := r.take(3)
	if r.err != nil {
		return r.err
	}
	request.destination = header[0]
	count := int(binary.LittleEndian.Uint16(header[1:]))
	if int(request.destination) >= handleCount {
		return ErrMalformed
	}
	wire := r.take(8 * count)
	flags := r.take(2)
	if r.err != nil {
		return r.err
	}
	if count > core.MaxMailContentMessages {
		return ErrUnsupported
	}
	if flags[0] > 1 || flags[1] > 1 {
		return ErrMalformed
	}
	request.async, request.copy = flags[0] != 0, flags[1] != 0
	var err error
	request.mids, err = decodeDistinctMessageIDs(wire)
	return err
}

func moveCopyMessages(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32, diagnostic *ExecutionDiagnostic) []byte {
	request := op.MoveCopyMessages
	reply := func(code uint32, partial bool) []byte {
		out := ropError(ropMoveCopyMessages, op.Input, code)
		if code == moveCopyNullDestination {
			out = append32(out, uint32(request.destination))
		}
		if partial {
			return append(out, 1)
		}
		return append(out, 0)
	}
	source, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return reply(code, false)
	}
	destination, code := objects.object(handles[request.destination], op.Logon)
	if code == nullObject {
		return reply(moveCopyNullDestination, false)
	}
	if code != 0 {
		return reply(code, false)
	}
	store := source.binding.store
	if source.isLogon || destination.isLogon || source.download != nil || destination.download != nil ||
		request.async || destination.binding.store != store {
		return reply(NotSupported, false)
	}
	from, err := store.messageAnchor(ctx, session, source.fid, false)
	if errors.Is(err, normalMetadataOnlyError{}) && destination.fid == source.binding.fids[DeletedItems] && !request.copy {
		code, partial := store.moveContactsToTrash(ctx, session, source.fid, destination.fid, request.mids)
		return reply(code, partial)
	}
	if err != nil {
		return reply(storeError(err), false)
	}
	to := from
	if destination.fid != source.fid {
		to, err = store.messageAnchor(ctx, session, destination.fid, false)
		if err != nil {
			return reply(storeError(err), false)
		}
	}
	if from.native == "" || to.native == "" {
		return reply(NotSupported, false)
	}
	box := session.Mailbox()
	writer, _ := box.(core.MailMessagePlacementWriter)
	recoverer, _ := box.(core.RecoverableMailWriter)
	if writer == nil && recoverer == nil {
		return reply(NotSupported, false)
	}

	code, partial := executeMessageBatch(ctx, request.mids, func(mid uint64) error {
		if ref, ok := store.recoverableMessage(mid); ok {
			diagnostic.recordRecoverableMoveCopy(request.copy)
			fromTrash := source.fid == source.binding.fids[DeletedItems] && ref.inTrashView
			if !fromTrash && ref.origin != from.native {
				return core.ErrNotFound
			}
			// Outlook's Restore Selected Items uses WantCopy from the Trash
			// recovery view. Native recovery preserves the item atomically.
			if recoverer == nil || (request.copy && !fromTrash) {
				return core.ErrUnsupported
			}
			return recoverer.RecoverMailMessage(ctx, ref.id, ref.origin, to.native)
		}
		if writer == nil {
			return core.ErrUnsupported
		}
		current, err := store.messagePlacementSource(ctx, box, from.native, mid)
		if err != nil {
			return err
		}
		place := writer.MoveMailMessage
		if request.copy {
			place = writer.CopyMailMessage
		}
		placed, err := place(ctx, current.ID, current.Revision, to.native)
		if err == nil && (placed.Kind != core.Message || placed.Parent != to.native || placed.ID == "" ||
			(placed.ID == current.ID) == request.copy) {
			return core.ErrOutcomeUnknown
		}
		return err
	})
	return reply(code, partial)
}

func (s *ramStore) messagePlacementSource(ctx context.Context, box core.Mailbox, folder core.ID, mid uint64) (core.Object, error) {
	s.mu.Lock()
	key, known := s.byMID[mid]
	move := s.messageMoves[mid]
	s.mu.Unlock()
	if move.Source != 0 && move.Result == 0 {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	if !known || key.associated || key.special != 0 || key.retired != 0 {
		return core.Object{}, core.ErrNotFound
	}
	current, err := box.Get(ctx, key.id)
	if err != nil {
		return core.Object{}, err
	}
	if current.ID != key.id || current.Kind != core.Message || current.Parent != folder {
		return core.Object{}, core.ErrNotFound
	}
	ms, ok := current.Properties[core.MetadataRevision].(core.Integer)
	if !ok || ms < 0 || uint64(ms) != uint64(current.Revision) {
		return core.Object{}, core.ErrUnsupported
	}
	return current, nil
}
