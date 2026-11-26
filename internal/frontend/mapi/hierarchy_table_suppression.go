// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"reflect"

	"ewsgo/internal/core"
)

type hierarchyCreationOrigin struct {
	parent  uint64
	context uint32
}

// Suppress a refresh only if the visible delta is explained entirely by
// creations acknowledged on this context and their derived parent counts.
// Coalesced external changes must still generate a notification.
func (t *contentsTable) suppressHierarchyCreations(next *contentsTable) bool {
	if !t.hierarchy || t.flags&tableSuppressNotifications == 0 || t.notificationOrigin == 0 {
		return false
	}
	added := make(map[uint64]bool)
	children := make(map[uint64]int)
	for fid, receipt := range next.hierarchyCreations {
		if receipt.context == t.notificationOrigin && t.hierarchyCreations[fid].context == 0 {
			added[fid] = true
			children[receipt.parent]++
		}
	}
	if len(added) == 0 {
		return false
	}
	before := t.baseRows
	for _, row := range next.rows {
		fid := uint64(t.bookmarkRow(row).id)
		if len(before) != 0 && uint64(t.bookmarkRow(before[0]).id) == fid {
			if !sameHierarchyRowExceptAddedChildren(before[0], row, children[fid]) {
				return false
			}
			before = before[1:]
			continue
		}
		if !added[fid] || row[fixedContentsProperty(0x66380003)] != core.Integer(children[fid]) {
			return false
		}
	}
	return len(before) == 0
}

func sameHierarchyRowExceptAddedChildren(before, after contentsValues, added int) bool {
	if added == 0 {
		return reflect.DeepEqual(before, after)
	}
	count := fixedContentsProperty(0x66380003)
	hasChildren := fixedContentsProperty(0x360a000b)
	oldCount, oldOK := before[count].(core.Integer)
	newCount, newOK := after[count].(core.Integer)
	if !oldOK || !newOK || newCount-oldCount != core.Integer(added) || len(before) != len(after) {
		return false
	}
	for property, value := range before {
		if property != count && property != hasChildren && !reflect.DeepEqual(value, after[property]) {
			return false
		}
	}
	return true
}
