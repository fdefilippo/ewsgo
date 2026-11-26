// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"reflect"
	"strconv"
	"testing"

	"ewsgo/internal/core"
)

func requireTableChanged(t *testing.T, wire []byte, handle uint32) []byte {
	t.Helper()
	if len(wire) < 10 {
		t.Fatal("missing TableChanged")
	}
	n := wire[len(wire)-10:]
	if n[0] != 0x2a || binary.LittleEndian.Uint32(n[1:5]) != handle || n[5] != 4 || binary.LittleEndian.Uint16(n[6:8]) != 0x100 || binary.LittleEndian.Uint16(n[8:]) != 1 {
		t.Fatal("incorrect standalone MS-OXCROPS/MS-OXCNOTIF notification")
	}
	return wire[:len(wire)-10]
}

func TestUpdatingContentsTableRefreshAndPolling(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	f.transport.EnableSynchronizationOptionsTrace()
	table, store := openTableTest(t, f, 0)
	tags := []uint32{0x674a0014, 0x0037001f}
	f.run(tableColumnsWire(2, tags...))
	if table.subscribed || box.sourceCalls != 1 {
		t.Fatal("SetColumns subscribed or refreshed before navigation")
	}
	out, _ := f.run(tableQueryWire(2, 0, 1, 65535))
	first := decodeTableRows(t, out, tags)
	if len(first) != 1 || table.cursor != 1 || !table.subscribed {
		t.Fatal("first dynamic navigation failed")
	}
	used := store.allocator.contents.used
	for range 3 {
		out, _ = f.run(nil)
		if len(out) != 0 || used != store.allocator.contents.used {
			t.Fatal("unchanged polling notified or leaked memory")
		}
	}
	box.objects = append(box.objects, messageTestObject("new-message", box.objects[0].Parent))
	out, counts := f.run(nil)
	if len(requireTableChanged(t, out, f.handles[2])) != 0 || len(table.rows) != 2 || table.cursor != 0 || table.pending {
		t.Fatal("poll did not refresh/reset/deliver exactly once")
	}
	if !reflect.DeepEqual(counts.execution.NotificationDelivery(), []string{"contents_table_changed"}) {
		t.Fatal("contents notification missing from delivery diagnostics")
	}
	out, _ = f.run(tableQueryWire(2, 0, 1, 65535))
	rows := decodeTableRows(t, out, tags)
	if len(rows) != 2 || !reflect.DeepEqual(first[0], rows[0]) {
		t.Fatal("new view lost stable identities or new mail")
	}
	// Deletion is observed on a targeted read even before a poll is sent.
	box.objects = box.objects[1:]
	out, _ = f.run(tableQueryWire(2, 1, 1, 65535))
	rows = decodeTableRows(t, requireTableChanged(t, out, f.handles[2]), tags)
	if len(rows) != 1 || rows[0][0] == first[0][0] || table.cursor != 0 {
		t.Fatal("targeted refresh returned stale rows or advanced NoAdvance")
	}
	if len(store.contentsProofs) != 0 {
		t.Fatal("Table refresh published an ICS checkpoint")
	}
	f.run([]byte{1, 4, 2})
	if table.lease != nil || store.allocator.contents.used != 0 {
		t.Fatal("dynamic Table Release leaked memory")
	}
	out, _ = f.run(nil)
	if len(out) != 0 {
		t.Fatal("released Table emitted notification")
	}
}

func TestUpdatingHierarchyTableNotificationAndDepth(t *testing.T) {
	for _, flags := range []byte{0, 4, 0x40, 0x44, 0x84, 0xc4} {
		t.Run(strconv.FormatUint(uint64(flags), 16), func(t *testing.T) {
			f, box := hierarchyTableFixture(t)
			f.transport.EnableSynchronizationOptionsTrace()
			table, store := hierarchyTableOpen(t, f, flags)
			tags := []uint32{0x67480014, 0x3001001f}
			if out, _ := f.run(tableColumnsWire(2, tags...)); len(out) != 7 {
				t.Fatal("SetColumns generated a notification")
			}
			if out, _ := f.run([]byte{0x17, 4, 2}); len(out) != 14 {
				t.Fatal("QueryPosition generated a notification")
			}
			// The implicit subscription still reports changes to native folders.
			before := len(table.rows)
			box.page.Objects = append(box.page.Objects, core.Object{ID: "new-root-child", Parent: box.Root(), Kind: core.Folder, Properties: core.Properties{core.DisplayName: core.Text("New child")}})
			out, counts := f.run(nil)
			event := out
			if len(table.rows) != before+1 || box.sourceCalls != 0 {
				t.Fatal("hierarchy poll did not observe real catalog or read mail")
			}
			if !reflect.DeepEqual(counts.execution.NotificationDelivery(), []string{"hierarchy_table_row_added"}) {
				t.Fatal("hierarchy notification missing from delivery diagnostics")
			}
			if out, _ := f.run(nil); len(out) != 0 {
				t.Fatal("duplicate TableRowAdded")
			}
			out, _ = f.run(tableQueryWire(2, 0, 1, 65535))
			rows := decodeTableRows(t, out, tags)
			found := false
			for index, row := range rows {
				if row[1] == "New child" {
					var previous uint64
					if index > 0 {
						previous = rows[index-1][0].(uint64)
					}
					requireHierarchyRowEvent(t, event, tableRowAdded, f.handles[2], row[0].(uint64), previous, tags, row)
					found = true
				}
			}
			if !found || len(store.contentsProofs) != 0 {
				t.Fatal("hierarchy refresh missing new row or publishing ICS state")
			}
		})
	}
}

func TestHierarchySuppressNotificationsByMAPIContext(t *testing.T) {
	for _, flags := range []byte{0x80, 0x84} {
		t.Run(strconv.FormatUint(uint64(flags), 16), func(t *testing.T) {
			f, box := hierarchyTableFixture(t)
			f.transport.EnableCreateFolderSpike()
			f.handles = append(f.handles, 0xffffffff)
			table, store := hierarchyTableOpen(t, f, flags)
			tags := []uint32{0x67480014, 0x3001001f, 0x66380003}
			subscribe := func(f *ostFixture) {
				t.Helper()
				faiRunOK(t, f, tableColumnsWire(2, tags...))
				faiRunOK(t, f, tableQueryWire(2, 1, 1, 65535))
			}
			subscribe(f)
			other := *f
			other.state = &contextState{mailbox: newMailboxObjects(f.transport.stores), codePage: 1252}
			t.Cleanup(other.state.mailbox.close)
			other.handles = []uint32{0xffffffff, 0xffffffff, 0xffffffff}
			faiRunOK(t, &other, logonOperation(other.identity.LegacyDN, 4, 0))
			other.open(int(IPMSubtree))
			otherTable, _ := hierarchyTableOpen(t, &other, flags)
			subscribe(&other)
			before := len(table.rows)
			out := faiRunOK(t, f, createOperation(4, 1, 3, "Created by this context", "", false))
			if len(out) != 15 || len(table.rows) != before+1 || table.pending {
				t.Fatal("own creation emitted a notification or kept stale rows", out)
			}
			fid := binary.LittleEndian.Uint64(out[6:])
			seen := false
			for _, row := range decodeTableRows(t, faiRunOK(t, f, tableQueryWire(2, 1, 1, 65535)), tags) {
				seen = seen || row[0] == fid && row[1] == "Created by this context"
			}
			if !seen {
				t.Fatal("suppression hid a created row")
			}
			out, _ = other.run(nil)
			var previous uint64
			for _, row := range otherTable.rows {
				current := uint64(otherTable.bookmarkRow(row).id)
				if current == fid {
					break
				}
				previous = current
			}
			requireHierarchyRowEvent(t, out, tableRowAdded, other.handles[2], fid, previous, tags, []any{fid, "Created by this context", uint32(0)})

			// In a shallow table this creation only changes the visible
			// parent's derived child count. A deep table also gains the row.
			parent := store.byKey[folderKey{native: "table-parent"}]
			faiRunOK(t, f, folderOperation(parent, 4, 0, 4))
			out = faiRunOK(t, f, createOperation(4, 4, 3, "Owned grandchild", "", false))
			if len(out) != 15 || table.pending {
				t.Fatal("derived parent counts emitted an own notification", out)
			}

			box.page.Objects = append(box.page.Objects, core.Object{ID: "coalesced-external", Parent: box.Root(), Kind: core.Folder,
				Properties: core.Properties{core.DisplayName: core.Text("External sibling")}})
			out = faiRunOK(t, f, createOperation(4, 1, 3, "Coalesced own sibling", "", false))
			if prefix := requireTableChanged(t, out, f.handles[2]); len(prefix) != 15 {
				t.Fatal("coalesced external change was suppressed")
			}
		})
	}
}

func TestHierarchySuppressionPreservesPreviouslyPendingChange(t *testing.T) {
	f, box := hierarchyTableFixture(t)
	f.transport.EnableCreateFolderSpike()
	table, _ := hierarchyTableOpen(t, f, 0x84)
	faiRunOK(t, f, tableColumnsWire(2, 0x67480014))
	faiRunOK(t, f, tableQueryWire(2, 1, 1, 65535))
	object := f.state.mailbox.objects[f.handles[2]]
	box.page.Objects = append(box.page.Objects, core.Object{ID: "previously-pending", Parent: box.Root(), Kind: core.Folder,
		Properties: core.Properties{core.DisplayName: core.Text("External change")}})
	if err := f.transport.refreshTable(t.Context(), f.session, object); err != nil || !table.pending || table.hierarchyChanges[0].kind != tableRowAdded {
		t.Fatal("external change was not queued", err)
	}
	out := faiRunOK(t, f, createOperation(4, 1, 3, "Created after external notification", "", false))
	if prefix := requireTableChanged(t, out, f.handles[2]); len(prefix) != 15 {
		t.Fatal("silent refresh consumed a previously pending event")
	}
}

func TestHierarchySuppressionInNotificationWait(t *testing.T) {
	f, box := hierarchyTableFixture(t)
	f.transport.EnableCreateFolderSpike()
	table, _ := hierarchyTableOpen(t, f, 0x84)
	faiRunOK(t, f, tableColumnsWire(2, 0x67480014))
	faiRunOK(t, f, tableQueryWire(2, 1, 1, 65535))
	// The Execute response has no room to poll the table after creation.
	maxOutput := uint32(10 + 4*len(f.handles) + 15)
	if _, outcome := f.execute(createOperation(4, 1, 3, "Created before Wait", "", false), maxOutput); outcome.errors != 0 {
		t.Fatal("create fixture failed")
	}
	target := f.state.mailbox.objects[f.handles[2]]
	probe := func() bool {
		t.Helper()
		view, err := captureHierarchyTable(t.Context(), f.session, target.binding, target.fid, table.flags, false)
		if err != nil {
			t.Fatal(err)
		}
		defer view.close()
		changed, retry := f.transport.contexts.noteTableChange(f.state, f.state.sequence, f.handles[2], view)
		if retry {
			t.Fatal("unexpectedly busy context")
		}
		return changed
	}
	if probe() || table.pending {
		t.Fatal("a creation in the same context woke NotificationWait")
	}
	box.page.Objects = append(box.page.Objects, core.Object{ID: "wake-hierarchy-wait", Parent: box.Root(), Kind: core.Folder,
		Properties: core.Properties{core.DisplayName: core.Text("External")}})
	if !probe() || !table.pending {
		t.Fatal("NotificationWait suppressed a coalesced external change")
	}
}

func requireHierarchyRowEvent(t *testing.T, wire []byte, event uint16, handle uint32, fid, previous uint64, tags []uint32, columns []any) {
	t.Helper()
	if len(wire) < 29 || wire[0] != 0x2a || binary.LittleEndian.Uint32(wire[1:5]) != handle ||
		wire[5] != 4 || binary.LittleEndian.Uint16(wire[6:8]) != 0x100 || binary.LittleEndian.Uint16(wire[8:10]) != event ||
		binary.LittleEndian.Uint64(wire[10:18]) != fid || binary.LittleEndian.Uint64(wire[18:26]) != previous ||
		int(binary.LittleEndian.Uint16(wire[26:28])) != len(wire)-28 {
		t.Fatal("incorrect standalone hierarchy row event")
	}
	query := append([]byte{0x15, 2, 0, 0, 0, 0, 1, 1, 0}, wire[28:]...)
	if got := decodeTableRows(t, query, tags); !reflect.DeepEqual(got, [][]any{columns}) {
		t.Fatal("row event does not encode the SetColumns row")
	}
}

func TestHierarchyRowAddedFallbackAndPending(t *testing.T) {
	for _, mode := range []string{"compound", "no-columns", "tight-output", "prior-refresh", "targeted"} {
		t.Run(mode, func(t *testing.T) {
			f, box := hierarchyTableFixture(t)
			table, _ := hierarchyTableOpen(t, f, 0)
			if mode != "no-columns" {
				f.run(tableColumnsWire(2, 0x67480014))
			}
			f.run([]byte{0x17, 4, 2})
			add := func(id core.ID) {
				box.page.Objects = append(box.page.Objects, core.Object{ID: id, Parent: box.Root(), Kind: core.Folder,
					Properties: core.Properties{core.DisplayName: core.Text(id)}})
			}
			add("native-addition-1")
			if mode == "compound" {
				add("native-addition-2")
			}
			maximum := uint32(65535)
			var ops []byte
			if mode == "tight-output" {
				maximum = uint32(10 + 4*len(f.handles) + tableChangedNotificationBytes)
			}
			if mode == "prior-refresh" {
				object := f.state.mailbox.objects[f.handles[2]]
				if err := f.transport.refreshTable(context.Background(), f.session, object); err != nil {
					t.Fatal(err)
				}
				add("native-addition-2")
			}
			if mode == "targeted" {
				ops = []byte{0x17, 4, 2}
			}
			body, _ := f.execute(ops, maximum)
			wire := storeResponse(t, body).Operations
			if mode == "targeted" {
				if len(wire) != 14+37 || wire[14] != 0x2a || wire[22] != 3 {
					t.Fatal("targeted Execute lost the exact insertion")
				}
			} else if len(requireTableChanged(t, wire, f.handles[2])) != 0 {
				t.Fatal("unexpected bytes preceding fallback")
			}
			if out, _ := f.run(nil); len(out) != 0 || table.pending || table.hierarchyChanges[0].kind != 0 {
				t.Fatal("consumed notification remained pending")
			}
		})
	}
}

func TestHierarchyRowModifiedReportsNonselectedFolderCounters(t *testing.T) {
	for _, mode := range []string{"one-folder", "move", "move-small-buffer", "three-folders"} {
		f, box := hierarchyTableFixture(t)
		f.transport.EnableSynchronizationOptionsTrace()
		setCounts := func(role string, count, unread int64) {
			for _, folder := range box.page.Objects {
				if folder.Properties[core.WellKnown] == core.Text(role) {
					folder.Properties[core.FolderItemCount] = core.Integer(count)
					folder.Properties[core.FolderUnreadCount] = core.Integer(unread)
					return
				}
			}
			t.Fatalf("missing fixture role %s", role)
		}
		setCounts("sentitems", 1, 0)
		setCounts("deleteditems", 2, 1)
		setCounts("inbox", 4, 0)
		table, store := hierarchyTableOpen(t, f, 4)
		tags := []uint32{0x67480014, 0x36020003, 0x36030003}
		f.run(tableColumnsWire(2, tags...))
		f.run([]byte{0x17, 4, 2})
		setCounts("sentitems", 2, 1)
		if mode != "one-folder" {
			setCounts("deleteditems", 1, 0)
		}
		if mode == "three-folders" {
			setCounts("inbox", 5, 1)
		}
		var after uint32
		if changed, retry := f.transport.checkNotificationTables(context.Background(), f.session, f.state, &after); !changed || retry {
			t.Fatal("counter change did not wake NotificationWait")
		}
		var wire []byte
		var counts storeResults
		if mode == "move-small-buffer" {
			const rowBytes = tableRowNotificationHeaderBytes + 1 + 8 + 4 + 4
			body, trace := f.execute(nil, uint32(10+4*len(f.handles)+rowBytes))
			wire, counts = storeResponse(t, body).Operations, trace
		} else {
			wire, counts = f.run(nil)
		}
		if duplicate, _ := f.run(nil); len(duplicate) != 0 || box.sourceCalls != 0 || len(store.contentsProofs) != 0 {
			t.Fatalf("%s: counter change replayed, read message contents or modified an ICS checkpoint", mode)
		}
		if mode == "move-small-buffer" || mode == "three-folders" {
			if trailing := requireTableChanged(t, wire, f.handles[2]); len(trailing) != 0 {
				t.Fatalf("%s: unexpected bytes before TableChanged", mode)
			}
			if got := counts.execution.NotificationDelivery(); !reflect.DeepEqual(got, []string{"hierarchy_table_changed"}) {
				t.Fatalf("%s: unexpected notification kind: %v", mode, got)
			}
			continue
		}
		counters := map[uint64][]any{
			f.fids[SentItems]: {f.fids[SentItems], uint32(2), uint32(1)},
		}
		if mode == "move" {
			counters[f.fids[DeletedItems]] = []any{f.fids[DeletedItems], uint32(1), uint32(0)}
		}
		var previous uint64
		emitted := 0
		for _, row := range table.rows {
			fid := uint64(table.bookmarkRow(row).id)
			if values, updated := counters[fid]; updated {
				if len(wire) < tableRowNotificationHeaderBytes {
					t.Fatal("missing row modification")
				}
				end := tableRowNotificationHeaderBytes + int(binary.LittleEndian.Uint16(wire[26:]))
				if end > len(wire) {
					t.Fatal("truncated row modification")
				}
				requireHierarchyRowEvent(t, wire[:end], tableRowModified, f.handles[2], fid, previous, tags, values)
				wire = wire[end:]
				emitted++
			}
			previous = fid
		}
		if emitted != len(counters) || len(wire) != 0 {
			t.Fatal("expected exactly one notification for each changed row")
		}
		if got := counts.execution.NotificationDelivery(); !reflect.DeepEqual(got, []string{"hierarchy_table_row_modified"}) {
			t.Fatalf("unexpected notification kind: %v", got)
		}
	}
}

func TestHierarchyInsertionPositions(t *testing.T) {
	table := contentsTable{hierarchy: true, columns: []uint32{0x67480014}}
	row := func(fid int64) contentsValues {
		return contentsValues{fixedContentsProperty(0x67480014): core.Integer(fid)}
	}
	a, b, c, d := row(1), row(2), row(3), row(4)
	for _, tc := range []struct {
		after []contentsValues
		want  int
	}{
		{[]contentsValues{c, a, b}, 1},
		{[]contentsValues{a, c, b}, 2},
		{[]contentsValues{a, b, c}, 3},
		{[]contentsValues{b, a, c}, -1},
		{[]contentsValues{a, c, d}, -1},
	} {
		change := table.hierarchyChange([]contentsValues{a, b}, tc.after)[0]
		if tc.want < 0 && change.kind != 0 || tc.want > 0 && (change.kind != tableRowAdded || change.index != tc.want-1) {
			t.Fatalf("insertion event %d index %d, expected one-based index %d", change.kind, change.index, tc.want)
		}
	}
}

func TestHierarchyInsertionWithVisibleParentChange(t *testing.T) {
	for _, mode := range []string{"without-parent-cells", "two-events", "budget", "other-row", "deferred"} {
		f, box := hierarchyTableFixture(t)
		f.transport.EnableSynchronizationOptionsTrace()
		table, _ := hierarchyTableOpen(t, f, 4)
		tags := []uint32{0x67480014, 0x3001001f}
		if mode != "without-parent-cells" {
			tags = append(tags, 0x66380003)
		}
		if mode == "two-events" || mode == "deferred" {
			tags = append(tags, folderFlagsTag)
		}
		faiRunOK(t, f, tableColumnsWire(2, tags...))
		faiRunOK(t, f, []byte{0x17, 4, 2})
		short := uint32(10 + 4*len(f.handles) + 8)
		if mode == "deferred" {
			body, _ := f.execute(nil, short)
			if wire := storeResponse(t, body).Operations; len(wire) != 0 {
				t.Fatalf("unchanged table emitted pending: %x", wire)
			}
		}
		box.page.Objects = append(box.page.Objects, core.Object{ID: "new-child", Parent: "table-parent", Kind: core.Folder,
			Properties: core.Properties{core.DisplayName: core.Text("New child")}})
		maximum := uint32(65535)
		if mode == "budget" {
			// Enough for the insertion but not enough for its parent row too.
			maximum = uint32(10 + 4*len(f.handles) + 75)
		}
		if mode == "other-row" {
			for i := range box.page.Objects {
				if box.page.Objects[i].ID == "table-child" {
					box.page.Objects[i].Properties[core.DisplayName] = core.Text("Renamed existing child")
				}
			}
		}
		if mode == "deferred" {
			body, counts := f.execute(nil, short)
			if wire := storeResponse(t, body).Operations; !bytes.Equal(wire, []byte{0x6e, 0, 0}) || !table.pending {
				t.Fatalf("short response failed to defer hierarchy rows: %x", wire)
			}
			if labels := counts.execution.NotificationDelivery(); !reflect.DeepEqual(labels, []string{"table_notification_pending"}) {
				t.Fatalf("pending diagnostic: %v", labels)
			}
		}
		body, counts := f.execute(nil, maximum)
		wire := storeResponse(t, body).Operations
		want := []string{"hierarchy_table_row_added"}
		if mode == "budget" || mode == "other-row" {
			want = []string{"hierarchy_table_changed"}
			if prefix := requireTableChanged(t, wire, f.handles[2]); len(prefix) != 0 {
				t.Fatal("published a partial row pair")
			}
		}
		if mode == "two-events" || mode == "deferred" {
			want = append(want, "hierarchy_table_row_modified")
			if len(wire) < 2*tableRowNotificationHeaderBytes {
				t.Fatal("missing hierarchy row notifications")
			}
			end := tableRowNotificationHeaderBytes + int(binary.LittleEndian.Uint16(wire[26:]))
			if end >= len(wire) {
				t.Fatal("missing parent row notification")
			}
			require := func(record []byte, event uint16, name string, children uint32) {
				t.Helper()
				var previous uint64
				for _, row := range table.rows {
					fid := uint64(table.bookmarkRow(row).id)
					if row[fixedContentsProperty(0x3001001f)] == core.Text(name) {
						requireHierarchyRowEvent(t, record, event, f.handles[2], fid, previous, tags, []any{fid, name, children, uint32(ipmNormalFolderFlags)})
						return
					}
					previous = fid
				}
				t.Fatalf("missing hierarchy row %s", name)
			}
			require(wire[:end], tableRowAdded, "New child", 0)
			require(wire[end:], tableRowModified, "Parent 📂", 2)
		}
		if got := counts.execution.NotificationDelivery(); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: got %v, want %v", mode, got, want)
		}
		if mode == "deferred" {
			body, _ = f.execute(nil, short)
			if wire := storeResponse(t, body).Operations; len(wire) != 0 || table.pending {
				t.Fatalf("replayed deferred notification: %x", wire)
			}
		}
		if duplicate, _ := f.run(nil); len(duplicate) != 0 {
			t.Fatalf("%s: replayed notification", mode)
		}
	}
}

func TestUpdatingTableFailuresKeepSnapshotAndPending(t *testing.T) {
	for _, mode := range []string{"source", "scope", "owner", "missing", "cancel", "budget", "invalid_source"} {
		t.Run(mode, func(t *testing.T) {
			f, box := newContentsDownloadFixture(t)
			f.transport.EnableContentsTableSpike()
			table, store := openTableTest(t, f, 0)
			f.run(tableColumnsWire(2, 0x674a0014))
			f.run(tableQueryWire(2, 0, 1, 1))
			old := table.rows
			oldLease, oldCursor := table.lease, table.cursor
			table.pending = true
			object := f.state.mailbox.objects[f.handles[2]]
			ctx := context.Background()
			switch mode {
			case "source":
				box.sourceErr = core.ErrResync
			case "scope":
				identity := f.session.Identity()
				identity.Scope.Account = "outside"
				f.session = scopedSession{Session: f.session, identity: identity}
			case "owner":
				box.state.Owner = false
			case "missing":
				store.mu.Lock()
				delete(store.byFID, object.fid)
				store.mu.Unlock()
			case "cancel":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			case "budget":
				store.allocator.contents.mu.Lock()
				store.allocator.contents.used = maxGlobalContentsMemory
				store.allocator.contents.mu.Unlock()
				defer func() { store.allocator.contents.used = oldLease.bytes }()
			case "invalid_source":
				box.objects[0].Parent = "foreign"
			}
			if err := f.transport.refreshTable(ctx, f.session, object); err == nil {
				t.Fatal("failed source accepted")
			}
			if table.lease != oldLease || table.cursor != oldCursor || !reflect.DeepEqual(table.rows, old) || !table.pending {
				t.Fatal("failed refresh replaced view, lease, cursor or pending state")
			}
			for _, maximum := range []uint32{65535, uint32(10 + 4*len(f.handles) + 8)} {
				if got, kind := f.transport.appendTableNotifications(ctx, f.session, f.state.mailbox, nil, len(f.handles), maximum, f.state.codePage); len(got) != 0 || kind != noTableNotification || !table.pending {
					t.Fatal("failed poll leaked notification or consumed pending state")
				}
			}
		})
	}
}

func TestUpdatingTableOutputBudgetAndFairness(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	a, _ := openTableTest(t, f, 0)
	f.run(tableColumnsWire(2, 0x674a0014))
	f.run(tableQueryWire(2, 0, 1, 1))
	first := f.handles[2]
	f.handles = append(f.handles, 0xffffffff)
	f.run([]byte{5, 4, 1, 4, 0})
	second := f.handles[4]
	b := f.state.mailbox.objects[second].table
	f.run(tableColumnsWire(4, 0x674a0014))
	f.run(tableQueryWire(4, 0, 1, 1))
	box.objects = append(box.objects, messageTestObject("new-row", box.objects[0].Parent))
	// Force one pending update while output has no room for a notification.
	object := f.state.mailbox.objects[first]
	if err := f.transport.refreshTable(context.Background(), f.session, object); err != nil || !a.pending {
		t.Fatal("failed to prepare pending update")
	}
	base := 10 + 4*len(f.handles)
	for _, extra := range []int{0, 1, 2, 3, 9} {
		out, _ := f.execute(nil, uint32(base+extra))
		var want []byte
		if extra >= pendingNotificationBytes {
			want = []byte{0x6e, 0, 0}
		}
		if !bytes.Equal(storeResponse(t, out).Operations, want) || !a.pending {
			t.Fatal("tight output consumed pending state or failed polling")
		}
	}
	seen := map[uint32]bool{}
	for range 2 {
		out, _ := f.run(nil)
		if len(out) != 10 {
			t.Fatal("poll did not deliver exactly one bounded notification")
		}
		handle := binary.LittleEndian.Uint32(out[1:5])
		requireTableChanged(t, out, handle)
		seen[handle] = true
	}
	if !seen[first] || !seen[second] || a.pending || b.pending {
		t.Fatal("round robin starved a subscribed view")
	}
	// No room and a malformed later ROP must not trigger polling or refresh.
	reads := box.sourceCalls
	f.execute([]byte{1, 4, 2, 0x99, 4, 1}, 65535)
	if box.sourceCalls != reads {
		t.Fatal("rejected batch polled a source")
	}
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	if a.lease != nil || b.lease != nil {
		t.Fatal("replacement Logon retained Table subscription leases")
	}
	if out, _ := f.run(nil); !bytes.Equal(out, nil) {
		t.Fatal("old handle notified after replacement")
	}
}

func TestExecuteSharesPendingSignalForTableAndObjectNotifications(t *testing.T) {
	f, box := hierarchyTableFixture(t)
	f.transport.EnableSynchronizationOptionsTrace()
	table, _ := hierarchyTableOpen(t, f, 4)
	faiRunOK(t, f, tableColumnsWire(2, 0x67480014))
	faiRunOK(t, f, []byte{0x17, 4, 2})
	journal := &notificationJournalFixture{}
	f.transport.EnableObjectNotifications(journal)
	faiRunOK(t, f, []byte{0x29, 4, 0, 3, notifyCreated, 0, 1})
	box.page.Objects = append(box.page.Objects, core.Object{ID: "pending-child", Parent: "table-parent", Kind: core.Folder,
		Properties: core.Properties{core.DisplayName: core.Text("Pending child")}})
	journal.events = []NotificationEvent{
		{core.Change{Kind: core.Created, ID: "pending-child", Parent: "table-parent", Revision: 1}, core.Folder},
	}
	sub := f.state.mailbox.objects[f.handles[3]].notification
	body, _ := f.execute(nil, uint32(10+4*len(f.handles)+8))
	if wire := storeResponse(t, body).Operations; !bytes.Equal(wire, []byte{0x6e, 0, 0}) || !table.pending || sub.position.Revision != 0 {
		t.Fatalf("both notifications should give one pending response: %x", wire)
	}
	_, counts := f.run(nil)
	if labels := counts.execution.NotificationDelivery(); !reflect.DeepEqual(labels, []string{"hierarchy_table_row_added", "folder_created"}) {
		t.Fatalf("deferred notification delivery: %v", labels)
	}
	if out, _ := f.run(nil); len(out) != 0 || table.pending || sub.position.Revision != 1 {
		t.Fatal("notification replay or uncommitted position")
	}
}
