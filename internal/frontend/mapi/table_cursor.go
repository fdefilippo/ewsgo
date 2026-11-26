// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/binary"
	"math"

	"ewsgo/internal/core"
)

const (
	invalidTableBookmark  uint32 = 0x80040405
	tableBookmarkWireSize        = 8
	maxTableBookmarks            = 64
)

type tableBookmark struct {
	row      tableRowRef
	position int
	invalid  bool // A cross-table collapse-state bookmark may only be freed.
}

type tableRowRef struct {
	id       core.Integer // Zero represents the position past the end.
	instance uint32
}

func (t *contentsTable) seekFrom(start int, offset int32) (less byte, moved int32) {
	target := max(0, min(int64(len(t.rows)), int64(start)+int64(offset)))
	t.cursor = int(target)
	t.subscribe()
	moved = int32(target - int64(start))
	if moved != offset {
		less = 1
	}
	return less, moved
}

func (t *contentsTable) executeBookmark(store *ramStore, handle uint32, op Operation) []byte {
	reply := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	if t.permissions != nil || t.attachmentMessage != nil {
		return reply(NotSupported)
	}
	switch op.ID {
	case 0x1b:
		if t.columns == nil {
			return reply(nullObject)
		}
		b := tableBookmark{position: t.cursor}
		if t.cursor < len(t.rows) {
			b.row = t.bookmarkRow(t.rows[t.cursor])
			if b.row.id == 0 {
				return reply(0x80004005)
			}
		}
		return t.putBookmark(store, handle, op, b)
	case 0x89:
		id, ok := t.bookmarkID(handle, op.Table.bookmark)
		if !ok {
			return reply(invalidTableBookmark)
		}
		delete(t.bookmarks, id)
		if len(t.bookmarks) == 0 {
			t.clearBookmarks()
		}
		return reply(0)
	default: // RopSeekRowBookmark
		start, invisible, code := t.bookmarkPosition(handle, op.Table.bookmark)
		if code != 0 {
			return reply(code)
		}
		less, moved := t.seekFrom(start, op.Table.seek)
		if !op.Table.wantMoved {
			less, moved = 0, 0
		}
		return append32(append(reply(0), invisible, less), uint32(moved))
	}
}

func (t *contentsTable) putBookmark(store *ramStore, handle uint32, op Operation, b tableBookmark) []byte {
	reply := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	if len(t.bookmarks) >= maxTableBookmarks || t.bookmarkSeq == math.MaxUint32 {
		return reply(0x8007000e)
	}
	if t.bookmarks == nil {
		lease, err := store.reserveContentsMemory(4096)
		if err != nil {
			return reply(storeError(err))
		}
		t.bookmarkMem = lease
		t.bookmarks = make(map[uint32]tableBookmark)
	}
	t.bookmarkSeq++
	t.bookmarks[t.bookmarkSeq] = b
	response := binary.LittleEndian.AppendUint16(reply(0), tableBookmarkWireSize)
	return append32(append32(response, handle), t.bookmarkSeq)
}

func (t *contentsTable) bookmarkRow(row contentsValues) tableRowRef {
	tag := uint32(tableInstIDTag)
	if t.hierarchy {
		tag = 0x67480014 // PidTagFolderId
	}
	id, _ := row[fixedContentsProperty(tag)].(core.Integer)
	instance, _ := row[fixedContentsProperty(tableInstanceNumTag)].(core.Integer)
	return tableRowRef{id: id, instance: uint32(instance)}
}

func (t *contentsTable) bookmarkID(handle uint32, wire []byte) (uint32, bool) {
	if len(wire) != tableBookmarkWireSize || binary.LittleEndian.Uint32(wire) != handle {
		return 0, false
	}
	id := binary.LittleEndian.Uint32(wire[4:])
	_, ok := t.bookmarks[id]
	return id, ok
}

func (t *contentsTable) bookmarkPosition(handle uint32, wire []byte) (int, byte, uint32) {
	if t.permissions != nil || t.attachmentMessage != nil {
		return 0, 0, NotSupported
	}
	id, ok := t.bookmarkID(handle, wire)
	if !ok {
		return 0, 0, invalidTableBookmark
	}
	b := t.bookmarks[id]
	if b.invalid {
		return 0, 0, invalidTableBookmark
	}
	if b.row.id == 0 {
		return len(t.rows), 0, 0
	}
	if position, found := t.rowPosition(b.row); found {
		return position, 0, 0
	}
	return min(b.position, len(t.rows)), 1, 0
}

func (t *contentsTable) rowPosition(id tableRowRef) (int, bool) {
	for position, row := range t.rows {
		if t.bookmarkRow(row) == id {
			return position, true
		}
	}
	return len(t.rows), false
}

func (t *contentsTable) clearBookmarks() {
	t.bookmarks = nil
	t.bookmarkMem.release()
	t.bookmarkMem = nil
}

func (t *contentsTable) reset() {
	if t.permissions == nil {
		t.rows = t.baseRows
	}
	t.columns, t.sortKeys, t.restriction = nil, nil, nil
	t.replaceCategories(nil)
	t.categorySort, t.restrictionID = [32]byte{}, [32]byte{}
	t.categoryExpanded = 0
	t.cursor = 0
	t.sortLease.release()
	t.sortLease = nil
	t.instanceLease.release()
	t.instanceLease = nil
	t.filterLease.release()
	t.filterLease = nil
	t.clearBookmarks()
}
