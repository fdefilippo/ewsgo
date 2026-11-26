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

	"ewsgo/internal/core"
)

func hierarchyTableFixture(t *testing.T) (*ostFixture, *messageFixtureMailbox) {
	f, b := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	root := b.Root()
	b.page.Objects = append(b.page.Objects,
		core.Object{ID: "table-parent", Parent: root, Kind: core.Folder, Properties: core.Properties{core.DisplayName: core.Text("Parent 📂")}},
		core.Object{ID: "table-child", Parent: "table-parent", Kind: core.Folder, Properties: core.Properties{core.DisplayName: core.Text("Child")}})
	f.open(int(IPMSubtree))
	return f, b
}
func hierarchyTableOpen(t *testing.T, f *ostFixture, flags byte) (*contentsTable, *ramStore) {
	t.Helper()
	out, c := f.run([]byte{4, 4, 1, 2, flags})
	if len(out) != 10 || c.errors != 0 || binary.LittleEndian.Uint32(out[2:]) != 0 {
		t.Fatal("hierarchy Table open failed")
	}
	o, code := f.state.mailbox.lookup(f.handles[2], 4)
	if code != 0 || o.table == nil || !o.table.hierarchy || uint32(len(o.table.rows)) != binary.LittleEndian.Uint32(out[6:]) {
		t.Fatal("hierarchy Table object/count mismatch")
	}
	return o.table, o.binding.store
}
func TestHierarchyTableCodecAndPreflight(t *testing.T) {
	f, b := hierarchyTableFixture(t)
	for flag := 0; flag < 256; flag++ {
		wire := []byte{4, 4, 1, 2, byte(flag)}
		_, err := (ROPBuffer{Operations: wire, Handles: f.handles}).decodeTableOperations(true, true, true, true, false, false, false, false, false, true, true, true)
		want := flag & ^0xf4 == 0
		if (err == nil) != want {
			t.Fatal("hierarchy table flags drift")
		}
	}
	wire := []byte{4, 4, 1, 2, 0x54}
	for n := 3; n < len(wire); n++ {
		r := reader{data: wire[3:n]}
		op := Operation{ID: 4}
		if !errors.Is(parseTableOperation(&r, &op, len(f.handles)), ErrMalformed) {
			t.Fatal("truncated open accepted")
		}
	}
	for _, bad := range [][]byte{{4, 4, 1, 2, 0x18}, {4, 4, 1, 2, 0x28}, {4, 4, 1, 9, 0x10}, {4, 4, 9, 2, 0x10}, {4, 4, 1, 2}} {
		f.execute(append([]byte{1, 4, 1}, bad...), 65535)
		if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 || b.sourceCalls != 0 || f.transport.stores.contents.used != 0 {
			t.Fatal("invalid batch executed prefix")
		}
	}
	f.transport.contentsTableSpike = false
	_, c := f.execute(append([]byte{1, 4, 1}, wire...), 65535)
	if c.unsupported != "RopGetHierarchyTable" {
		t.Fatal("default Table capability enabled")
	}
}

func TestSoftDeletedHierarchyThroughExecute(t *testing.T) {
	f, b := hierarchyTableFixture(t)
	table, store := hierarchyTableOpen(t, f, 0x40)
	if len(table.rows) == 0 {
		t.Fatal("active hierarchy fixture is empty")
	}
	f.run([]byte{1, 4, 2})
	used := store.allocator.contents.used
	for _, flags := range []byte{0x20, 0x64} {
		table, _ = hierarchyTableOpen(t, f, flags)
		if len(table.rows) != 0 || table.updating() {
			t.Fatal("soft-deleted hierarchy was populated or made live")
		}
		tags := []uint32{0x67480014, 0x3001001f}
		f.run(tableColumnsWire(2, tags...))
		out, c := f.run(tableQueryWire(2, 0, 1, 1))
		if c.errors != 0 || len(decodeTableRows(t, out, tags)) != 0 {
			t.Fatal("soft-deleted hierarchy query returned rows or failed")
		}
		f.run([]byte{1, 4, 2})
		if store.allocator.contents.used != used {
			t.Fatal("soft-deleted hierarchy leaked memory")
		}
	}
	b.state.Owner = false
	out, c := f.run([]byte{4, 4, 1, 2, 0x20})
	if c.errors != 1 || binary.LittleEndian.Uint32(out[2:]) != accessDenied {
		t.Fatal("soft-deleted hierarchy accepted unauthorized open")
	}
}

func TestHierarchyTableRowsDepthSnapshotAndReconnect(t *testing.T) {
	f, b := hierarchyTableFixture(t)
	table, store := hierarchyTableOpen(t, f, 0x50)
	tags := []uint32{0x67480014, 0x67490014, 0x3001001f, 0x66380003, 0x360a000b, 0x65e00102, 0x674a0014, folderEntryIDTag, parentFolderEntryIDTag}
	f.run(tableColumnsWire(2, tags...))
	out, _ := f.run(tableQueryWire(2, 1, 1, 65535))
	rows := decodeTableRows(t, out, tags)
	if len(rows) != 7 || table.cursor != 0 || b.sourceCalls != 0 {
		t.Fatal("immediate child selection/native message read")
	}
	var parent uint64
	view, _ := f.state.mailbox.lookup(f.handles[2], 4)
	for _, r := range rows {
		if r[1] != f.fids[IPMSubtree] || r[6] != uint32(PropertyNotFound) {
			t.Fatal("wrong parent or invented message ID")
		}
		if r[2] == "Parent 📂" {
			parent = r[0].(uint64)
			if r[3] != uint32(1) || r[4] != true {
				t.Fatal("child count")
			}
		}
		key := store.hierarchyXID(r[0].(uint64))
		if !bytes.Equal(r[5].([]byte), key[:]) {
			t.Fatal("source key mismatch")
		}
		for i := range 2 {
			fid, err := parseFolderEntryID(r[7+i].([]byte), view.binding.mailboxGUID, store.replica)
			if err != nil || fid != r[i].(uint64) {
				t.Fatal("folder or parent EntryID does not resolve to its row FID")
			}
		}
		if r[0] == f.fids[Inbox] {
			openAndRead := append(folderOperation(f.fids[Inbox], 4, 0, 3),
				folderPropertiesOperation(4, 3, 1, folderEntryIDTag, parentFolderEntryIDTag)...)
			direct, counts := f.run(openAndRead)
			want := append([]byte{2, 3, 0, 0, 0, 0, 0, 0, 7, 3, 0, 0, 0, 0, 0, folderEntryIDBytes, 0}, r[7].([]byte)...)
			want = append(append(want, folderEntryIDBytes, 0), r[8].([]byte)...)
			if counts.errors != 0 || !bytes.Equal(direct, want) {
				t.Fatal("direct read differs from the hierarchy EntryIDs")
			}
			f.run([]byte{1, 4, 3})
		}
	}
	if parent == 0 {
		t.Fatal("native parent omitted")
	}
	f.run([]byte{1, 4, 2})
	table, store = hierarchyTableOpen(t, f, 0x54)
	f.run(tableColumnsWire(2, tags...))
	out, _ = f.run(tableQueryWire(2, 0, 1, 65535))
	all := decodeTableRows(t, out, tags)
	if len(all) != 8 || table.cursor != 8 {
		t.Fatal("depth selection")
	}
	seen := map[uint64]bool{f.fids[IPMSubtree]: true}
	for _, r := range all {
		if !seen[r[1].(uint64)] {
			t.Fatal("parent not before child")
		}
		seen[r[0].(uint64)] = true
	}
	if len(store.contentsProofs) != 0 {
		t.Fatal("hierarchy Table published an ICS proof")
	}
	// Snapshot is detached. Reopening, rather than moving the cursor, observes changes.
	b.page.Objects = append(b.page.Objects, core.Object{ID: "new-table-child", Parent: b.Root(), Kind: core.Folder, Properties: core.Properties{core.DisplayName: core.Text("Later")}})
	f.run(tableSeekWire(2, 0, 0))
	out, _ = f.run(tableQueryWire(2, 0, 1, 65535))
	if !reflect.DeepEqual(all, decodeTableRows(t, out, tags)) {
		t.Fatal("open snapshot changed")
	}
	f.run([]byte{1, 4, 2})
	table, store = hierarchyTableOpen(t, f, 0x54)
	if len(table.rows) != 9 {
		t.Fatal("reopen missed new folder")
	}
	previous := f.handles[2]
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	if table.lease != nil || store.allocator.contents.used != 0 {
		t.Fatal("Logon replacement leaked Table")
	}
	if _, code := f.state.mailbox.lookup(previous, 4); code != invalidObject {
		t.Fatal("old Table survived replacement")
	}
	f.open(int(IPMSubtree))
	table, _ = hierarchyTableOpen(t, f, 0x50)
	f.run(tableColumnsWire(2, 0x67480014, 0x3001001f))
	out, _ = f.run(tableQueryWire(2, 0, 1, 65535))
	for _, r := range decodeTableRows(t, out, []uint32{0x67480014, 0x3001001f}) {
		if r[1] == "Parent 📂" && r[0] != parent {
			t.Fatal("FID changed across reconnect")
		}
	}
	f.run([]byte{1, 4, 2})
	if store.allocator.contents.used != 0 {
		t.Fatal("Release leaked Table")
	}
}
func TestHierarchyTableAccessAndResourceFailures(t *testing.T) {
	for _, scenario := range []string{"owner", "scope", "missing", "cancel", "budget", "output", "wrong_handle"} {
		t.Run(scenario, func(t *testing.T) {
			f, b := hierarchyTableFixture(t)
			table, store := hierarchyTableOpen(t, f, 0x54)
			f.run(tableColumnsWire(2, 0x3001001f))
			used := store.allocator.contents.used
			switch scenario {
			case "owner":
				b.state.Owner = false
			case "scope":
				identity := f.session.Identity()
				identity.Scope.Account = "foreign"
				f.session = scopedSession{Session: f.session, identity: identity}
			case "missing":
				// Bind to a native non-root anchor, then remove it from the source.
				fid := uint64(0)
				for _, r := range table.rows {
					if r[fixedContentsProperty(0x3001001f)] == core.Text("Parent 📂") {
						fid = uint64(r[fixedContentsProperty(0x67480014)].(core.Integer))
					}
				}
				f.run([]byte{1, 4, 2})
				f.run(folderOperation(fid, 4, 0, 1))
				table, store = hierarchyTableOpen(t, f, 0x50)
				f.run(tableColumnsWire(2, 0x3001001f))
				used = store.allocator.contents.used
				for i, object := range b.page.Objects {
					if object.ID == "table-parent" {
						b.page.Objects = append(b.page.Objects[:i], b.page.Objects[i+1:]...)
						break
					}
				}
			case "cancel":
				o, _ := f.state.mailbox.lookup(f.handles[2], 4)
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if f.transport.authorizeTable(ctx, f.session, o, 0x15) == nil {
					t.Fatal("cancellation ignored")
				}
				return
			case "budget":
				lease, err := store.reserveContentsMemory(maxStoreContentsMemory - used)
				if err != nil {
					t.Fatal("budget fixture")
				}
				defer lease.release()
			case "output":
				raw, c := f.execute(tableQueryWire(2, 0, 1, 1), uint32(10+4*len(f.handles)+9))
				out := storeResponse(t, raw).Operations
				if c.errors != 1 || !bytes.Equal(out, ropError(0x15, 2, tableBufferTooSmall)) || table.cursor != 0 {
					t.Fatal("output limit advanced cursor")
				}
				return
			case "wrong_handle":
				out, c := f.run([]byte{4, 4, 0, 3, 0x50})
				if c.errors != 1 || binary.LittleEndian.Uint32(out[2:]) != NotSupported {
					t.Fatal("Logon interpreted as Folder")
				}
				return
			}
			out, c := f.run(tableQueryWire(2, 0, 1, 1))
			if c.errors != 1 || binary.LittleEndian.Uint32(out[2:]) == 0 || table.cursor != 0 {
				t.Fatal("invalid access/resource used snapshot")
			}
			if scenario != "budget" && store.allocator.contents.used != used {
				t.Fatal("failed query leaked memory")
			}
		})
	}
}
