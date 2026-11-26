// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"ewsgo/internal/core"
	"testing"
	"time"
)

type calendarMailbox struct {
	*stateMailbox
	calendar     core.Object
	calendarErr  error
	specialCalls int
}

func (b *calendarMailbox) SpecialFolder(ctx context.Context, kind core.SpecialFolderKind) (core.Object, error) {
	b.specialCalls++
	if kind != core.CalendarFolder {
		return core.Object{}, core.ErrUnsupported
	}
	return b.calendar, b.calendarErr
}
func newCalendarFixture(t *testing.T) (*ostFixture, *calendarMailbox) {
	t.Helper()
	f := newOSTFixture(t)
	f.box.page.Objects[0].Properties = core.Properties{core.DisplayName: core.Text("Mail root")}
	b := &calendarMailbox{stateMailbox: f.box, calendar: core.Object{ID: "opaque-calendar", Parent: f.box.Root(), Kind: core.Folder, Revision: 7, Properties: core.Properties{core.DisplayName: core.Text("Calendario nativo"), core.WellKnown: core.Text("calendar"), core.MetadataRevision: core.Integer(7)}}}
	f.session = &testSession{box: b}
	f.transport.EnableCalendarMetadataSpike()
	// The common fixture has already logged on: explicitly model configuration
	// before its first Calendar request. Production config precedes any bind.
	for _, s := range f.transport.stores.entries {
		s.calendarMetadata = true
	}
	return f, b
}
func calendarEntry(t *testing.T, f *ostFixture) []byte {
	t.Helper()
	row, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36d00102))
	if len(row) != 55 || !bytes.Equal(row[:9], []byte{7, 1, 0, 0, 0, 0, 0, 46, 0}) || c.properties.Values != 1 {
		t.Fatal("Calendar EntryID row", len(row))
	}
	return bytes.Clone(row[9:])
}
func conversionOperation(id byte, value []byte) []byte { return append([]byte{id, 4, 0}, value...) }

func TestFolderEntryIDIndependentWireFixture(t *testing.T) {
	var mailbox, replica [16]byte
	for i := range mailbox {
		mailbox[i] = byte(i)
		replica[i] = byte(0x10 + i)
	}
	// Expected bytes are specified independently of the encoder, including the
	// big-endian six-byte global counter within the otherwise opaque FID.
	want, _ := hex.DecodeString("00000000000102030405060708090a0b0c0d0e0f0100101112131415161718191a1b1c1d1e1f0102030405060000")
	fid := uint64(0x0605040302010001)
	if got := marshalFolderEntryID(mailbox, replica, fid); !bytes.Equal(got, want) {
		t.Fatal("46-byte independent fixture")
	}
	if got, err := parseFolderEntryID(want, mailbox, replica); err != nil || got != fid {
		t.Fatal("fixture lookup")
	}
	for _, index := range []int{0, 4, 19, 20, 21, 22, 37, 44, 45} {
		bad := bytes.Clone(want)
		bad[index] ^= 1
		if _, err := parseFolderEntryID(bad, mailbox, replica); err == nil {
			t.Fatal("foreign/malformed EntryID accepted", index)
		}
	}
	for n := 0; n < len(want); n++ {
		if _, err := parseFolderEntryID(want[:n], mailbox, replica); err == nil {
			t.Fatal("truncated EntryID")
		}
	}
}

func TestCalendarReferenceResolveOpenPropertiesAndHierarchy(t *testing.T) {
	f, b := newCalendarFixture(t)
	entry := calendarEntry(t, f)
	object, code := f.state.mailbox.object(f.handles[0], 4)
	if code != 0 || !bytes.Equal(entry[4:20], object.binding.mailboxGUID[:]) || !bytes.Equal(entry[22:38], object.binding.store.replica[:]) || object.binding.mailboxGUID == ([16]byte{}) {
		t.Fatal("Logon identity correspondence")
	}
	converted, c := f.run(conversionOperation(0x44, entry[22:]))
	if len(converted) != 14 || c.errors != 0 {
		t.Fatal("long-term resolution")
	}
	fid := binary.LittleEndian.Uint64(converted[6:])
	reverse, _ := f.run(conversionOperation(0x43, converted[6:]))
	if !bytes.Equal(reverse, append([]byte{0x43, 0, 0, 0, 0, 0}, entry[22:]...)) {
		t.Fatal("conversion round trip")
	}
	opened, _ := f.run(folderOperation(fid, 4, 0, 1))
	if len(opened) != 8 || binary.LittleEndian.Uint32(opened[2:]) != 0 {
		t.Fatal("reference cannot open real Calendar")
	}
	row, c := f.run(folderPropertiesOperation(4, 1, 1, 0x3001001f, 0x3613001f, 0x67480014, 0x67490014))
	if c.properties.Errors != 0 || c.properties.Values != 4 || !bytes.Contains(row, func() []byte { b, _ := propertyString("IPF.Appointment", 0x1f, 1252); return b }()) {
		t.Fatal("Calendar native properties/class")
	}
	first, err := object.binding.store.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime)
	if err != nil {
		t.Fatal("Calendar hierarchy", err)
	}
	v := hierarchyRow(t, first, fid)
	if v.parent != f.fids[IPMSubtree] {
		t.Fatal("Calendar parent differs from native parent")
	}
	same, err := object.binding.store.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime.Add(time.Second))
	if err != nil || hierarchyRow(t, same, fid).cn != v.cn {
		t.Fatal("identical Calendar scan changes CN")
	}
	b.calendar.Properties[core.DisplayName] = core.Text("Renamed")
	b.calendar.Properties[core.MetadataRevision] = core.Integer(8)
	b.calendar.Revision = 8
	next, err := object.binding.store.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime.Add(2*time.Second))
	if err != nil || hierarchyRow(t, next, fid).cn == v.cn {
		t.Fatal("native metadata change lost")
	}
	f.open(int(MailboxRoot))
	rootEntry := calendarEntry(t, f)
	if !bytes.Equal(entry, rootEntry) {
		t.Fatal("Inbox and Root identities differ")
	}
	// PT_UNSPECIFIED resolves to PtypBinary while retaining the same EntryID.
	row, c = f.run(folderPropertiesOperation(4, 1, 1, 0x36d00000))
	if c.properties.Errors != 0 || len(row) != 58 || !bytes.Equal(row[12:], entry) {
		t.Fatal("unspecified Calendar type", len(row))
	}
	// Replacement contexts bind the same store and must retain the same identity.
	f.state.mailbox = newMailboxObjects(f.transport.stores)
	logon, _ := f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	if len(logon) != 166 {
		t.Fatal("reconnect Logon")
	}
	f.open(int(Inbox))
	if !bytes.Equal(calendarEntry(t, f), entry) {
		t.Fatal("reconnect changed Calendar identity")
	}
	if _, err := b.Get(context.Background(), b.calendar.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("mail-only Get expanded")
	}
}

func TestCalendarErrorsAndGate(t *testing.T) {
	f, b := newCalendarFixture(t)
	entry := calendarEntry(t, f)
	for _, tc := range []struct {
		err  error
		code uint32
	}{{core.ErrNotFound, PropertyNotFound}, {core.ErrAccessDenied, accessDenied}, {core.ErrUnsupported, NotSupported}} {
		b.calendarErr = tc.err
		row, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36d00102))
		if c.properties.Errors != 1 || binary.LittleEndian.Uint32(row[len(row)-4:]) != tc.code {
			t.Fatal("native error transformed", tc.err)
		}
	}
	b.calendarErr = nil
	b.state.Owner = false
	row, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36d00102))
	if c.properties.Errors != 1 || binary.LittleEndian.Uint32(row[len(row)-4:]) != accessDenied {
		t.Fatal("owner revocation ignored")
	}
	b.state.Owner = true
	for _, role := range []SpecialFolderRole{SentItems, Schedule} {
		f.open(int(role))
		row, c = f.run(folderPropertiesOperation(4, 1, 1, 0x36d00102))
		if c.properties.Errors != 1 || binary.LittleEndian.Uint32(row[len(row)-4:]) != PropertyNotFound {
			t.Fatal("reference leaked to arbitrary folder")
		}
	}
	f.open(int(Inbox))
	for _, tag := range []uint32{0x36d00003, 0x36d10102, 0x36d40102, 0x36d50102} {
		row, c = f.run(folderPropertiesOperation(4, 1, 1, tag))
		if c.properties.Errors != 1 || binary.LittleEndian.Uint32(row[len(row)-4:]) != NotSupported {
			t.Fatal("unsupported type/role invented")
		}
	}
	for _, i := range []int{0, 16, 22, 23} {
		bad := bytes.Clone(entry[22:])
		bad[i] ^= 1
		r, c := f.run(conversionOperation(0x44, bad))
		if len(r) != 14 || c.errors != 0 {
			t.Fatal("namespace conversion incorrectly validates object existence or padding")
		}
	}
	// An assigned Calendar FID does not retain access after the native role disappears.
	converted, _ := f.run(conversionOperation(0x44, entry[22:]))
	fid := binary.LittleEndian.Uint64(converted[6:])
	b.calendarErr = core.ErrAccessDenied
	opened, c := f.run(folderOperation(fid, 4, 0, 1))
	if c.errors != 1 || binary.LittleEndian.Uint32(opened[2:]) != accessDenied {
		t.Fatal("stale mapping grants access")
	}
	diagnostic := ROPBuffer{Operations: conversionOperation(0x44, entry[22:]), Handles: []uint32{1}}
	if _, err := diagnostic.DecodeOperations(); !errors.Is(err, ErrUnsupported) {
		t.Fatal("diagnostic-only transport decoded a store operation")
	}
}

func TestCalendarBatchPreflightAndQuotas(t *testing.T) {
	f, b := newCalendarFixture(t)
	before := f.handles[1]
	calls := b.specialCalls
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	mappings := len(object.binding.store.byFID)
	request := append([]byte{1, 4, 1}, conversionOperation(0x43, binary.LittleEndian.AppendUint64(nil, f.fids[Inbox]))...)
	for _, maximum := range []uint32{24, 47} {
		f.execute(request, maximum)
		if _, code := f.state.mailbox.object(before, 4); code != 0 {
			t.Fatal("small output executed prefix")
		}
	}
	f.execute(append(request, 0xef, 4, 0), 65535)
	f.execute(request[:len(request)-1], 65535)
	if b.specialCalls != calls || len(object.binding.store.byFID) != mappings {
		t.Fatal("preflight read native or assigned IDs")
	}
	object.binding.store.next = (1 << 48) - 1
	if _, err := object.binding.store.specialFolderFID(context.Background(), f.session, calendarMetadataRole); err == nil {
		t.Fatal("mapping quota ignored")
	}
}

func FuzzCalendarEntryIDAndConversions(f *testing.F) {
	f.Add([]byte{0x43, 4, 0, 1, 0, 0, 0, 0, 0, 0, 0})
	f.Add(append([]byte{0x44, 4, 0}, make([]byte, 24)...))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 65535 {
			return
		}
		_, _ = parseFolderEntryID(b, [16]byte{}, [16]byte{})
		ops, err := (ROPBuffer{Operations: b, Handles: []uint32{1}}).decodeCalendarFolderOperations(false, false, false, false, false, true)
		if err != nil && ops != nil {
			t.Fatal("partial decoded batch")
		}
	})
}

func TestCalendarScopeTreeAndPropertyPreflight(t *testing.T) {
	f, b := newCalendarFixture(t)
	original := b.calendar
	obj, _ := f.state.mailbox.object(f.handles[0], 4)
	for _, change := range []func(){
		func() { b.calendar.Parent = "unrepresented-parent" },
		func() { b.calendar.Kind = core.Message },
		func() { b.calendar.ID = b.Root() },
		func() { b.calendar.Properties = core.Properties{core.WellKnown: core.Text("tasks")} },
	} {
		b.calendar = original
		change()
		if _, err := obj.binding.store.specialFolderFID(context.Background(), f.session, calendarMetadataRole); err == nil {
			t.Fatal("invalid Calendar descriptor accepted")
		}
	}
	b.calendar = original
	foreign := scopedSession{Session: f.session, identity: f.session.Identity()}
	foreign.identity.Scope.Account = "other"
	before := len(obj.binding.store.byFID)
	if _, err := obj.binding.store.specialFolderFID(context.Background(), foreign, calendarMetadataRole); !errors.Is(err, core.ErrAccessDenied) || len(obj.binding.store.byFID) != before {
		t.Fatal("foreign scope granted Calendar mapping")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := obj.binding.store.specialFolderFID(ctx, f.session, calendarMetadataRole); !errors.Is(err, context.Canceled) {
		t.Fatal("cancellation ignored")
	}
	for _, tag := range []uint32{0x36d00102, 0x36d00000} {
		calls := b.specialCalls
		handle := f.handles[1]
		request := append([]byte{1, 4, 0}, folderPropertiesOperation(4, 1, 1, tag)...)
		max := uint32(10 + 4*len(f.handles) + specificPropertiesMetadataBound(specificProperties{tags: []uint32{tag}}, -1, nil) - 1)
		f.execute(request, max)
		if b.specialCalls != calls || len(obj.binding.store.byFID) != before {
			t.Fatal("short property output performed read/mapping")
		}
		if _, code := f.state.mailbox.object(handle, 4); code != 0 {
			t.Fatal("short property output released handle")
		}
	}
	// The opt-in does not grant event contents or fabricate message counts.
	entry := calendarEntry(t, f)
	resolved, _ := f.run(conversionOperation(0x44, entry[22:]))
	fid := binary.LittleEndian.Uint64(resolved[6:])
	f.run(folderOperation(fid, 4, 0, 1))
	row, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36020003, 0x36030003))
	if c.properties.Errors != 2 || row[6] != 1 {
		t.Fatal("invented Calendar content count")
	}
	f.transport.EnableHierarchyDownloadSpike()
	request := hierarchyConfigureOperation(4, 1, 0)
	request[4] = 1 // SynchronizationType Contents.
	_, c = f.execute(request, 65535)
	if c.unsupported != "RopSynchronizationConfigure" {
		t.Fatal("Calendar Contents was accepted")
	}
	// Native ancestry changes after a mapping must not create a dangling parent.
	b.calendar.Parent = "unrepresented-parent"
	if _, err := obj.binding.store.folder(context.Background(), f.session, fid); err == nil {
		t.Fatal("mapped Calendar lost parent validation")
	}
}
