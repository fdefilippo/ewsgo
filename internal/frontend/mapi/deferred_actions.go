// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"maps"
	"sort"

	"ewsgo/internal/core"
)

const (
	damClass       = "IPC.Microsoft Exchange 4.0.Deferred Action"
	damActions     = 0x66450102
	damOriginal    = 0x66460102
	damBackPatched = 0x6647000b
	damRuleFolder  = 0x66510102
	damRuleIDs     = 0x66750102
	damServerEntry = 0x674100fb
	damRuleFID     = 0x67420014 // Outlook also requests the numeric FID of the rule's folder.
	damCreated     = 0x30070040
	hasDAMs        = 0x3fea000b
)

var damTags = [...]uint32{damActions, damOriginal, damBackPatched, damRuleFolder, damRuleIDs, ruleProvider, damServerEntry, damRuleFID, damCreated}

type deferredActionRequest struct {
	server, client []byte
}

func (s *ramStore) isDeferredActionFolder(fid uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return fid != 0 && s.byFID[fid] == (folderKey{role: DeferredAction})
}

func (s *ramStore) publishDeferredActionChangesLocked(view messageSnapshot, byMID map[uint64]messageKey, removed []uint64) {
	var events []objectNotification
	for _, row := range view.messages {
		old := s.messages[byMID[row.mid]]
		switch {
		case !old.present:
			events = append(events, objectNotification{flag: notifyCreated | notifyMessage, fid: view.root, mid: row.mid})
		case old.cn != row.cn:
			events = append(events, objectNotification{flag: notifyModified | notifyMessage, fid: view.root, mid: row.mid})
		}
	}
	for _, mid := range removed {
		events = append(events, objectNotification{flag: notifyDeleted | notifyMessage, fid: view.root, mid: mid})
	}
	sort.Slice(events, func(i, j int) bool { return events[i].mid < events[j].mid })
	s.localNotifications.publish(events...)
}

func deferredActionNotificationTarget(targets []objectNotificationTarget) *objectNotificationTarget {
	for i := range targets {
		if targets[i].sub.deferred != 0 {
			return &targets[i]
		}
	}
	return nil
}

func (s *ramStore) signalDeferredActionChange() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.localNotifications.signal()
}

func deferredActionMailbox(ctx context.Context, session core.Session, store *ramStore, fid uint64) (core.DeferredActionMailbox, error) {
	if !store.isDeferredActionFolder(fid) {
		return nil, core.ErrNotFound
	}
	if _, err := store.messageAnchor(ctx, session, fid, false); err != nil {
		return nil, err
	}
	backend, ok := session.Mailbox().(core.DeferredActionMailbox)
	if !ok {
		return nil, core.ErrUnsupported
	}
	return backend, nil
}

func updateDeferredActionMessages(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	logon, code := objects.logonObject(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	store := logon.binding.store
	backend, err := deferredActionMailbox(ctx, session, store, logon.binding.fids[DeferredAction])
	if err != nil {
		return fail(storeError(err))
	}
	server, client := op.DeferredAction.server, op.DeferredAction.client
	if len(server) > core.MaxDeferredActionBytes || len(client) > core.MaxDeferredActionBytes ||
		!validScalarServerID(server) || !validScalarServerID(client) || len(server) < 2 || len(client) < 2 {
		return fail(0x80070057)
	}
	var folder, message core.ID
	if server[0] == 1 {
		fid, mid := binary.LittleEndian.Uint64(server[1:]), binary.LittleEndian.Uint64(server[9:])
		if uint16(fid) != 1 || fid>>16 == 0 || uint16(mid) != 1 || mid>>16 == 0 {
			return fail(0x80070057)
		}
		store.mu.Lock()
		f, m := store.byFID[fid], store.byMID[mid]
		store.mu.Unlock()
		if f.native != "" && m.id != "" && !m.associated && m.special == 0 && m.retired == 0 {
			folder, message = f.native, m.id
		}
	}
	err = backend.PatchDeferredActions(ctx, folder, message, server, client)
	if err == nil {
		store.signalDeferredActionChange()
	}
	return fail(storeErrorOrSuccess(err))
}

func (s *ramStore) observeDeferredActions(ctx context.Context, session core.Session, fid uint64) (messageSnapshot, error) {
	return s.observeMessageSet(ctx, session, fid, false, func(ctx context.Context, box core.Mailbox, _ folderKey) ([]core.Object, error) {
		backend, ok := box.(core.DeferredActionMailbox)
		if !ok {
			return nil, nil
		}
		records, err := backend.ReadDeferredActions(ctx)
		if errors.Is(err, core.ErrUnsupported) {
			return nil, nil
		}
		if err != nil {
			return nil, err
		}
		catalog, err := privateFolders(ctx, session)
		if err != nil {
			return nil, err
		}
		keys := []folderKey{{native: catalog.folders[Inbox].NativeID}}
		for _, record := range records {
			if record.Folder == "" || record.Message == "" || record.ID == record.Message || record.RuleID == 0 ||
				!core.ValidDeferredActions(record.Actions) || len(record.ClientEntry) != 0 && !validScalarServerID(record.ClientEntry) ||
				len(record.ClientEntry) > core.MaxDeferredActionBytes || !core.ValidInboxRuleProvider(record.Provider) {
				return nil, core.ErrInvalid
			}
			keys = append(keys, folderKey{native: record.Folder})
		}
		fids, err := s.assign(ctx, keys)
		if err != nil {
			return nil, err
		}
		rows := make([]core.Object, 0, len(records))
		for i, record := range records {
			s.mu.Lock()
			mid, err := s.nativeMessageMIDLocked(ctx, record.Message, true)
			s.mu.Unlock()
			if err != nil {
				return nil, err
			}
			origin := fids[i+1]
			server := deferredServerID(origin, mid)
			if len(record.ClientEntry) != 0 {
				server = record.ClientEntry
			}
			ms := record.Modified.UnixMilli()
			properties := core.Properties{
				core.IsRead: core.Bool(false), core.LastModifiedAt: core.Timestamp(record.Modified),
				damKey(damCreated): core.Timestamp(record.Created), damKey(damRuleFID): core.Integer(fids[0]),
				core.MetadataRevision: core.Integer(ms), core.ContentRevision: core.Integer(ms), core.VersionedMIME: core.Bool(true),
				core.Key{ID: 0x001a}: core.Text(damClass), damKey(damActions): core.Binary(record.Actions),
				damKey(damServerEntry): core.Binary(server), damKey(damBackPatched): core.Bool(len(record.ClientEntry) != 0),
				damKey(damRuleIDs):  core.Binary(binary.LittleEndian.AppendUint64(nil, record.RuleID)),
				damKey(damOriginal): core.Binary(append64Pair(origin, mid)), damKey(damRuleFolder): core.Integer(fids[0]),
				damKey(ruleProvider): core.Text(core.EffectiveInboxRuleProvider(record.Provider)),
			}
			rows = append(rows, core.Object{ID: record.ID, Kind: core.Message, Revision: core.Revision(ms), Properties: properties})
		}
		return rows, nil
	})
}

func append64Pair(fid, mid uint64) []byte {
	return binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint64(nil, fid), mid)
}

func deferredServerID(fid, mid uint64) []byte {
	return append32(append([]byte{1}, append64Pair(fid, mid)...), 0)
}

func damKey(tag uint32) core.Key { return core.Key{ID: tag >> 16} }

func isDeferredAction(row messageRow) bool {
	return row.object.Properties[core.Key{ID: 0x001a}] == core.Text(damClass) && !row.associated && !row.privateConfig
}

func deferredActionProperty(row messageRow, binding storeBinding, tag uint32) core.Value {
	value := row.object.Properties[damKey(tag)]
	switch tag {
	case damOriginal:
		pair := value.(core.Binary)
		fid, mid := binary.LittleEndian.Uint64(pair), binary.LittleEndian.Uint64(pair[8:])
		value = core.Binary(marshalMessageEntryID(binding.mailboxGUID, binding.store.replica, fid, mid))
	case damRuleFolder:
		fid := uint64(value.(core.Integer))
		value = core.Binary(marshalFolderEntryID(binding.mailboxGUID, binding.store.replica, fid))
	}
	return value
}

func deleteDeferredActionMessages(ctx context.Context, session core.Session, source serverObject, mids []uint64) (uint32, bool) {
	store := source.binding.store
	backend, err := deferredActionMailbox(ctx, session, store, source.fid)
	if err != nil {
		return storeError(err), false
	}
	view, err := store.observeDeferredActions(ctx, session, source.fid)
	if err != nil {
		return storeError(err), false
	}
	changed := false
	code, partial := executeMessageBatch(ctx, mids, func(mid uint64) error {
		for _, row := range view.messages {
			if row.mid == mid {
				err := backend.DeleteDeferredAction(ctx, row.object.ID)
				changed = changed || err == nil
				return err
			}
		}
		return core.ErrNotFound
	})
	if changed {
		store.signalDeferredActionChange()
	}
	return code, partial
}

func openedMessageHasDeferredActions(ctx context.Context, session core.Session, row messageRow) folderPropertyValue {
	backend, ok := session.Mailbox().(core.DeferredActionMailbox)
	if !ok || row.object.ID == "" || row.object.Parent == "" {
		return folderPropertyValue{kind: 0x000a, code: PropertyNotFound}
	}
	targets, err := backend.DeferredActionTargets(ctx)
	if errors.Is(err, core.ErrUnsupported) {
		return folderPropertyValue{kind: 0x000a, code: PropertyNotFound}
	}
	if err != nil {
		return folderPropertyValue{kind: 0x000a, code: storeError(err)}
	}
	if _, pending := targets[row.object.ID]; pending {
		return folderPropertyValue{kind: 0x000b, data: []byte{1}}
	}
	return folderPropertyValue{kind: 0x000b, data: []byte{0}}
}

func annotateDeferredActionTargets(ctx context.Context, box core.Mailbox, objects []core.Object) ([]core.Object, error) {
	backend, ok := box.(core.DeferredActionMailbox)
	if !ok || len(objects) == 0 {
		return objects, nil
	}
	targets, err := backend.DeferredActionTargets(ctx)
	if errors.Is(err, core.ErrUnsupported) {
		return objects, nil
	}
	if err != nil {
		return nil, err
	}
	for i := range objects {
		properties := maps.Clone(objects[i].Properties)
		if properties == nil {
			properties = make(core.Properties, 1)
		}
		_, pending := targets[objects[i].ID]
		properties[core.DeferredActionsPending] = core.Bool(pending)
		objects[i].Properties = properties
	}
	return objects, nil
}
