// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

func createStoreMessage(ctx context.Context, session core.Session, state *contextState, op Operation, handles []uint32, creator mailDraftCreator) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Output, code) }
	objects := state.mailbox
	input, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if input.download != nil {
		return fail(NotSupported)
	}
	if op.Flags > 1 {
		return fail(0x80070057)
	}
	if !input.isLogon && input.fid != op.Folder {
		return fail(PropertyNotFound)
	}
	cp := uint32(op.CreateMessageCodePage)
	if cp == 0x0fff {
		cp = state.codePage
	}
	if cp != 1200 {
		if _, supported := propertyString8("", cp); !supported {
			return fail(0x8004011e)
		}
	}
	if len(objects.objects) >= maxLiveStoreHandles || objects.allocated >= maxContextHandleAllocations {
		return fail(0x8007000e)
	}
	message := &openedMessage{codePage: cp}
	store := input.binding.store
	privateConfig := op.Flags == 0 && store.isFreeBusyFolder(op.Folder)
	if op.Flags == 1 || privateConfig {
		draft, err := store.stageLocalMessage(ctx, session, op.Folder, privateConfig)
		if err != nil {
			return fail(faiError(err, false))
		}
		message.mid, message.draft = draft.newMID, draft
	} else {
		err := core.ErrUnsupported
		if creator != nil {
			var draft *mailDraft
			draft, err = store.stageMailDraft(ctx, session, op.Folder, creator)
			if err == nil {
				message.mid, message.mailDraft = draft.mid, draft
			}
		}
		if store.contactsMetadata && errors.Is(err, core.ErrUnsupported) {
			message.contactDraft, err = store.stageNewContact(ctx, session, op.Folder)
			if errors.Is(err, core.ErrNotFound) {
				err = core.ErrUnsupported
			}
		}
		if err != nil {
			return fail(hierarchyError(err))
		}
	}
	handle, err := objects.allocator.handle()
	if err != nil {
		message.draft.close()
		message.mailDraft.close()
		message.contactDraft.close()
		return fail(0x8007000e)
	}
	objects.objects[handle] = serverObject{
		logon: op.Logon, binding: input.binding, fid: op.Folder,
		message: message,
	}
	objects.allocated++
	handles[op.Output] = handle
	if message.mid == 0 {
		return append(fail(0), 0)
	}
	return binary.LittleEndian.AppendUint64(append(fail(0), 1), message.mid)
}

func (s *ramStore) stageLocalMessage(ctx context.Context, session core.Session, fid uint64, privateConfig bool) (*faiDraft, error) {
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	if err := s.authorizeLocalMessage(ctx, session, fid, privateConfig); err != nil {
		return nil, err
	}
	if privateConfig && s.freeBusyMessage().mid != 0 {
		return nil, core.ErrConflict
	}
	if !privateConfig && activeFAICount(s.fai.records) >= maxFAIObjects {
		return nil, core.ErrUnsupported
	}
	lease, err := s.reserveContentsMemory(maxFAIObjectBytes)
	if err != nil {
		return nil, err
	}
	// Allocate an ID, but leave the object and MID mapping invisible until Save.
	namespace := mapireplica.AssociatedMessage
	if privateConfig {
		namespace = mapireplica.Message
	}
	s.mu.Lock()
	mid, err := s.stageIDLocked(ctx, namespace)
	s.mu.Unlock()
	if err != nil {
		lease.release()
		return nil, err
	}
	source := s.hierarchyXID(mid)
	change := gidChangeXID(source)
	now := time.Now().UTC()
	return &faiDraft{
		store: s, root: fid, newMID: mid, id: faiIDForSource(source), lease: lease, dirty: true, privateConfig: privateConfig,
		request: faiImportRequest{
			source: source, change: change, modified: now,
			predecessors: faiPCL{change.namespace(): change.local()},
		},
		properties: core.Properties{
			{ID: 0x001a}: core.Text("IPM.Note"),
			{ID: 0x0017}: core.Integer(1),
			{ID: 0x0036}: core.Integer(0),
			{ID: 0x3007}: core.Timestamp(now),
			{ID: 0x67aa}: core.Bool(!privateConfig),
			core.IsRead:  core.Bool(false), core.LastModifiedAt: core.Timestamp(now),
		},
	}, nil
}
