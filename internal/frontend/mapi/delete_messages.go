// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"maps"

	"ewsgo/internal/core"
)

const (
	ropDeleteMessages           = 0x1e
	ropEmptyFolder              = 0x58
	ropHardDeleteMessages       = 0x91
	ropHardDeleteFolderContents = 0x92
	deleteMessagesResponseSize  = 7
)

type deleteMessagesRequest struct {
	async         bool
	notifyNonRead bool
	mids          []uint64
}

func parseDeleteMessages(r *reader, request *deleteMessagesRequest) error {
	header := r.take(4)
	if r.err != nil {
		return r.err
	}
	count := int(binary.LittleEndian.Uint16(header[2:]))
	wire := r.take(8 * count)
	if r.err != nil {
		return r.err
	}
	if count > core.MaxMailContentMessages {
		return ErrUnsupported
	}
	request.async, request.notifyNonRead = header[0] != 0, header[1] != 0
	var err error
	request.mids, err = decodeDistinctMessageIDs(wire)
	return err
}

func deleteMessages(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	reply := func(code uint32, partial bool) []byte {
		out := ropError(op.ID, op.Input, code)
		if partial {
			return append(out, 1)
		}
		return append(out, 0)
	}
	source, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return reply(code, false)
	}
	request := op.DeleteMessages
	if source.isLogon || source.download != nil || request.async {
		return reply(NotSupported, false)
	}
	store := source.binding.store
	if store.isDeferredActionFolder(source.fid) {
		// A DAM is an internal work item. It has no sender and cannot request
		// a receipt. Acknowledge only the DAM, never the original email.
		code, partial := deleteDeferredActionMessages(ctx, session, source, request.mids)
		return reply(code, partial)
	}
	if op.ID == ropHardDeleteMessages {
		if request.notifyNonRead {
			return reply(NotSupported, false)
		}
		partial, err := store.hardDeleteLocalMessages(ctx, session, source.fid, request.mids)
		if err != nil {
			return reply(storeError(err), false)
		}
		return reply(0, partial)
	}
	box := session.Mailbox()
	var apply func(context.Context, core.ID, core.Revision) error
	if request.notifyNonRead {
		if writer, ok := box.(core.MailMessageSoftDeleterWithNonReadReceipt); ok {
			apply = writer.SoftDeleteMailMessageWithNonReadReceipt
		}
	} else if writer, ok := box.(core.MailMessageSoftDeleter); ok {
		apply = writer.SoftDeleteMailMessage
	}
	if apply == nil {
		return reply(NotSupported, false)
	}
	folder, err := store.messageAnchor(ctx, session, source.fid, false)
	if err != nil {
		return reply(storeError(err), false)
	}
	if folder.native == "" {
		return reply(NotSupported, false)
	}
	code, partial := executeMessageBatch(ctx, request.mids, func(mid uint64) error {
		current, err := store.messagePlacementSource(ctx, box, folder.native, mid)
		if err != nil {
			return err
		}
		return apply(ctx, current.ID, current.Revision)
	})
	return reply(code, partial)
}

func (s *ramStore) hardDeleteLocalMessages(ctx context.Context, session core.Session, fid uint64, mids []uint64) (bool, error) {
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	anchor, err := s.messageAnchor(ctx, session, fid, true)
	if err != nil {
		return false, err
	}
	var targets []core.ID
	var partial bool
	s.mu.Lock()
	if s.dynamic[anchor.dynamic].search {
		s.mu.Unlock()
		return false, core.ErrUnsupported
	}
	for _, mid := range mids {
		if uint16(mid) != 1 {
			partial = true
			continue
		}
		key, known := s.byMID[mid]
		if !known && anchor.dynamic != 0 {
			// A removed or missing local MID has nothing left to delete.
			continue
		}
		if !key.associated {
			s.mu.Unlock()
			return false, core.ErrUnsupported // Never substitute native soft deletion.
		}
		record, ok := s.fai.records[key.id]
		if !ok || record.deleted {
			continue
		}
		if record.folder != fid {
			partial = true
			continue
		}
		targets = append(targets, key.id)
	}
	s.mu.Unlock()
	if len(targets) == 0 {
		return partial, nil
	}
	records := maps.Clone(s.fai.records)
	for _, id := range targets {
		record := records[id]
		record.deleted = true
		record.object.Revision++
		record.object.Properties = nil
		records[id] = record
	}
	_, err = s.commitFAIRecords(ctx, session, fid, records, "")
	return partial, err
}
