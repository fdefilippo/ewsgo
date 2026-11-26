// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"slices"

	"ewsgo/internal/core"
)

const (
	ropDeleteFolder          = 0x1d
	deleteFolderResponseSize = 7
	deleteFolderMessages     = 0x01
	deleteFolderSubfolders   = 0x04
	deleteFolderHard         = 0x10
)

type emptyFolderRequest struct {
	async            bool
	deleteAssociated bool
}

func emptyStoreFolder(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	folder, code := objects.object(handles[op.Input], op.Logon)
	if code == 0 {
		if folder.isLogon || folder.download != nil || op.EmptyFolder.async {
			code = NotSupported
		} else {
			err := folder.binding.store.emptyLocalFolder(ctx, session, folder.fid, op.EmptyFolder.deleteAssociated, op.ID == ropHardDeleteFolderContents)
			if err != nil {
				code = storeError(err)
			}
		}
	}
	return append(ropError(op.ID, op.Input, code), 0) // The local operation is atomic.
}

func (s *ramStore) emptyLocalFolder(ctx context.Context, session core.Session, fid uint64, deleteAssociated, hard bool) error {
	s.hierarchyMu.Lock()
	defer s.hierarchyMu.Unlock()
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()

	s.mu.Lock()
	root := s.byFID[fid]
	s.mu.Unlock()
	if root.dynamic == 0 {
		return core.ErrUnsupported
	}
	folder, err := s.folder(ctx, session, fid)
	if err != nil {
		return err
	}
	if folder.search {
		return core.ErrUnsupported // Search results can reference mail in other folders.
	}
	if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
		return err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}

	var targets []folderKey
	fids := make(map[uint64]bool)
	parents := []folderKey{root}
	for i := 0; i < len(parents); i++ {
		for id, child := range s.dynamic {
			if child.parent != parents[i] || child.softDeleted && !hard {
				continue
			}
			if child.search && !child.softDeleted {
				return core.ErrUnsupported
			}
			key := folderKey{dynamic: id}
			parents = append(parents, key)
			targets = append(targets, key)
			fids[s.byKey[key]] = true
		}
	}

	var removed []uint64
	if deleteAssociated {
		for id, record := range s.fai.records {
			if record.folder == fid && !record.deleted {
				if !hard {
					// Moving the selected folder's FAI into a recoverable
					// namespace requires a soft-deleted FAI representation.
					return core.ErrUnsupported
				}
				removed = append(removed, s.byMessage[messageKey{id: id, associated: true}])
			}
		}
	}
	if len(targets) == 0 && len(removed) == 0 {
		return nil
	}
	var clearAssociatedFID uint64
	if len(removed) != 0 {
		clearAssociatedFID = fid
	}
	if err := s.commitFolderRemovalLocked(ctx, targets, fids, hard, clearAssociatedFID); err != nil {
		return err
	}
	for i := len(targets) - 1; i >= 0; i-- {
		s.removeRAMFolderLocked(targets[i], hard)
	}
	if len(removed) != 0 {
		slices.Sort(removed)
		events := make([]objectNotification, 0, len(removed)+1)
		for _, mid := range removed {
			events = append(events, objectNotification{flag: notifyDeleted | notifyMessage, fid: fid, mid: mid})
		}
		s.localNotifications.publish(append(events, objectNotification{flag: notifyModified, fid: fid})...)
	}
	return nil
}

func parseDeleteFolder(r *reader, op *Operation) error {
	data := r.take(9)
	if r.err != nil {
		return r.err
	}
	op.Flags = data[0]
	op.Folder = binary.LittleEndian.Uint64(data[1:])
	return nil
}

func deleteStoreFolder(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	if op.Flags & ^byte(deleteFolderMessages|deleteFolderSubfolders|deleteFolderHard) != 0 {
		return append(ropError(op.ID, op.Input, 0x80070057), 0)
	}
	parent, code := objects.object(handles[op.Input], op.Logon)
	var partial bool
	if code == 0 {
		switch {
		case parent.isLogon || parent.download != nil:
			code = NotSupported
		case op.Folder == parent.binding.fids[MailboxRoot]:
			code = NotSupported
		default:
			partial, code = parent.binding.store.deleteRAMFolder(ctx, session, parent.fid, op.Folder, op.Flags)
		}
	}
	var completion byte
	if partial {
		completion = 1
	}
	return append(ropError(op.ID, op.Input, code), completion)
}

// Remove a local subtree only after checking every retained child and FAI
// against the request flags. Retain the FID mappings for hierarchy tombstones.
func (s *ramStore) deleteRAMFolder(ctx context.Context, session core.Session, parent, fid uint64, flags byte) (bool, uint32) {
	s.hierarchyMu.Lock()
	defer s.hierarchyMu.Unlock()
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()

	if _, err := s.folder(ctx, session, parent); err != nil {
		return false, storeError(err)
	}
	if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
		return false, storeError(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return false, storeError(err)
	}
	key, known := s.byFID[fid]
	if !known {
		return false, PropertyNotFound
	}
	if key.dynamic == 0 {
		if key.native == "" {
			return false, accessDenied
		}
		return false, NotSupported
	}
	folder, exists := s.dynamic[key.dynamic]
	if !exists || folder.parent != s.byFID[parent] {
		return false, PropertyNotFound
	}
	hard := flags&deleteFolderHard != 0
	if folder.softDeleted && !hard {
		return false, PropertyNotFound
	}
	targets := []folderKey{key}
	fids := map[uint64]bool{fid: true}
	for i := 0; i < len(targets); i++ {
		for id, child := range s.dynamic {
			if child.parent != targets[i] {
				continue
			}
			if flags&deleteFolderSubfolders == 0 {
				return true, 0
			}
			childKey := folderKey{dynamic: id}
			targets = append(targets, childKey)
			fids[s.byKey[childKey]] = true
		}
	}
	for _, record := range s.fai.records {
		// A soft-deleted folder retains its FAI, inaccessible through
		// the active hierarchy. A hard delete purges its stored payload.
		if fids[record.folder] && !record.deleted && flags&deleteFolderMessages == 0 {
			return true, 0
		}
	}
	if s.durable.store != nil {
		if err := s.commitFolderRemovalLocked(ctx, targets, fids, hard, 0); err != nil {
			return false, storeError(err)
		}
	} else if hard {
		if err := s.purgeDeletedFolderFAI(fids); err != nil {
			return false, storeError(err)
		}
	}
	for i := len(targets) - 1; i >= 0; i-- {
		s.removeRAMFolderLocked(targets[i], hard)
	}
	return false, 0
}

func (s *ramStore) removeRAMFolderLocked(key folderKey, hard bool) {
	folder := s.dynamic[key.dynamic]
	fid, parent := s.byKey[key], s.byKey[folder.parent]
	if folder.search {
		s.unlinkRemindersFolder(fid)
		folder.criteria.releaseStatic()
		folder.criteria = nil
	}
	if hard {
		delete(s.dynamic, key.dynamic)
		s.metadataBytes -= s.metadata[fid].accountedBytes()
		delete(s.metadata, fid)
		delete(s.ost, fid)
		s.dynamicBytes -= 128 + len(folder.name) + len(folder.comment) + len(folder.parent.native)
	} else {
		folder.softDeleted = true
		s.dynamic[key.dynamic] = folder
	}
	if !hard || !folder.softDeleted {
		s.localNotifications.publish(
			objectNotification{flag: notifyDeleted, fid: fid, parent: parent},
			objectNotification{flag: notifyModified, fid: parent},
		)
	}
}

// Called with both contentsMu and mu held. Keep allocated message IDs so
// stale clients cannot bind the retired IDs to another object.
func (s *ramStore) purgeDeletedFolderFAI(folders map[uint64]bool) error {
	remaining := s.fai.bytes
	for _, record := range s.fai.records {
		if folders[record.folder] {
			remaining -= record.bytes
		}
	}
	for _, fid := range s.fai.absent {
		if folders[fid] {
			remaining -= faiAbsentRecordBytes
		}
	}
	if remaining == s.fai.bytes {
		return nil
	}
	if err := s.fai.lease.shrink(remaining); err != nil {
		return err
	}
	for id, record := range s.fai.records {
		if folders[record.folder] {
			delete(s.fai.records, id)
			key := messageKey{id: id, associated: true}
			version := s.messages[key]
			version.present = false
			s.messages[key] = version
		}
	}
	for source, fid := range s.fai.absent {
		if folders[fid] {
			delete(s.fai.absent, source)
		}
	}
	s.fai.bytes = remaining
	return nil
}
