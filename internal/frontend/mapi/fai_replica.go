// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

// Bindings use the counter as their storage key. The frontend reconstructs
// its internal FAI identity from the authoritative SourceKey on load, also
// after a backup restore rotates the local replica.
func persistedFAIBinding(mid uint64) mapireplica.Binding {
	counter := folderGlobalCount(mid)
	key := globalCount(counter)
	return mapireplica.Binding{Namespace: mapireplica.AssociatedMessage, Key: key[:], Counter: counter}
}

func decodePersistedFAI(binding mapireplica.Binding, stored mapireplica.FAIObject) (messageKey, error) {
	counter := globalCount(binding.Counter)
	if !bytes.Equal(binding.Key, counter[:]) || stored.PrivateConfig || !validFAIXID(stored.Source) {
		return messageKey{}, core.ErrInvalid
	}
	return messageKey{id: faiIDForSource(stored.Source), associated: true}, nil
}

func (s *ramStore) loadFAIReplica(state mapireplica.FAIState, projections []mapireplica.Projection) error {
	if len(state.Objects)+len(state.Tombstones) == 0 {
		return nil
	}
	versions := make(map[uint64]mapireplica.Projection, len(projections))
	for _, version := range projections {
		versions[version.Counter] = version
	}
	records := make(map[core.ID]faiRecord, len(state.Objects))
	for _, stored := range state.Objects {
		if stored.PrivateConfig {
			continue
		}
		version, exists := versions[stored.Counter]
		mid := folderID(stored.Counter)
		key := s.byMID[mid]
		if !exists || !key.associated {
			return core.ErrInvalid
		}
		pcl, err := parseFAIPCL(version.Predecessors)
		if err != nil {
			return err
		}
		fid := folderID(version.Parent)
		record := faiRecord{
			folder: fid, deleted: !version.Present,
			sync: &faiSyncVersion{source: stored.Source, change: changeXID(version.ChangeKey), predecessors: pcl},
			object: core.Object{
				ID: key.id, Parent: s.byFID[fid].native, Kind: core.Message,
				Revision: version.NativeRevision, Properties: stored.Properties,
			},
		}
		size, err := s.validateFAIRecord(record)
		if err != nil {
			return err
		}
		if size != stored.CacheBytes {
			return core.ErrInvalid
		}
		record.bytes = size
		records[key.id] = record
	}
	absent := make(map[[22]byte]uint64, len(state.Tombstones))
	for _, tombstone := range state.Tombstones {
		absent[tombstone.Source] = folderID(tombstone.Folder)
	}
	total, err := s.validateFAIHistory(records, absent)
	if err != nil {
		return err
	}
	activeBytes := 0
	for _, record := range records {
		if !record.deleted {
			activeBytes += record.bytes
		}
	}
	if activeFAICount(records) > maxFAIObjects || activeBytes > maxFAIStoreBytes {
		return core.ErrUnsupported
	}
	lease, err := s.reserveContentsMemory(total + activeBytes)
	if err != nil {
		return err
	}
	s.fai = faiNamespace{records: records, absent: absent, bytes: total + activeBytes, lease: lease}
	return nil
}

func (s *ramStore) appendFAIReplicaVersion(mutation *mapireplica.Mutation, key messageKey, p mapireplica.Projection) error {
	record, exists := s.fai.records[key.id]
	if !exists {
		return core.ErrInvalid
	}
	return appendFAIReplicaRecord(mutation, record, p)
}

func appendFAIReplicaRecord(mutation *mapireplica.Mutation, record faiRecord, p mapireplica.Projection) error {
	if record.sync == nil || record.deleted == p.Present ||
		record.folder != folderID(p.Parent) || !validFAISync(record.sync) {
		return core.ErrInvalid
	}
	p.ChangeKey = []byte(record.sync.change)
	p.Predecessors = record.sync.predecessors.marshal()
	p.NativeRevision = record.object.Revision
	mutation.Projection = append(mutation.Projection, p)
	if mutation.FAI == nil {
		mutation.FAI = &mapireplica.FAIChanges{}
	}
	mutation.FAI.Objects = append(mutation.FAI.Objects, mapireplica.FAIObject{
		Counter: p.Counter, Source: record.sync.source, Properties: record.object.Properties, CacheBytes: record.bytes,
	})
	return nil
}

func (s *ramStore) appendAbsentFAIMutation(mutation *mapireplica.Mutation, previous *faiNamespace) {
	if previous == nil {
		return
	}
	for source, fid := range s.fai.absent {
		if previous.absent[source] != fid {
			if mutation.FAI == nil {
				mutation.FAI = &mapireplica.FAIChanges{}
			}
			mutation.FAI.Tombstones = append(mutation.FAI.Tombstones, mapireplica.FAITombstone{Source: source, Folder: folderGlobalCount(fid)})
		}
	}
}
