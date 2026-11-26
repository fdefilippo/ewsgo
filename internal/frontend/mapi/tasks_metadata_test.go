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
type tasksMailbox struct {
	*stateMailbox
	tasks    core.Object
	tasksErr error
	calls    int
}

func (b *tasksMailbox) SpecialFolder(ctx context.Context, kind core.SpecialFolderKind) (core.Object, error) {
	b.calls++
	if kind != core.TasksFolder {
		return core.Object{}, core.ErrUnsupported
	}
	return b.tasks, b.tasksErr
}
func newTasksFixture(t *testing.T) (*ostFixture, *tasksMailbox) {
	t.Helper()
	f := newOSTFixture(t)
	f.box.page.Objects[0].Properties = core.Properties{core.DisplayName: core.Text("Mail root")}
	b := &tasksMailbox{stateMailbox: f.box, tasks: core.Object{ID: "opaque-tasks", Parent: f.box.Root(), Kind: core.Folder, Revision: 7, Properties: core.Properties{core.DisplayName: core.Text("Attivita native"), core.WellKnown: core.Text("tasks"), core.MetadataRevision: core.Integer(7)}}}
	f.session = &testSession{box: b}
	f.transport.EnableTasksMetadataSpike()
	for _, s := range f.transport.stores.entries {
		s.tasksMetadata = true
	}
	return f, b
}
func tasksEntry(t *testing.T, f *ostFixture) []byte {
	t.Helper()
	row, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36d40102))
	if len(row) != 55 || c.properties.Values != 1 || !bytes.Equal(row[:9], []byte{7, 1, 0, 0, 0, 0, 0, 46, 0}) {
		t.Fatal("Tasks canonical EntryID row")
	}
	return bytes.Clone(row[9:])
}
func tasksSetOperation(entry []byte) []byte {
	out := calendarReferenceSetOperation(4, 1, entry)
	out[9] = 0xd4
	return out
}
func TestTasksReferenceIdentityPropertiesReconnectAndSet(t *testing.T) {
	f, b := newTasksFixture(t)
	entry := tasksEntry(t, f)
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	converted, c := f.run(conversionOperation(0x44, entry[22:]))
	if len(converted) != 14 || c.errors != 0 {
		t.Fatal("Tasks conversion")
	}
	fid := binary.LittleEndian.Uint64(converted[6:])
	reverse, _ := f.run(conversionOperation(0x43, converted[6:]))
	if !bytes.Equal(reverse[6:], entry[22:]) {
		t.Fatal("Tasks conversion round trip")
	}
	opened, c := f.run(folderOperation(fid, 4, 0, 1))
	if len(opened) != 8 || c.errors != 0 {
		t.Fatal("Tasks OpenFolder")
	}
	class, _ := propertyString("IPF.Task", 0x1f, 1252)
	properties, c := f.run(folderPropertiesOperation(4, 1, 1, 0x3001001f, 0x3613001f, 0x67480014, 0x67490014))
	if c.properties.Values != 4 || c.properties.Errors != 0 || !bytes.Contains(properties, class) {
		t.Fatal("Tasks native metadata/class")
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
		if !bytes.Equal(tasksEntry(t, f), entry) {
			t.Fatal("Tasks root and Inbox differ")
		}
		out, c := f.run(tasksSetOperation(entry))
		if !bytes.Equal(out, []byte{0xa, 1, 0, 0, 0, 0, 0, 0}) || c.calendarSet.Requests != 0 {
			t.Fatal("Tasks Set or Calendar diagnostics contamination")
		}
		row, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36d40000))
		if c.properties.Errors != 0 || len(row) != 58 || !bytes.Equal(row[12:], entry) {
			t.Fatal("Tasks unspecified type")
		}
	}
	after, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime.Add(time.Second))
	if err != nil || !reflect.DeepEqual(before, after) || next != s.next || mappings != len(s.byFID) || len(s.metadata) != 0 {
		t.Fatal("Tasks Set changed state")
	}
	f.state.mailbox = newMailboxObjects(f.transport.stores)
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	f.open(int(Inbox))
	if !bytes.Equal(tasksEntry(t, f), entry) {
		t.Fatal("reconnect changed Tasks")
	}
	if _, err := b.Get(context.Background(), b.tasks.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("mail catalog expanded")
	}
}
func TestTasksRejectsNativeContentsButAllowsBoundedLocalFAI(t *testing.T) {
	f, _ := newTasksFixture(t)
	tasksEntry(t, f)
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	fid, err := s.specialFolderFID(context.Background(), f.session, tasksMetadataRole)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	source := func(context.Context, core.Mailbox, folderKey) ([]core.Object, error) { calls++; return nil, nil }
	before := messageProof(s)
	if _, err := s.observeMessageSet(context.Background(), f.session, fid, false, source); !errors.Is(err, core.ErrUnsupported) || calls != 0 || !reflect.DeepEqual(before, messageProof(s)) {
		t.Fatal("Tasks normal reader invoked")
	}
	if _, err := s.observeMailMessages(context.Background(), f.session, fid); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal("Tasks normal synchronization enabled")
	}
	record := faiTestRecord(s, fid, 1)
	seedFAI(t, s, record)
	observation, err := s.observeFAIMessages(context.Background(), f.session, fid)
	if err != nil || len(observation.snapshot.messages) != 1 {
		t.Fatal("local Tasks FAI unavailable")
	}
	observation.close()
	f.run(folderOperation(fid, 4, 0, 1))
	_, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36020003, 0x36030003))
	if c.properties.Errors != 2 {
		t.Fatal("Tasks counts invented")
	}
}
func TestTasksIndependentGatesErrorsAndNoRebinding(t *testing.T) {
	f, b := newTasksFixture(t)
	entry := tasksEntry(t, f)
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	for _, tc := range []struct {
		err  error
		code uint32
	}{{core.ErrNotFound, PropertyNotFound}, {core.ErrAccessDenied, accessDenied}, {core.ErrUnsupported, NotSupported}} {
		b.tasksErr = tc.err
		out, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36d40102))
		if c.properties.Errors != 1 || binary.LittleEndian.Uint32(out[len(out)-4:]) != tc.code {
			t.Fatal("Tasks backend error masked")
		}
	}
	b.tasksErr = nil
	original := b.tasks
	for _, change := range []func(){func() { b.tasks.Parent = "missing" }, func() { b.tasks.Kind = core.Message }, func() { b.tasks.ID = b.Root() }, func() { b.tasks.Properties = core.Properties{core.WellKnown: core.Text("calendar")} }} {
		b.tasks = original
		change()
		if _, err := s.specialFolderFID(context.Background(), f.session, tasksMetadataRole); err == nil {
			t.Fatal("invalid Tasks descriptor")
		}
	}
	b.tasks = original
	b.state.Owner = false
	if _, err := s.specialFolderFID(context.Background(), f.session, tasksMetadataRole); !errors.Is(err, core.ErrAccessDenied) {
		t.Fatal("revoked Tasks grant")
	}
	b.state.Owner = true
	foreign := scopedSession{Session: f.session, identity: f.session.Identity()}
	foreign.identity.Scope.Account = "foreign"
	if _, err := s.specialFolderFID(context.Background(), foreign, tasksMetadataRole); !errors.Is(err, core.ErrAccessDenied) {
		t.Fatal("foreign Tasks grant")
	}
	for _, bad := range [][]byte{nil, entry[:45], marshalFolderEntryID(object.binding.mailboxGUID, s.replica, f.fids[Inbox]), marshalFolderEntryID(object.binding.mailboxGUID, s.replica, folderID(900))} {
		out, _ := f.run(tasksSetOperation(bad))
		if len(out) != 18 {
			t.Fatal("Tasks rebind accepted")
		}
	}
	// A Tasks-only server does not decode Calendar writes, and vice versa.
	for _, calendar := range []bool{false, true} {
		fixture := newOSTFixture(t)
		if calendar {
			fixture, _ = newCalendarFixture(t)
		}
		prefix := append([]byte{1, 4, 1}, tasksSetOperation(entry)...)
		before := fixture.handles[1]
		fixture.execute(prefix, 65535)
		if _, code := fixture.state.mailbox.object(before, 4); code != 0 {
			t.Fatal("Tasks gate-off executed Release")
		}
	}
	calls, next := b.calls, s.next
	for _, tag := range []uint32{0x36d40102, 0x36d40000} {
		request := append([]byte{1, 4, 0}, folderPropertiesOperation(4, 1, 1, tag)...)
		maximum := uint32(10 + 4*len(f.handles) + specificPropertiesMetadataBound(specificProperties{tags: []uint32{tag}}, -1, nil) - 1)
		f.execute(request, maximum)
	}
	if calls != b.calls || next != s.next {
		t.Fatal("Tasks preflight did native read or allocation")
	}
}
func TestTasksHierarchyClassReferenceFiltersAndDependency(t *testing.T) {
	for _, tags := range [][]uint32{nil, {0x36130000}, {0x36d40000}, {0x36d40003, 0x36d40102}, {0x36d00000}} {
		f, b := newTasksFixture(t)
		entry := tasksEntry(t, f)
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
			if token.tag == 0x36d40102 {
				refs++
				if !bytes.Equal(token.value, entry) {
					t.Fatal("Tasks ICS reference differs")
				}
			}
			if token.tag == 0x3613001f {
				want, _ := propertyString("IPF.Task", 0x1f, 1252)
				if bytes.Equal(token.value, want) {
					classes++
				}
			}
		}
		wantRefs, wantClasses := 1, 1
		for _, tag := range tags {
			if tag>>16 == 0x36d4 {
				wantRefs = 0
			}
			if tag>>16 == 0x3613 {
				wantClasses = 0
			}
		}
		if refs != wantRefs || classes != wantClasses {
			t.Fatal("Tasks exclusions", refs, classes)
		}
		inbox, root := s.byFID[f.fids[Inbox]], folderKey{role: MailboxRoot}
		beforeInbox, beforeRoot := s.hierarchy[inbox].cn, s.hierarchy[root].cn
		b.tasks.Properties[core.DisplayName] = core.Text("Renamed Tasks")
		if _, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime.Add(time.Second)); err != nil {
			t.Fatal(err)
		}
		if s.hierarchy[inbox].cn != beforeInbox || s.hierarchy[root].cn != beforeRoot {
			t.Fatal("rename changed source reference CN")
		}
		b.tasks.ID = "replacement-tasks"
		if _, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime.Add(2*time.Second)); err != nil {
			t.Fatal(err)
		}
		if s.hierarchy[inbox].cn == beforeInbox || s.hierarchy[root].cn == beforeRoot {
			t.Fatal("replacement did not change sources")
		}
		beforeInbox, beforeRoot = s.hierarchy[inbox].cn, s.hierarchy[root].cn
		b.tasksErr = core.ErrNotFound
		if _, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime.Add(3*time.Second)); err != nil {
			t.Fatal(err)
		}
		if s.hierarchy[inbox].cn == beforeInbox || s.hierarchy[root].cn == beforeRoot {
			t.Fatal("removal did not change sources")
		}
	}
}

type allMetadataMailbox struct {
	*bothMetadataMailbox
	tasks core.Object
}

func (b *allMetadataMailbox) SpecialFolder(ctx context.Context, kind core.SpecialFolderKind) (core.Object, error) {
	if kind == core.TasksFolder {
		return b.tasks, nil
	}
	return b.bothMetadataMailbox.SpecialFolder(ctx, kind)
}
func TestThreeNativeRolesCanonicalBatchAndIdentity(t *testing.T) {
	f, calendar, contacts := newReferenceBatchFixture(t)
	_, task := newTasksFixture(t)
	both := f.session.Mailbox().(*bothMetadataMailbox)
	f.session = &testSession{box: &allMetadataMailbox{bothMetadataMailbox: both, tasks: task.tasks}}
	f.transport.EnableTasksMetadataSpike()
	for _, s := range f.transport.stores.entries {
		s.tasksMetadata = true
	}
	tasks := tasksEntry(t, f)
	if bytes.Equal(tasks, calendar) || bytes.Equal(tasks, contacts) {
		t.Fatal("Tasks identity reused")
	}
	values := []referenceTestValue{{0x36d00102, calendar}, {0x36d40102, tasks}, {0x36d10102, contacts}}
	f.transport.EnableCalendarReferenceSetTrace()
	out, c := f.run(referenceBatchOperation(values...))
	if !bytes.Equal(out, []byte{0xa, 1, 0, 0, 0, 0, 0, 0}) || c.calendarSet.Confirmed != 1 {
		t.Fatal("three-role canonical batch")
	}
	values[1].entry = contacts
	values[2].entry = calendar
	out, c = f.run(referenceBatchOperation(values...))
	if len(out) != 28 || binary.LittleEndian.Uint16(out[6:]) != 2 || binary.LittleEndian.Uint16(out[8:]) != 1 || binary.LittleEndian.Uint32(out[10:]) != 0x36d40102 || binary.LittleEndian.Uint16(out[18:]) != 2 || binary.LittleEndian.Uint32(out[20:]) != 0x36d10102 || c.calendarSet.Confirmed != 1 {
		t.Fatal("three-role problems or Calendar contamination")
	}
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	snapshot, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime)
	if err != nil {
		t.Fatal(err)
	}
	classes := map[string]int{}
	for _, folder := range snapshot.folders {
		classes[folder.containerClass]++
		if folder.fid == f.fids[Inbox] {
			for _, fid := range folder.referenceFIDs[:draftsReferenceRole] {
				if fid == 0 {
					t.Fatal("missing special target")
				}
			}
		}
	}
	if classes["IPF.Appointment"] != 1 || classes["IPF.Contact"] != 1 || classes["IPF.Task"] != 1 {
		t.Fatal("three native classes")
	}
	before, next := f.handles[1], s.next
	maximum := uint32(10 + 4*len(f.handles) + 8 + 30 - 1)
	f.execute(append([]byte{1, 4, 1}, referenceBatchOperation(values...)...), maximum)
	if _, code := f.state.mailbox.object(before, 4); code != 0 || next != s.next {
		t.Fatal("three-role output bound executed prefix")
	}
}
func TestTasksCapabilityAbsentAndAmbiguousRole(t *testing.T) {
	f := newOSTFixture(t)
	f.box.page.Objects[0].Properties = core.Properties{core.DisplayName: core.Text("Mail root")}
	f.transport.EnableTasksMetadataSpike()
	for _, s := range f.transport.stores.entries {
		s.tasksMetadata = true
	}
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	if _, err := object.binding.store.specialFolderFID(context.Background(), f.session, tasksMetadataRole); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal("missing Tasks capability invented")
	}
	f, b := newTasksFixture(t)
	entry := tasksEntry(t, f)
	object, _ = f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	fid, _ := parseFolderEntryID(entry, object.binding.mailboxGUID, s.replica)
	extra := b.tasks
	extra.ID = "another-task-role"
	b.page.Objects = append(b.page.Objects, extra)
	if _, err := s.folder(context.Background(), f.session, fid); !errors.Is(err, core.ErrConflict) {
		t.Fatal("ambiguous Tasks mapping reused")
	}
}
func FuzzSpecialReferenceBatches(f *testing.F) {
	f.Add(referenceBatchOperation(referenceTestValue{0x36d00102, make([]byte, 46)}, referenceTestValue{0x36d40102, nil}, referenceTestValue{0x36d10102, make([]byte, 45)}))
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 65535 {
			return
		}
		ops, err := (ROPBuffer{Operations: data, Handles: []uint32{1, 2}}).decodeSpecialFolderOperations(true, true, true, true, true, true, true, true, true, true)
		if err != nil && ops != nil {
			t.Fatal("partial reference batch")
		}
		for _, op := range ops {
			if op.SpecialFolderReference != nil {
				if len(op.SpecialFolderReference.values) > 3 {
					t.Fatal("unbounded list")
				}
				for _, v := range op.SpecialFolderReference.values {
					if len(v.entry) > 46 {
						t.Fatal("unbounded EntryID")
					}
				}
			}
		}
	})
}

func TestReferenceSetWrongSourceDoesNotReadOrAssignParent(t *testing.T) {
	f, b := newTasksFixture(t)
	entry := tasksEntry(t, f)
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	fid, _ := parseFolderEntryID(entry, object.binding.mailboxGUID, s.replica)
	f.run(folderOperation(fid, 4, 0, 1))
	calls, next, mappings := b.calls, s.next, len(s.byFID)
	out, _ := f.run(tasksSetOperation(entry))
	if len(out) != 18 || binary.LittleEndian.Uint32(out[14:]) != accessDenied || calls != b.calls || next != s.next || mappings != len(s.byFID) {
		t.Fatal("wrong source caused read/allocation")
	}
}
