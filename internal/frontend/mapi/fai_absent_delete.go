// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const faiAbsentRecordBytes = 128

// Called with contentsMu. An absent deletion retains only its SourceKey and
// folder. It never becomes a Message object or consumes a MID/change number.
// Existing and absent tombstones share a separate historical quota.
func (s *ramStore) validateFAIHistory(records map[core.ID]faiRecord, absent map[[22]byte]uint64) (int, error) {
	if len(records)-activeFAICount(records)+len(absent) > mapireplica.MaxFAITombstones {
		return 0, core.ErrUnsupported
	}
	used := faiAbsentRecordBytes * len(absent)
	for _, record := range records {
		if record.deleted {
			n, err := s.validateFAIRecord(record)
			if err != nil {
				return 0, err
			}
			used += n
		}
	}
	if used > mapireplica.MaxFAITombstoneBytes {
		return 0, core.ErrUnsupported
	}
	if len(absent) == 0 {
		return used, nil
	}
	blockedReplicas := make(map[[16]byte]bool)
	for _, record := range records {
		if record.sync == nil {
			continue
		}
		if _, exists := absent[record.sync.source]; exists {
			return 0, core.ErrInvalid
		}
		for replica, local := range record.sync.predecessors {
			if len(local) != 6 {
				blockedReplicas[replica] = true
			}
		}
	}
	for source, folder := range absent {
		if !validFAIXID(source) || blockedReplicas[gidChangeXID(source).namespace()] {
			return 0, core.ErrInvalid
		}
		s.mu.Lock()
		_, known := s.byFID[folder]
		s.mu.Unlock()
		if !known {
			return 0, core.ErrInvalid
		}
	}
	return used, nil
}
