// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import "encoding/binary"

// Only closed classes survive this inspector. No requested ID, replica, native
// key, counter, handle, object or error string is retained in the diagnostic.
var folderOpenProfileLabels = [...]string{
	"target:native", "target:service", "target:dynamic", "target:message",
	"target:reserved_local", "target:unmapped", "target:other_replica", "target:zero",
	"input:unavailable",
	"result:success", "result:not_found", "result:access_denied",
	"result:not_supported", "result:other",
}

func (d ExecutionDiagnostic) FolderOpenProfiles() []string {
	out := []string{}
	for i, label := range folderOpenProfileLabels {
		if d.folderOpenProfiles&(1<<i) != 0 {
			out = append(out, label)
		}
	}
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}

// Called only for an executed OpenFolder, using its already authorized context.
// A mapped native target plus NotFound is distinct from an unmapped client ID;
// the former alone does not claim the native object was deleted or why it failed.
// The store mutex protects a bounded metadata read; no backend I/O is added.
func (c *storeResults) recordFolderOpen(objects *mailboxObjects, op Operation, handles []uint32, response []byte) {
	if !c.folderOpenEnabled || op.ID != 2 || len(response) < 6 || int(op.Input) >= len(handles) {
		return
	}
	result := 13
	switch binary.LittleEndian.Uint32(response[2:]) {
	case 0:
		result = 9
	case PropertyNotFound:
		result = 10
	case accessDenied:
		result = 11
	case NotSupported:
		result = 12
	}
	c.execution.folderOpenProfiles |= 1 << result
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 || object.binding.store == nil || object.table != nil || object.download != nil {
		c.execution.folderOpenProfiles |= 1 << 8
		return
	}
	s := object.binding.store
	s.mu.Lock()
	defer s.mu.Unlock()
	kind := 5
	if key, ok := s.byFID[op.Folder]; ok {
		kind = 1
		if key.native != "" {
			kind = 0
		} else if key.dynamic != 0 {
			kind = 2
		}
	} else if op.Folder == 0 {
		kind = 7
	} else if uint16(op.Folder) != 1 {
		kind = 6
	} else if _, ok := s.byMID[op.Folder]; ok {
		kind = 3
	} else {
		counter := folderGlobalCount(op.Folder)
		for _, reserved := range s.localReservations {
			if counter >= reserved.low && counter <= reserved.high {
				kind = 4
				break
			}
		}
	}
	c.execution.folderOpenProfiles |= 1 << kind
}
