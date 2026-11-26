// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/json"
	"slices"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

type storedFolderDefinition struct {
	Parent          uint64
	Name, Comment   string
	Depth           int
	Search, Deleted bool
}

type storedSearchCriteria struct {
	Revision          uint64
	Restriction       []byte
	Folders           []uint64
	Recursive, Static bool
	Links             map[uint64][]uint64
}

func folderValue(fid uint64, kind mapireplica.FolderValueKind, value any) (mapireplica.FolderValue, error) {
	result := mapireplica.FolderValue{Counter: folderGlobalCount(fid), Kind: kind}
	if value == nil {
		return result, nil
	}
	data, err := json.Marshal(value)
	result.Data = data
	return result, err
}

func (s *ramStore) folderDefinitionValue(fid uint64, folder ramFolder) (mapireplica.FolderValue, error) {
	return folderValue(fid, mapireplica.FolderDefinition, storedFolderDefinition{
		Parent: s.byKey[folder.parent], Name: folder.name, Comment: folder.comment,
		Depth: folder.depth, Search: folder.search, Deleted: folder.softDeleted,
	})
}

func (s *ramStore) loadFolderValues(values []mapireplica.FolderValue) error {
	s.dynamic = make(map[uint64]ramFolder)
	s.metadata = make(map[uint64]folderMetadata)
	s.ost = make(map[uint64][]byte)
	for _, value := range values {
		fid := folderID(value.Counter)
		key, bound := s.byFID[fid]
		if !bound {
			return core.ErrInvalid
		}
		switch value.Kind {
		case mapireplica.FolderDefinition:
			var definition storedFolderDefinition
			if err := json.Unmarshal(value.Data, &definition); err != nil {
				return err
			}
			parent, exists := s.byFID[definition.Parent]
			if !exists || definition.Parent == fid || key.dynamic == 0 || definition.Name == "" ||
				definition.Depth < 1 || definition.Depth > maxDynamicDepth {
				return core.ErrInvalid
			}
			s.dynamic[key.dynamic] = ramFolder{id: key.dynamic, parent: parent, name: definition.Name,
				comment: definition.Comment, depth: definition.Depth, search: definition.Search, softDeleted: definition.Deleted}
			s.dynamicBytes += 128 + len(definition.Name) + len(definition.Comment) + len(parent.native)
		case mapireplica.FolderProperties:
			var properties folderMetadata
			if err := json.Unmarshal(value.Data, &properties); err != nil {
				return err
			}
			for id := range properties {
				if !metadataID(id) {
					return core.ErrInvalid
				}
			}
			if properties.accountedBytes()-metadataBagOverhead > maxMetadataBytes {
				return core.ErrLimit
			}
			s.metadataBytes += properties.accountedBytes()
			s.metadata[fid] = properties
		case mapireplica.FolderSearch:
			folder, exists := s.dynamic[key.dynamic]
			if !exists || !folder.search || folder.softDeleted {
				return core.ErrInvalid
			}
			criteria, err := s.decodeSearchValue(value.Data)
			if err != nil {
				return err
			}
			folder.criteria = criteria
			s.dynamic[key.dynamic] = folder
		case mapireplica.FolderOST:
			if len(value.Data) == 0 || value.Data[0] != 1 || len(value.Data)-1 > maxOSTValueBytes {
				return core.ErrInvalid
			}
			s.ost[fid] = value.Data[1:]
		case mapireplica.FolderRetiredMIDs:
			if err := s.loadRetiredMIDs(fid, value.Data); err != nil {
				return err
			}
		default:
			return core.ErrInvalid
		}
	}
	if len(s.dynamic) > maxDynamicFolders || s.dynamicBytes > maxDynamicBytes ||
		s.metadataBytes > maxStoreMetadataBytes || len(s.ost) > maxOSTFolders {
		return core.ErrLimit
	}
	if len(s.retiredMIDs) != 0 {
		ranges := make([]idRange, 0, s.retiredRangeCount)
		for _, folderRanges := range s.retiredMIDs {
			ranges = append(ranges, folderRanges...)
		}
		s.retiredMIDUnion = mergeIDRanges(ranges)
		if s.retirementConflictsLocked(s.retiredMIDUnion) {
			return core.ErrInvalid
		}
	}
	for _, folder := range s.dynamic {
		if folder.parent.dynamic == 0 {
			if folder.depth == 1 {
				continue
			}
		} else if parent, exists := s.dynamic[folder.parent.dynamic]; exists && folder.depth == parent.depth+1 {
			continue
		}
		return core.ErrInvalid
	}
	return nil
}

func (s *ramStore) decodeSearchValue(data []byte) (*folderSearchCriteria, error) {
	var saved storedSearchCriteria
	if err := json.Unmarshal(data, &saved); err != nil {
		return nil, err
	}
	if saved.Revision == 0 || len(saved.Restriction) == 0 || len(saved.Restriction) > maxContentsRestrictionBytes ||
		len(saved.Folders) == 0 || len(saved.Folders) > maxSearchSources || !saved.Static && len(saved.Links) != 0 {
		return nil, core.ErrInvalid
	}
	for _, fid := range saved.Folders {
		if _, ok := s.byFID[fid]; !ok {
			return nil, core.ErrInvalid
		}
	}
	text := searchRestrictionText{codePage: 1252}
	restriction, err := parseContentsRestrictionText(saved.Restriction, &text)
	if err != nil {
		return nil, err
	}
	predicate, err := s.bindSearchPredicate(restriction, &storeResults{})
	if err != nil {
		return nil, err
	}
	criteria := &folderSearchCriteria{
		revision: saved.Revision, raw: saved.Restriction, predicate: predicate, folders: saved.Folders,
		recursive: saved.Recursive, textFields: text.fields,
	}
	if saved.Static {
		rows := 0
		for fid, mids := range saved.Links {
			if _, ok := s.byFID[fid]; !ok || !slices.IsSorted(mids) {
				return nil, core.ErrInvalid
			}
			rows += len(mids)
		}
		if rows > maxRAMMappings {
			return nil, core.ErrLimit
		}
		lease, err := s.reserveContentsMemory(1024 + 64*rows)
		if err != nil {
			return nil, err
		}
		criteria.static = &staticSearchSnapshot{links: saved.Links, lease: lease}
		criteria.populated.Store(true)
	}
	return criteria, nil
}

func (s *ramStore) persistSearchLocked(ctx context.Context, fid uint64, criteria *folderSearchCriteria, codePage uint32) error {
	if s.durable.store == nil {
		return nil
	}
	raw, ok := searchRestrictionWire(criteria.raw, criteria.textFields, true, codePage)
	if !ok {
		return core.ErrInvalid
	}
	saved := storedSearchCriteria{Revision: criteria.revision, Restriction: raw, Folders: criteria.folders,
		Recursive: criteria.recursive, Static: criteria.static != nil}
	if criteria.static != nil {
		saved.Links = criteria.static.links
	}
	value, err := folderValue(fid, mapireplica.FolderSearch, saved)
	if err != nil {
		return err
	}
	return s.advanceReplicaLocked(ctx, mapireplica.Mutation{FolderValues: []mapireplica.FolderValue{value}})
}

func (s *ramStore) persistMetadataLocked(ctx context.Context, updates map[uint64]folderMetadata) error {
	if s.durable.store == nil || len(updates) == 0 {
		return nil
	}
	mutation := mapireplica.Mutation{}
	for fid, properties := range updates {
		value := mapireplica.FolderValue{Counter: folderGlobalCount(fid), Kind: mapireplica.FolderProperties}
		if len(properties) != 0 {
			var err error
			value, err = folderValue(fid, mapireplica.FolderProperties, properties)
			if err != nil {
				return err
			}
		}
		mutation.FolderValues = append(mutation.FolderValues, value)
	}
	return s.advanceReplicaLocked(ctx, mutation)
}

// commitFolderRemovalLocked updates the replica and retires FAI with s.mu held.
// clearAssociatedFID optionally clears FAI from a folder that remains in place.
func (s *ramStore) commitFolderRemovalLocked(ctx context.Context, targets []folderKey, fids map[uint64]bool, hard bool, clearAssociatedFID uint64) error {
	if s.next >= maxGlobalCount {
		return core.ErrLimit
	}
	change := mapireplica.Mutation{HighWater: s.next + 1}
	for _, key := range targets {
		fid := s.byKey[key]
		if hard {
			for kind := mapireplica.FolderDefinition; kind <= mapireplica.FolderOST; kind++ {
				change.FolderValues = append(change.FolderValues, mapireplica.FolderValue{Counter: key.dynamic, Kind: kind})
			}
			continue
		}
		folder := s.dynamic[key.dynamic]
		folder.softDeleted = true
		definition, err := s.folderDefinitionValue(fid, folder)
		if err != nil {
			return err
		}
		change.FolderValues = append(change.FolderValues, definition,
			mapireplica.FolderValue{Counter: key.dynamic, Kind: mapireplica.FolderSearch})
	}
	inbox := s.byKey[folderKey{native: s.inbox}]
	properties := s.metadata[inbox]
	reminders := properties[remindersOnlineEntryID]
	if len(reminders) == folderEntryIDBytes && fids[fidFromGlobalCounter(reminders[38:44])] {
		next := cloneMetadata(properties)
		delete(next, remindersOnlineEntryID)
		value := mapireplica.FolderValue{Counter: folderGlobalCount(inbox), Kind: mapireplica.FolderProperties}
		if len(next) != 0 {
			var err error
			value, err = folderValue(inbox, mapireplica.FolderProperties, next)
			if err != nil {
				return err
			}
		}
		change.FolderValues = append(change.FolderValues, value)
	}
	retired := make(map[messageKey]faiRecord)
	if hard {
		for id, record := range s.fai.records {
			if record.deleted || !(fids[record.folder] || clearAssociatedFID != 0 && record.folder == clearAssociatedFID) {
				continue
			}
			if change.HighWater >= maxGlobalCount {
				return core.ErrLimit
			}
			change.HighWater++
			record.deleted = true
			record.object.Properties = nil
			record.object.Revision++
			n, err := s.validateFAIRecord(record)
			if err != nil {
				return err
			}
			record.bytes = n
			key := messageKey{id: id, associated: true}
			retired[key] = record
			version := s.messages[key]
			version.present, version.cn = false, change.HighWater
			p := s.persistedMessageProjection(s.byMessage[key], version)
			if err := appendFAIReplicaRecord(&change, record, p); err != nil {
				return err
			}
		}
	}
	if s.durable.store != nil {
		if err := s.commitReplica(ctx, change); err != nil {
			return err
		}
	}
	for key, record := range retired {
		s.fai.bytes += record.bytes - s.fai.records[key.id].bytes
		s.fai.records[key.id] = record
	}
	for _, p := range change.Projection {
		key := s.byMID[folderID(p.Counter)]
		version := s.messages[key]
		version.cn, version.present, version.nativeRevision = p.ChangeNumber, false, p.NativeRevision
		s.messages[key] = version
	}
	if len(retired) != 0 {
		_ = s.fai.lease.shrink(s.fai.bytes) // Shrank by removing retained property values.
	}
	s.next = change.HighWater
	return nil
}
