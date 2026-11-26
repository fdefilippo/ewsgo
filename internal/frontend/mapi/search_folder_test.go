// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"ewsgo/internal/core"
)

func TestNewSearchFolder(t *testing.T) {
	f := newCreateFixture(t)
	ctx := context.Background()
	create := createOperation(4, 1, 2, "Synthetic search", "", false)
	create[4] = 2
	wire, _ := f.run(create)
	if len(wire) != 15 || binary.LittleEndian.Uint32(wire[2:]) != 0 {
		t.Fatalf("Search creation: %x", wire)
	}
	fid := binary.LittleEndian.Uint64(wire[6:])
	binding := f.state.mailbox.objects[f.handles[0]].binding
	parent := binding.fids[IPMSubtree]
	getState := []byte{ropGetStoreState, 4, 0}
	if got := faiRunOK(t, f, getState); !bytes.Equal(got, append32(ropError(ropGetStoreState, 0, 0), 0)) {
		t.Fatalf("uninitialized Search set the store flag: %x", got)
	}

	wire, _ = f.run(folderPropertiesOperation(4, 2, 0, 0x36010003))
	if !bytes.Equal(wire, append(ropError(7, 2, 0), 0, 2, 0, 0, 0)) {
		t.Fatalf("Search FolderType: %x", wire)
	}
	criteria := []byte{0x31, 4, 2, 1, 1, 1}
	wire, _ = f.run(criteria)
	if !bytes.Equal(wire, append(ropError(0x31, 2, 0), 0, 0, 4, 0, 0, 0, 0, 0, 0)) {
		t.Fatalf("uninitialized Search criteria: %x", wire)
	}
	criteria[2] = 1
	wire, _ = f.run(criteria)
	if !bytes.Equal(wire, ropError(0x31, 1, 0x461)) {
		t.Fatalf("criteria on generic folder: %x", wire)
	}
	wire, _ = f.run(createOperation(4, 2, 3, "Not a valid child", "", false))
	if !bytes.Equal(wire, ropError(0x1c, 3, NotSupported)) {
		t.Fatalf("Search folder accepted a subfolder: %x", wire)
	}
	view, err := captureHierarchyTable(ctx, f.session, binding, parent, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	defer view.close()
	var isSearch bool
	for _, row := range view.rows {
		if row[contentsProperty{id: 0x6748, kind: 0x14}] == core.Integer(fid) {
			isSearch = row[contentsProperty{id: 0x3601, kind: 3}] == core.Integer(2)
		}
	}
	if !isSearch {
		t.Fatal("Search type missing from hierarchy table")
	}
	snapshot, err := binding.store.observeHierarchy(ctx, f.session, parent, hierarchyTestTime)
	if err != nil {
		t.Fatal(err)
	}
	entry := hierarchyRow(t, snapshot, fid)
	inbox := f.fids[Inbox]
	restriction := append32(append32([]byte{6, 1}, 0x0e070003), 1)
	faiRunOK(t, f, setSearchWire(2, searchRestart, restriction, inbox))
	if got := faiRunOK(t, f, getState); !bytes.Equal(got, append32(ropError(ropGetStoreState, 0, 0), storeHasSearches)) {
		t.Fatalf("active dynamic Search did not set the store flag: %x", got)
	}
	faiRunOK(t, f, binary.LittleEndian.AppendUint64([]byte{0x1d, 4, 1, 0x10}, fid))
	if got := faiRunOK(t, f, getState); !bytes.Equal(got, append32(ropError(ropGetStoreState, 0, 0), 0)) {
		t.Fatalf("removed Search kept the store flag: %x", got)
	}
	stream, err := hierarchyFastTransfer(snapshot.replica, binding.mailboxGUID, synchronizationConfigure{}, hierarchyDelta{changes: []hierarchyFolder{entry}})
	if err != nil {
		t.Fatal(err)
	}
	for _, token := range readTestFX(t, stream.data) {
		if token.tag == 0x36010003 && bytes.Equal(token.value, []byte{2, 0, 0, 0}) {
			return
		}
	}
	t.Fatal("Search type missing from hierarchy FastTransfer")
}

func setSearchWire(slot byte, flags uint32, restriction []byte, sources ...uint64) []byte {
	out := binary.LittleEndian.AppendUint16([]byte{0x30, 4, slot}, uint16(len(restriction)))
	out = append(out, restriction...)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(sources)))
	for _, source := range sources {
		out = binary.LittleEndian.AppendUint64(out, source)
	}
	return binary.LittleEndian.AppendUint32(out, flags)
}

func TestStaticSearchKeepsReferencesUntilRestart(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &openMessageFixtureMailbox{messageFixtureMailbox: base}
	f.session = &testSession{box}
	f.handles = append(f.handles, 0xffffffff)
	f.transport.EnableCreateFolderSpike()
	f.transport.EnableContentsTableSpike()
	f.open(int(MailboxRoot))

	store := f.state.mailbox.objects[f.handles[0]].binding.store
	budget := &store.allocator.contents
	initialBytes := budget.used
	inbox := f.fids[Inbox]
	first := &box.objects[0]
	first.Properties[core.IsRead] = core.Bool(true)
	first.MIME = []byte("From: synthetic@example.invalid\r\nSubject: Synthetic subject\r\n\r\nBody")

	create := createOperation(4, 1, 2, "Static search", "", false)
	create[4] = 2
	fid := binary.LittleEndian.Uint64(faiRunOK(t, f, create)[6:])
	restriction := append32(append32([]byte{6, 1}, 0x0e070003), 1)
	restart := setSearchWire(2, searchRestart|searchStatic, nil)
	faiRunOK(t, f, setSearchWire(2, searchRestart|searchStatic, restriction, inbox))
	if got := faiRunOK(t, f, []byte{ropGetStoreState, 4, 0}); !bytes.Equal(got, append32(ropError(ropGetStoreState, 0, 0), 0)) {
		t.Fatalf("completed static Search set the store flag: %x", got)
	}

	get := []byte{0x31, 4, 2, 1, 1, 1}
	want := binary.LittleEndian.AppendUint16(ropError(0x31, 2, 0), uint16(len(restriction)))
	want = append(want, restriction...)
	want = binary.LittleEndian.AppendUint64(append(want, 4, 1, 0), inbox)
	want = append32(want, 0x08001000)
	if got := faiRunOK(t, f, get); !bytes.Equal(got, want) {
		t.Fatalf("static criteria: %x", got)
	}
	readRow := func() []any {
		t.Helper()
		faiRunOK(t, f, []byte{5, 4, 2, 3, 0x10})
		tags := []uint32{0x67480014, 0x674a0014, 0x0037001f}
		faiRunOK(t, f, tableColumnsWire(3, tags...))
		rows := decodeTableRows(t, faiRunOK(t, f, tableQueryWire(3, 0, 1, 10)), tags)
		faiRunOK(t, f, []byte{1, 4, 3})
		if len(rows) != 1 || rows[0][0] != inbox {
			t.Fatalf("unexpected static membership: %v", rows)
		}
		return rows[0]
	}
	row := readRow()
	mid := row[1].(uint64)
	if mid == 0 || row[2] != "Synthetic subject" {
		t.Fatalf("initial result: %v", row)
	}
	frozenBytes := budget.used
	if frozenBytes <= initialBytes {
		t.Fatalf("retained search references are unaccounted: initial %d, frozen %d", initialBytes, frozenBytes)
	}

	first.Properties[core.IsRead] = core.Bool(false)
	first.Properties[core.Subject] = core.Text("Edited original")
	first.Properties[core.ContentRevision] = core.Integer(4)
	first.MIME = []byte("From: synthetic@example.invalid\r\nSubject: Edited original\r\n\r\nBody")
	second := messageTestObject("new-matching-message", first.Parent)
	second.Properties[core.IsRead] = core.Bool(true)
	second.Properties[core.Subject] = core.Text("Second message")
	second.MIME = []byte("From: synthetic@example.invalid\r\nSubject: Second message\r\n\r\nBody")
	box.objects = append(box.objects, second)
	if row := readRow(); row[1] != mid || row[2] != "Edited original" {
		t.Fatalf("static membership or live properties changed unexpectedly: %v", row)
	}
	open := openMessageOperation(4, 2, 4, fid, mid, 1252, 0)
	faiRunOK(t, f, open)
	if handle := f.state.mailbox.objects[f.handles[4]]; handle.message == nil || handle.fid != inbox {
		t.Fatal("opened link did not resolve to the source message")
	}
	faiRunOK(t, f, []byte{1, 4, 4})

	box.objects = box.objects[1:]
	if row := readRow(); row[1] != mid || row[2] != uint32(PropertyNotFound) {
		t.Fatalf("deleted original properties remain visible or its reference was lost: %v", row)
	}
	reads := box.envelopeReads
	if got, _ := f.run(open); !bytes.Equal(got, ropError(3, 4, PropertyNotFound)) || box.envelopeReads != reads {
		t.Fatalf("opening deleted original: %x", got)
	}

	box.sourceErr = core.ErrResync
	if got, _ := f.run(restart); !bytes.Equal(got, ropError(0x30, 2, storeError(core.ErrResync))) {
		t.Fatalf("failed static restart: %x", got)
	}
	box.sourceErr = nil
	if row := readRow(); row[1] != mid || budget.used != frozenBytes {
		t.Fatalf("failed restart altered membership or budget: %v, %d", row, budget.used)
	}
	faiRunOK(t, f, restart)
	if row := readRow(); row[1] == mid || row[2] != "Second message" || budget.used != frozenBytes {
		t.Fatalf("restart did not replace membership and its lease: %v, %d", row, budget.used)
	}
	faiRunOK(t, f, binary.LittleEndian.AppendUint64([]byte{0x1d, 4, 1, 0x10}, fid))
	if budget.used != initialBytes {
		t.Fatalf("deleting the search retained %d budget bytes", budget.used-initialBytes)
	}
}

func TestSearchCompletionNotifications(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	f.handles = append(f.handles, 0xffffffff, 0xffffffff, 0xffffffff)
	f.transport.EnableCreateFolderSpike()
	f.transport.EnableContentsTableSpike()
	f.transport.synchronizationOptionsTrace = true
	f.open(int(MailboxRoot))
	create := createOperation(4, 1, 2, "Search with subscribers", "", false)
	create[4] = 2
	fid := binary.LittleEndian.Uint64(faiRunOK(t, f, create)[6:])
	registration := func(slot, whole byte, scope uint64) {
		t.Helper()
		wire := []byte{0x29, 4, 0, slot, 0x80, 0, whole}
		if whole == 0 {
			wire = binary.LittleEndian.AppendUint64(wire, scope)
			wire = binary.LittleEndian.AppendUint64(wire, 0)
		}
		if got := faiRunOK(t, f, wire); !bytes.Equal(got, ropError(0x29, slot, 0)) {
			t.Fatalf("SearchCompleted registration: %x", got)
		}
	}
	registration(4, 0, fid)
	registration(5, 1, 0)
	registration(6, 0, f.fids[Inbox])
	wantEvent := func(slot byte) []byte {
		event := append32([]byte{0x2a}, f.handles[slot])
		return binary.LittleEndian.AppendUint64(append(event, 4, 0x80, 0), fid)
	}
	events := append(wantEvent(4), wantEvent(5)...)
	checkEvents := func(request []byte, prefix int) {
		t.Helper()
		got, counts := f.run(request)
		if counts.errors != 0 || len(got) < prefix || !bytes.Equal(got[prefix:], events) {
			t.Fatalf("completion events for ROP %#x: %x", request[0], got)
		}
		delivered := counts.execution.NotificationDelivery()
		if len(delivered) != 1 || delivered[0] != "search_completed" {
			t.Fatalf("completion delivery log: %v", delivered)
		}
	}
	checkQuiet := func() {
		t.Helper()
		if got, _ := f.run(nil); len(got) != 0 {
			t.Fatalf("completion was replayed: %x", got)
		}
	}

	criterion := append32([]byte{8}, 0x0e070003)
	checkEvents(setSearchWire(2, searchRestart|searchStatic, criterion, f.fids[Inbox]), 6)
	checkQuiet()
	checkEvents(setSearchWire(2, searchRestart|searchStatic, nil), 6)
	checkQuiet()

	box.sourceErr = core.ErrResync
	failed, _ := f.run(setSearchWire(2, searchRestart|searchStatic, nil))
	if !bytes.Equal(failed, ropError(0x30, 2, storeError(core.ErrResync))) {
		t.Fatalf("failed static restart emitted completion: %x", failed)
	}

	if got := faiRunOK(t, f, setSearchWire(2, searchRestart, nil)); len(got) != 6 {
		t.Fatalf("unpopulated dynamic search emitted completion: %x", got)
	}
	openTable := []byte{5, 4, 2, 3, 0x10}
	failed, _ = f.run(openTable)
	if len(failed) != 6 || binary.LittleEndian.Uint32(failed[2:]) == 0 {
		t.Fatalf("failed dynamic population emitted completion: %x", failed)
	}
	checkQuiet()
	box.sourceErr = nil
	checkEvents(openTable, 10)
	faiRunOK(t, f, []byte{1, 4, 3})
	if got := faiRunOK(t, f, openTable); len(got) != 10 {
		t.Fatalf("second dynamic read emitted completion: %x", got)
	}
	checkQuiet()
}

func TestSearchRestrictionString8(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableCreateFolderSpike()
	f.transport.EnableContentsTableSpike()
	f.state.codePage = 1252
	f.open(int(MailboxRoot))
	create := createOperation(4, 1, 2, "Search code pages", "", false)
	create[4] = 2
	faiRunOK(t, f, create)
	box.objects[0].MIME = []byte("From: synthetic@example.invalid\r\nSubject: =?UTF-8?Q?caff=C3=A8_=E2=82=AC?=\r\n\r\nBody")

	criterion := func(kind uint16, value []byte) []byte {
		tag := 0x00370000 | uint32(kind)
		raw := append32([]byte{0, 2, 0, 8}, tag) // AND(Subject exists, Subject equals value).
		return append(append32(append32(append(raw, 4, 4), tag), tag), value...)
	}
	windows := criterion(0x1e, []byte{'c', 'a', 'f', 'f', 0xe8, ' ', 0x80, 0})
	wide := criterion(0x1f, []byte{'c', 0, 'a', 0, 'f', 0, 'f', 0, 0xe8, 0, ' ', 0, 0xac, 0x20, 0, 0})
	inbox := f.fids[Inbox]
	getANSI, getWide := []byte{0x31, 4, 2, 0, 1, 1}, []byte{0x31, 4, 2, 1, 1, 1}
	checkCriteria := func(request, predicate []byte, status uint32) {
		t.Helper()
		want := binary.LittleEndian.AppendUint16(ropError(0x31, 2, 0), uint16(len(predicate)))
		want = append(want, predicate...)
		want = binary.LittleEndian.AppendUint64(append(want, 4, 1, 0), inbox)
		if got := faiRunOK(t, f, request); !bytes.Equal(got, append32(want, status)) {
			t.Fatalf("search text and scope: %x", got)
		}
	}
	for _, input := range [][]byte{windows, wide} {
		faiRunOK(t, f, setSearchWire(2, searchRestart, input, inbox))
		checkCriteria(getANSI, windows, 0x08000003)
		checkCriteria(getWide, wide, 0x08000003)
		got := faiRunOK(t, f, []byte{5, 4, 2, 3, 0x10})
		if len(got) != 10 || binary.LittleEndian.Uint32(got[6:]) != 1 {
			t.Fatalf("criteria encoding changed match result: %x", got)
		}
		faiRunOK(t, f, []byte{1, 4, 3})
	}
	body, _ := f.execute(getANSI, uint32(10+4*len(f.handles)+15+8+len(windows)-1))
	if got := storeResponse(t, body).Operations; !bytes.Equal(got, ropError(0x31, 2, tableBufferTooSmall)) {
		t.Fatalf("encoded wire size did not respect Execute budget: %x", got)
	}

	f.state.codePage = 65001
	checkCriteria(getANSI, criterion(0x1e, []byte("caffè €\x00")), 0x08000001)
	for _, bad := range [][]byte{windows[:len(windows)-1], criterion(0x1e, []byte{0xff, 0})} {
		if got, _ := f.run(setSearchWire(2, searchRestart, bad, inbox)); !bytes.Equal(got, ropError(0x30, 2, 0x80070057)) {
			t.Fatalf("malformed String8 criterion: %x", got)
		}
	}
	checkCriteria(getWide, wide, 0x08000001)

	f.state.codePage = 1252
	japanese := criterion(0x1f, []byte{0xe5, 0x65, 0x2c, 0x67, 0, 0})
	faiRunOK(t, f, setSearchWire(2, searchRestart, japanese, inbox))
	if got, _ := f.run(getANSI); !bytes.Equal(got, ropError(0x31, 2, NotSupported)) {
		t.Fatalf("unrepresentable restriction was silently changed: %x", got)
	}
	checkCriteria(getWide, japanese, 0x08000003)
}

func TestSearchRestrictionTypeTrace(t *testing.T) {
	f := newCreateFixture(t)
	create := createOperation(4, 1, 2, "Synthetic search", "", false)
	create[4] = 2
	if wire, _ := f.run(create); len(wire) != 15 || binary.LittleEndian.Uint32(wire[2:]) != 0 {
		t.Fatalf("Search creation: %x", wire)
	}
	for i, tc := range []struct {
		wire, operator, kind string
	}{
		{"08 00005565", "", ""},
		{"08 00005565", "exist", "unspecified"},
		{"04 04 48105565 48100000 00000000", "property", "multi_value"},
		{"03 02000000 48305565 48300000 00000000000000000000000000000000", "content", "multi_value_instance"},
	} {
		if i == 1 {
			f.transport.EnableSynchronizationOptionsTrace()
		}
		request := setSearchWire(2, searchRestart, filterWire(t, tc.wire), f.fids[Inbox])
		wire, counts := f.run(request)
		if !bytes.Equal(wire, ropError(0x30, 2, searchRestrictionTooComplex)) {
			t.Fatalf("Search restriction changed protocol result: %x", wire)
		}
		op, kind := counts.execution.SearchRestrictionType()
		if op != tc.operator || kind != tc.kind {
			t.Fatalf("Search restriction diagnostic = %q %q, want %q %q", op, kind, tc.operator, tc.kind)
		}
	}
}

func TestSearchBindingTrace(t *testing.T) {
	f := newCreateFixture(t)
	f.transport.EnableUnknownPropertyTrace()
	create := createOperation(4, 1, 2, "Search binding diagnostics", "", false)
	create[4] = 2
	if wire, _ := f.run(create); len(wire) != 15 || binary.LittleEndian.Uint32(wire[2:]) != 0 {
		t.Fatalf("Search creation: %x", wire)
	}
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	ids, code := store.named.ids(context.Background(), []propertyName{{Set: [16]byte{0x91}, Kind: 0, LID: 12}}, true)
	if code != 0 || len(ids) != 1 {
		t.Fatal("could not register synthetic named property")
	}
	for _, tc := range []struct {
		enabled      bool
		tag          uint32
		reason, kind string
	}{
		{false, 0x23450102, "", ""},
		{true, 0x23450102, "fixed_property_outside_schema", "binary"},
		{true, 0x0ff600fb, "unsupported_type", "server_id"},
		{true, 0x8000000b, "reserved_property", "boolean"},
		{true, 0xfffe000b, "unregistered_named_property", "boolean"},
		{true, uint32(ids[0])<<16 | 0x000b, "named_property_outside_schema", "boolean"},
	} {
		if tc.enabled {
			f.transport.EnableSynchronizationOptionsTrace()
		}
		request := setSearchWire(2, searchRestart, append32([]byte{8}, tc.tag), f.fids[Inbox])
		wire, counts := f.run(request)
		if !bytes.Equal(wire, ropError(0x30, 2, searchRestrictionTooComplex)) {
			t.Fatalf("binding diagnostics changed the protocol result: %x", wire)
		}
		reason, kind := counts.execution.SearchBindingRejection()
		if reason != tc.reason || kind != tc.kind {
			t.Fatalf("Search binding diagnostic = %q/%q, want %q/%q", reason, kind, tc.reason, tc.kind)
		}
		unknown := counts.properties.UnknownProperties()
		if len(unknown.Tags) != 0 || unknown.HighIDs != 0 || unknown.ReservedIDs != 0 || unknown.Overflow != 0 {
			t.Fatal("Search binding exposed the rejected tag through GetProperties diagnostics")
		}
	}
}

func TestSearchParentServerID(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	f.transport.EnableCreateFolderSpike()
	f.transport.EnableContentsTableSpike()
	f.open(int(MailboxRoot))
	run := func(request []byte, code uint32) []byte {
		t.Helper()
		wire, _ := f.run(request)
		if len(wire) < 6 || binary.LittleEndian.Uint32(wire[2:]) != code {
			t.Fatalf("ROP %#x want %#x, got %x", request[0], code, wire)
		}
		return wire[6:]
	}
	create := createOperation(4, 1, 2, "Search by parent", "", false)
	create[4] = 2
	search := binary.LittleEndian.Uint64(run(create, 0))
	inbox := f.fids[Inbox]
	serverID := func(fid uint64) []byte {
		return append(binary.LittleEndian.AppendUint64([]byte{1}, fid), make([]byte, 12)...)
	}
	criterion := func(tag uint32, relation byte, operand []byte) []byte {
		// Compose the predicate with an existing restriction to check that
		// recursive evaluation forwards the source folder.
		raw := append32([]byte{0, 2, 0, 8}, 0x0e070003)
		raw = append32(append32(append(raw, 4, relation), tag), tag)
		return append(binary.LittleEndian.AppendUint16(raw, uint16(len(operand))), operand...)
	}
	for _, tc := range []struct {
		relation byte
		operand  []byte
		rows     uint32
	}{
		{4, serverID(inbox), 1},
		{5, serverID(inbox), 0},
		{5, serverID(search), 1},
		{5, []byte{0, 0xde, 0xad}, 1},
	} {
		raw := criterion(parentServerIDTag, tc.relation, tc.operand)
		run(setSearchWire(2, searchRestart, raw, inbox), 0)
		get := []byte{0x31, 4, 2, 1, 1, 1}
		want := binary.LittleEndian.AppendUint16(nil, uint16(len(raw)))
		want = append(want, raw...)
		if !bytes.HasPrefix(run(get, 0), want) {
			t.Fatal("Search did not return the original ServerId restriction")
		}
		table := run([]byte{5, 4, 2, 3, 0}, 0)
		if got := binary.LittleEndian.Uint32(table); got != tc.rows {
			t.Fatalf("ServerId relation %d produced %d rows, want %d", tc.relation, got, tc.rows)
		}
		if wire, _ := f.run([]byte{1, 4, 3}); len(wire) != 0 {
			t.Fatal("Release returned an unexpected ROP response")
		}
	}
	badInstance := serverID(inbox)
	badInstance[17] = 1
	for _, bad := range [][]byte{nil, {1}, {2}, badInstance} {
		run(setSearchWire(2, searchRestart, criterion(parentServerIDTag, 4, bad), inbox), 0x80070057)
	}
	run(setSearchWire(2, searchRestart, criterion(0x0ff600fb, 4, serverID(inbox)), inbox), searchRestrictionTooComplex)
	run(setSearchWire(2, searchRestart, criterion(parentServerIDTag, 2, serverID(inbox)), inbox), searchRestrictionTooComplex)
}

func TestSearchCriteriaAndDynamicResults(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &openMessageFixtureMailbox{messageFixtureMailbox: base}
	f.session = &testSession{box}
	f.handles = append(f.handles, 0xffffffff)
	f.transport.EnableCreateFolderSpike()
	f.transport.EnableContentsTableSpike()
	f.transport.EnableFolderMetadataSpike()
	f.open(int(MailboxRoot))
	box.objects[0].MIME = []byte("From: synthetic@example.invalid\r\nSubject: Synthetic subject\r\n\r\nBody")
	run := func(wire []byte, code uint32) []byte {
		t.Helper()
		got, _ := f.run(wire)
		if len(got) < 6 || binary.LittleEndian.Uint32(got[2:]) != code {
			t.Fatalf("ROP %#x want %#x, got %x", wire[0], code, got)
		}
		return got[6:]
	}
	create := createOperation(4, 1, 2, "Quartz Search", "", false)
	create[4] = 2
	fid := binary.LittleEndian.Uint64(run(create, 0))
	inbox := f.fids[Inbox]
	// RES_BITMASK/NEZ on PidTagMessageFlags, matching read messages.
	restriction := append32(append32([]byte{6, 1}, 0x0e070003), 1)
	run(setSearchWire(2, 2, restriction), 0x80040605)
	run(setSearchWire(2, 2, restriction, fid), 0x490)
	run(setSearchWire(2, 2, restriction, 0xdeadbeef), 0x490)
	run(setSearchWire(2, 3, restriction, inbox), 0x80070057)
	run(setSearchWire(1, 2, restriction, inbox), 0x461)
	run(setSearchWire(2, 2, restriction, inbox), 0)
	if box.sourceCalls != 0 {
		t.Fatal("Setting criteria eagerly read the Inbox")
	}

	get := []byte{0x31, 4, 2, 1, 1, 1}
	want := binary.LittleEndian.AppendUint16(nil, uint16(len(restriction)))
	want = append(want, restriction...)
	want = binary.LittleEndian.AppendUint64(append(want, 4, 1, 0), inbox)
	if got := run(get, 0); !bytes.Equal(got, append32(want, 0x08000003)) {
		t.Fatalf("criteria round trip: %x", got)
	}
	getStatus := []byte{0x31, 4, 2, 0, 0, 0}
	if got := run(getStatus, 0); !bytes.Equal(got, append32([]byte{0, 0, 4, 0, 0}, 0x08000003)) {
		t.Fatalf("criteria flags ignored: %x", got)
	}
	body, _ := f.execute(append(bytes.Clone(get), getStatus...), uint32(10+4*len(f.handles)+2*15))
	if got := storeResponse(t, body).Operations; len(got) != 6+15 || binary.LittleEndian.Uint32(got[2:]) != 0x47d || got[6] != 0x31 {
		t.Fatalf("large response did not fail locally: %x", got)
	}

	makeTable := []byte{5, 4, 2, 3, 0}
	if got := run(makeTable, 0); binary.LittleEndian.Uint32(got) != 0 {
		t.Fatal("unread message matched a read-only predicate")
	}
	if got := run(get, 0); !bytes.Equal(got, append32(want, 0x08000001)) {
		t.Fatal("initial population was not reflected in status")
	}
	tags := []uint32{0x67480014, 0x674a0014, 0x0037001f}
	run(tableColumnsWire(3, tags...), 0)
	box.objects[0].Properties[core.IsRead] = core.Bool(true)
	query := tableQueryWire(3, 0, 1, 10)
	rows := decodeTableRows(t, append(ropError(0x15, 3, 0), run(query, 0)...), tags)
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	original, err := store.observeMailMessages(context.Background(), f.session, inbox)
	if err != nil || len(original.messages) != 1 {
		t.Fatal("original folder read failed", err)
	}
	if len(rows) != 1 || rows[0][0] != inbox || rows[0][1] != original.messages[0].mid || rows[0][2] != "Synthetic subject" {
		t.Fatalf("Search results lost original identity: %v", rows)
	}
	mid := original.messages[0].mid
	openResult := openMessageOperation(4, 2, 4, fid, mid, 1252, 0)
	openOriginal := openMessageOperation(4, 2, 4, inbox, mid, 1252, 0)
	for _, wire := range [][]byte{openResult, openOriginal} {
		run(wire, 0)
		if object := f.state.mailbox.objects[f.handles[4]]; object.message == nil || object.fid != inbox {
			t.Fatal("message handle did not retain the source folder")
		}
		faiRunOK(t, f, []byte{1, 4, 4})
	}
	run(openMessageOperation(4, 2, 4, fid, mid+1<<32, 1252, 0), PropertyNotFound)
	other := f.session.Identity()
	other.Scope.Account = "other"
	if _, code := store.readSearchCriteria(context.Background(), scopedSession{Session: f.session, identity: other}, fid); code != accessDenied {
		t.Fatal("foreign mailbox exposed Search definition")
	}
	// Restart without a new restriction or scope reuses both fields.
	run(setSearchWire(2, 2, nil), 0)
	if got := run(get, 0); !bytes.Equal(got, append32(want, 0x08000001)) {
		t.Fatalf("Restart lost previous criteria: %x", got)
	}
	box.objects[0].Properties[core.IsRead] = core.Bool(false)
	empty := run(query, 0)
	if len(empty) != 3+10 || binary.LittleEndian.Uint16(empty[1:]) != 0 || empty[3] != 0x2a {
		t.Fatal("Search did not remove the row and announce the table change")
	}
	reads := box.envelopeReads
	run(openResult, PropertyNotFound)
	if box.envelopeReads != reads {
		t.Fatal("a stale search link read the original envelope")
	}
	run(openOriginal, 0)
	faiRunOK(t, f, []byte{1, 4, 4})

	entry := run(folderPropertiesOperation(4, 2, 1, 0x0fff0102), 0)
	if len(entry) != 3+46 || entry[0] != 0 || binary.LittleEndian.Uint16(entry[1:]) != 46 {
		t.Fatalf("Search EntryID: %x", entry)
	}
	metadata := func(entryID []byte) []byte {
		wire := binary.LittleEndian.AppendUint16([]byte{0x0a, 4, 1}, uint16(8+len(entryID)))
		wire = append32(append(wire, 1, 0), 0x36d50102)
		return append(binary.LittleEndian.AppendUint16(wire, uint16(len(entryID))), entryID...)
	}
	foreignEntry := bytes.Clone(entry[3:])
	foreignEntry[4] ^= 1
	run(metadata(foreignEntry), 0x80070057)
	run(metadata(entry[3:]), 0)
	f.open(int(Inbox))
	readReference := folderPropertiesOperation(4, 1, 1, 0x36d50102)
	if got := run(readReference, 0); !bytes.Equal(got, entry) {
		t.Fatal("Inbox did not return the reference written through Mailbox Root")
	}
	f.open(int(MailboxRoot))
	if got := run(readReference, 0); !bytes.Equal(got, entry) {
		t.Fatal("root did not return the canonical Inbox reference")
	}

	run(binary.LittleEndian.AppendUint64([]byte{0x1d, 4, 1, 0x10}, fid), 0)
	if got := run(readReference, 0); len(got) != 6 || binary.LittleEndian.Uint32(got[2:]) != PropertyNotFound {
		t.Fatalf("deleted Search remains linked from Reminders: %x", got)
	}
	run(get, PropertyNotFound)
	after, err := store.observeMailMessages(context.Background(), f.session, inbox)
	if err != nil || len(after.messages) != 1 || after.messages[0].mid != original.messages[0].mid {
		t.Fatal("deleting Search folder affected the original message")
	}
}
