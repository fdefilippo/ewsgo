// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

type boundReplica struct {
	store   mapireplica.Store
	mailbox string
}

const (
	replicaNativeFolder  = 1
	replicaServiceFolder = 2
	replicaLocalFolder   = 3
)

func persistedFolderBinding(key folderKey, fid uint64) (mapireplica.Binding, error) {
	binding := mapireplica.Binding{Namespace: mapireplica.Folder, Counter: folderGlobalCount(fid)}
	switch {
	case key.dynamic != 0:
		if key.native != "" || key.role != 0 || key.dynamic != binding.Counter {
			return mapireplica.Binding{}, core.ErrInvalid
		}
		count := globalCount(binding.Counter)
		binding.Key = append([]byte{replicaLocalFolder}, count[:]...)
	case key.native != "" && key.role == 0 && len(key.native) <= maxHierarchyText:
		binding.Key = append([]byte{replicaNativeFolder}, key.native...)
	case key.native == "" && key.role < serviceFolderRoleCount:
		binding.Key = []byte{replicaServiceFolder, byte(key.role)}
	default:
		return mapireplica.Binding{}, core.ErrInvalid
	}
	return binding, nil
}

func decodePersistedFolder(key []byte, counter uint64) (folderKey, error) {
	if len(key) == 0 {
		return folderKey{}, core.ErrInvalid
	}
	switch key[0] {
	case replicaNativeFolder:
		if len(key) > 1 && len(key)-1 <= maxHierarchyText {
			return folderKey{native: core.ID(key[1:])}, nil
		}
	case replicaServiceFolder:
		if len(key) == 2 && SpecialFolderRole(key[1]) < serviceFolderRoleCount {
			return folderKey{role: SpecialFolderRole(key[1])}, nil
		}
	case replicaLocalFolder:
		if len(key) == 7 {
			count := globalCount(counter)
			if bytes.Equal(key[1:], count[:]) {
				return folderKey{dynamic: counter}, nil
			}
		}
	}
	return folderKey{}, core.ErrInvalid
}

func (s *ramStore) loadReplica(ctx context.Context, backing boundReplica) (err error) {
	if backing.store == nil || backing.mailbox == "" {
		return core.ErrInvalid
	}
	snapshot, err := backing.store.LoadMAPIReplica(ctx, backing.mailbox)
	if err != nil {
		return err
	}
	if snapshot.Version.Epoch == ([16]byte{}) || snapshot.Version.HighWater > maxGlobalCount || len(snapshot.Bindings) > maxRAMMappings {
		return core.ErrInvalid
	}
	defer func() {
		if err != nil {
			s.releaseReplicaLeases()
		}
	}()
	s.byMessage = make(map[messageKey]uint64)
	s.byMID = make(map[uint64]messageKey)
	s.recoverableMIDs = make(map[core.ID]uint64)
	s.recoverableByMID = make(map[uint64]recoverableReference)
	faiObjects := make(map[uint64]mapireplica.FAIObject, len(snapshot.FAI.Objects))
	for _, object := range snapshot.FAI.Objects {
		if _, exists := faiObjects[object.Counter]; exists {
			return core.ErrInvalid
		}
		faiObjects[object.Counter] = object
	}
	var freeBusyCounter uint64
	counters := make(map[uint64]bool, len(snapshot.Bindings))
	for _, binding := range snapshot.Bindings {
		id := folderID(binding.Counter)
		if counters[id] || binding.Counter == 0 || binding.Counter > snapshot.Version.HighWater {
			return core.ErrInvalid
		}
		counters[id] = true
		switch binding.Namespace {
		case mapireplica.Folder:
			key, err := decodePersistedFolder(binding.Key, binding.Counter)
			if err != nil || s.byKey[key] != 0 {
				return core.ErrInvalid
			}
			s.byKey[key], s.byFID[id] = id, key
		case mapireplica.Message:
			if object := faiObjects[binding.Counter]; object.PrivateConfig {
				if freeBusyCounter != 0 || !bytes.Equal(binding.Key, []byte{privateConfigurationKey}) {
					return core.ErrInvalid
				}
				freeBusyCounter = binding.Counter
				continue
			}
			if len(binding.Key) != 0 && binding.Key[0] == embeddedMessageKey {
				if len(binding.Key) != 33 {
					return core.ErrInvalid
				}
				key := [32]byte(binding.Key[1:])
				if s.embeddedMIDs[key] != 0 {
					return core.ErrInvalid
				}
				if s.embeddedMIDs == nil {
					s.embeddedMIDs = make(map[[32]byte]uint64)
				}
				s.embeddedMIDs[key] = id
				continue
			}
			key, err := decodePersistedMessage(binding.Key)
			if bytes.Equal(binding.Key, mapireplica.RetiredMessageKey(binding.Counter)) {
				key, err = messageKey{retired: binding.Counter}, nil
			}
			if err != nil || s.byMessage[key] != 0 {
				return core.ErrInvalid
			}
			s.byMessage[key], s.byMID[id] = id, key
		case mapireplica.AssociatedMessage:
			object, exists := faiObjects[binding.Counter]
			key, err := decodePersistedFAI(binding, object)
			if !exists || err != nil || s.byMessage[key] != 0 {
				return core.ErrInvalid
			}
			s.byMessage[key], s.byMID[id] = id, key
		case mapireplica.RecoverableMessage:
			key := core.ID(binding.Key)
			if len(key) == 0 || len(key) > maxHierarchyText || !validMailText(string(key)) || s.recoverableMIDs[key] != 0 {
				return core.ErrInvalid
			}
			s.recoverableMIDs[key] = id
			s.recoverableBytes += 96 + len(key)
		default:
			return core.ErrUnsupported
		}
	}
	s.hierarchy = make(map[folderKey]hierarchyVersion)
	s.messages = make(map[messageKey]messageVersion)
	var freeBusyVersion mapireplica.Projection
	for _, p := range snapshot.Projection {
		id := folderID(p.Counter)
		if p.ChangeNumber == 0 || p.ChangeNumber > snapshot.Version.HighWater || p.ReadNumber > snapshot.Version.HighWater {
			return core.ErrInvalid
		}
		var parent uint64
		if p.Parent != 0 {
			parent = folderID(p.Parent)
			if _, exists := s.byFID[parent]; !exists || parent == folderID(p.Counter) {
				return core.ErrInvalid
			}
		}
		change := changeXID(p.ChangeKey)
		pcl, err := parseFAIPCL(p.Predecessors)
		if err != nil || !change.valid() || !pcl.includesXID(change) {
			return core.ErrInvalid
		}
		if freeBusyCounter == p.Counter {
			freeBusyVersion = p
		} else if folder, mapped := s.byFID[id]; mapped {
			if _, seen := s.hierarchy[folder]; seen || p.ReadNumber != 0 || p.Read {
				return core.ErrInvalid
			}
			s.hierarchy[folder] = hierarchyVersion{
				fingerprint: p.Fingerprint, parent: parent, cn: p.ChangeNumber, observed: p.Modified,
				change: change, history: string(p.Predecessors), present: p.Present,
			}
		} else {
			if err := s.loadMessageProjection(snapshot.Version.Epoch, id, parent, p); err != nil {
				return err
			}
		}
	}
	if len(snapshot.Reserved) > maxIDSetRanges {
		return core.ErrUnsupported
	}
	for _, issued := range snapshot.Reserved {
		n := len(s.localReservations)
		if issued.First == 0 || issued.Last < issued.First || issued.Last > snapshot.Version.HighWater ||
			n != 0 && s.localReservations[n-1].high+1 >= issued.First {
			return core.ErrInvalid
		}
		s.localReservations = append(s.localReservations, idRange{issued.First, issued.Last})
	}
	if err := s.loadSpoolerQueue(snapshot.Spooler); err != nil {
		return err
	}
	if err := s.loadDraftReplica(snapshot.Drafts); err != nil {
		return err
	}
	if err := s.loadFolderCreations(snapshot.FolderCreations); err != nil {
		return err
	}
	if err := s.loadMessageMoves(snapshot.MessageMoves); err != nil {
		return err
	}
	if err := s.loadMessageImports(snapshot.MessageImports); err != nil {
		return err
	}
	if s.messageRegistryBytesLocked() > maxMessageRegistryBytes {
		return core.ErrUnsupported
	}
	if err := s.loadForeignReplicas(snapshot.Version.Epoch, snapshot.ForeignReplicas); err != nil {
		return err
	}
	if err := s.loadFolderValues(snapshot.FolderValues); err != nil {
		return err
	}
	if err := s.loadFAIReplica(snapshot.FAI, snapshot.Projection); err != nil {
		return err
	}
	if freeBusyCounter != 0 {
		if err := s.loadFreeBusyReplica(snapshot.Version.Epoch, faiObjects[freeBusyCounter], freeBusyVersion); err != nil {
			return err
		}
	}
	if len(s.recoverableMIDs) != 0 {
		if s.recoverableBytes > recoverableMappingBudget {
			return core.ErrUnsupported
		}
		lease, err := s.reserveContentsMemory(recoverableMappingBudget)
		if err != nil {
			return err
		}
		s.recoverableMIDLease = lease
	}
	s.durable = backing
	s.replica, s.next = snapshot.Version.Epoch, snapshot.Version.HighWater
	return nil
}

func (s *ramStore) releaseReplicaLeases() {
	s.fai.lease.release()
	s.localFreeBusyLease.release()
	s.recoverableMIDLease.release()
	s.foreignReplicaLease.release()
	for _, folder := range s.dynamic {
		folder.criteria.releaseStatic()
	}
}

// stageIDLocked commits a server-issued counter before sending it on the wire.
// The object remains invisible until the handle saves it.
func (s *ramStore) stageIDLocked(ctx context.Context, namespace mapireplica.Namespace) (uint64, error) {
	if s.next >= maxGlobalCount {
		return 0, core.ErrLimit
	}
	next := s.next + 1
	if s.durable.store != nil {
		if err := s.commitReplica(ctx, mapireplica.Mutation{HighWater: next, Stage: namespace}); err != nil {
			return 0, err
		}
	}
	s.next = next
	return folderID(next), nil
}

func (s *ramStore) commitReplica(ctx context.Context, mutation mapireplica.Mutation) error {
	return s.durable.store.CommitMAPIReplica(ctx, s.durable.mailbox, s.replicaPosition(), mutation)
}

func (s *ramStore) replicaPosition() mapireplica.Version {
	return mapireplica.Version{Epoch: s.replica, HighWater: s.next}
}

func (s *ramStore) commitHierarchyReplica(ctx context.Context, byKey map[folderKey]uint64, versions map[folderKey]hierarchyVersion, next uint64) error {
	if s.durable.store == nil || next == s.next {
		return nil
	}
	mutation := mapireplica.Mutation{HighWater: next}
	for key, fid := range byKey {
		if s.byKey[key] != 0 {
			continue
		}
		binding, err := persistedFolderBinding(key, fid)
		if err != nil {
			return err
		}
		mutation.Bindings = append(mutation.Bindings, binding)
	}
	for key, version := range versions {
		if version.cn <= s.next {
			continue
		}
		mutation.Projection = append(mutation.Projection, folderProjection(byKey[key], version))
	}
	return s.commitReplica(ctx, mutation)
}

func folderProjection(fid uint64, version hierarchyVersion) mapireplica.Projection {
	return mapireplica.Projection{
		Counter: folderGlobalCount(fid), Parent: folderGlobalCount(version.parent),
		ChangeNumber: version.cn, Fingerprint: version.fingerprint, Modified: version.observed,
		ChangeKey: []byte(version.change), Predecessors: []byte(version.history), Present: version.present,
	}
}

func (s *ramStore) advanceReplicaLocked(ctx context.Context, mutation mapireplica.Mutation) error {
	if s.durable.store == nil {
		return nil
	}
	if s.next >= maxGlobalCount {
		return core.ErrLimit
	}
	mutation.HighWater = s.next + 1
	if err := s.commitReplica(ctx, mutation); err != nil {
		return err
	}
	s.next = mutation.HighWater
	return nil
}
