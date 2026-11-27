// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"strings"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const recoverableMappingBudget = maxRAMMappings * 128

type recoverableReference struct {
	id, origin  core.ID
	inTrashView bool
}

func recoverableMailFolder(ctx context.Context, session core.Session, binding storeBinding, fid uint64) (core.ID, error) {
	key, err := binding.store.messageAnchor(ctx, session, fid, false)
	if err != nil {
		return "", err
	}
	if key.native == "" {
		return "", core.ErrUnsupported
	}
	if fid == binding.fids[DeletedItems] {
		return "", nil // Outlook opens its mailbox-wide recovery dialog from Trash.
	}
	return key.native, nil
}

func captureRecoverableTable(ctx context.Context, session core.Session, binding storeBinding, fid uint64, flags byte) (*contentsTable, error) {
	store := binding.store
	folder, err := recoverableMailFolder(ctx, session, binding, fid)
	if err != nil {
		return nil, err
	}
	reader, ok := session.Mailbox().(core.RecoverableMailReader)
	if !ok {
		return nil, core.ErrUnsupported
	}
	table := &contentsTable{flags: flags}
	fail := func(err error) (*contentsTable, error) { table.close(); return nil, err }
	table.lease, err = store.reserveContentsPeak(ctx, 2*maxMailProjectionBytes)
	if err != nil {
		return nil, err
	}
	items, err := reader.ReadRecoverableMail(ctx, folder)
	if err != nil {
		return fail(err)
	}
	if len(items) > core.MaxRecoverableMailMessages {
		return fail(core.ErrUnsupported)
	}
	references := make([]recoverableReference, 0, len(items))
	seen := make(map[core.ID]bool, len(items))
	used := 1024 + 4*maxTableColumns
	for _, item := range items {
		if item.Kind != core.Message || item.ID == "" || len(item.ID) > maxHierarchyText || !validMailText(string(item.ID)) ||
			item.Parent == "" || len(item.Parent) > maxHierarchyText || !validMailText(string(item.Parent)) ||
			folder != "" && item.Parent != folder || seen[item.ID] {
			return fail(core.ErrInvalid)
		}
		references = append(references, recoverableReference{id: item.ID, origin: item.Parent, inTrashView: folder == ""})
		seen[item.ID] = true
		values := contentsValues{}
		for _, field := range []struct {
			key core.Key
			tag uint32
		}{
			{core.Subject, 0x0037001f},
			{core.FromAddress, 0x0c1a001f},
			{core.FromAddress, 0x0c1f001f},
			{core.SentRepresentingName, 0x0042001f},
			{core.DisplayTo, 0x0e04001f},
		} {
			if value, ok := item.Properties[field.key].(core.Text); ok && validMailText(string(value)) {
				putContentsValue(values, field.tag, value)
				used += 2*len(value) + 128
			}
		}
		for _, field := range []struct {
			key core.Key
			tag uint32
		}{{core.ReceivedAt, 0x0e060040}, {core.DeletedOn, 0x668f0040}} {
			if stamp, ok := item.Properties[field.key].(core.Timestamp); ok && time.Time(stamp).Year() >= 1601 && time.Time(stamp).Year() <= 9999 {
				putContentsValue(values, field.tag, stamp)
			}
		}
		if size, ok := item.Properties[core.MessageSize].(core.Integer); ok && size >= 0 && size <= 0x7fffffff {
			putContentsValue(values, 0x0e080003, size)
		}
		putContentsValue(values, 0x001a001f, core.Text("IPM.Note"))
		putContentsValue(values, 0x67480014, core.Integer(fid))
		used += 1536
		if used > maxMailProjectionBytes {
			return fail(core.ErrUnsupported)
		}
		table.rows = append(table.rows, values)
	}
	again, err := recoverableMailFolder(ctx, session, binding, fid)
	if err != nil {
		return fail(err)
	}
	if again != folder {
		return fail(core.ErrConflict)
	}
	if err := table.lease.shrink(used); err != nil {
		return fail(err)
	}
	mids, err := store.assignRecoverableMIDs(ctx, references)
	if err != nil {
		return fail(err)
	}
	for i, mid := range mids {
		values := table.rows[i]
		putContentsValue(values, 0x674a0014, core.Integer(mid))
		putTableRowIdentity(values, mid)
		entryID := marshalMessageEntryID(binding.mailboxGUID, store.replica, fid, mid)
		putContentsValue(values, 0x0fff0102, core.Binary(entryID))
		putContentsValue(values, 0x0e090102, core.Binary(marshalFolderEntryID(binding.mailboxGUID, store.replica, fid)))
		putContentsValue(values, 0x0ff60102, core.Binary(binary.LittleEndian.AppendUint64(nil, mid)))
	}
	return table, nil
}

// Recoverable identities occupy the same counter space as active messages,
// but stay outside the maps used by download and ordinary write operations.
func (s *ramStore) assignRecoverableMIDs(ctx context.Context, references []recoverableReference) ([]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	newMappings, newBytes := 0, 0
	for _, ref := range references {
		if mid := s.recoverableMIDs[ref.id]; mid == 0 {
			newMappings++
			newBytes += 96 + len(ref.id)
		} else if prior, authorized := s.recoverableByMID[mid]; authorized {
			if prior.origin != ref.origin {
				return nil, core.ErrConflict
			}
			continue
		}
		newBytes += len(ref.origin)
	}
	if s.mappingCountLocked()+newMappings > s.mappingLimit() || s.next+uint64(newMappings) >= 1<<48 || newBytes > recoverableMappingBudget-s.recoverableBytes {
		return nil, core.ErrUnsupported
	}
	var lease *contentsLease
	if len(references) > 0 && s.recoverableMIDLease == nil {
		var err error
		lease, err = s.reserveContentsMemory(recoverableMappingBudget)
		if err != nil {
			return nil, err
		}
		if err := ctx.Err(); err != nil {
			lease.release()
			return nil, err
		}
	}
	next := s.next
	mutation := mapireplica.Mutation{}
	result := make([]uint64, len(references))
	for i, ref := range references {
		mid := s.recoverableMIDs[ref.id]
		if mid == 0 {
			next++
			mid = folderID(next)
			if s.durable.store != nil {
				mutation.Bindings = append(mutation.Bindings, mapireplica.Binding{
					Namespace: mapireplica.RecoverableMessage, Counter: next, Key: []byte(ref.id),
				})
			}
		}
		result[i] = mid
	}
	if len(mutation.Bindings) != 0 {
		mutation.HighWater = next
		if err := s.commitReplica(ctx, mutation); err != nil {
			lease.release()
			return nil, err
		}
	}
	if len(references) > 0 && s.recoverableMIDs == nil {
		s.recoverableMIDs = make(map[core.ID]uint64, len(references))
		s.recoverableByMID = make(map[uint64]recoverableReference, len(references))
	}
	if lease != nil {
		s.recoverableMIDLease = lease
	}
	for i, ref := range references {
		mid := result[i]
		if old, ok := s.recoverableByMID[mid]; ok {
			ref = recoverableReference{id: old.id, origin: old.origin, inTrashView: old.inTrashView || ref.inTrashView}
		} else {
			ref.id = core.ID(strings.Clone(string(ref.id)))
			ref.origin = core.ID(strings.Clone(string(ref.origin)))
			s.recoverableMIDs[ref.id] = mid
		}
		s.recoverableByMID[mid] = ref
	}
	s.next = next
	s.recoverableBytes += newBytes
	return result, nil
}

func (s *ramStore) recoverableMessage(mid uint64) (recoverableReference, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref, ok := s.recoverableByMID[mid]
	return ref, ok
}

const messageEntryIDBytes = 70

func marshalMessageEntryID(mailbox, replica [16]byte, fid, mid uint64) []byte {
	out := make([]byte, messageEntryIDBytes)
	copy(out, marshalFolderEntryID(mailbox, replica, fid))
	binary.LittleEndian.PutUint16(out[20:], 7)
	copy(out[46:62], replica[:])
	wire := binary.LittleEndian.AppendUint64(nil, mid)
	copy(out[62:68], wire[2:])
	return out
}
