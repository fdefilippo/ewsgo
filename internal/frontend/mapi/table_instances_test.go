// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"slices"
	"testing"

	"ewsgo/internal/core"
)

func TestContentsTableKeywordInstances(t *testing.T) {
	f, box, _, _ := categoryFixture(t, 0x10)
	box.objects[1].Properties[core.MailCategories] = core.Strings{"Bravo", "Alpha", "Alpha"}
	box.objects[2].Properties[core.MailCategories] = core.Strings{"Bravo"}
	faiRunOK(t, f, []byte{1, 4, 2})
	table, store := openTableTest(t, f, 0x10)
	ids, code := store.named.ids(context.Background(), []propertyName{keywordsName}, true)
	if code != 0 || len(ids) != 1 {
		t.Fatal("cannot register Keywords")
	}
	keywords := uint32(ids[0])<<16 | 0x301f
	columns := []uint32{tableMIDTag, tableInstIDTag, tableInstanceNumTag, keywords, instanceServerIDTag}
	faiRunOK(t, f, tableColumnsWire(2, columns...))
	query := func(count uint16) [][]any {
		t.Helper()
		return decodeTableRows(t, faiRunOK(t, f, tableQueryWire(2, 1, 1, count)), columns)
	}
	for _, row := range query(9) {
		if row[2] != uint32(0) || row[3] != uint32(PropertyNotFound) {
			t.Fatal("requesting an instance column on a flat view must not expand it")
		}
	}
	key := tableSortKey{keywords, 0}
	requireSortSuccess(t, f, tableSortWire(2, 0, 0, 0, key))
	rows := query(9)
	if len(rows) != 5 || rows[1][0] != rows[2][0] || rows[1][0] != rows[3][0] || rows[3][0] == rows[4][0] {
		t.Fatal("duplicate instances lost their message identity")
	}
	for i, row := range rows {
		wantNum := []uint32{0, 2, 3, 1, 1}[i]
		wantValue := []any{uint32(PropertyNotFound), "Alpha", "Alpha", "Bravo", "Bravo"}[i]
		if row[0] != row[1] || row[2] != wantNum || row[3] != wantValue ||
			binary.LittleEndian.Uint32(row[4].([]byte)[17:]) != wantNum {
			t.Errorf("row %d has wrong instance or sort position", i)
		}
	}
	faiRunOK(t, f, tableSeekWire(2, 0, 2))
	bookmark := faiRunOK(t, f, []byte{0x1b, 4, 2})[8:]
	faiRunOK(t, f, tableSeekWire(2, 0, 0))
	faiRunOK(t, f, seekBookmarkWire(2, bookmark, 0))
	if got := query(1)[0]; got[0] != rows[2][0] || got[2] != uint32(3) {
		t.Fatal("bookmark restored the wrong duplicate")
	}

	requireSortSuccess(t, f, tableSortWire(2, 0, 1, 1, key))
	categorized := query(12)
	var alpha uint64
	for _, row := range categorized {
		if row[3] == "Alpha" && row[2] == uint32(0) {
			alpha = row[1].(uint64)
		}
	}
	if alpha == 0 {
		t.Fatal("missing Alpha header")
	}
	state := faiRunOK(t, f, getCollapseStateWire(2, rows[2][0].(uint64), 3))[8:]
	faiRunOK(t, f, collapseCategoryWire(2, alpha))
	faiRunOK(t, f, bookmarkWire(0x6c, 2, state))
	if got := query(1)[0]; got[0] != rows[2][0] || got[2] != uint32(3) {
		t.Fatal("collapse state restored the wrong duplicate")
	}
	requireSortSuccess(t, f, tableSortWire(2, 0, 1, 0, tableSortKey{categorySubjectTag, 0}))
	out, _ := f.run(bookmarkWire(0x6c, 2, state))
	if !bytes.Equal(out, ropError(0x6c, 2, invalidCollapseState)) {
		t.Fatalf("incompatible collapse state returned %x", out)
	}
	requireSortSuccess(t, f, tableSortWire(2, 0, 0, 0, tableSortKey{categorySubjectTag, 0}))
	if got := query(9); len(got) != 3 || table.instanceLease != nil {
		t.Fatal("scalar sort retained the instance view")
	}
	for _, row := range query(9) {
		if row[2] != uint32(0) || row[3] != uint32(PropertyNotFound) {
			t.Fatal("scalar sort retained instance data")
		}
	}

	// An expansion that exceeds the row budget must leave the current view and
	// memory accounting intact.
	table.baseRows[0][keywordsValuesProperty] = core.Strings(slices.Repeat([]string{"Same"}, maxTableInstanceRows+1))
	used := store.allocator.contents.used
	out, _ = f.run(tableSortWire(2, 0, 0, 0, key))
	if len(out) != 6 || binary.LittleEndian.Uint32(out[2:]) == 0 ||
		store.allocator.contents.used != used || len(table.rows) != 3 || table.instanceLease != nil {
		t.Fatalf("failed expansion changed the previous table view: %x", out)
	}
}
