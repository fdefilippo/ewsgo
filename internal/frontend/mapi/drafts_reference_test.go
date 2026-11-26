// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"ewsgo/internal/core"
	"reflect"
	"testing"
	"time"
)

func addDrafts(f *ostFixture) {
	f.box.page.Objects[0].Properties = core.Properties{core.DisplayName: core.Text("Mail root")}
	f.box.page.Objects = append(f.box.page.Objects, core.Object{ID: "opaque-drafts", Parent: f.box.Root(), Kind: core.Folder, Properties: core.Properties{core.DisplayName: core.Text("Bozze native"), core.WellKnown: core.Text("drafts")}})
	f.transport.EnableDraftsReferenceSpike()
	for _, s := range f.transport.stores.entries {
		s.draftsReference = true
	}
}
func newDraftsFixture(t *testing.T) (*ostFixture, *stateMailbox) {
	f := newOSTFixture(t)
	addDrafts(f)
	return f, f.box
}
func draftsEntry(t *testing.T, f *ostFixture) []byte {
	t.Helper()
	row, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36d70102))
	if len(row) != 55 || c.properties.Values != 1 {
		t.Fatal("Drafts canonical row")
	}
	return bytes.Clone(row[9:])
}
func draftsSetOperation(entry []byte) []byte {
	return referenceBatchOperation(referenceTestValue{0x36d70102, entry})
}
func TestDraftsReferenceIdentityPropertiesReconnectAndSet(t *testing.T) {
	f, b := newDraftsFixture(t)
	entry := draftsEntry(t, f)
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	converted, c := f.run(conversionOperation(0x44, entry[22:]))
	if len(converted) != 14 || c.errors != 0 {
		t.Fatal("Drafts conversion")
	}
	fid := binary.LittleEndian.Uint64(converted[6:])
	reverse, _ := f.run(conversionOperation(0x43, converted[6:]))
	if !bytes.Equal(reverse[6:], entry[22:]) {
		t.Fatal("Drafts conversion round trip")
	}
	opened, c := f.run(folderOperation(fid, 4, 0, 1))
	if len(opened) != 8 || c.errors != 0 {
		t.Fatal("Drafts OpenFolder")
	}
	class, _ := propertyString("IPF.Note", 0x1f, 1252)
	properties, c := f.run(folderPropertiesOperation(4, 1, 1, 0x3001001f, 0x3613001f, 0x67480014, 0x67490014))
	if c.properties.Values != 4 || c.properties.Errors != 0 || !bytes.Contains(properties, class) {
		t.Fatal("Drafts native metadata/class")
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
		if !bytes.Equal(draftsEntry(t, f), entry) {
			t.Fatal("Drafts root and Inbox differ")
		}
		out, c := f.run(draftsSetOperation(entry))
		if !bytes.Equal(out, []byte{0xa, 1, 0, 0, 0, 0, 0, 0}) || c.calendarSet.Requests != 0 {
			t.Fatal("Drafts Set or Calendar diagnostics contamination")
		}
		row, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36d70000))
		if c.properties.Errors != 0 || len(row) != 58 || !bytes.Equal(row[12:], entry) {
			t.Fatal("Drafts unspecified type")
		}
	}
	after, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime.Add(time.Second))
	if err != nil || !reflect.DeepEqual(before, after) || next != s.next || mappings != len(s.byFID) || len(s.metadata) != 0 {
		t.Fatal("Drafts Set changed state")
	}
	f.state.mailbox = newMailboxObjects(f.transport.stores)
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	f.open(int(Inbox))
	if !bytes.Equal(draftsEntry(t, f), entry) {
		t.Fatal("reconnect changed Drafts")
	}
	if _, err := b.Get(context.Background(), "opaque-drafts"); err != nil {
		t.Fatal("ordinary Drafts disappeared")
	}
}
func TestDraftsHierarchyClassReferenceFiltersAndDependency(t *testing.T) {
	for _, tags := range [][]uint32{nil, {0x36130000}, {0x36d70000}, {0x36d70003, 0x36d70102}, {0x36d00000}} {
		f, b := newDraftsFixture(t)
		entry := draftsEntry(t, f)
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
			if token.tag == 0x36d70102 {
				refs++
				if !bytes.Equal(token.value, entry) {
					t.Fatal("Drafts ICS reference differs")
				}
			}
			if token.tag == 0x3613001f {
				want, _ := propertyString("IPF.Note", 0x1f, 1252)
				if bytes.Equal(token.value, want) {
					classes++
				}
			}
		}
		wantRefs, wantClasses := 1, 1
		for _, tag := range tags {
			if tag>>16 == 0x36d7 {
				wantRefs = 0
			}
			if tag>>16 == 0x3613 {
				wantClasses = 0
			}
		}
		if refs != wantRefs || classes != wantClasses {
			t.Fatal("Drafts exclusions", refs, classes)
		}
		inbox, root := s.byFID[f.fids[Inbox]], folderKey{role: MailboxRoot}
		beforeInbox, beforeRoot := s.hierarchy[inbox].cn, s.hierarchy[root].cn
		b.page.Objects[4].Properties[core.DisplayName] = core.Text("Renamed Drafts")
		if _, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if s.hierarchy[inbox].cn != beforeInbox || s.hierarchy[root].cn != beforeRoot {
			t.Fatal("rename changed source reference CN")
		}
		b.page.Objects[4].ID = "replacement-drafts"
		if _, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		if s.hierarchy[inbox].cn == beforeInbox || s.hierarchy[root].cn == beforeRoot {
			t.Fatal("replacement did not change sources")
		}
		beforeInbox, beforeRoot = s.hierarchy[inbox].cn, s.hierarchy[root].cn
		b.page.Objects = b.page.Objects[:4]
		if _, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		if s.hierarchy[inbox].cn == beforeInbox || s.hierarchy[root].cn == beforeRoot {
			t.Fatal("removal did not change sources")
		}
	}
}

func TestDraftsFailuresGatesAndNoRebinding(t *testing.T) {
	f, b := newDraftsFixture(t)
	entry := draftsEntry(t, f)
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	original := b.page.Objects[4]
	for _, change := range []func(){func() { b.page.Objects[4].Parent = "missing" }, func() { b.page.Objects[4].Kind = core.Message }, func() { b.page.Objects[4].ID = b.Root() }, func() {
		b.page.Objects = append(b.page.Objects, core.Object{ID: "duplicate", Parent: b.Root(), Kind: core.Folder, Properties: core.Properties{core.WellKnown: core.Text("drafts")}})
	}} {
		b.page.Objects = b.page.Objects[:5]
		b.page.Objects[4] = original
		change()
		if _, err := s.specialFolderFID(context.Background(), f.session, draftsReferenceRole); err == nil {
			t.Fatal("invalid ordinary target accepted")
		}
	}
	b.page.Objects = b.page.Objects[:5]
	b.page.Objects[4] = original
	for _, tc := range []struct {
		err  error
		code uint32
	}{{core.ErrAccessDenied, accessDenied}, {core.ErrUnsupported, NotSupported}, {core.ErrNotFound, PropertyNotFound}} {
		b.err = tc.err
		row, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36d70102))
		if c.properties.Errors != 1 || binary.LittleEndian.Uint32(row[len(row)-4:]) != tc.code {
			t.Fatal("error masked")
		}
	}
	b.err = nil
	b.state.Owner = false
	if _, err := s.specialFolderFID(context.Background(), f.session, draftsReferenceRole); !errors.Is(err, core.ErrAccessDenied) {
		t.Fatal("revoked owner")
	}
	b.state.Owner = true
	foreign := scopedSession{Session: f.session, identity: f.session.Identity()}
	foreign.identity.Scope.Account = "foreign"
	if _, err := s.specialFolderFID(context.Background(), foreign, draftsReferenceRole); !errors.Is(err, core.ErrAccessDenied) {
		t.Fatal("foreign scope")
	}
	for _, bad := range [][]byte{nil, entry[:45], marshalFolderEntryID(object.binding.mailboxGUID, s.replica, f.fids[Inbox]), marshalFolderEntryID(object.binding.mailboxGUID, s.replica, folderID(900))} {
		out, _ := f.run(draftsSetOperation(bad))
		if len(out) != 18 {
			t.Fatal("rebind acknowledged")
		}
	}
	calls, next := b.calls, s.next
	for _, tag := range []uint32{0x36d70102, 0x36d70000} {
		request := append([]byte{1, 4, 0}, folderPropertiesOperation(4, 1, 1, tag)...)
		maximum := uint32(10 + 4*len(f.handles) + specificPropertiesMetadataBound(specificProperties{tags: []uint32{tag}}, -1, nil) - 1)
		f.execute(request, maximum)
	}
	if calls != b.calls || next != s.next {
		t.Fatal("preflight effects")
	}
	f.open(int(SentItems))
	calls, next = b.calls, s.next
	out, _ := f.run(draftsSetOperation(entry))
	if len(out) != 18 || calls != b.calls || next != s.next {
		t.Fatal("wrong source read or allocation")
	}
	f, b = newDraftsFixture(t)
	b.page.Objects = b.page.Objects[:4]
	row, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36d70102))
	if c.properties.Errors != 1 || binary.LittleEndian.Uint32(row[len(row)-4:]) != PropertyNotFound {
		t.Fatal("absent ordinary target fabricated")
	}
	fixture := newOSTFixture(t)
	before := fixture.handles[1]
	fixture.execute(append([]byte{1, 4, 1}, draftsSetOperation(entry)...), 65535)
	if _, code := fixture.state.mailbox.object(before, 4); code != 0 {
		t.Fatal("gate-off executed prefix")
	}
}

func TestDraftsDoesNotDisableExistingNormalMailReader(t *testing.T) {
	f, _ := newDraftsFixture(t)
	draftsEntry(t, f)
	o, _ := f.state.mailbox.object(f.handles[0], 4)
	s := o.binding.store
	fid, err := s.specialFolderFID(context.Background(), f.session, draftsReferenceRole)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	source := func(context.Context, core.Mailbox, folderKey) ([]core.Object, error) { calls++; return nil, nil }
	_, err = s.observeMessageSet(context.Background(), f.session, fid, false, source)
	if err != nil || calls != 1 {
		t.Fatal("ordinary Drafts changed to metadata-only", err)
	}
	s.draftsReference = false
	f.transport.draftsReferenceSpike = false
	row, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36d70102))
	if c.properties.Errors != 1 || len(row) != 12 {
		t.Fatal("reference escaped gate")
	}
	f.run(folderOperation(fid, 4, 0, 1))
	_, c = f.run(folderPropertiesOperation(4, 1, 1, 0x3613001f))
	if c.properties.Errors != 1 {
		t.Fatal("class escaped gate")
	}
}

func TestFourCanonicalReferencesAndBoundedProblems(t *testing.T) {
	f, _, _ := newReferenceBatchFixture(t)
	_, task := newTasksFixture(t)
	f.session = &testSession{box: &allMetadataMailbox{bothMetadataMailbox: f.session.Mailbox().(*bothMetadataMailbox), tasks: task.tasks}}
	f.transport.EnableTasksMetadataSpike()
	for _, s := range f.transport.stores.entries {
		s.tasksMetadata = true
	}
	addDrafts(f)
	values := []referenceTestValue{{0x36d70102, draftsEntry(t, f)}, {0x36d00102, calendarEntry(t, f)}, {0x36d40102, tasksEntry(t, f)}, {0x36d10102, contactsEntry(t, f)}}
	out, _ := f.run(referenceBatchOperation(values...))
	if !bytes.Equal(out, []byte{0xa, 1, 0, 0, 0, 0, 0, 0}) {
		t.Fatal("four canonical references")
	}
	for i := range values {
		values[i].entry = nil
	}
	out, _ = f.run(referenceBatchOperation(values...))
	if len(out) != 48 || binary.LittleEndian.Uint16(out[6:]) != 4 {
		t.Fatal("four problems lost")
	}
	for i, v := range values {
		at := 8 + i*10
		if binary.LittleEndian.Uint16(out[at:]) != uint16(i) || binary.LittleEndian.Uint32(out[at+2:]) != v.tag {
			t.Fatal("problem indices")
		}
	}
	before := f.handles[1]
	maximum := uint32(10 + 4*len(f.handles) + 48 - 1)
	f.execute(append([]byte{1, 4, 1}, referenceBatchOperation(values...)...), maximum)
	if _, code := f.state.mailbox.object(before, 4); code != 0 {
		t.Fatal("short output released prefix")
	}
	for _, bad := range [][]referenceTestValue{append(values, values[0]), {values[0], values[0]}, {{0x36d7001f, nil}}, {{0x36d70102, make([]byte, 47)}}, {{0x12340102, nil}}} {
		f.execute(append([]byte{1, 4, 1}, referenceBatchOperation(bad...)...), 65535)
		if _, code := f.state.mailbox.object(before, 4); code != 0 {
			t.Fatal("bad framing executed prefix")
		}
	}
}

func FuzzFourFolderReferenceFraming(f *testing.F) {
	f.Add(referenceBatchOperation(referenceTestValue{0x36d70102, make([]byte, 46)}, referenceTestValue{0x36d00102, nil}, referenceTestValue{0x36d10102, nil}, referenceTestValue{0x36d40102, nil}))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65520 {
			return
		}
		ops, err := (ROPBuffer{Operations: data, Handles: []uint32{1, 2}}).decodeReferenceFolderOperations(true, true, true, true, true, true, true, true, true, true, true)
		if err != nil && ops != nil {
			t.Fatal("partial decode")
		}
		for _, op := range ops {
			if set := op.SpecialFolderReference; set != nil {
				if len(set.values) < 1 || len(set.values) > 4 {
					t.Fatal("unbounded list")
				}
				for _, v := range set.values {
					if len(v.entry) > 46 {
						t.Fatal("unbounded EntryID")
					}
				}
			}
		}
	})
}
