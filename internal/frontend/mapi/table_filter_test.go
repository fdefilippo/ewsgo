// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
	"unicode/utf16"

	"ewsgo/internal/core"
)

func tableRestrictWire(index, flags byte, raw []byte) []byte {
	return append(binary.LittleEndian.AppendUint16([]byte{0x14, 4, index, flags}, uint16(len(raw))), raw...)
}
func tableFindWire(index, flags, origin byte, raw, bookmark []byte) []byte {
	w := append(binary.LittleEndian.AppendUint16([]byte{0x4f, 4, index, flags}, uint16(len(raw))), raw...)
	return append(binary.LittleEndian.AppendUint16(append(w, origin), uint16(len(bookmark))), bookmark...)
}
func tableSubjectRestriction(value string, relation byte) []byte {
	raw := binary.LittleEndian.AppendUint32([]byte{4, relation}, 0x0037001f)
	raw = binary.LittleEndian.AppendUint32(raw, 0x0037001f)
	for _, unit := range utf16.Encode([]rune(value)) {
		raw = binary.LittleEndian.AppendUint16(raw, unit)
	}
	return append(raw, 0, 0)
}
func decodeFindRow(t *testing.T, raw []byte, tags []uint32) []any {
	t.Helper()
	if len(raw) < 9 || !bytes.Equal(raw[:8], []byte{0x4f, 2, 0, 0, 0, 0, 0, 1}) {
		t.Fatal("FindRow response shape")
	}
	// Independently interpret the common PropertyRow payload using the wire
	// decoder, with a synthetic QueryRows header only (no production encoder).
	wire := append([]byte{0x15, 2, 0, 0, 0, 0, 1, 1, 0}, raw[8:]...)
	return decodeTableRows(t, wire, tags)[0]
}
func tableFilterFixture(t *testing.T, flags byte) (*ostFixture, *messageFixtureMailbox, *contentsTable, *ramStore) {
	t.Helper()
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	parent := box.objects[0].Parent
	box.objects = nil
	for i, subject := range []string{"Zulu", "Alpha", "Bravo"} {
		o := messageTestObject(core.ID(string(rune('a'+i))), parent)
		o.MIME = []byte("From: synthetic@example.invalid\r\nSubject: " + subject + "\r\n\r\nSynthetic body")
		box.objects = append(box.objects, o)
	}
	table, store := openTableTest(t, f, flags)
	columns := tableColumnsWire(2, 0x0037001f, 0x674a0014)
	columns[3] = 1
	out, _ := f.run(columns)
	if !bytes.Equal(out, []byte{0x12, 2, 0, 0, 0, 0, 0}) {
		t.Fatal("async columns not completed synchronously")
	}
	return f, box, table, store
}
func requireRestrictSuccess(t *testing.T, f *ostFixture, raw []byte) {
	t.Helper()
	out, _ := f.run(tableRestrictWire(2, 1, raw))
	if !bytes.Equal(out, []byte{0x14, 2, 0, 0, 0, 0, 0}) {
		t.Fatal("Restrict wire success")
	}
}
func TestTableFilterFindCodecAtomicPreflight(t *testing.T) {
	decode := func(w []byte) error {
		_, err := (ROPBuffer{Operations: w, Handles: make([]uint32, 3)}).decodeTableOperations(false, false, false, false, false, false, false, false, false, false, false, true)
		return err
	}
	for _, w := range [][]byte{tableRestrictWire(2, 1, tableSubjectRestriction("Alpha", 4)), tableRestrictWire(2, 0, nil), tableFindWire(2, 0, 0, tableSubjectRestriction("Alpha", 4), nil), tableFindWire(2, 1, 2, nil, nil), tableFindWire(2, 0, 3, nil, []byte{1, 2})} {
		if err := decode(w); err != nil {
			t.Fatal("valid restriction request", err)
		}
		for n := 1; n < len(w); n++ {
			if !errors.Is(decode(w[:n]), ErrMalformed) {
				t.Fatal("truncated request accepted")
			}
		}
	}
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	for _, w := range [][]byte{tableRestrictWire(2, 2, nil), tableFindWire(2, 2, 0, nil, nil), tableFindWire(2, 0, 4, nil, nil), tableFindWire(2, 0, 0, nil, []byte{1}), tableRestrictWire(2, 0, []byte{8, 0})} {
		if decode(w) == nil {
			t.Fatal("invalid table restriction accepted")
		}
		f.execute(append([]byte{1, 4, 1}, w...), 65535)
		if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 || box.sourceCalls != 0 {
			t.Fatal("rejected batch executed Release prefix")
		}
	}
	f, _, table, store := tableFilterFixture(t, 0x10)
	table.cursor = 2
	used := store.allocator.contents.used
	for _, w := range [][]byte{tableRestrictWire(2, 0, nil), tableFindWire(2, 0, 0, nil, nil)} {
		f.execute(w, uint32(10+4*len(f.handles)+6))
		if table.cursor != 2 || table.restriction != nil || used != store.allocator.contents.used {
			t.Fatal("preflight mutated filter/search")
		}
	}
}
func TestTableRestrictionFindRowsCursorAndReset(t *testing.T) {
	f, _, table, store := tableFilterFixture(t, 0x10)
	tags := []uint32{0x0037001f, 0x674a0014}
	baseline := store.allocator.contents.used
	requireRestrictSuccess(t, f, tableSubjectRestriction("Bravo", 3)) // >=
	if len(table.rows) != 2 || len(table.baseRows) != 3 || table.cursor != 0 {
		t.Fatal("restriction did not select source rows")
	}
	requireSortSuccess(t, f, tableSortWire(2, 0, 0, 0, tableSortKey{0x0037001f, 1}))
	out, _ := f.run(tableQueryWire(2, 0, 1, 9))
	rows := decodeTableRows(t, out, tags)
	if rows[0][0] != "Zulu" || rows[1][0] != "Bravo" {
		t.Fatal("restriction and sort compose incorrectly")
	}
	out, _ = f.run(tableFindWire(2, 0, 0, tableSubjectRestriction("Bravo", 4), nil))
	if !reflect.DeepEqual(decodeFindRow(t, out, tags), rows[1]) || table.cursor != 1 {
		t.Fatal("FindRow did not locate exact row and cursor")
	}
	out, _ = f.run(tableQueryWire(2, 0, 1, 1))
	if !reflect.DeepEqual(decodeTableRows(t, out, tags)[0], rows[1]) {
		t.Fatal("FindRow positioned after found row")
	}
	out, _ = f.run(tableFindWire(2, 1, 2, tableSubjectRestriction("Zulu", 4), nil))
	if decodeFindRow(t, out, tags)[0] != "Zulu" || table.cursor != 0 {
		t.Fatal("backward find from End")
	}
	out, _ = f.run(tableFindWire(2, 0, 1, tableSubjectRestriction("Missing", 4), nil))
	if !bytes.Equal(out, ropError(0x4f, 2, 0x8004010f)) || table.cursor != 0 {
		t.Fatal("NotFound was hidden or moved cursor")
	}
	requireRestrictSuccess(t, f, tableSubjectRestriction("Alpha", 4))
	if len(table.rows) != 1 {
		t.Fatal("replacement filter searched already restricted rows")
	}
	out, _ = f.run(tableQueryWire(2, 0, 1, 9))
	if decodeTableRows(t, out, tags)[0][0] != "Alpha" {
		t.Fatal("replacement filter lost full source")
	}
	requireRestrictSuccess(t, f, nil)
	if len(table.rows) != 3 {
		t.Fatal("empty restriction failed to restore all rows")
	}
	requireSortSuccess(t, f, tableSortWire(2, 0, 0, 0))
	if store.allocator.contents.used != baseline {
		t.Fatal("cleared filter/sort retained their memory")
	}
	f.run([]byte{1, 4, 2})
	if store.allocator.contents.used != 0 {
		t.Fatal("Release leaked filter/source memory")
	}
}
func TestTableFilterRefreshHiddenChangesAndFindOutput(t *testing.T) {
	f, box, table, _ := tableFilterFixture(t, 0)
	requireRestrictSuccess(t, f, tableSubjectRestriction("Bravo", 3))
	requireSortSuccess(t, f, tableSortWire(2, 0, 0, 0, tableSortKey{0x0037001f, 0}))
	added := messageTestObject("hidden-new", box.objects[0].Parent)
	added.MIME = []byte("Subject: A hidden item\r\n\r\nSynthetic body")
	box.objects = append(box.objects, added)
	out, _ := f.run(nil)
	requireTableChanged(t, out, f.handles[2])
	if len(table.rows) != 2 || len(table.baseRows) != 4 {
		t.Fatal("filtered refresh lost hidden source updates")
	}
	out, _ = f.run(nil)
	if len(out) != 0 {
		t.Fatal("unchanged filtered/sorted refresh notified repeatedly")
	}
	requireRestrictSuccess(t, f, nil)
	if len(table.rows) != 4 {
		t.Fatal("reset did not recover hidden new mail")
	}
	w := tableFindWire(2, 0, 0, tableSubjectRestriction("Zulu", 4), nil)
	body, _ := f.execute(w, uint32(10+4*len(f.handles)+8))
	if !bytes.Equal(storeResponse(t, body).Operations, []byte{0x4f, 2, 0, 0, 0, 0, 0, 0}) || table.cursor != 3 {
		t.Fatal("tight FindRow should position and omit row data")
	}
	out, _ = f.run(tableFindWire(2, 1, 1, tableSubjectRestriction("Bravo", 4), nil))
	if decodeFindRow(t, out, []uint32{0x0037001f, 0x674a0014})[0] != "Bravo" || table.cursor != 2 {
		t.Fatal("backward Current included current row or skipped previous")
	}
	out, _ = f.run(tableFindWire(2, 0, 3, nil, []byte{1}))
	if !bytes.Equal(out, ropError(0x4f, 2, 0x80040405)) {
		t.Fatal("custom bookmark falsely accepted")
	}
}
func TestTableRestrictionErrorsPreserveViewAndQuota(t *testing.T) {
	for _, mode := range []string{"unknown", "budget", "cancel", "owner"} {
		t.Run(mode, func(t *testing.T) {
			f, box, table, store := tableFilterFixture(t, 0x10)
			requireRestrictSuccess(t, f, tableSubjectRestriction("Alpha", 4))
			table.cursor = 1
			before := append([]contentsValues(nil), table.rows...)
			used := store.allocator.contents.used
			previous := table.restriction
			ctx := context.Background()
			raw := tableSubjectRestriction("Bravo", 4)
			switch mode {
			case "unknown":
				binary.LittleEndian.PutUint32(raw[2:6], 0x1234001f)
			case "budget":
				store.allocator.contents.used = maxGlobalContentsMemory
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "owner":
				box.state.Owner = false
			}
			ops, err := (ROPBuffer{Operations: tableRestrictWire(2, 0, raw), Handles: f.handles}).decodeTableOperations(false, false, false, false, false, false, false, false, false, false, false, true)
			if err != nil {
				t.Fatal(err)
			}
			out, lease := f.transport.executeTable(ctx, f.session, f.state.mailbox, ops[0], f.handles, 1252, nil)
			if lease != nil || binary.LittleEndian.Uint32(out[2:]) == 0 || table.restriction != previous || table.cursor != 1 || !reflect.DeepEqual(table.rows, before) {
				t.Fatal("failed filter changed state")
			}
			if mode == "budget" {
				store.allocator.contents.used = used
			}
			if store.allocator.contents.used != used {
				t.Fatal("failed filter leaked quota")
			}
		})
	}
}

func TestTableHierarchyRestrictionAndAssociatedFind(t *testing.T) {
	f, _ := hierarchyTableFixture(t)
	table, _ := hierarchyTableOpen(t, f, 0x10)
	f.run(tableColumnsWire(2, 0x3001001f, 0x67480014))
	// Existence on a real hierarchy column, not a fabricated mail column.
	raw := []byte{8, 0x1f, 0, 1, 0x30}
	requireRestrictSuccess(t, f, raw)
	out, _ := f.run(tableFindWire(2, 0, 0, raw, nil))
	if len(decodeFindRow(t, out, []uint32{0x3001001f, 0x67480014})) != 2 || len(table.rows) == 0 {
		t.Fatal("hierarchy search failed")
	}
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	seedFAI(t, object.binding.store, faiTestRecord(object.binding.store, f.fids[Inbox], 1))
	openTableTest(t, f, 0x12)
	f.run(tableColumnsWire(2, 0x70070003))
	out, _ = f.run(tableFindWire(2, 0, 0, filterWire(t, "04 04 03000770 03000000 08000000"), nil))
	if decodeFindRow(t, out, []uint32{0x70070003})[0] != uint32(8) || box.sourceCalls != 0 {
		t.Fatal("FAI find read native content or returned invented value")
	}
}

func TestTableFilterSortFindBatchExecutesCompletePrefix(t *testing.T) {
	f, _, _, _ := tableFilterFixture(t, 0x10)
	columns := tableColumnsWire(2, 0x0037001f, 0x674a0014)
	columns[3] = 1
	batch := append(columns, tableRestrictWire(2, 1, tableSubjectRestriction("Bravo", 3))...)
	batch = append(batch, tableSortWire(2, 1, 0, 0, tableSortKey{0x0037001f, 0})...)
	batch = append(batch, tableFindWire(2, 0, 0, tableSubjectRestriction("Bravo", 4), nil)...)
	f.transport.EnableSynchronizationOptionsTrace()
	out, counts := f.run(batch)
	_, operations, firstError, _, _ := counts.execution.Fields()
	if counts.errors != 0 || firstError != "none" || !reflect.DeepEqual(operations, []string{"set_columns", "sort_table", "restrict_table", "find_row"}) {
		t.Fatal("full Table batch did not execute")
	}
	if !bytes.Equal(out[:21], []byte{0x12, 2, 0, 0, 0, 0, 0, 0x14, 2, 0, 0, 0, 0, 0, 0x13, 2, 0, 0, 0, 0, 0}) {
		t.Fatal("Table batch response prefix")
	}
	if decodeFindRow(t, out[21:], []uint32{0x0037001f, 0x674a0014})[0] != "Bravo" {
		t.Fatal("Table batch did not return actual matching row")
	}
}
