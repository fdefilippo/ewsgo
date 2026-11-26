// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"

	"ewsgo/internal/core"
)

func bookmarkWire(id, slot byte, bookmark []byte) []byte {
	return append(binary.LittleEndian.AppendUint16([]byte{id, 4, slot}, uint16(len(bookmark))), bookmark...)
}

func seekBookmarkWire(slot byte, bookmark []byte, offset int32) []byte {
	return append(append32(bookmarkWire(0x19, slot, bookmark), uint32(offset)), 1)
}

func fractionalWire(slot byte, numerator, denominator uint32) []byte {
	return append32(append32([]byte{0x1a, 4, slot}, numerator), denominator)
}

func TestTableBookmarkNavigationAndBatch(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	for _, id := range []core.ID{"second-message", "third-message"} {
		box.objects = append(box.objects, messageTestObject(id, box.objects[0].Parent))
	}
	table, store := openTableTest(t, f, 0x10)
	faiRunOK(t, f, tableColumnsWire(2, 0x674a0014))
	pos := []byte{0x17, 4, 2}
	create := []byte{0x1b, 4, 2}
	checkPosition := func(current uint32) {
		t.Helper()
		result := faiRunOK(t, f, pos)
		if !bytes.Equal(result, append32(append32(ropError(0x17, 2, 0), current), uint32(len(table.rows)))) {
			t.Fatalf("wrong position: %x", result)
		}
	}
	faiRunOK(t, f, fractionalWire(2, ^uint32(0)/2+1, ^uint32(0)))
	checkPosition(1)
	baseline := store.allocator.contents.used
	mark := faiRunOK(t, f, create)
	if len(mark) != 16 || binary.LittleEndian.Uint16(mark[6:]) != 8 {
		t.Fatal("invalid bookmark wire format")
	}
	token := mark[8:]
	seek := seekBookmarkWire(2, token, 0)
	faiRunOK(t, f, fractionalWire(2, 9, 8))
	checkPosition(3)
	out := faiRunOK(t, f, seekBookmarkWire(2, token, -4))
	if !bytes.Equal(out, append32(append(ropError(0x19, 2, 0), 0, 1), ^uint32(0))) {
		t.Fatal("seek did not report signed movement limited by beginning")
	}
	checkPosition(0)

	// Keep the selected row identifiable across a refresh that changes order.
	original := table.rows
	table.rows = slices.Clone(original)
	table.rows[0], table.rows[1] = table.rows[1], table.rows[0]
	faiRunOK(t, f, seek)
	checkPosition(0)
	// Remove the bookmarked row, which used to have the original ordinal 1.
	table.rows = original[2:]
	result := faiRunOK(t, f, seek)
	if !bytes.Equal(result, append32(append(ropError(0x19, 2, 0), 1, 0), 0)) {
		t.Fatal("removed bookmark row was reported visible")
	}
	checkPosition(1)

	faiRunOK(t, f, []byte{0x05, 4, 1, 3, 0x10})
	faiRunOK(t, f, tableColumnsWire(3, 0x674a0014))
	faiRunOK(t, f, []byte{0x1b, 4, 3})
	out, _ = f.run(seekBookmarkWire(3, token, 0))
	if !bytes.Equal(out, ropError(0x19, 3, invalidTableBookmark)) {
		t.Fatal("another table accepted the bookmark")
	}
	faiRunOK(t, f, []byte{1, 4, 3})

	body, _ := f.execute(slices.Concat(bookmarkWire(0x89, 2, token), seek[:len(seek)-1]), 65535)
	if !bytes.Equal(body, executeError(0x80070057, 65535)) {
		t.Fatal("truncated bookmark seek did not reject the batch")
	}
	faiRunOK(t, f, seek)
	faiRunOK(t, f, bookmarkWire(0x89, 2, token))
	if table.bookmarks != nil || store.allocator.contents.used != baseline {
		t.Fatal("bookmarks retained memory after FreeBookmark")
	}
	out, _ = f.run(slices.Concat(pos, seek, []byte{0x81, 4, 2}))
	if len(out) != 20 || !bytes.Equal(out[14:], ropError(0x19, 2, invalidTableBookmark)) || table.columns == nil {
		t.Fatal("batch did not stop at invalid bookmark")
	}
	abort := []byte{0x38, 4, 2}
	out, _ = f.run(slices.Concat(pos, abort, pos))
	if len(out) != 34 || !bytes.Equal(out[14:20], ropError(0x38, 2, 0x80040114)) || !bytes.Equal(out[:14], out[20:]) {
		t.Fatal("abort without active work changed navigation")
	}
}

func TestTableResetAndBookmarkInvalidation(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	table, _ := openTableTest(t, f, 0x10)
	columns := tableColumnsWire(2, 0x674a0014)
	faiRunOK(t, f, columns)
	create := []byte{0x1b, 4, 2}
	reset := []byte{0x81, 4, 2}
	token := faiRunOK(t, f, create)[8:]
	match := binary.LittleEndian.AppendUint32([]byte{8}, 0x674a0014)
	out := faiRunOK(t, f, tableFindWire(2, 0, 3, match, token))
	if out[6] != 0 || out[7] != 1 {
		t.Fatal("FindRow could not use bookmark")
	}
	for _, op := range [][]byte{
		tableSortWire(2, 0, 0, 0, tableSortKey{0x674a0014, 1}),
		tableRestrictWire(2, 0, tableSubjectRestriction("missing-subject", 4)),
		reset,
	} {
		token = faiRunOK(t, f, create)[8:]
		faiRunOK(t, f, op)
		if table.bookmarks != nil {
			t.Fatal("table operation retained bookmarks")
		}
		out, _ := f.run(seekBookmarkWire(2, token, 0))
		if !bytes.Equal(out, ropError(0x19, 2, invalidTableBookmark)) {
			t.Fatal("table operation retained bookmark validity")
		}
	}
	out, _ = f.run(tableQueryWire(2, 0, 1, 1))
	if !bytes.Equal(out, ropError(0x15, 2, nullObject)) || table.cursor != 0 ||
		table.sortKeys != nil || table.restriction != nil || len(table.rows) != 1 ||
		table.sortLease != nil || table.filterLease != nil {
		t.Fatal("ResetTable did not restore the unconfigured view")
	}
	faiRunOK(t, f, columns)
	faiRunOK(t, f, tableQueryWire(2, 0, 1, 1))
	out, _ = f.run(fractionalWire(2, 1, 0))
	if !bytes.Equal(out, ropError(0x1a, 2, invalidTableBookmark)) || table.cursor != 1 {
		t.Fatal("zero denominator changed the position")
	}
}

func TestTableBookmarkCapacityAndRelease(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	table, _ := openTableTest(t, f, 0x10)
	faiRunOK(t, f, tableColumnsWire(2, 0x674a0014))
	create := []byte{0x1b, 4, 2}
	first := faiRunOK(t, f, create)[8:]
	for i := 1; i < maxTableBookmarks; i++ {
		faiRunOK(t, f, create)
	}
	out, _ := f.run(create)
	if !bytes.Equal(out, ropError(0x1b, 2, 0x8007000e)) || len(table.bookmarks) != maxTableBookmarks {
		t.Fatal("bookmark allocation bypassed the per-table capacity")
	}
	faiRunOK(t, f, bookmarkWire(0x89, 2, first))
	newest := faiRunOK(t, f, create)[8:]
	if bytes.Equal(first, newest) {
		t.Fatal("released bookmark token was reissued")
	}
	faiRunOK(t, f, []byte{1, 4, 2})
	if table.bookmarks != nil || table.bookmarkMem != nil {
		t.Fatal("Release retained bookmark resources")
	}
}
