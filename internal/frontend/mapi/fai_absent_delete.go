// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import "ewsgo/internal/core"

const faiAbsentRecordBytes = 128

// Called with contentsMu. An absent deletion retains only its SourceKey and
// folder. It never becomes a Message object or consumes a MID/change number.
// Existing and absent tombstones share the namespace's object and memory limits.
func (s *ramStore) validateAbsentFAI(records map[core.ID]faiRecord, absent map[[22]byte]uint64) (int, error) {
	if len(records)+len(absent) > maxFAIObjects {
		return 0, core.ErrUnsupported
	}
	for source, folder := range absent {
		if !validFAIXID(source) {
			return 0, core.ErrInvalid
		}
		s.mu.Lock()
		_, known := s.byFID[folder]
		s.mu.Unlock()
		if !known {
			return 0, core.ErrInvalid
		}
		ns := gidChangeXID(source).namespace()
		for _, record := range records {
			if record.sync == nil {
				continue
			}
			if record.sync.source == source {
				return 0, core.ErrInvalid
			}
			if local, exists := record.sync.predecessors[ns]; exists && len(local) != 6 {
				return 0, core.ErrInvalid
			}
		}
	}
	return faiAbsentRecordBytes * len(absent), nil
}
