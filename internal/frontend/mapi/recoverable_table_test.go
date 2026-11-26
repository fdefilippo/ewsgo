// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"slices"
	"testing"

	"ewsgo/internal/core"
)

type recoverableTableMailbox struct {
	*messageFixtureMailbox
	items    []core.Object
	queries  []core.ID
	restored [][3]core.ID
}

func (b *recoverableTableMailbox) ReadRecoverableMail(_ context.Context, folder core.ID) ([]core.Object, error) {
	b.queries = append(b.queries, folder)
	var selected []core.Object
	for _, item := range b.items {
		if folder == "" || item.Parent == folder {
			selected = append(selected, item)
		}
	}
	return selected, nil
}

func (b *recoverableTableMailbox) RecoverMailMessage(_ context.Context, id, origin, to core.ID) error {
	b.restored = append(b.restored, [3]core.ID{id, origin, to})
	return nil
}

func TestRecoverableContentsTableScopeRowsAndIdentity(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	inbox := base.objects[0].Parent
	first := core.Object{
		ID: base.objects[0].ID, Parent: inbox, Kind: core.Message,
		Properties: core.Properties{
			core.Subject: core.Text("Recovered candidate"), core.ReceivedAt: core.Timestamp(hierarchyTestTime),
			core.SentRepresentingName: core.Text("Sender Name"), core.DisplayTo: core.Text("First Recipient; second@example.test"),
			core.DeletedOn: core.Timestamp(hierarchyTestTime),
		},
	}
	second := core.Object{ID: "second", Parent: "removed-folder", Kind: core.Message}
	box := &recoverableTableMailbox{messageFixtureMailbox: base, items: []core.Object{first, second}}
	f.session = &testSession{box}
	f.open(int(DeletedItems))
	trashFID := f.fids[DeletedItems]
	table, store := openTableTest(t, f, tableSoftDeletes)
	if len(table.rows) != 2 || table.updating() || len(box.queries) != 1 || box.queries[0] != "" {
		t.Fatal("Trash recovery did not open a mailbox-wide snapshot")
	}
	tags := []uint32{0x674a0014, 0x0fff0102, 0x0037001f, 0x668f0040, 0x674d0014, 0x674e0003, 0x0042001f, 0x0e04001f}
	f.run(tableColumnsWire(2, tags...))
	if _, c := f.run(tableSortWire(2, 0, 0, 0, tableSortKey{0x668f0040, 1})); c.errors != 0 {
		t.Fatal("recovery table could not sort missing deletion dates")
	}
	out, c := f.run(tableQueryWire(2, 0, 1, 2))
	if c.errors != 0 {
		t.Fatal("recoverable row query failed")
	}
	rows := decodeTableRows(t, out, tags)
	mid := rows[0][0].(uint64)
	entryID := rows[0][1].([]byte)
	expectedDeletedOn := uint64(hierarchyTestTime.Unix()+11644473600)*10000000 + uint64(hierarchyTestTime.Nanosecond()/100)
	if rows[0][2] != "Recovered candidate" || rows[0][3] != expectedDeletedOn || rows[1][3] != uint32(0x8004010f) ||
		len(entryID) != 70 || binary.LittleEndian.Uint16(entryID[20:]) != 7 || fidFromGlobalCounter(entryID[38:44]) != trashFID || fidFromGlobalCounter(entryID[62:68]) != mid {
		t.Fatal("recoverable Message EntryID or metadata mismatch")
	}
	if rows[0][4] != mid || rows[0][5] != uint32(0) || rows[0][6] != "Sender Name" || rows[0][7] != "First Recipient; second@example.test" ||
		rows[1][4] != rows[1][0] || rows[1][4] == mid || rows[1][5] != uint32(0) {
		t.Fatal("recoverable row identity or display fields mismatch")
	}
	if store.byMID[mid].id != "" || len(store.messages) != 0 || base.sourceCalls != 0 {
		t.Fatal("recoverable row entered normal message observations")
	}
	f.run([]byte{1, 4, 2})
	f.open(int(Inbox))
	table, _ = openTableTest(t, f, tableSoftDeletes)
	if len(table.rows) != 1 || table.rows[0][fixedContentsProperty(0x674a0014)] != core.Integer(mid) || len(box.queries) != 2 || box.queries[1] != inbox {
		t.Fatal("folder-scoped recovery confused origin, rows, or MID")
	}
	base.state.Owner = false
	out, c = f.run(tableQueryWire(2, 0, 1, 1))
	if c.errors != 1 || binary.LittleEndian.Uint32(out[2:]) != accessDenied || len(box.queries) != 2 {
		t.Fatal("revoked owner could access the recovery snapshot")
	}
}

func TestRecoverableContentsDiagnosticsReportOnlyReturnedShape(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	box := &recoverableTableMailbox{messageFixtureMailbox: base, items: []core.Object{{
		ID: base.objects[0].ID, Parent: base.objects[0].Parent, Kind: core.Message,
		Properties: core.Properties{core.Subject: core.Text("Private fixture")},
	}}}
	f.session = &testSession{box}
	f.open(int(DeletedItems))
	f.transport.synchronizationOptionsTrace = true
	f.transport.unknownPropertyTrace = true
	_, c := f.run([]byte{0x05, 4, 1, 2, tableSoftDeletes})
	opened, queried, _, _ := c.execution.RecoverableTable()
	if c.errors != 0 || opened != "1" || queried != "" {
		t.Fatal("recovery open diagnostic did not report one row")
	}
	f.run(tableColumnsWire(2, 0x0037001f, 0x668f0040, 0x8123001f))
	_, c = f.run(tableQueryWire(2, 1, 1, 1))
	opened, queried, requested, missing := c.execution.RecoverableTable()
	if c.errors != 0 || opened != "" || queried != "1" || requested.HighIDs != 1 ||
		!slices.Equal(requested.Tags, []string{"0x0037001f", "0x668f0040"}) || !slices.Equal(missing.Tags, []string{"0x668f0040"}) {
		t.Fatal("unexpected query diagnostics", opened, queried, requested, missing)
	}
	f.transport.unknownPropertyTrace = false
	_, c = f.run(tableQueryWire(2, 0, 1, 1))
	opened, queried, _, _ = c.execution.RecoverableTable()
	if c.errors != 0 || opened != "" || queried != "" {
		t.Fatal("recovery query diagnostics were enabled without explicit opt-in")
	}
}

func TestMoveRecoverableMessageThroughExecute(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	f.transport.synchronizationOptionsTrace = true
	f.transport.unknownPropertyTrace = true
	item := core.Object{ID: "recoverable", Parent: base.objects[0].Parent, Kind: core.Message}
	box := &recoverableTableMailbox{messageFixtureMailbox: base, items: []core.Object{item}}
	f.session = &testSession{box}
	f.open(int(DeletedItems))
	table, store := openTableTest(t, f, tableSoftDeletes)
	mid := uint64(table.rows[0][fixedContentsProperty(0x674a0014)].(core.Integer))
	f.run([]byte{1, 4, 2})
	faiRunOK(t, f, folderOperation(f.fids[SentItems], 4, 0, 2))
	faiRunOK(t, f, folderOperation(f.fids[Inbox], 4, 0, 3))

	_, crossScope := f.run(moveCopyWire(4, 2, 3, 0, 0, mid))
	_, copyCase := f.run(moveCopyWire(4, 3, 2, 0, 1, mid))
	if crossScope.errors != 1 || copyCase.errors != 1 || len(box.restored) != 0 ||
		!slices.Equal(copyCase.execution.RecoverablePlacement(), []string{"copy"}) {
		t.Fatal("wrong-folder move or non-consuming copy reached the recovery backend")
	}

	_, restored := f.run(moveCopyWire(4, 1, 1, 0, 1, mid))
	target := store.byFID[f.fids[DeletedItems]].native
	if restored.errors != 0 || !slices.Equal(restored.execution.RecoverablePlacement(), []string{"copy"}) ||
		len(box.restored) != 1 || box.restored[0] != [3]core.ID{item.ID, item.Parent, target} {
		t.Fatal("Outlook move did not route the selected dumpster message to the native folder")
	}
}
