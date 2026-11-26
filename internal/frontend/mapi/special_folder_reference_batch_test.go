// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"reflect"
	"testing"
)

type referenceTestValue struct {
	tag   uint32
	entry []byte
}

func referenceBatchOperation(values ...referenceTestValue) []byte {
	var properties []byte
	for _, v := range values {
		properties = binary.LittleEndian.AppendUint32(properties, v.tag)
		properties = binary.LittleEndian.AppendUint16(properties, uint16(len(v.entry)))
		properties = append(properties, v.entry...)
	}
	out := binary.LittleEndian.AppendUint16([]byte{0xa, 4, 1}, uint16(2+len(properties)))
	out = binary.LittleEndian.AppendUint16(out, uint16(len(values)))
	return append(out, properties...)
}
func newReferenceBatchFixture(t *testing.T) (*ostFixture, []byte, []byte) {
	t.Helper()
	f, cal := newCalendarFixture(t)
	_, con := newContactsFixture(t)
	f.session = &testSession{box: &bothMetadataMailbox{calendarMailbox: cal, contacts: con.contacts}}
	f.transport.EnableContactsMetadataSpike()
	for _, s := range f.transport.stores.entries {
		s.contactsMetadata = true
	}
	return f, calendarEntry(t, f), contactsEntry(t, f)
}
func TestSpecialReferenceBatchCanonicalAndIndexedProblems(t *testing.T) {
	f, calendar, contacts := newReferenceBatchFixture(t)
	f.transport.EnableCalendarReferenceSetTrace()
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	before, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime)
	if err != nil {
		t.Fatal(err)
	}
	mappings, next := len(s.byFID), s.next
	for _, values := range [][]referenceTestValue{{{0x36d00102, calendar}, {0x36d10102, contacts}}, {{0x36d10102, contacts}, {0x36d00102, calendar}}} {
		out, c := f.run(referenceBatchOperation(values...))
		if !bytes.Equal(out, []byte{0xa, 1, 0, 0, 0, 0, 0, 0}) || c.calendarSet.Confirmed != 1 {
			t.Fatal("canonical multi Set", out)
		}
	}
	bad := bytes.Clone(contacts)
	bad[22] ^= 1
	out, c := f.run(referenceBatchOperation(referenceTestValue{0x36d00102, calendar}, referenceTestValue{0x36d10102, bad}))
	want := []byte{0xa, 1, 0, 0, 0, 0, 1, 0, 1, 0, 2, 1, 0xd1, 0x36, 0x57, 0, 7, 0x80}
	if !bytes.Equal(out, want) || c.calendarSet.Confirmed != 1 || c.calendarSet.PropertyProblems != 0 {
		t.Fatal("indexed Contacts problem contaminated Calendar", out, c.calendarSet)
	}
	out, c = f.run(referenceBatchOperation(referenceTestValue{0x36d10102, contacts}, referenceTestValue{0x36d00102, contacts}))
	want = []byte{0xa, 1, 0, 0, 0, 0, 1, 0, 1, 0, 2, 1, 0xd0, 0x36, 5, 0, 7, 0x80}
	if !bytes.Equal(out, want) || c.calendarSet.PropertyProblems != 1 {
		t.Fatal("Calendar rebinding/index", out)
	}
	out, c = f.run(referenceBatchOperation(referenceTestValue{0x36d10102, nil}, referenceTestValue{0x36d00102, nil}))
	if len(out) != 28 || binary.LittleEndian.Uint16(out[6:]) != 2 || binary.LittleEndian.Uint16(out[8:]) != 0 || binary.LittleEndian.Uint16(out[18:]) != 1 || c.calendarSet.PropertyProblems != 1 {
		t.Fatal("all PropertyProblems retained")
	}
	after, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime)
	if err != nil || !reflect.DeepEqual(before, after) || next != s.next || mappings != len(s.byFID) || len(s.metadata) != 0 || len(s.ost) != 0 {
		t.Fatal("reference validation wrote state")
	}
}
func TestSpecialReferenceBatchPreflightClosedTagsAndTypes(t *testing.T) {
	f, calendar, contacts := newReferenceBatchFixture(t)
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	valid := referenceBatchOperation(referenceTestValue{0x36d00102, calendar}, referenceTestValue{0x36d10102, contacts})
	for _, request := range [][]byte{
		referenceBatchOperation(referenceTestValue{0x36d00102, calendar}, referenceTestValue{0x36d00102, calendar}),
		referenceBatchOperation(referenceTestValue{0x36d00102, calendar}, referenceTestValue{0x36130102, contacts}),
		referenceBatchOperation(referenceTestValue{0x36d00102, calendar}, referenceTestValue{0x36d10000, contacts}),
		referenceBatchOperation(referenceTestValue{0x36d00102, calendar}, referenceTestValue{0x36d10102, bytes.Repeat([]byte{0}, 47)}),
	} {
		before, next := f.handles[1], s.next
		_, c := f.execute(append([]byte{1, 4, 1}, request...), 65535)
		if c.unsupported != "RopSetProperties" {
			t.Fatal("closed batch not rejected")
		}
		if _, code := f.state.mailbox.object(before, 4); code != 0 || s.next != next {
			t.Fatal("rejected batch executed prefix")
		}
	}
	for n := 1; n < len(valid); n++ {
		ops, err := (ROPBuffer{Operations: valid[:n], Handles: f.handles}).decodeSpecialFolderOperations(false, false, false, false, false, true, true, false, false, false)
		if err == nil || ops != nil {
			t.Fatal("truncated reference batch", n)
		}
	}
	before, next := f.handles[1], s.next
	// Prefix Release must survive output shorter than the full two-problem bound.
	maximum := uint32(10 + 4*len(f.handles) + 8 + 20 - 1)
	f.execute(append([]byte{1, 4, 1}, valid...), maximum)
	if _, code := f.state.mailbox.object(before, 4); code != 0 || next != s.next {
		t.Fatal("short multi output executed prefix")
	}
	single, _ := newCalendarFixture(t)
	before = single.handles[1]
	single.execute(append([]byte{1, 4, 1}, valid...), 65535)
	if _, code := single.state.mailbox.object(before, 4); code != 0 {
		t.Fatal("disabled Contacts executed prefix")
	}
}

func TestSpecialReferenceBatchWithReminders(t *testing.T) {
	f, calendar, contacts := newReferenceBatchFixture(t)
	f.transport.EnableFolderMetadataSpike()
	f.transport.EnableCreateFolderSpike()
	f.transport.EnableCalendarReferenceSetTrace()
	f.handles = append(f.handles, 0xffffffff)
	f.open(int(MailboxRoot))
	create := createOperation(4, 1, 2, "Synthetic Reminders Search", "", false)
	create[4] = 2
	created, _ := f.run(create)
	if len(created) < 14 || binary.LittleEndian.Uint32(created[2:]) != 0 {
		t.Fatal("cannot create Reminders search folder")
	}
	root, _ := f.state.mailbox.object(f.handles[1], 4)
	entry := marshalFolderEntryID(root.binding.mailboxGUID, root.binding.store.replica, binary.LittleEndian.Uint64(created[6:]))
	reminders := referenceTestValue{remindersReferenceTag, entry}
	if _, c := f.execute(referenceBatchOperation(
		referenceTestValue{calendarReferenceTag, calendar}, reminders, reminders,
	), 65535); c.unsupported != "RopSetProperties" {
		t.Fatal("duplicate Reminders decoded")
	}
	read := folderPropertiesOperation(4, 1, 1, remindersReferenceTag)
	bad := bytes.Clone(entry)
	bad[4] ^= 1
	out, c := f.run(referenceBatchOperation(referenceTestValue{remindersReferenceTag, bad}, referenceTestValue{calendarReferenceTag, calendar}))
	want := propertyProblemsResponse(0xa, 1, []faiPropertyProblem{{index: 0, tag: remindersReferenceTag, code: 0x80070057}})
	if !bytes.Equal(out, want) || c.calendarSet.Confirmed != 1 {
		t.Fatalf("foreign search reference or Calendar result: %x", out)
	}
	for _, batch := range [][]referenceTestValue{
		{{calendarReferenceTag, calendar}, {0x36d10102, contacts}, reminders},
		{reminders, {calendarReferenceTag, calendar}},
	} {
		out, c := f.run(append(referenceBatchOperation(batch...), read...))
		want := append(propertyProblemsResponse(0xa, 1, nil), 7, 1, 0, 0, 0, 0, 0, 46, 0)
		want = append(want, entry...)
		if !bytes.Equal(out, want) || c.calendarSet.Confirmed != 1 {
			t.Fatalf("mixed reference did not retain Reminders in the batch: %x", out)
		}
	}
}
