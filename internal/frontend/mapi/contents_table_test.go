// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"
	"unicode/utf8"

	"ewsgo/internal/core"
)

func tableColumnsWire(index byte, tags ...uint32) []byte {
	out := binary.LittleEndian.AppendUint16([]byte{0x12, 4, index, 0}, uint16(len(tags)))
	for _, tag := range tags {
		out = append32(out, tag)
	}
	return out
}
func tableQueryWire(index, flags, forward byte, count uint16) []byte {
	return binary.LittleEndian.AppendUint16([]byte{0x15, 4, index, flags, forward}, count)
}
func tableSeekWire(index, origin byte, n int32) []byte {
	return append(append32([]byte{0x18, 4, index, origin}, uint32(n)), 1)
}
func openTableTest(t *testing.T, f *ostFixture, flags byte) (*contentsTable, *ramStore) {
	t.Helper()
	out, counts := f.run([]byte{5, 4, 1, 2, flags})
	if counts.errors != 0 || len(out) != 10 || binary.LittleEndian.Uint32(out[2:]) != 0 {
		t.Fatal("opening Table failed")
	}
	o, code := f.state.mailbox.lookup(f.handles[2], 4)
	if code != 0 || o.table == nil {
		t.Fatal("missing Table object")
	}
	return o.table, o.binding.store
}

// Independent MS-OXCDATA PropertyRow decoder: does not call production row,
// value, FastTransfer or request codec helpers. Values remain fixture-local.
func decodeTableRows(t *testing.T, wire []byte, tags []uint32) [][]any {
	t.Helper()
	if len(wire) < 9 || wire[0] != 0x15 || binary.LittleEndian.Uint32(wire[2:]) != 0 {
		t.Fatal("invalid QueryRows success")
	}
	n := int(binary.LittleEndian.Uint16(wire[7:]))
	pos := 9
	take := func(n int) []byte {
		t.Helper()
		if n < 0 || pos+n > len(wire) {
			t.Fatal("truncated table row")
		}
		out := wire[pos : pos+n]
		pos += n
		return out
	}
	rows := make([][]any, n)
	for i := range rows {
		flagged := take(1)[0]
		if flagged > 1 {
			t.Fatal("invalid row flag")
		}
		rows[i] = make([]any, len(tags))
		for j, tag := range tags {
			if flagged == 1 {
				flag := take(1)[0]
				if flag == 0xa {
					rows[i][j] = binary.LittleEndian.Uint32(take(4))
					continue
				}
				if flag != 0 {
					t.Fatal("invalid cell flag")
				}
			}
			switch uint16(tag) {
			case 2:
				rows[i][j] = binary.LittleEndian.Uint16(take(2))
			case 3:
				rows[i][j] = binary.LittleEndian.Uint32(take(4))
			case 0x14, 0x40:
				rows[i][j] = binary.LittleEndian.Uint64(take(8))
			case 0xb:
				rows[i][j] = take(1)[0] == 1
			case 0xfb, 0x102:
				rows[i][j] = append([]byte(nil), take(int(binary.LittleEndian.Uint16(take(2))))...)
			case 0x1f, 0x301f:
				var units []uint16
				for {
					u := binary.LittleEndian.Uint16(take(2))
					if u == 0 {
						break
					}
					units = append(units, u)
				}
				rows[i][j] = string(utf16.Decode(units))
			case 0x1e, 0x301e:
				var value []byte
				for {
					b := take(1)[0]
					if b == 0 {
						break
					}
					value = append(value, b)
				}
				rows[i][j] = string(value)
			default:
				t.Fatal("unsupported independent fixture type")
			}
		}
	}
	if pos != len(wire) {
		t.Fatal("trailing table response bytes")
	}
	return rows
}

func TestContentsTableCodecAndWholeBatchRejection(t *testing.T) {
	valid := [][]byte{{5, 4, 1, 2, 0x10}, {5, 4, 1, 2, 0x52}, tableColumnsWire(2, 0x674a0014, 0x37001f), tableQueryWire(2, 0, 1, 1), tableQueryWire(2, 1, 0, 2), {0x16, 4, 2}, {0x17, 4, 2}, tableSeekWire(2, 2, -1)}
	decode := func(w []byte, on bool) ([]Operation, error) {
		return (ROPBuffer{Operations: w, Handles: make([]uint32, 3)}).decodeTableOperations(false, false, false, false, false, false, false, false, false, false, false, on)
	}
	for _, wire := range valid {
		if _, err := decode(wire, true); err != nil {
			t.Fatal("valid table request rejected")
		}
		if _, err := decode(wire, false); !errors.Is(err, ErrUnsupported) {
			t.Fatal("default gate admitted tables")
		}
		for n := 1; n < len(wire); n++ {
			if _, err := decode(wire[:n], true); !errors.Is(err, ErrMalformed) {
				t.Fatalf("truncation %x/%d: %v", wire[0], n, err)
			}
		}
	}
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	for _, bad := range [][]byte{
		{5, 4, 1, 2, 0x22}, {5, 4, 1, 2, 0x18}, {5, 4, 1, 2, 0x80},
		{5, 4, 1, 4, 0x10}, {5, 4, 4, 2, 0x10}, {0x12, 4, 2, 2, 1, 0, 3, 0, 7, 0xe},
		tableColumnsWire(2), tableColumnsWire(2, 0x370000), tableColumnsWire(2, 0x37000a), tableColumnsWire(2, 0x373003),
		tableQueryWire(2, 4, 1, 2), tableQueryWire(2, 0, 2, 2), tableQueryWire(2, 0, 1, 0), tableSeekWire(2, 3, 1),
		{0x13, 4, 2}, {0x14, 4, 2}, {0x4f, 4, 2},
	} {
		f.execute(append([]byte{1, 4, 1}, bad...), 65535)
		if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 || box.sourceCalls != 0 || f.transport.stores.contents.used != 0 {
			t.Fatal("rejected batch had prefix/native/memory effects")
		}
	}
	for flags := 0; flags < 256; flags++ {
		_, err := decode([]byte{5, 4, 1, 2, byte(flags)}, true)
		allowed := flags & ^0x72 == 0 && flags&0x22 != 0x22
		if (err == nil) != allowed {
			t.Fatal("TableFlags acceptance drift")
		}
	}
}
func TestContentsTableNormalRowsCursorAndSnapshot(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	parent := box.objects[0].Parent
	for i := 2; i <= 3; i++ {
		box.objects = append(box.objects, messageTestObject(core.ID(fmt.Sprintf("message-%d", i)), parent))
	}
	table, store := openTableTest(t, f, 0x50)
	if len(table.rows) != 3 || box.sourceCalls != 1 {
		t.Fatal("snapshot capture")
	}
	for _, id := range []byte{0x16, 0x17} {
		out, _ := f.run([]byte{id, 4, 2})
		if binary.LittleEndian.Uint32(out[2:]) != 0 {
			t.Fatal("Table status/position")
		}
	}
	out, _ := f.run(tableQueryWire(2, 0, 1, 1))
	if !bytes.Equal(out, ropError(0x15, 2, nullObject)) {
		t.Fatal("QueryRows without columns")
	}
	tags := []uint32{
		0x67480014, 0x674a0014, 0x1a001f, 0x67aa000b, 0x65e00102, 0x30080040,
		0x37001f, 0x0e69000b, 0x12340003, 0x8001101f, 0x8001101e,
		0x667900fd, 0x668000fe, 0x802a0005, 0x802b1003,
	}
	out, counts := f.run(tableColumnsWire(2, tags...))
	if counts.errors != 0 || !bytes.Equal(out, []byte{0x12, 2, 0, 0, 0, 0, 0}) {
		t.Fatal("SetColumns")
	}
	query := func(flags, forward byte, n uint16) [][]any {
		t.Helper()
		out, counts := f.run(tableQueryWire(2, flags, forward, n))
		if counts.errors != 0 {
			t.Fatal("QueryRows")
		}
		return decodeTableRows(t, out, tags)
	}
	first := query(1, 1, 1)
	if len(first) != 1 || table.cursor != 0 || first[0][0] != f.fids[Inbox] || first[0][2] != "IPM.Note" || first[0][3] != false || first[0][8] != uint32(0x8004010f) {
		t.Fatal("typed row, absence, NoAdvance")
	}
	if first[0][9] != uint32(0x8004010f) || first[0][10] != uint32(0x8004010f) {
		t.Fatal("absent multivalue properties must report per-cell ecNotFound")
	}
	for _, missing := range first[0][11:] {
		if missing != uint32(0x8004010f) {
			t.Fatal("missing restriction, rule actions or numeric property must report per-cell ecNotFound")
		}
	}
	if len(first[0][4].([]byte)) != 22 || first[0][5].(uint64) == 0 {
		t.Fatal("real sync metadata")
	}
	if !reflect.DeepEqual(first, query(0, 1, 1)) || table.cursor != 1 {
		t.Fatal("cursor advance")
	}
	rest := query(0, 1, 65535)
	if len(rest) != 2 || table.cursor != 3 || rest[0][1] == first[0][1] {
		t.Fatal("whole snapshot enumeration")
	}
	if rows := query(0, 1, 1); len(rows) != 0 {
		t.Fatal("end must be empty")
	}
	backwards := query(1, 0, 2)
	if !reflect.DeepEqual(backwards, rest) || table.cursor != 3 {
		t.Fatal("backwards order and NoAdvance")
	}
	out, _ = f.run(tableSeekWire(2, 2, -99))
	if table.cursor != 0 || out[6] != 1 || int32(binary.LittleEndian.Uint32(out[7:])) != -3 {
		t.Fatal("clamped backward seek")
	}
	out, _ = f.run(tableSeekWire(2, 1, 2))
	if table.cursor != 2 || out[6] != 0 || binary.LittleEndian.Uint32(out[7:]) != 2 {
		t.Fatal("relative seek")
	}
	out, _ = f.run([]byte{0x17, 4, 2})
	if binary.LittleEndian.Uint32(out[6:]) != 2 || binary.LittleEndian.Uint32(out[10:]) != 3 {
		t.Fatal("position numerator/denominator")
	}
	// Underlying changes affect only a newly opened snapshot, never this cursor.
	box.objects = nil
	if len(query(1, 0, 2)) != 2 || box.sourceCalls != 1 {
		t.Fatal("snapshot unexpectedly refreshed or exposed new source")
	}
	f.run([]byte{1, 4, 2})
	if store.allocator.contents.used != 0 {
		t.Fatal("Release leaked Table snapshot/output")
	}
	table, _ = openTableTest(t, f, 0x50)
	if len(table.rows) != 0 || box.sourceCalls != 2 {
		t.Fatal("empty source represented incorrectly")
	}
}
func TestContentsTableAssociatedActualRowsAndIsolation(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	table, store := openTableTest(t, f, 0x52)
	if len(table.rows) != 0 || box.sourceCalls != 0 {
		t.Fatal("initial FAI table invented rows/native reads")
	}
	f.run([]byte{1, 4, 2})
	seedFAI(t, store, faiTestRecord(store, f.fids[Inbox], 1), faiTestRecord(store, f.fids[SentItems], 2))
	table, _ = openTableTest(t, f, 0x12)
	if len(table.rows) != 1 || box.sourceCalls != 0 {
		t.Fatal("FAI selection isolation")
	}
	tags := []uint32{0x674a0014, 0x1a001f, 0x70070003, 0x70010102, 0x67aa000b, 0x68330048}
	f.run(tableColumnsWire(2, tags...))
	out, _ := f.run(tableQueryWire(2, 0, 1, 9))
	rows := decodeTableRows(t, out, tags)
	if len(rows) != 1 || rows[0][1] != "IPM.Microsoft.FolderDesign.NamedView" || rows[0][2] != uint32(8) || !bytes.Equal(rows[0][3].([]byte), []byte{1, 2, 3}) || rows[0][4] != true || rows[0][5] != uint32(PropertyNotFound) {
		t.Fatal("actual FAI columns")
	}
	before := rows[0][0]
	f.state.mailbox.close()
	if store.allocator.contents.used != store.fai.bytes {
		t.Fatal("disconnect-equivalent cleanup leaked Table")
	}
	f.state.mailbox = newMailboxObjects(f.transport.stores)
	f.handles = []uint32{0xffffffff, 0xffffffff, 0xffffffff, 0xffffffff}
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	f.open(int(Inbox))
	openTableTest(t, f, 0x52)
	f.run(tableColumnsWire(2, tags...))
	out, _ = f.run(tableQueryWire(2, 0, 1, 9))
	if decodeTableRows(t, out, tags)[0][0] != before {
		t.Fatal("MID changed on reconnect in same process")
	}
}
func TestContentsTableOutputMemoryAndObjectGuards(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	body, _ := f.execute([]byte{1, 4, 1, 5, 4, 1, 2, 0x50}, uint32(10+4*len(f.handles)+9))
	if storeResponse(t, body).Operations[0] != 0xff || box.sourceCalls != 0 {
		t.Fatal("output preflight had effects")
	}
	table, store := openTableTest(t, f, 0x50)
	tags := []uint32{0x674a0014}
	f.run(tableColumnsWire(2, tags...))
	baseline := store.allocator.contents.used
	// Exact minimum response budget leaves no room for the nine-byte row.
	body, _ = f.execute(tableQueryWire(2, 0, 1, 1), uint32(10+4*len(f.handles)+9))
	if !bytes.Equal(storeResponse(t, body).Operations, ropError(0x15, 2, tableBufferTooSmall)) || table.cursor != 0 || store.allocator.contents.used != baseline {
		t.Fatal("small output changed cursor or leaked")
	}
	body, _ = f.execute(tableQueryWire(2, 0, 1, 1), uint32(10+4*len(f.handles)+18))
	if len(decodeTableRows(t, storeResponse(t, body).Operations, tags)) != 1 || table.cursor != 1 {
		t.Fatal("exact single row capacity")
	}
	f.run(tableSeekWire(2, 0, 0))
	reserved, err := store.reserveContentsMemory(maxStoreContentsMemory - baseline)
	if err != nil {
		t.Fatal("budget fixture")
	}
	out, counts := f.run(tableQueryWire(2, 0, 1, 1))
	if counts.errors != 1 || binary.LittleEndian.Uint32(out[2:]) == 0 || table.cursor != 0 {
		t.Fatal("unaccounted output/cursor on ledger exhaustion")
	}
	f.transport.EnableUnknownPropertyTrace()
	f.transport.EnableSynchronizationOptionsTrace()
	out, counts = f.run([]byte{5, 4, 1, 3, 0x50})
	if !bytes.Equal(out, ropError(5, 3, NotSupported)) ||
		!reflect.DeepEqual(counts.execution.TableProfiles(), []string{"get_contents_table:budget"}) {
		t.Fatal("unexpected diagnostic for contents table admission")
	}
	reserved.release()
	// The single legacy accessor guards every preexisting Folder/Logon consumer.
	if _, code := f.state.mailbox.object(f.handles[2], 4); code != NotSupported {
		t.Fatal("Table interpreted as legacy object")
	}
	for _, wire := range [][]byte{folderPropertiesOperation(4, 2, 0, 0x67480014), folderOperation(f.fids[Inbox], 4, 2, 3), []byte{5, 4, 2, 3, 0x50}} {
		out, counts = f.run(wire)
		if counts.errors != 1 || binary.LittleEndian.Uint32(out[2:]) != NotSupported {
			t.Fatal("wrong-type dispatch admitted Table")
		}
	}
	out, _ = f.run([]byte{0x16, 5, 2})
	if binary.LittleEndian.Uint32(out[2:]) != accessDenied {
		t.Fatal("cross-logon Table access")
	}
	out, _ = f.run([]byte{0x16, 4, 1})
	if binary.LittleEndian.Uint32(out[2:]) != NotSupported {
		t.Fatal("Folder interpreted as Table")
	}
	f.run([]byte{1, 4, 2})
	out, _ = f.run([]byte{0x16, 4, 2})
	if binary.LittleEndian.Uint32(out[2:]) != invalidObject || store.allocator.contents.used != 0 {
		t.Fatal("released Table lifecycle")
	}
}
func TestContentsTableRevocationAndCaptureFailures(t *testing.T) {
	for _, mode := range []string{"owner", "scope", "folder", "cancel", "capture_owner", "capture_error", "live_limit", "allocation_limit"} {
		t.Run(mode, func(t *testing.T) {
			f, box := newContentsDownloadFixture(t)
			f.transport.EnableContentsTableSpike()
			o, _ := f.state.mailbox.object(f.handles[1], 4)
			store := o.binding.store
			if mode == "capture_owner" {
				box.readHook = func() { f.box.state.Owner = false }
			}
			if mode == "capture_error" {
				box.sourceErr = core.ErrConflict
			}
			if mode == "allocation_limit" {
				f.state.mailbox.allocated = maxContextHandleAllocations
			}
			if mode == "live_limit" {
				for len(f.state.mailbox.objects) < maxLiveStoreHandles {
					h, _ := f.transport.stores.handle()
					f.state.mailbox.objects[h] = o
				}
			}
			if strings.HasPrefix(mode, "capture_") || strings.HasSuffix(mode, "limit") {
				out, counts := f.run([]byte{5, 4, 1, 2, 0x50})
				if counts.errors != 1 || binary.LittleEndian.Uint32(out[2:]) == 0 || store.allocator.contents.used != 0 {
					t.Fatal("failed construction leaked/succeeded")
				}
				if strings.HasSuffix(mode, "limit") && box.sourceCalls != 0 {
					t.Fatal("handle limit read native payload")
				}
				return
			}
			table, _ := openTableTest(t, f, 0x50)
			f.run(tableColumnsWire(2, 0x674a0014))
			baseline := store.allocator.contents.used
			switch mode {
			case "owner":
				f.box.state.Owner = false
			case "scope":
				id := f.session.Identity()
				id.Scope.Account = "different"
				f.session = scopedSession{Session: f.session, identity: id}
			case "folder":
				delete(store.byFID, f.fids[Inbox])
			}
			wire, _ := (ROPBuffer{Operations: tableQueryWire(2, 0, 1, 1), Handles: f.handles}).Marshal()
			ctx := context.Background()
			if mode == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			body, counts := f.transport.executeStore(ctx, f.session, f.state, f.identity, Execute{ROP: wire, MaxOutput: 65535})
			out := storeResponse(t, body).Operations
			if counts.errors != 1 || binary.LittleEndian.Uint32(out[2:]) == 0 || table.cursor != 0 || store.allocator.contents.used != baseline || box.sourceCalls != 1 {
				t.Fatal("revoked Table read leaked rows/advanced/reloaded")
			}
			f.state.mailbox.close()
			if store.allocator.contents.used != 0 {
				t.Fatal("failed Table cleanup")
			}
		})
	}
}
func TestContentsTableCellWireTruncationAndEncoding(t *testing.T) {
	text := strings.Repeat("a", 253) + "😀" + strings.Repeat("z", 1000)
	values := contentsValues{fixedContentsProperty(0x37001f): core.Text(text), fixedContentsProperty(0x70010102): core.Binary(bytes.Repeat([]byte{7}, 900)), fixedContentsProperty(0xe69000b): core.Bool(true)}
	tags := []uint32{0x37001f, 0x70010102, 0xe69000b, 0x12340003}
	cells := make([]folderPropertyValue, len(tags))
	for i, tag := range tags {
		cells[i] = tableCell(values, tag, nil, 1252)
	}
	if len(cells[0].data) != 508 || len(cells[1].data) != 512 || len(cells[2].data) != 1 {
		t.Fatal("cell width/truncation boundary")
	}
	row := folderPropertyRow(specificProperties{tags: tags}, cells)
	wire := append([]byte{0x15, 2, 0, 0, 0, 0, 2, 1, 0}, row...)
	decoded := decodeTableRows(t, wire, tags)
	if decoded[0][0] != strings.Repeat("a", 253) || len(decoded[0][1].([]byte)) != 510 || decoded[0][2] != true || decoded[0][3] != uint32(0x8004010f) {
		t.Fatal("truncated PropertyRow values")
	}
	utf8Text := core.Text(strings.Repeat("a", 508) + "é")
	cell := tableCell(contentsValues{fixedContentsProperty(0x37001f): utf8Text}, 0x37001e, nil, 65001)
	if cell.code != 0 || len(cell.data) != 509 || !utf8.Valid(cell.data) {
		t.Fatal("String8 UTF8 truncation split character")
	}
	for _, v := range []any{values, &contentsTable{rows: []contentsValues{values}}, &tableRequest{columns: tags}} {
		if strings.Contains(fmt.Sprintf("%+v %#v", v, v), "😀") {
			t.Fatal("private Table formatting")
		}
	}
}
func FuzzContentsTableCodec(f *testing.F) {
	for _, seed := range [][]byte{{5, 4, 1, 2, 0x50}, tableColumnsWire(2, 0x674a0014), tableQueryWire(2, 1, 0, 2), tableSeekWire(2, 1, -1)} {
		f.Add(seed)
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = (ROPBuffer{Operations: data, Handles: make([]uint32, 4)}).decodeTableOperations(false, false, false, false, false, false, false, false, false, false, false, true)
	})
}

func TestContentsTableSpecialFoldersRemainMetadataOnly(t *testing.T) {
	for _, role := range []string{"calendar", "contacts", "tasks"} {
		t.Run(role, func(t *testing.T) {
			var f *ostFixture
			var entry []byte
			switch role {
			case "calendar":
				f, _ = newCalendarFixture(t)
				entry = calendarEntry(t, f)
			case "contacts":
				f, _ = newContactsFixture(t)
				entry = contactsEntry(t, f)
			case "tasks":
				f, _ = newTasksFixture(t)
				entry = tasksEntry(t, f)
			}
			t.Cleanup(f.state.mailbox.close)
			f.handles = append(f.handles, 0xffffffff)
			f.transport.EnableContentsTableSpike()
			resolved, c := f.run(conversionOperation(0x44, entry[22:]))
			if c.errors != 0 {
				t.Fatal("special folder conversion")
			}
			fid := binary.LittleEndian.Uint64(resolved[6:])
			f.run(folderOperation(fid, 4, 0, 1))
			out, c := f.run([]byte{5, 4, 1, 2, 0x50})
			if c.errors != 1 || binary.LittleEndian.Uint32(out[2:]) != NotSupported {
				t.Fatal("special native objects became available")
			}
			// Associated is the separately approved, initially empty EWSGo namespace.
			out, c = f.run([]byte{5, 4, 1, 2, 0x52})
			if c.errors != 0 || len(out) != 10 || binary.LittleEndian.Uint32(out[6:]) != 0 {
				t.Fatal("local FAI namespace unavailable")
			}
		})
	}
}

func TestContentsTableSameBatchAndLogonReplacement(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	wire := append([]byte{1, 4, 1}, []byte{5, 4, 1, 2, 0x50}...)
	f.execute(wire, 65535)
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 || box.sourceCalls != 0 {
		t.Fatal("default-off batch executed prefix")
	}
	f.transport.EnableContentsTableSpike()
	tags := []uint32{0x674a0014, 0x67480014}
	batch := append([]byte{5, 4, 1, 2, 0x50}, tableColumnsWire(2, tags...)...)
	batch = append(batch, tableQueryWire(2, 0, 1, 1)...)
	out, c := f.run(batch)
	if c.errors != 0 || len(decodeTableRows(t, out[17:], tags)) != 1 {
		t.Fatal("open/set/query in same batch")
	}
	if binary.LittleEndian.Uint32(out[6:]) != 1 {
		t.Fatal("open count differs from query")
	}
	tableObject, _ := f.state.mailbox.lookup(f.handles[2], 4)
	oldHandle := f.handles[2]
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	if tableObject.table.lease != nil || f.transport.stores.contents.used != 0 {
		t.Fatal("replacement Logon did not discard Table")
	}
	if _, code := f.state.mailbox.lookup(oldHandle, 4); code != invalidObject {
		t.Fatal("previous Table handle survived Logon replacement")
	}
}
