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
	"time"

	"ewsgo/internal/core"
)

func tableSortWire(index, flags byte, categories, expanded uint16, keys ...tableSortKey) []byte {
	w := binary.LittleEndian.AppendUint16([]byte{0x13, 4, index, flags}, uint16(len(keys)))
	w = binary.LittleEndian.AppendUint16(w, categories)
	w = binary.LittleEndian.AppendUint16(w, expanded)
	for _, key := range keys {
		w = append(binary.LittleEndian.AppendUint32(w, key.tag), key.order)
	}
	return w
}

func requireSortSuccess(t *testing.T, f *ostFixture, wire []byte) {
	t.Helper()
	out, _ := f.run(wire)
	if !bytes.Equal(out, []byte{0x13, 2, 0, 0, 0, 0, 0}) {
		t.Fatal("SortTable did not return the exact synchronous wire success")
	}
}

func TestTableSortCodecAndPreflight(t *testing.T) {
	decode := func(w []byte) error {
		_, err := (ROPBuffer{Operations: w, Handles: make([]uint32, 3)}).decodeTableOperations(false, false, false, false, false, false, false, false, false, false, false, true)
		return err
	}
	valid := tableSortWire(2, 1, 0, 0, tableSortKey{0x0e060040, 1}, tableSortKey{0x674a0014, 0})
	if err := decode(valid); err != nil {
		t.Fatal(err)
	}
	for n := 1; n < len(valid); n++ {
		if !errors.Is(decode(valid[:n]), ErrMalformed) {
			t.Fatal("truncated sort accepted")
		}
	}
	// The aggregate may appear immediately after the last category key.
	category := tableSortWire(2, 0, 1, 1, tableSortKey{0x0037001f, 0}, tableSortKey{0x0e060040, 4})
	if err := decode(category); err != nil {
		t.Fatal("valid category layout rejected")
	}
	bad := [][]byte{
		tableSortWire(2, 2, 0, 0),
		tableSortWire(2, 4, 0, 0),
		tableSortWire(2, 0, 1, 0),
		tableSortWire(2, 0, 0, 1),
		tableSortWire(2, 0, 0, 0, tableSortKey{0x0037001f, 4}),
		tableSortWire(2, 0, 1, 0, tableSortKey{0x0037001f, 0}, tableSortKey{0x0e060040, 0}, tableSortKey{0x674a0014, 4}),
		tableSortWire(2, 0, 0, 0, tableSortKey{0x0037001f, 2}),
		tableSortWire(2, 0, 0, 0, tableSortKey{0x0037001f, 3}),
	}
	tooMany := make([]tableSortKey, maxTableSortKeys+1)
	bad = append(bad, tableSortWire(2, 0, 0, 0, tooMany...))
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	for _, w := range bad {
		if decode(w) == nil {
			t.Fatal("invalid sort accepted")
		}
		f.execute(append([]byte{1, 4, 1}, w...), 65535)
		if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 || box.sourceCalls != 0 {
			t.Fatal("malformed batch executed Release prefix")
		}
	}
	table, store := openTableTest(t, f, 0x10)
	table.cursor = 1
	used, reads := store.allocator.contents.used, box.sourceCalls
	f.execute(valid, uint32(10+4*len(f.handles)+6))
	if table.cursor != 1 || len(table.sortKeys) != 0 || used != store.allocator.contents.used || reads != box.sourceCalls {
		t.Fatal("output preflight changed table")
	}
}

func TestTableSortRowsCursorReplacementAndRelease(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	parent := box.objects[0].Parent
	box.objects = nil
	for i, subject := range []string{"Zulu", "Alpha", "Alpha"} {
		o := messageTestObject(core.ID(string(rune('a'+i))), parent)
		o.Properties[core.Subject] = core.Text(subject)
		o.MIME = []byte("From: synthetic@example.invalid\r\nSubject: " + subject + "\r\n\r\nSynthetic body")
		o.Properties[core.ReceivedAt] = core.Timestamp(hierarchyTestTime.Add(time.Duration(i) * time.Hour))
		box.objects = append(box.objects, o)
	}
	table, store := openTableTest(t, f, 0x10)
	tags := []uint32{0x0037001f, 0x0e060040, 0x674a0014}
	f.run(tableColumnsWire(2, tags...))
	out, _ := f.run(tableQueryWire(2, 0, 1, 9))
	original := decodeTableRows(t, out, tags)
	if original[0][0] != "Zulu" || table.cursor != 3 {
		t.Fatal("fixture was not unsorted")
	}
	baseline := store.allocator.contents.used
	sortWire := tableSortWire(2, 1, 0, 0, tableSortKey{0x30070040, 0}, tableSortKey{0x0037001e, 0}, tableSortKey{0x0e060040, 1})
	requireSortSuccess(t, f, sortWire)
	for _, row := range table.rows {
		if _, ok := row[fixedContentsProperty(0x30070040)]; ok {
			t.Fatal("sorting projected a native creation time without a source")
		}
	}
	if table.cursor != 0 {
		t.Fatal("sort did not reset cursor")
	}
	out, _ = f.run(tableQueryWire(2, 0, 1, 9))
	rows := decodeTableRows(t, out, tags)
	if !reflect.DeepEqual(rows, [][]any{original[2], original[1], original[0]}) {
		t.Fatal("multi-key sort did not reorder real wire rows")
	}
	charged := store.allocator.contents.used
	if charged <= baseline {
		t.Fatal("retained sort specification was not charged")
	}
	for range 3 {
		requireSortSuccess(t, f, sortWire)
	}
	if store.allocator.contents.used != charged {
		t.Fatal("replacing a sort leaked memory")
	}
	f.run(tableQueryWire(2, 0, 1, 1))
	cursor := table.cursor
	for _, w := range [][]byte{
		tableSortWire(2, 0, 0, 0, tableSortKey{0x12340003, 0}),
		tableSortWire(2, 0, 1, 0, tableSortKey{0x0037001f, 0}, tableSortKey{0x12340003, 4}),
	} {
		out, _ = f.run(w)
		if !bytes.Equal(out, ropError(0x13, 2, NotSupported)) || table.cursor != cursor || charged != store.allocator.contents.used {
			t.Fatal("unsupported sort changed the old view")
		}
	}
	requireSortSuccess(t, f, tableSortWire(2, 0, 0, 0, tableSortKey{0x674a0014, 1}))
	out, _ = f.run(tableQueryWire(2, 0, 1, 9))
	rows = decodeTableRows(t, out, tags)
	if !reflect.DeepEqual(rows, [][]any{original[2], original[1], original[0]}) {
		t.Fatal("numeric descending MID sort failed")
	}
	requireSortSuccess(t, f, tableSortWire(2, 0, 0, 0))
	if table.cursor != 0 || store.allocator.contents.used != baseline {
		t.Fatal("empty sort did not clear retained specification")
	}
	f.run([]byte{1, 4, 2})
	if store.allocator.contents.used != 0 {
		t.Fatal("Release leaked sorted table")
	}
}

func TestTableSortRefreshKeepsOrder(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	box.objects[0].MIME = []byte("From: synthetic@example.invalid\r\nSubject: Z last item\r\n\r\nSynthetic body")
	table, _ := openTableTest(t, f, 0)
	tags := []uint32{0x0037001f}
	f.run(tableColumnsWire(2, tags...))
	requireSortSuccess(t, f, tableSortWire(2, 0, 0, 0, tableSortKey{0x0037001f, 0}))
	added := messageTestObject("new-message", box.objects[0].Parent)
	added.Properties[core.Subject] = core.Text("A first item")
	added.MIME = []byte("From: synthetic@example.invalid\r\nSubject: A first item\r\n\r\nSynthetic body")
	box.objects = append(box.objects, added)
	out, _ := f.run(nil)
	requireTableChanged(t, out, f.handles[2])
	out, _ = f.run(tableQueryWire(2, 0, 1, 9))
	rows := decodeTableRows(t, out, tags)
	if len(rows) != 2 || rows[0][0] != "A first item" || len(table.sortKeys) != 1 {
		t.Fatalf("refresh discarded sort or new mail: rows=%v keys=%d", rows, len(table.sortKeys))
	}
	out, _ = f.run(nil)
	if len(out) != 0 {
		t.Fatal("unchanged sorted refresh notified again")
	}
}

func TestTableSortFailureGuardsAndOtherObjects(t *testing.T) {
	for _, mode := range []string{"cancel", "owner", "budget", "bad_type"} {
		t.Run(mode, func(t *testing.T) {
			f, box := newContentsDownloadFixture(t)
			f.transport.EnableContentsTableSpike()
			table, store := openTableTest(t, f, 0x10)
			table.cursor = 1
			old := append([]contentsValues(nil), table.rows...)
			used := store.allocator.contents.used
			ctx := context.Background()
			switch mode {
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "owner":
				box.state.Owner = false
			case "budget":
				store.allocator.contents.used = maxGlobalContentsMemory
			case "bad_type":
				table.rows[0][fixedContentsProperty(0x0037001f)] = core.Integer(5)
			}
			ops, err := (ROPBuffer{Operations: tableSortWire(2, 0, 0, 0, tableSortKey{0x0037001f, 0}), Handles: f.handles}).decodeTableOperations(false, false, false, false, false, false, false, false, false, false, false, true)
			if err != nil {
				t.Fatal(err)
			}
			out, lease := f.transport.executeTable(ctx, f.session, f.state.mailbox, ops[0], f.handles, 1252, nil)
			if lease != nil || binary.LittleEndian.Uint32(out[2:]) == 0 || table.cursor != 1 || table.sortLease != nil || !reflect.DeepEqual(table.rows, old) {
				t.Fatal("failed sort published changes")
			}
			if mode == "budget" {
				store.allocator.contents.used = used
			}
			if store.allocator.contents.used != used {
				t.Fatal("failed sort leaked memory")
			}
		})
	}
	f, _ := hierarchyTableFixture(t)
	hierarchyTableOpen(t, f, 0x10)
	out, _ := f.run(tableSortWire(2, 0, 0, 0, tableSortKey{0x3001001f, 0}))
	if !bytes.Equal(out, ropError(0x13, 2, NotSupported)) {
		t.Fatal("hierarchy table sorting falsely advertised")
	}
}

func TestTableSortBinaryAbsentAndFullTextValues(t *testing.T) {
	p := fixedContentsProperty(0x00710102)
	rows := []contentsValues{{p: core.Binary{3}}, {}, {p: core.Binary{1}}}
	if err := sortBoundTableRows(rows, []boundTableSort{{property: p}}); err != nil || len(rows[0]) != 0 || !bytes.Equal(rows[1][p].(core.Binary), []byte{1}) {
		t.Fatal("binary or absent ordering")
	}
	p = fixedContentsProperty(0x0037001f)
	prefix := string(bytes.Repeat([]byte{'x'}, 600))
	rows = []contentsValues{{p: core.Text(prefix + "z")}, {p: core.Text(prefix + "a")}}
	if err := sortBoundTableRows(rows, []boundTableSort{{property: p}}); err != nil || rows[0][p] != core.Text(prefix+"a") {
		t.Fatal("sorted clipped wire cells instead of full values")
	}
}

func TestTableSortAssociatedRowsAndDiagnostic(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	f.transport.EnableSynchronizationOptionsTrace()
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	store := object.binding.store
	a, b := faiTestRecord(store, f.fids[Inbox], 1), faiTestRecord(store, f.fids[Inbox], 2)
	a.object.Properties[core.Key{ID: 0x7007}] = core.Integer(9)
	b.object.Properties[core.Key{ID: 0x7007}] = core.Integer(-2)
	seedFAI(t, store, a, b)
	openTableTest(t, f, 0x12)
	f.run(tableColumnsWire(2, 0x70070003))
	out, counts := f.run(tableSortWire(2, 0, 0, 0, tableSortKey{0x70070003, 0}))
	_, operations, firstError, _, _ := counts.execution.Fields()
	if !bytes.Equal(out, []byte{0x13, 2, 0, 0, 0, 0, 0}) || !reflect.DeepEqual(operations, []string{"sort_table"}) || firstError != "none" {
		t.Fatal("sort completion diagnostic lost operation beyond bit 31")
	}
	out, _ = f.run(tableQueryWire(2, 0, 1, 9))
	rows := decodeTableRows(t, out, []uint32{0x70070003})
	if len(rows) != 2 || rows[0][0] != uint32(0xfffffffe) || rows[1][0] != uint32(9) || box.sourceCalls != 0 {
		t.Fatal("associated signed sort failed or read native mail")
	}
	f.run([]byte{1, 4, 2})
	if store.allocator.contents.used != store.fai.bytes {
		t.Fatal("associated table retained a sort lease after Release")
	}

	pf, _, _ := permissionsFixture(t)
	permissionsOpen(t, pf, 0)
	out, _ = pf.run(tableSortWire(2, 0, 0, 0))
	if !bytes.Equal(out, ropError(0x13, 2, NotSupported)) {
		t.Fatal("permission sorting falsely advertised")
	}
}
