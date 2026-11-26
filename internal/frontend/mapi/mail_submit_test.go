// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"testing"
)

func TestLegacySubmissionBatch(t *testing.T) {
	f := newCreateFixture(t)
	openInbox := folderOperation(f.fids[Inbox], 4, 0, 2)
	cases := []struct {
		id     byte
		fields []byte
		result uint32
	}{
		{ropAbortSubmit, make([]byte, 16), unableToAbort},
		{ropSpoolerLockMessage, make([]byte, 9), NotSupported},
	}
	for _, tc := range cases {
		raw := append([]byte{tc.id, 4, 0}, tc.fields...)
		for size := 1; size < len(raw); size++ {
			if _, err := (ROPBuffer{Operations: raw[:size], Handles: f.handles}).DecodeOperations(); err == nil {
				t.Fatalf("truncated 0x%02x accepted at byte %d", tc.id, size)
			}
		}
		out, result := f.run(append(bytes.Clone(raw), openInbox...))
		want := append(ropError(tc.id, 0, tc.result), 2, 2, 0, 0, 0, 0, 0, 0)
		if !bytes.Equal(out, want) || result.errors != 1 {
			t.Fatalf("0x%02x followed by OpenFolder = %x, want %x", tc.id, out, want)
		}
	}
}

func TestSpoolerNewMailPrivateLogonAndSubscribedMessage(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &openMessageFixtureMailbox{messageFixtureMailbox: base}
	f.session = &testSession{box}
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	fid := f.fids[Inbox]
	snapshot, err := store.observeMailMessages(t.Context(), f.session, fid)
	if err != nil || len(snapshot.messages) != 1 {
		t.Fatal("observe Inbox:", err)
	}
	mid := snapshot.messages[0].mid
	sent := binary.LittleEndian.AppendUint64([]byte{0x29, 4, 0, 3, notifyNewMail, 0, 0}, f.fids[SentItems])
	faiRunOK(t, f, binary.LittleEndian.AppendUint64(sent, 0))
	faiRunOK(t, f, []byte{0x29, 4, 0, 2, notifyNewMail, 0, 1})

	newMail := func(folder, message uint64) []byte {
		wire := binary.LittleEndian.AppendUint64([]byte{ropTransportNewMail, 4, 0}, message)
		wire = binary.LittleEndian.AppendUint64(wire, folder)
		return append32(append(wire, "IPM.Note\x00"...), 0x80)
	}
	check := func(wire []byte, expected uint32) {
		t.Helper()
		response, _ := f.run(wire)
		if want := ropError(wire[0], wire[2], expected); !bytes.Equal(response, want) {
			t.Fatalf("response %x, want %x", response, want)
		}
	}
	wire := newMail(fid, mid)
	for size := 1; size < len(wire); size++ {
		if _, err := (ROPBuffer{Operations: wire[:size], Handles: f.handles}).DecodeOperations(); err == nil {
			t.Fatalf("truncated TransportNewMail at %d", size)
		}
	}
	check(wire, accessDenied)
	check([]byte{ropSetSpooler, 5, 0}, accessDenied)
	check([]byte{ropSetSpooler, 4, 1}, NotSupported)
	check([]byte{ropSetSpooler, 4, 0}, 0)
	check(newMail(f.fids[SentItems], mid), PropertyNotFound)
	check(newMail(fid, 0), PropertyNotFound)
	box.softDeleted = true
	check(wire, PropertyNotFound)
	box.softDeleted = false
	want := append32(append(ropError(ropTransportNewMail, 0, 0), 0x2a), f.handles[2])
	want = append(want, 4, notifyNewMail, 0x80)
	want = binary.LittleEndian.AppendUint64(want, fid)
	want = binary.LittleEndian.AppendUint64(want, mid)
	want = append32(want, 0x80)
	want = append(append(want, 0), "IPM.Note\x00"...)
	if got, _ := f.run(wire); !bytes.Equal(got, want) {
		t.Fatalf("NewMail response %x, want %x", got, want)
	}
	if got, _ := f.run(nil); len(got) != 0 {
		t.Fatalf("replayed NewMail: %x", got)
	}
	if box.envelopeReads != 2 {
		t.Fatalf("unexpected native envelope reads %d", box.envelopeReads)
	}
	binding := f.state.mailbox.objects[f.handles[0]].binding
	handle, err := f.state.mailbox.addLogon(4, binding)
	if err != nil {
		t.Fatal(err)
	}
	f.handles[0] = handle
	check(wire, accessDenied)
	if _, _, err := store.localNotificationPage(1); err != nil {
		t.Fatalf("read journal: %v", err)
	}
	if events, _, _ := store.localNotificationPage(0); len(events) != 1 || events[0].flag != notifyNewMail|notifyMessage {
		t.Fatalf("journal had unexpected notification(s): %v", events)
	}
}
