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

func calendarReferenceSetOperation(logon, input byte, entry []byte) []byte {
	out := binary.LittleEndian.AppendUint16([]byte{0xa, logon, input}, uint16(8+len(entry)))
	out = append(out, 1, 0, 2, 1, 0xd0, 0x36)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(entry)))
	return append(out, entry...)
}

func TestCalendarReferenceSetIndependentFramingAndGate(t *testing.T) {
	// This fixture spells out the SetProperties header and all length fields.
	entry := bytes.Repeat([]byte{0xaa}, 46)
	wire := append([]byte{0x0a, 4, 1, 54, 0, 1, 0, 2, 1, 0xd0, 0x36, 46, 0}, entry...)
	if !bytes.Equal(wire, calendarReferenceSetOperation(4, 1, entry)) {
		t.Fatal("independent Set fixture")
	}
	b := ROPBuffer{Operations: wire, Handles: []uint32{1, 2}}
	if _, err := b.decodeCalendarFolderOperations(true, true, true, true, true, false); !errors.Is(err, ErrUnsupported) {
		t.Fatal("Calendar default gate")
	}
	ops, err := b.decodeCalendarFolderOperations(false, false, false, false, false, true)
	if err != nil || len(ops) != 1 || ops[0].SpecialFolderReference == nil || !bytes.Equal(ops[0].SpecialFolderReference.values[0].entry, entry) {
		t.Fatal("closed Calendar decoder")
	}
	wire[13] ^= 1
	if !bytes.Equal(ops[0].SpecialFolderReference.values[0].entry, entry) {
		t.Fatal("request aliases caller")
	}
	for n := 1; n < len(wire); n++ {
		if decoded, err := (ROPBuffer{Operations: wire[:n], Handles: b.Handles}).decodeCalendarFolderOperations(false, false, false, false, false, true); err == nil || decoded != nil {
			t.Fatal("truncated Set accepted", n)
		}
	}
	for _, change := range []func([]byte){func(b []byte) { b[3]-- }, func(b []byte) { b[3]++ }, func(b []byte) { b[11]-- }, func(b []byte) { b[11]++ }, func(b []byte) { b[2] = 2 }, func(b []byte) { b[7] = 3 }, func(b []byte) { b[5] = 2 }} {
		bad := bytes.Clone(wire)
		change(bad)
		if decoded, err := (ROPBuffer{Operations: bad, Handles: b.Handles}).decodeCalendarFolderOperations(false, false, false, false, false, true); err == nil || decoded != nil {
			t.Fatal("malformed/unsupported Calendar Set accepted")
		}
	}
	for _, n := range []int{0, 45, 46} {
		decoded, err := (ROPBuffer{Operations: calendarReferenceSetOperation(4, 1, make([]byte, n)), Handles: b.Handles}).decodeCalendarFolderOperations(false, false, false, false, false, true)
		if err != nil || decoded[0].SpecialFolderReference == nil {
			t.Fatal("structurally valid Binary must reach semantic validation")
		}
	}
	if _, err := (ROPBuffer{Operations: calendarReferenceSetOperation(4, 1, make([]byte, 47)), Handles: b.Handles}).decodeCalendarFolderOperations(false, false, false, false, false, true); !errors.Is(err, ErrUnsupported) {
		t.Fatal("closed size bound")
	}
}

func TestCalendarReferenceSetReaffirmsNativeIdentityWithoutState(t *testing.T) {
	f, _ := newCalendarFixture(t)
	entry := calendarEntry(t, f)
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	before, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime)
	if err != nil {
		t.Fatal(err)
	}
	fid, _ := parseFolderEntryID(entry, object.binding.mailboxGUID, s.replica)
	mappings, next := len(s.byFID), s.next
	for _, role := range []SpecialFolderRole{Inbox, MailboxRoot, Inbox} {
		f.open(int(role))
		response, c := f.run(calendarReferenceSetOperation(4, 1, entry))
		if !bytes.Equal(response, []byte{0xa, 1, 0, 0, 0, 0, 0, 0}) || c.errors != 0 {
			t.Fatal("canonical idempotent Set failed")
		}
		if !bytes.Equal(calendarEntry(t, f), entry) {
			t.Fatal("canonical reference changed")
		}
	}
	after, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime.Add(time.Second))
	if err != nil || !reflect.DeepEqual(hierarchyRow(t, before, fid), hierarchyRow(t, after, fid)) || len(s.byFID) != mappings || s.next != next || len(s.metadata) != 0 || len(s.ost) != 0 {
		t.Fatal("Set mutated hierarchy/CN/mapping/metadata/OST")
	}
	// Native Get/Query cannot see Calendar, and writes cannot use either API.
	// Reopening Inbox in the same batch must precede the accepted Set normally.
	prefix := append([]byte{1, 4, 1}, folderOperation(f.fids[Inbox], 4, 0, 1)...)
	response, c := f.run(append(prefix, calendarReferenceSetOperation(4, 1, entry)...))
	if len(response) != 16 || c.errors != 0 || !bytes.Equal(response[8:], []byte{0xa, 1, 0, 0, 0, 0, 0, 0}) {
		t.Fatal("Set after decoded prefix")
	}
}

func TestCalendarReferenceSetRefusesRebindingAndUntrustedIDs(t *testing.T) {
	f, _ := newCalendarFixture(t)
	entry := calendarEntry(t, f)
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	cases := []struct {
		name  string
		value []byte
		code  uint32
	}{
		{"other native folder", marshalFolderEntryID(object.binding.mailboxGUID, s.replica, f.fids[Inbox]), accessDenied},
		{"service folder", marshalFolderEntryID(object.binding.mailboxGUID, s.replica, f.fids[Schedule]), accessDenied},
		{"unassigned counter", marshalFolderEntryID(object.binding.mailboxGUID, s.replica, folderID(900)), PropertyNotFound},
		{"empty", nil, 0x80070057},
		{"short", entry[:45], 0x80070057},
	}
	for _, i := range []int{0, 4, 20, 22, 44} {
		bad := bytes.Clone(entry)
		bad[i] ^= 1
		cases = append(cases, struct {
			name  string
			value []byte
			code  uint32
		}{"foreign or malformed", bad, 0x80070057})
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			mappings, next := len(s.byFID), s.next
			response, c := f.run(calendarReferenceSetOperation(4, 1, tc.value))
			// Independent PropertyProblem layout: success ROP, Count1, Index0, Tag,
			// ErrorCode. ROP success alone is not evidence that a property was accepted.
			want := []byte{0xa, 1, 0, 0, 0, 0, 1, 0, 0, 0, 2, 1, 0xd0, 0x36}
			want = binary.LittleEndian.AppendUint32(want, tc.code)
			if !bytes.Equal(response, want) || c.errors != 0 || len(s.byFID) != mappings || s.next != next || len(s.metadata) != 0 {
				t.Fatal("rebind changed state or incorrect PropertyProblem")
			}
			if !bytes.Equal(calendarEntry(t, f), entry) {
				t.Fatal("failed Set replaced native reference")
			}
		})
	}
	for _, role := range []SpecialFolderRole{SentItems, Schedule} {
		f.open(int(role))
		r, _ := f.run(calendarReferenceSetOperation(4, 1, entry))
		if len(r) != 18 || binary.LittleEndian.Uint32(r[14:]) != accessDenied {
			t.Fatal("arbitrary source accepted")
		}
	}
	f.open(int(Inbox))
	r, c := f.run(calendarReferenceSetOperation(4, 0, entry))
	if c.errors != 1 || binary.LittleEndian.Uint32(r[2:]) != NotSupported {
		t.Fatal("Logon accepted Folder property")
	}
}

func TestCalendarReferenceSetCurrentAuthorizationAndPreflight(t *testing.T) {
	f, b := newCalendarFixture(t)
	entry := calendarEntry(t, f)
	object, _ := f.state.mailbox.object(f.handles[0], 4)
	s := object.binding.store
	for _, tc := range []struct {
		err  error
		code uint32
	}{{core.ErrAccessDenied, accessDenied}, {core.ErrNotFound, PropertyNotFound}, {core.ErrUnsupported, NotSupported}} {
		b.calendarErr = tc.err
		r, c := f.run(calendarReferenceSetOperation(4, 1, entry))
		if c.errors != 1 || binary.LittleEndian.Uint32(r[2:]) != tc.code {
			t.Fatal("native error masked")
		}
	}
	b.calendarErr = nil
	b.state.Owner = false
	r, c := f.run(calendarReferenceSetOperation(4, 1, entry))
	if c.errors != 1 || binary.LittleEndian.Uint32(r[2:]) != accessDenied {
		t.Fatal("revocation ignored")
	}
	b.state.Owner = true
	own := f.session
	foreign := scopedSession{Session: own, identity: own.Identity()}
	foreign.identity.Scope.Account = "foreign"
	f.session = foreign
	r, c = f.run(calendarReferenceSetOperation(4, 1, entry))
	if c.errors != 1 || binary.LittleEndian.Uint32(r[2:]) != accessDenied {
		t.Fatal("foreign scope accepted")
	}
	f.session = own
	b.calendar.Parent = "not-in-projection"
	r, c = f.run(calendarReferenceSetOperation(4, 1, entry))
	if c.errors != 1 {
		t.Fatal("dangling native parent accepted")
	}
	b.calendar.Parent = b.Root()
	request := append([]byte{1, 4, 0}, calendarReferenceSetOperation(4, 1, entry)...)
	handle := f.handles[0]
	calls := b.specialCalls
	next := s.next
	f.execute(request, 35) // 10 + two handles + worst-case18 = 36.
	f.execute(append(request, 0xef, 4, 0), 65535)
	f.execute(request[:len(request)-1], 65535)
	if _, code := f.state.mailbox.object(handle, 4); code != 0 || b.specialCalls != calls || s.next != next {
		t.Fatal("invalid batch read/allocated/released")
	}
	baseline := newOSTFixture(t)
	handle = baseline.handles[1]
	baseline.execute(append([]byte{1, 4, 1}, calendarReferenceSetOperation(4, 1, entry)...), 65535)
	if _, code := baseline.state.mailbox.object(handle, 4); code != 0 {
		t.Fatal("gate-off batch executes prefix")
	}
}

func FuzzCalendarReferenceSet(f *testing.F) {
	f.Add(calendarReferenceSetOperation(4, 1, make([]byte, 46)))
	f.Add(calendarReferenceSetOperation(4, 1, nil))
	f.Fuzz(func(t *testing.T, b []byte) {
		if len(b) > 65535 {
			return
		}
		ops, err := (ROPBuffer{Operations: b, Handles: []uint32{1, 2}}).decodeCalendarFolderOperations(true, true, true, true, true, true)
		if err != nil && ops != nil {
			t.Fatal("partially decoded batch")
		}
		if err == nil {
			for _, op := range ops {
				if op.SpecialFolderReference != nil && len(op.SpecialFolderReference.values[0].entry) > 46 {
					t.Fatal("unbounded reference retained")
				}
			}
		}
	})
}
