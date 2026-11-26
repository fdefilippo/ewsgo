// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"math"

	"ewsgo/internal/core"
)

type folderCounters struct {
	values  [2]uint32
	present [2]bool
}

func readFolderCounters(properties core.Properties) (folderCounters, error) {
	var out folderCounters
	for i, key := range []core.Key{core.FolderItemCount, core.FolderUnreadCount} {
		value, exists := properties[key]
		if !exists {
			continue
		}
		n, ok := value.(core.Integer)
		if !ok || n < 0 || n > math.MaxInt32 {
			return folderCounters{}, core.ErrInvalid
		}
		out.values[i], out.present[i] = uint32(n), true
	}
	if out.present[0] && out.present[1] && out.values[1] > out.values[0] {
		return folderCounters{}, core.ErrInvalid
	}
	return out, nil
}

func emptyNormalFolderProperties(name string) core.Properties {
	// The service/dynamic RAM normal namespace is actually empty; its FAI
	// configuration namespace is separate and excluded from these counters.
	return core.Properties{core.DisplayName: core.Text(name), core.FolderItemCount: core.Integer(0), core.FolderUnreadCount: core.Integer(0)}
}
