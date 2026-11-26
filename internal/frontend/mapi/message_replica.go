// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

func persistedMessageBinding(key messageKey, mid uint64) (mapireplica.Binding, error) {
	if key.associated || key.special > core.TasksFolder || key.id == "" || len(key.id) > maxHierarchyText {
		return mapireplica.Binding{}, core.ErrInvalid
	}
	return mapireplica.Binding{
		Namespace: mapireplica.Message, Counter: folderGlobalCount(mid),
		Key: append([]byte{byte(key.special)}, key.id...),
	}, nil
}

func decodePersistedMessage(raw []byte) (messageKey, error) {
	if len(raw) < 2 || len(raw)-1 > maxHierarchyText || core.SpecialFolderKind(raw[0]) > core.TasksFolder {
		return messageKey{}, core.ErrInvalid
	}
	return messageKey{id: core.ID(raw[1:]), special: core.SpecialFolderKind(raw[0])}, nil
}

func (s *ramStore) loadMessageProjection(epoch [16]byte, mid, parent uint64, p mapireplica.Projection) error {
	key, mapped := s.byMID[mid]
	if _, seen := s.messages[key]; seen || !mapped || parent == 0 || p.ReadNumber == 0 || key.retired != 0 && p.Present {
		return core.ErrInvalid
	}
	s.messages[key] = messageVersion{
		fingerprint: p.Fingerprint, cn: p.ChangeNumber, readCN: p.ReadNumber, parent: parent,
		nativeRevision: p.NativeRevision, modified: p.Modified, present: p.Present, read: p.Read,
	}
	own := hierarchyXID(epoch, p.ChangeNumber)
	if !key.associated && (!bytes.Equal(p.ChangeKey, own[:]) || !bytes.Equal(p.Predecessors, append([]byte{22}, own[:]...))) {
		v := s.messages[key]
		v.change, v.history = changeXID(p.ChangeKey), string(p.Predecessors)
		s.messages[key] = v
	}
	return nil
}

func (s *ramStore) commitMessageReplica(ctx context.Context, byKey map[messageKey]uint64, versions map[messageKey]messageVersion, next uint64, previousFAI *faiNamespace, reconciled map[string]core.ID) error {
	if s.durable.store == nil {
		return nil
	}
	mutation := mapireplica.Mutation{HighWater: next}
	acknowledgedDrafts := make(map[core.ID]uint64, len(reconciled))
	for reference, id := range reconciled {
		pending := s.pendingMailDrafts[reference]
		acknowledgedDrafts[id] = pending.mid
		s.addDraftAcknowledgment(&mutation, reference, pending, id)
	}
	for key, mid := range byKey {
		if s.byMessage[key] != 0 {
			continue
		}
		if key.associated {
			binding := persistedFAIBinding(mid)
			switch {
			case binding.Counter > s.next:
				mutation.Bindings = append(mutation.Bindings, binding)
			case s.clientReservedCounterLocked(binding.Counter):
				mutation.ReservedBindings = append(mutation.ReservedBindings, binding)
			default:
				mutation.StagedBindings = append(mutation.StagedBindings, binding)
			}
			continue
		}
		binding, err := persistedMessageBinding(key, mid)
		if err != nil {
			return err
		}
		if binding.Counter <= s.next {
			if key.special != 0 || acknowledgedDrafts[key.id] != mid {
				return core.ErrInvalid
			}
			mutation.StagedBindings = append(mutation.StagedBindings, binding)
		} else {
			mutation.Bindings = append(mutation.Bindings, binding)
		}
	}
	for key, version := range versions {
		if max(version.cn, version.readCN) <= s.next {
			continue
		}
		p := s.persistedMessageProjection(byKey[key], version)
		if key.associated {
			if err := s.appendFAIReplicaVersion(&mutation, key, p); err != nil {
				return err
			}
		} else {
			mutation.Projection = append(mutation.Projection, p)
		}
	}
	s.appendAbsentFAIMutation(&mutation, previousFAI)
	if next == s.next {
		return nil
	}
	return s.commitReplica(ctx, mutation)
}

func (s *ramStore) persistedMessageProjection(mid uint64, version messageVersion) mapireplica.Projection {
	return mapireplica.Projection{
		Counter: folderGlobalCount(mid), Parent: folderGlobalCount(version.parent),
		ChangeNumber: version.cn, ReadNumber: version.readCN, Fingerprint: version.fingerprint,
		NativeRevision: version.nativeRevision, Modified: version.modified, Present: version.present, Read: version.read,
		ChangeKey: []byte(s.messageChangeKey(version)), Predecessors: s.messagePredecessors(version),
	}
}

func (s *ramStore) messageChangeKey(v messageVersion) changeXID {
	if v.change != "" {
		return v.change
	}
	return gidChangeXID(hierarchyXID(s.replica, v.cn))
}

func (s *ramStore) messagePredecessors(v messageVersion) []byte {
	if v.history != "" {
		return []byte(v.history)
	}
	return append([]byte{22}, s.messageChangeKey(v)...)
}

func (s *ramStore) advanceMessageHistory(v *messageVersion) error {
	if v.history == "" {
		return nil
	}
	pcl, err := parseFAIPCL([]byte(v.history))
	if err != nil {
		return err
	}
	change := gidChangeXID(hierarchyXID(s.replica, v.cn))
	pcl, err = mergeFAIPCL(pcl, faiPCL{change.namespace(): change.local()})
	if err != nil {
		return err
	}
	v.change, v.history = change, string(pcl.marshal())
	return nil
}
