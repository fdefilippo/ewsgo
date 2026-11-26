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

// No contact-card or event reader is present: only the folder capability exists.
type contactsMailbox struct {
	*stateMailbox
	contacts    core.Object
	contactsErr error
	calls       int
}

func (b *contactsMailbox) SpecialFolder(ctx context.Context, kind core.SpecialFolderKind) (core.Object, error) {
	b.calls++
	if kind != core.ContactsFolder {
		return core.Object{}, core.ErrUnsupported
	}
	return b.contacts, b.contactsErr
}
func newContactsFixture(t *testing.T) (*ostFixture, *contactsMailbox) {
	t.Helper()
	f := newOSTFixture(t)
	f.box.page.Objects[0].Properties = core.Properties{core.DisplayName: core.Text("Mail root")}
	b := &contactsMailbox{stateMailbox: f.box, contacts: core.Object{ID: "opaque-contacts", Parent: f.box.Root(), Kind: core.Folder, Revision: 7, Properties: core.Properties{core.DisplayName: core.Text("Contatti nativi"), core.WellKnown: core.Text("contacts"), core.MetadataRevision: core.Integer(7)}}}
	f.session = &testSession{box: b}
	f.transport.EnableContactsMetadataSpike()
	for _, s := range f.transport.stores.entries {
		s.contactsMetadata = true
	}
	return f, b
}
func contactsEntry(t *testing.T, f *ostFixture) []byte {
	t.Helper()
	row, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36d10102))
	if len(row) != 55 || c.properties.Values != 1 || !bytes.Equal(row[:9], []byte{7, 1, 0, 0, 0, 0, 0, 46, 0}) {
		t.Fatal("Contacts canonical EntryID row")
	}
	return bytes.Clone(row[9:])
}
func contactsSetOperation(entry []byte) []byte {
	out := calendarReferenceSetOperation(4, 1, entry)
	out[9] = 0xd1
	return out
}
func TestContactsReferenceIdentityPropertiesReconnectAndSet(t *testing.T) {
	f, b := newContactsFixture(t)
	entry := contactsEntry(t, f)
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	converted, c := f.run(conversionOperation(0x44, entry[22:]))
	if len(converted) != 14 || c.errors != 0 {
		t.Fatal("Contacts conversion")
	}
	fid := binary.LittleEndian.Uint64(converted[6:])
	reverse, _ := f.run(conversionOperation(0x43, converted[6:]))
	if !bytes.Equal(reverse[6:], entry[22:]) {
		t.Fatal("Contacts conversion round trip")
	}
	opened, c := f.run(folderOperation(fid, 4, 0, 1))
	if len(opened) != 8 || c.errors != 0 {
		t.Fatal("Contacts OpenFolder")
	}
	class, _ := propertyString("IPF.Contact", 0x1f, 1252)
	properties, c := f.run(folderPropertiesOperation(4, 1, 1, 0x3001001f, 0x3613001f, 0x67480014, 0x67490014))
	if c.properties.Values != 4 || c.properties.Errors != 0 || !bytes.Contains(properties, class) {
		t.Fatal("Contacts native metadata/class")
	}
	f.open(int(Inbox))
	before, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime)
	if err != nil {
		t.Fatal(err)
	}
	next, mappings := s.next, len(s.byFID)
	f.transport.EnableCalendarReferenceSetTrace()
	for _, role := range []SpecialFolderRole{Inbox, MailboxRoot} {
		f.open(int(role))
		if !bytes.Equal(contactsEntry(t, f), entry) {
			t.Fatal("Contacts root and Inbox differ")
		}
		out, c := f.run(contactsSetOperation(entry))
		if !bytes.Equal(out, []byte{0xa, 1, 0, 0, 0, 0, 0, 0}) || c.calendarSet.Requests != 0 {
			t.Fatal("Contacts Set or Calendar diagnostics contamination")
		}
		row, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36d10000))
		if c.properties.Errors != 0 || len(row) != 58 || !bytes.Equal(row[12:], entry) {
			t.Fatal("Contacts unspecified type")
		}
	}
	after, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime.Add(time.Second))
	if err != nil || !reflect.DeepEqual(before, after) || next != s.next || mappings != len(s.byFID) || len(s.metadata) != 0 {
		t.Fatal("Contacts Set changed state")
	}
	f.state.mailbox = newMailboxObjects(f.transport.stores)
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	f.open(int(Inbox))
	if !bytes.Equal(contactsEntry(t, f), entry) {
		t.Fatal("reconnect changed Contacts")
	}
	if _, err := b.Get(context.Background(), b.contacts.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("mail catalog expanded")
	}
}
func TestContactsRejectsNativeContentsButAllowsBoundedLocalFAI(t *testing.T) {
	f, _ := newContactsFixture(t)
	contactsEntry(t, f)
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	fid, err := s.specialFolderFID(context.Background(), f.session, contactsMetadataRole)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	source := func(context.Context, core.Mailbox, folderKey) ([]core.Object, error) { calls++; return nil, nil }
	before := messageProof(s)
	if _, err := s.observeMessageSet(context.Background(), f.session, fid, false, source); !errors.Is(err, core.ErrUnsupported) || calls != 0 || !reflect.DeepEqual(before, messageProof(s)) {
		t.Fatal("Contacts normal reader invoked")
	}
	if _, err := s.observeMailMessages(context.Background(), f.session, fid); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal("Contacts normal synchronization enabled")
	}
	record := faiTestRecord(s, fid, 1)
	seedFAI(t, s, record)
	observation, err := s.observeFAIMessages(context.Background(), f.session, fid)
	if err != nil || len(observation.snapshot.messages) != 1 {
		t.Fatal("local Contacts FAI unavailable")
	}
	observation.close()
	f.run(folderOperation(fid, 4, 0, 1))
	_, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36020003, 0x36030003))
	if c.properties.Errors != 2 {
		t.Fatal("Contacts counts invented")
	}
}
func TestContactsIndependentGatesErrorsAndNoRebinding(t *testing.T) {
	f, b := newContactsFixture(t)
	entry := contactsEntry(t, f)
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	for _, tc := range []struct {
		err  error
		code uint32
	}{{core.ErrNotFound, PropertyNotFound}, {core.ErrAccessDenied, accessDenied}, {core.ErrUnsupported, NotSupported}} {
		b.contactsErr = tc.err
		out, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36d10102))
		if c.properties.Errors != 1 || binary.LittleEndian.Uint32(out[len(out)-4:]) != tc.code {
			t.Fatal("Contacts backend error masked")
		}
	}
	b.contactsErr = nil
	original := b.contacts
	for _, change := range []func(){func() { b.contacts.Parent = "missing" }, func() { b.contacts.Kind = core.Message }, func() { b.contacts.ID = b.Root() }, func() { b.contacts.Properties = core.Properties{core.WellKnown: core.Text("calendar")} }} {
		b.contacts = original
		change()
		if _, err := s.specialFolderFID(context.Background(), f.session, contactsMetadataRole); err == nil {
			t.Fatal("invalid Contacts descriptor")
		}
	}
	b.contacts = original
	b.state.Owner = false
	if _, err := s.specialFolderFID(context.Background(), f.session, contactsMetadataRole); !errors.Is(err, core.ErrAccessDenied) {
		t.Fatal("revoked Contacts grant")
	}
	b.state.Owner = true
	foreign := scopedSession{Session: f.session, identity: f.session.Identity()}
	foreign.identity.Scope.Account = "foreign"
	if _, err := s.specialFolderFID(context.Background(), foreign, contactsMetadataRole); !errors.Is(err, core.ErrAccessDenied) {
		t.Fatal("foreign Contacts grant")
	}
	for _, bad := range [][]byte{nil, entry[:45], marshalFolderEntryID(object.binding.mailboxGUID, s.replica, f.fids[Inbox]), marshalFolderEntryID(object.binding.mailboxGUID, s.replica, folderID(900))} {
		out, _ := f.run(contactsSetOperation(bad))
		if len(out) != 18 {
			t.Fatal("Contacts rebind accepted")
		}
	}
	// A Contacts-only server does not decode Calendar writes, and vice versa.
	for _, calendar := range []bool{false, true} {
		fixture := newOSTFixture(t)
		if calendar {
			fixture, _ = newCalendarFixture(t)
		}
		prefix := append([]byte{1, 4, 1}, contactsSetOperation(entry)...)
		before := fixture.handles[1]
		fixture.execute(prefix, 65535)
		if _, code := fixture.state.mailbox.object(before, 4); code != 0 {
			t.Fatal("Contacts gate-off executed Release")
		}
	}
	calls, next := b.calls, s.next
	for _, tag := range []uint32{0x36d10102, 0x36d10000} {
		request := append([]byte{1, 4, 0}, folderPropertiesOperation(4, 1, 1, tag)...)
		maximum := uint32(10 + 4*len(f.handles) + specificPropertiesMetadataBound(specificProperties{tags: []uint32{tag}}, -1, nil) - 1)
		f.execute(request, maximum)
	}
	if calls != b.calls || next != s.next {
		t.Fatal("Contacts preflight did native read or allocation")
	}
}
func TestContactsHierarchyClassReferenceFiltersAndDependency(t *testing.T) {
	for _, tags := range [][]uint32{nil, {0x36130000}, {0x36d10000}, {0x36d10003, 0x36d10102}, {0x36d00000}} {
		f, b := newContactsFixture(t)
		entry := contactsEntry(t, f)
		object, _ := f.state.mailbox.object(f.handles[0], 4)
		s := object.binding.store
		first, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime)
		if err != nil {
			t.Fatal(err)
		}
		transfer, err := hierarchyFastTransfer(s.replica, object.binding.mailboxGUID, synchronizationConfigure{tags: tags}, hierarchyDelta{changes: first.folders})
		if err != nil {
			t.Fatal(err)
		}
		wire := transfer.data
		// Decode the actual stream via the existing independent FX reader.
		tokens := readTestFX(t, wire)
		refs, classes := 0, 0
		for _, token := range tokens {
			if token.tag == 0x36d10102 {
				refs++
				if !bytes.Equal(token.value, entry) {
					t.Fatal("Contacts ICS reference differs")
				}
			}
			if token.tag == 0x3613001f {
				want, _ := propertyString("IPF.Contact", 0x1f, 1252)
				if bytes.Equal(token.value, want) {
					classes++
				}
			}
		}
		wantRefs, wantClasses := 1, 1
		for _, tag := range tags {
			if tag>>16 == 0x36d1 {
				wantRefs = 0
			}
			if tag>>16 == 0x3613 {
				wantClasses = 0
			}
		}
		if refs != wantRefs || classes != wantClasses {
			t.Fatal("Contacts exclusions", refs, classes)
		}
		inbox, root := s.byFID[f.fids[Inbox]], folderKey{role: MailboxRoot}
		beforeInbox, beforeRoot := s.hierarchy[inbox].cn, s.hierarchy[root].cn
		b.contacts.Properties[core.DisplayName] = core.Text("Renamed Contacts")
		if _, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if s.hierarchy[inbox].cn != beforeInbox || s.hierarchy[root].cn != beforeRoot {
			t.Fatal("rename changed source reference CN")
		}
		b.contacts.ID = "replacement-contacts"
		if _, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		if s.hierarchy[inbox].cn == beforeInbox || s.hierarchy[root].cn == beforeRoot {
			t.Fatal("replacement did not change sources")
		}
		beforeInbox, beforeRoot = s.hierarchy[inbox].cn, s.hierarchy[root].cn
		b.contactsErr = core.ErrNotFound
		if _, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		if s.hierarchy[inbox].cn == beforeInbox || s.hierarchy[root].cn == beforeRoot {
			t.Fatal("removal did not change sources")
		}
	}
}

type bothMetadataMailbox struct {
	*calendarMailbox
	contacts core.Object
}

func (b *bothMetadataMailbox) SpecialFolder(ctx context.Context, kind core.SpecialFolderKind) (core.Object, error) {
	if kind == core.ContactsFolder {
		return b.contacts, nil
	}
	return b.calendarMailbox.SpecialFolder(ctx, kind)
}
func TestContactsAndCalendarCoexistWithoutIdentityOrDiagnosticMixup(t *testing.T) {
	f, calendar := newCalendarFixture(t)
	_, contacts := newContactsFixture(t)
	b := &bothMetadataMailbox{calendarMailbox: calendar, contacts: contacts.contacts}
	f.session = &testSession{box: b}
	f.transport.EnableContactsMetadataSpike()
	for _, s := range f.transport.stores.entries {
		s.contactsMetadata = true
	}
	calEntry, contactEntry := calendarEntry(t, f), contactsEntry(t, f)
	if bytes.Equal(calEntry, contactEntry) {
		t.Fatal("special roles share identity")
	}
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	scan, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string]int{}
	for _, folder := range scan.folders {
		classes[folder.containerClass]++
		if folder.fid == f.fids[Inbox] && (folder.referenceFIDs[0] == 0 || folder.referenceFIDs[1] == 0 || folder.referenceFIDs[0] == folder.referenceFIDs[1]) {
			t.Fatal("Inbox mixed canonical targets")
		}
	}
	if classes["IPF.Appointment"] != 1 || classes["IPF.Contact"] != 1 {
		t.Fatal("native role classes")
	}
	f.transport.EnableCalendarReferenceSetTrace()
	out, c := f.run(contactsSetOperation(calEntry))
	if len(out) != 18 || binary.LittleEndian.Uint32(out[14:]) != accessDenied || c.calendarSet.Requests != 0 {
		t.Fatal("Contacts rebound to Calendar")
	}
	out, c = f.run(calendarReferenceSetOperation(4, 1, contactEntry))
	if len(out) != 18 || binary.LittleEndian.Uint32(out[14:]) != accessDenied || c.calendarSet.PropertyProblems != 1 {
		t.Fatal("Calendar rebound to Contacts")
	}
}
func TestContactsCapabilityAbsentIsNotAnEmptyNativeContentsClaim(t *testing.T) {
	f := newOSTFixture(t)
	f.box.page.Objects[0].Properties = core.Properties{core.DisplayName: core.Text("Mail root")}
	f.transport.EnableContactsMetadataSpike()
	for _, s := range f.transport.stores.entries {
		s.contactsMetadata = true
	}
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	if _, err := object.binding.store.specialFolderFID(context.Background(), f.session, contactsMetadataRole); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal("missing capability invented")
	}
	if _, err := object.binding.store.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime); err != nil {
		t.Fatal("optional absent capability breaks mail tree", err)
	}
}

func TestContactsAmbiguousRoleCannotReuseOldMapping(t *testing.T) {
	f, b := newContactsFixture(t)
	entry := contactsEntry(t, f)
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	fid, _ := parseFolderEntryID(entry, object.binding.mailboxGUID, s.replica)
	extra := b.contacts
	extra.ID = "other-native-contact-role"
	b.page.Objects = append(b.page.Objects, extra)
	before := len(s.byFID)
	if _, err := s.folder(context.Background(), f.session, fid); !errors.Is(err, core.ErrConflict) || len(s.byFID) != before {
		t.Fatal("ambiguous role reused mapping")
	}
}
