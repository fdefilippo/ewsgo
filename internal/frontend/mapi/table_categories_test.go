// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func tableCategoryRequests() [][]byte {
	return [][]byte{
		{0x59, 4, 2, 1, 0, 2, 0, 0, 0, 0, 0, 0, 1},
		{0x5a, 4, 2, 2, 0, 0, 0, 0, 0, 0, 1},
		{0x6b, 4, 2, 2, 0, 0, 0, 0, 0, 0, 1, 3, 0, 0, 0},
		// CollapseState is opaque and can contain bytes that resemble ROPs.
		{0x6c, 4, 2, 5, 0, 0x17, 4, 2, 0x01, 0x04},
	}
}

const categorySubjectTag = 0x0037001f

func categoryFixture(t *testing.T, flags byte) (*ostFixture, *messageFixtureMailbox, *contentsTable, *ramStore) {
	t.Helper()
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	parent := box.objects[0].Parent
	box.objects = nil
	for i, subject := range []string{"Bravo", "Alpha", "Alpha"} {
		o := messageTestObject(core.ID(string(rune('a'+i))), parent)
		o.MIME = []byte("Subject: " + subject + "\r\n\r\nSynthetic mail")
		o.Properties[core.ReceivedAt] = core.Timestamp(hierarchyTestTime.Add(time.Duration(i) * time.Hour))
		o.Properties[core.IsRead] = core.Bool(i == 1)
		box.objects = append(box.objects, o)
	}
	table, store := openTableTest(t, f, flags)
	return f, box, table, store
}

func expandCategoryWire(slot byte, id uint64, rows uint16) []byte {
	return binary.LittleEndian.AppendUint64(binary.LittleEndian.AppendUint16([]byte{0x59, 4, slot}, rows), id)
}

func collapseCategoryWire(slot byte, id uint64) []byte {
	return binary.LittleEndian.AppendUint64([]byte{0x5a, 4, slot}, id)
}

func getCollapseStateWire(slot byte, id uint64, instance uint32) []byte {
	return append32(binary.LittleEndian.AppendUint64([]byte{0x6b, 4, slot}, id), instance)
}

func TestCategorizedContentsRoundTripAcrossCompatibleTables(t *testing.T) {
	f, _, table, store := categoryFixture(t, 0x10)
	columns := []uint32{tableInstIDTag, tableInstanceNumTag, tableRowTypeTag, tableDepthTag, categorySubjectTag}
	query := func(slot byte) [][]any {
		t.Helper()
		return decodeTableRows(t, faiRunOK(t, f, tableQueryWire(slot, 1, 1, 9)), columns)
	}
	first := func(slot byte) {
		t.Helper()
		faiRunOK(t, f, tableSeekWire(slot, 0, 0))
	}
	fail := func(wire []byte, code uint32) {
		t.Helper()
		got, _ := f.run(wire)
		if !bytes.Equal(got, ropError(wire[0], wire[2], code)) {
			t.Fatalf("ROP %02x returned %x, want %08x", wire[0], got, code)
		}
	}
	key := tableSortKey{categorySubjectTag, 0}
	faiRunOK(t, f, tableColumnsWire(2, columns...))
	requireSortSuccess(t, f, tableSortWire(2, 0, 1, 0, key))
	headers := query(2)
	if len(headers) != 2 || headers[0][4] != "Alpha" || headers[1][4] != "Bravo" || headers[0][2] != uint32(4) || headers[1][2] != uint32(4) {
		t.Fatal("expected two initially collapsed category headers")
	}
	alpha, bravo := headers[0][0].(uint64), headers[1][0].(uint64)
	if alpha == 0 || bravo == 0 || alpha == bravo {
		t.Fatal("category IDs are not distinct")
	}
	faiRunOK(t, f, tableColumnsWire(2, tableContentCount, tableUnreadCount))
	counts := decodeTableRows(t, faiRunOK(t, f, tableQueryWire(2, 1, 1, 9)), []uint32{tableContentCount, tableUnreadCount})
	if counts[0][0] != uint32(2) || counts[0][1] != uint32(1) || counts[1][0] != uint32(1) || counts[1][1] != uint32(1) {
		t.Fatal("header counts depend on visible leaves or lose read state")
	}
	faiRunOK(t, f, tableColumnsWire(2, columns...))
	response := faiRunOK(t, f, expandCategoryWire(2, alpha, 1))
	if binary.LittleEndian.Uint32(response[6:]) != 2 || binary.LittleEndian.Uint16(response[10:]) != 1 || table.cursor != 0 {
		t.Fatal("ExpandRow count, max row count or cursor")
	}
	expandRows := decodeTableRows(t, append([]byte{0x15, 2, 0, 0, 0, 0, 1, 1, 0}, response[12:]...), columns)
	if expandRows[0][2] != uint32(1) || expandRows[0][3] != uint32(1) {
		t.Fatal("ExpandRow returned the wrong type or depth")
	}
	fail(expandCategoryWire(2, alpha, 0), 0x4f8)
	rows := query(2)
	if len(rows) != 4 || rows[0][2] != uint32(3) || rows[3][0] != bravo || rows[3][2] != uint32(4) {
		t.Fatal("expansion leaked to the other category")
	}
	leaf := rows[2][0].(uint64)
	state := faiRunOK(t, f, getCollapseStateWire(2, leaf, 0))
	if int(binary.LittleEndian.Uint16(state[6:])) != len(state)-8 {
		t.Fatal("collapse state did not have a complete length")
	}
	original := bytes.Clone(state[8:])
	tampered := bytes.Clone(original)
	tampered[5] ^= 1
	fail(bookmarkWire(0x6c, 2, tampered), invalidCollapseState)
	fail(getCollapseStateWire(2, leaf, 1), tableCategoryNotFound)
	body, _ := f.execute(getCollapseStateWire(2, leaf, 0), uint32(10+4*len(f.handles)+8))
	if got := storeResponse(t, body).Operations; !bytes.Equal(got, ropError(0x6b, 2, tableBufferTooSmall)) {
		t.Fatal("tight output included a partial collapse state")
	}
	faiRunOK(t, f, tableSeekWire(2, 0, 2))
	if binary.LittleEndian.Uint32(faiRunOK(t, f, collapseCategoryWire(2, alpha))[6:]) != 2 || table.cursor != 0 {
		t.Fatal("collapsing around the cursor did not move it to the header")
	}
	fail(collapseCategoryWire(2, alpha), 0x4f7)
	fail(getCollapseStateWire(2, leaf, 0), tableCategoryNotFound)
	faiRunOK(t, f, expandCategoryWire(2, bravo, 0))
	bookmark := faiRunOK(t, f, bookmarkWire(0x6c, 2, original))[8:]
	if at := query(2); len(at) != 2 || at[0][0] != leaf || at[1][0] != bravo || at[1][2] != uint32(4) {
		t.Fatal("restore did not move the cursor and restore both categories")
	}
	faiRunOK(t, f, seekBookmarkWire(2, bookmark, 0))
	faiRunOK(t, f, bookmarkWire(0x89, 2, bookmark))
	// A compatible independent view restores the state even when its initial
	// expanded depth is different. Its returned cursor bookmark is for Free.
	faiRunOK(t, f, []byte{0x05, 4, 1, 3, 0x10})
	faiRunOK(t, f, tableColumnsWire(3, columns...))
	faiRunOK(t, f, tableSortWire(3, 0, 1, 1, key))
	cross := faiRunOK(t, f, bookmarkWire(0x6c, 3, original))[8:]
	if at := query(3); len(at) != 2 || at[0][0] != leaf || at[1][2] != uint32(4) {
		t.Fatal("same sort and restriction did not allow cross-table restore")
	}
	fail(seekBookmarkWire(3, cross, 0), invalidTableBookmark)
	faiRunOK(t, f, bookmarkWire(0x89, 3, cross))
	faiRunOK(t, f, collapseCategoryWire(3, alpha))
	first(2)
	if rows := query(2); len(rows) != 4 || rows[0][0] != alpha || rows[0][2] != uint32(3) {
		t.Fatal("cross-table operation mutated the original view")
	}
	faiRunOK(t, f, tableSortWire(3, 0, 1, 0, tableSortKey{categorySubjectTag, 1}))
	fail(bookmarkWire(0x6c, 3, original), invalidCollapseState)
	faiRunOK(t, f, tableSortWire(3, 0, 1, 0, key))
	faiRunOK(t, f, tableRestrictWire(3, 0, tableSubjectRestriction("Alpha", 4)))
	fail(bookmarkWire(0x6c, 3, original), invalidCollapseState)
	if rows := query(3); len(rows) != 1 || rows[0][4] != "Alpha" {
		t.Fatal("restriction was applied to headers or to only visible leaves")
	}
	faiRunOK(t, f, tableRestrictWire(3, 0, nil))
	cross = faiRunOK(t, f, bookmarkWire(0x6c, 3, original))[8:]
	faiRunOK(t, f, bookmarkWire(0x89, 3, cross))
	headerState := faiRunOK(t, f, getCollapseStateWire(2, alpha, 0))[8:]
	faiRunOK(t, f, collapseCategoryWire(2, alpha))
	mark := faiRunOK(t, f, bookmarkWire(0x6c, 2, headerState))[8:]
	faiRunOK(t, f, seekBookmarkWire(2, mark, 0))
	if query(2)[0][0] != alpha {
		t.Fatal("header bookmark failed")
	}
	faiRunOK(t, f, []byte{0x81, 4, 2})
	if table.categories != nil || len(table.rows) != 3 || table.cursor != 0 || table.columns != nil {
		t.Fatal("reset retained category state, removed leaves or missed columns")
	}
	fail(seekBookmarkWire(2, mark, 0), invalidTableBookmark)
	faiRunOK(t, f, []byte{1, 4, 2, 1, 4, 3})
	if store.allocator.contents.used != 0 {
		t.Fatal("release leaked category or bookmark memory")
	}
}

func TestCategorizedTableLiveRefreshPreservesNestedVisibility(t *testing.T) {
	f, box, _, _ := categoryFixture(t, 0)
	tags := []uint32{tableInstIDTag, tableRowTypeTag, tableDepthTag}
	faiRunOK(t, f, tableColumnsWire(2, tags...))
	requireSortSuccess(t, f, tableSortWire(2, 0, 2, 1,
		tableSortKey{categorySubjectTag, 0}, tableSortKey{0x0e060040, 0}))
	query := func() [][]any {
		return decodeTableRows(t, faiRunOK(t, f, tableQueryWire(2, 1, 1, 20)), tags)
	}
	rows := query()
	if len(rows) != 5 || rows[0][1] != uint32(3) || rows[1][1] != uint32(4) || rows[1][2] != uint32(1) {
		t.Fatal("ExpandedCount did not open only the first level")
	}
	nested := rows[1][0].(uint64)
	faiRunOK(t, f, expandCategoryWire(2, nested, 0))
	before := query()
	if len(before) != 6 || before[2][1] != uint32(1) || before[2][2] != uint32(2) {
		t.Fatal("nested category did not reveal a leaf with depth 2")
	}
	added := messageTestObject("new-category", box.objects[0].Parent)
	added.MIME = []byte("Subject: Zulu\r\n\r\nSynthetic mail")
	box.objects = append(box.objects, added)
	out, _ := f.run(nil)
	requireTableChanged(t, out, f.handles[2])
	after := query()
	if len(after) != 8 || after[1][0] != nested || after[1][1] != uint32(3) || after[2][0] != before[2][0] {
		t.Fatal("refresh changed category identity or collapse state")
	}
	out, _ = f.run(nil)
	if len(out) != 0 {
		t.Fatal("refresh notified again without a change")
	}
}

func TestMaximumCategorySortsHeadersAndKeepsLeafOrder(t *testing.T) {
	f, box, _, _ := categoryFixture(t, 0)
	const delivery = 0x0e060040
	tags := []uint32{tableInstIDTag, tableRowTypeTag, categorySubjectTag, delivery}
	faiRunOK(t, f, tableColumnsWire(2, tags...))
	query := func() [][]any {
		t.Helper()
		return decodeTableRows(t, faiRunOK(t, f, tableQueryWire(2, 1, 1, 20)), tags)
	}
	stamp := func(hour int) uint64 {
		return fileTime(hierarchyTestTime.Add(time.Duration(hour) * time.Hour))
	}
	for _, outer := range []byte{0, 1} {
		requireSortSuccess(t, f, tableSortWire(2, 0, 1, 1, tableSortKey{categorySubjectTag, outer},
			tableSortKey{delivery, 4}, tableSortKey{delivery, 0}))
		rows := query()
		alphaHeader, bravoHeader := 2, 0
		if outer == 1 {
			alphaHeader, bravoHeader = 0, 3
		}
		if len(rows) != 5 || rows[alphaHeader][2] != "Alpha" || rows[alphaHeader][3] != stamp(2) ||
			rows[alphaHeader+1][1] != uint32(1) || rows[alphaHeader+1][3] != stamp(1) ||
			rows[alphaHeader+2][1] != uint32(1) || rows[alphaHeader+2][3] != stamp(2) ||
			rows[bravoHeader][2] != "Bravo" || rows[bravoHeader][3] != stamp(0) {
			t.Fatal("headers did not follow their maxima or the aggregate reordered leaves")
		}
	}
	before := query()
	alpha := before[0][0].(uint64)
	faiRunOK(t, f, collapseCategoryWire(2, alpha))
	box.objects[0].Properties[core.ReceivedAt] = core.Timestamp(hierarchyTestTime.Add(3 * time.Hour))
	out, _ := f.run(nil)
	requireTableChanged(t, out, f.handles[2])
	after := query()
	if len(after) != 3 || after[0][2] != "Bravo" || after[0][3] != stamp(3) ||
		after[2][0] != alpha || after[2][1] != uint32(4) {
		t.Fatal("refresh did not reorder headers or preserve their identity and visibility")
	}
	requireRestrictSuccess(t, f, tableSubjectRestriction("Alpha", 4))
	restricted := query()
	if len(restricted) != 3 || restricted[0][2] != "Alpha" || restricted[0][3] != stamp(2) {
		t.Fatal("restriction lost the aggregate profile")
	}
}

func TestTableCategoryCodecPreflightsBeforeEffects(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	table, _ := openTableTest(t, f, 0x10)
	faiRunOK(t, f, tableColumnsWire(2, 0x674a0014))
	for _, request := range tableCategoryRequests() {
		for n := 1; n < len(request); n++ {
			wire := slices.Concat(tableSeekWire(2, 2, 0), request[:n])
			body, _ := f.execute(wire, 65535)
			if !bytes.Equal(body, executeError(0x80070057, 65535)) || table.cursor != 0 {
				t.Fatalf("ROP %02x truncated at %d executed the prefix", request[0], n)
			}
		}
	}
	invalidIndex := []byte{0x6c, 4, 0xff, 0, 0}
	body, _ := f.execute(slices.Concat(tableSeekWire(2, 2, 0), invalidIndex), 65535)
	if !bytes.Equal(body, executeError(0x80070057, 65535)) || table.cursor != 0 {
		t.Fatal("out-of-range handle index executed the prefix")
	}
}

func TestTableCategoryErrorsPreserveFlatView(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	f.transport.synchronizationOptionsTrace = true
	table, store := openTableTest(t, f, 0x10)
	requests := tableCategoryRequests()
	for _, request := range requests {
		for _, tc := range []struct {
			logon, slot byte
			code        uint32
		}{
			{4, 1, NotSupported}, // The folder is not a table.
			{5, 2, accessDenied}, // A different logon cannot use the table.
			{4, 3, nullObject},   // An unallocated handle slot.
			{4, 2, nullObject},   // Contents Table without columns.
		} {
			wire := slices.Clone(request)
			wire[1], wire[2] = tc.logon, tc.slot
			out, _ := f.run(wire)
			if !bytes.Equal(out, ropError(wire[0], tc.slot, tc.code)) {
				t.Fatalf("wrong response for ROP %02x, logon %d, slot %d: %x", wire[0], tc.logon, tc.slot, out)
			}
		}
	}
	faiRunOK(t, f, tableColumnsWire(2, 0x674a0014))
	requireSortSuccess(t, f, tableSortWire(2, 0, 0, 0, tableSortKey{0x674a0014, 0}))
	faiRunOK(t, f, tableSeekWire(2, 2, 0))
	token := faiRunOK(t, f, []byte{0x1b, 4, 2})[8:]
	position := []byte{0x17, 4, 2}
	before := faiRunOK(t, f, position)
	handles := slices.Clone(f.handles)
	used := store.allocator.contents.used
	sort := tableSortWire(2, 0, 1, 0, tableSortKey{0x0037001f, 0}, tableSortKey{0x12340003, 4})
	batch := slices.Concat(position, sort, requests[0], requests[1], requests[2], requests[3], position)
	want := slices.Concat(before, ropError(0x13, 2, NotSupported),
		ropError(0x59, 2, 0x8004010f), ropError(0x5a, 2, 0x8004010f),
		ropError(0x6b, 2, NotSupported), ropError(0x6c, 2, NotSupported), before)
	out, counts := f.run(batch)
	if !bytes.Equal(out, want) || counts.errors != 5 || table.cursor != 1 ||
		!slices.Equal(handles, f.handles) || len(table.sortKeys) != 1 || store.allocator.contents.used != used {
		t.Fatal("categorized requests changed the flat table or broke the batch")
	}
	faiRunOK(t, f, seekBookmarkWire(2, token, 0))
	_, _, first, _, _ := counts.execution.Fields()
	if first != "sort_table:not_supported" {
		t.Fatal("wrong execution diagnostic", first)
	}
}
