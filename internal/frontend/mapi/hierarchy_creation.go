// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const maxPendingHierarchyCreations = 32

type hierarchyCreation struct {
	placement core.MailFolderPlacement
	change    changeXID
	native    core.ID
}

func (hierarchyCreation) String() string   { return "hierarchyCreation{private}" }
func (hierarchyCreation) GoString() string { return "hierarchyCreation{private}" }

// The collector and hierarchy locks are already held. Claim the client FID
// before SOAP I/O so a concurrent FAI upload cannot turn it into a MID.
func (s *hierarchyState) importNewMailFolder(ctx context.Context, box core.Mailbox, writer core.MailFolderHierarchyWriter, fid uint64, placement core.MailFolderPlacement, change changeXID, history faiPCL, budget int, now time.Time, pending hierarchyCreation, retry bool) error {
	if retry && (pending.placement != placement || pending.change != change || pending.native == "") {
		// Without a native acknowledgment there is no safe object to adopt or
		// retry. Name equality is not evidence of which request created it.
		return core.ErrConflict
	}
	store := s.store
	if err := hierarchyOwner(ctx, box); err != nil {
		return err
	}
	store.mu.Lock()
	if !retry {
		if _, claimed := store.byMID[fid]; claimed {
			store.mu.Unlock()
			return core.ErrConflict
		}
		if len(store.pendingCreations) >= maxPendingHierarchyCreations || store.mappingCountLocked() >= maxRAMMappings {
			store.mu.Unlock()
			return hierarchyImportLimit
		}
	} else if prior := store.hierarchy[folderKey{native: pending.native}]; prior.present {
		// An authorized intervening observation may have already seen the
		// acknowledged native object. Keep its independently assigned version
		// as a predecessor of the reconciled creation.
		previous, err := parseFAIPCL([]byte(prior.history))
		if err == nil {
			history, err = mergeFAIPCL(history, previous)
		}
		if err != nil {
			store.mu.Unlock()
			return err
		}
	}
	rawHistory := history.marshal()
	if budget+160+maxHierarchyText+384+3*len(placement.Name)+2*(len(rawHistory)+len(change)) > maxHierarchyBytes {
		store.mu.Unlock()
		return hierarchyImportLimit
	}
	cn, seen, err := s.reserveHierarchyCNLocked(true)
	if err != nil {
		store.mu.Unlock()
		return err
	}
	if !retry {
		pending = hierarchyCreation{placement: placement, change: change}
		claim := persistedFolderCreation(fid, pending)
		if err := store.advanceReplicaLocked(ctx, mapireplica.Mutation{FolderCreation: &claim}); err != nil {
			store.mu.Unlock()
			return err
		}
		if store.pendingCreations == nil {
			store.pendingCreations = make(map[uint64]hierarchyCreation)
		}
		store.pendingCreations[fid] = pending
		store.unboundCreations++
	}
	store.mu.Unlock()

	var native core.Object
	if retry {
		native, err = box.Get(ctx, pending.native)
		if err != nil {
			return err
		}
	} else {
		if err := ctx.Err(); err != nil {
			store.dropUnboundCreation(ctx, fid)
			return err
		}
		native, err = writer.CreateMailFolder(ctx, placement)
		if native.ID == "" {
			if errors.Is(err, core.ErrConflict) {
				store.dropUnboundCreation(ctx, fid)
			}
			if err == nil {
				return core.ErrInvalid
			}
			return err
		}
		pending.native = native.ID
	}
	if native.ID != pending.native || native.Kind != core.Folder || native.ID == placement.Parent ||
		native.ID == store.root || len(native.ID) > maxHierarchyText {
		return core.ErrInvalid
	}
	key, bindErr := store.bindAcknowledgedCreation(ctx, fid, pending)
	if bindErr != nil {
		return bindErr
	}
	if err != nil {
		return err
	}
	if native.Parent != placement.Parent || native.Properties[core.DisplayName] != core.Text(placement.Name) ||
		native.Properties[core.WellKnown] != nil {
		return core.ErrConflict
	}
	store.mu.Lock()
	metadata := store.metadata[fid]
	store.mu.Unlock()
	fingerprint, err := importFolderFingerprint(native, key.native, store, metadata)
	if err != nil {
		return err
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.durable.store != nil {
		cn, seen, err = s.reserveHierarchyCNLocked(true)
		if err != nil {
			return err
		}
	}
	if old := store.hierarchy[key]; now.Before(old.observed) {
		now = old.observed
	}
	version := hierarchyVersion{
		fingerprint: fingerprint, parent: store.byKey[folderKey{native: placement.Parent}],
		cn: cn, observed: now, present: true, change: change, history: string(rawHistory),
	}
	if err := store.commitImportedFolder(ctx, key, fid, version, true); err != nil {
		return err
	}
	delete(store.pendingCreations, fid)
	s.seen = seen
	return nil
}

func (s *ramStore) dropUnboundCreation(ctx context.Context, fid uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	// The backend was not called or definitively rejected creation. Clear
	// the claim even when the request ended, subject to a short DB deadline.
	if s.durable.store != nil {
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		if err := s.advanceReplicaLocked(ctx, mapireplica.Mutation{ForgetFolderCreation: folderGlobalCount(fid)}); err != nil {
			return
		}
	}
	delete(s.pendingCreations, fid)
	s.unboundCreations--
}

// A known SOAP identity can be reserved immediately even when readback
// failed. OpenFolder still requires a current authorized native read. Without
// this binding a concurrent downloader could assign a second FID to that ID.
func (s *ramStore) bindAcknowledgedCreation(ctx context.Context, fid uint64, pending hierarchyCreation) (folderKey, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := folderKey{native: pending.native}
	if other := s.byKey[key]; other != 0 && other != fid {
		return folderKey{}, core.ErrConflict
	}
	if previous, bound := s.byFID[fid]; bound {
		if previous != key {
			return folderKey{}, core.ErrConflict
		}
		return key, nil
	}
	if s.durable.store != nil {
		binding, err := persistedFolderBinding(key, fid)
		if err != nil {
			return folderKey{}, err
		}
		claim := persistedFolderCreation(fid, pending)
		ctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
		defer cancel()
		if err := s.advanceReplicaLocked(ctx, mapireplica.Mutation{
			ReservedBindings: []mapireplica.Binding{binding}, FolderCreation: &claim,
		}); err != nil {
			return folderKey{}, err
		}
	}
	s.pendingCreations[fid] = pending
	s.byKey[key], s.byFID[fid] = fid, key
	s.unboundCreations--
	return key, nil
}

func persistedFolderCreation(fid uint64, pending hierarchyCreation) mapireplica.FolderCreation {
	return mapireplica.FolderCreation{
		Counter: folderGlobalCount(fid), Parent: pending.placement.Parent,
		Name: pending.placement.Name, ChangeKey: []byte(pending.change), NativeID: pending.native,
	}
}

func (s *ramStore) loadFolderCreations(creations []mapireplica.FolderCreation) error {
	if len(creations) == 0 {
		return nil
	}
	s.pendingCreations = make(map[uint64]hierarchyCreation, len(creations))
	for _, creation := range creations {
		fid := folderID(creation.Counter)
		change := changeXID(creation.ChangeKey)
		key, bound := s.byFID[fid]
		if !change.valid() || !validMailText(creation.Name) ||
			bound && (creation.NativeID == "" || key != (folderKey{native: creation.NativeID})) ||
			!bound && creation.NativeID != "" {
			return core.ErrInvalid
		}
		s.pendingCreations[fid] = hierarchyCreation{
			placement: core.MailFolderPlacement{Parent: creation.Parent, Name: creation.Name},
			change:    change, native: creation.NativeID,
		}
		if !bound {
			s.unboundCreations++
		}
	}
	if s.mappingCountLocked() > maxRAMMappings {
		return core.ErrLimit
	}
	return nil
}
