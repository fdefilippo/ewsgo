// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"ewsgo/internal/core"
)

func TestOutlookPrivateConfigurationRoundTrip(t *testing.T) {
	f, box, store := newFAIExecuteFixture(t)
	f.transport.EnableContentsTableSpike()
	ctx := context.Background()
	before := faiRunOK(t, f, getSpecificWire(1, freeBusyEntryIDsTag))
	if len(before) != 12 || binary.LittleEndian.Uint32(before[8:]) != PropertyNotFound {
		t.Fatal("private configuration appeared without opt-in")
	}
	f.transport.EnablePrivateOutlookConfiguration()
	store.privateConfig = true
	readLink := func(slot byte, withMessage bool) (uint64, []byte) {
		t.Helper()
		object, _ := f.state.mailbox.lookup(f.handles[slot], 4)
		out := faiRunOK(t, f, getSpecificWire(slot, freeBusyEntryIDsTag))
		wantMessage := 0
		if withMessage {
			wantMessage = messageEntryIDBytes
		}
		if len(out) != 19+wantMessage+folderEntryIDBytes || out[6] != 0 || binary.LittleEndian.Uint32(out[7:]) != 4 ||
			binary.LittleEndian.Uint16(out[11:]) != 0 || int(binary.LittleEndian.Uint16(out[13:])) != wantMessage ||
			binary.LittleEndian.Uint16(out[15+wantMessage:]) != 0 || binary.LittleEndian.Uint16(out[17+wantMessage:]) != folderEntryIDBytes {
			t.Fatalf("unexpected Freebusy EntryID array of %d bytes", len(out))
		}
		fid, err := parseFolderEntryID(out[19+wantMessage:], object.binding.mailboxGUID, store.replica)
		if err != nil {
			t.Fatal(err)
		}
		return fid, out[15 : 15+wantMessage]
	}
	freeBusyFID, _ := readLink(1, false)
	faiRunOK(t, f, folderOperation(f.fids[MailboxRoot], 4, 0, 2))
	rootFID, _ := readLink(2, false)
	if freeBusyFID != rootFID {
		t.Fatal("Root and Inbox resolved different local folders")
	}
	faiRunOK(t, f, folderOperation(freeBusyFID, 4, 0, 2))
	values := faiSetRequest{
		{0x001a001f, localFreeBusyClass},
		{0x0037001f, localFreeBusySubject},
		{delegatorWantsCopyTag, core.Bool(true)},
		{delegatorWantsInfoTag, core.Bool(false)},
	}
	batch := append(createAssociatedWire(2, 3, freeBusyFID, 0), faiSetOperation(3, values)...)
	out := faiRunOK(t, f, append(batch, 0x0c, 4, 2, 3, 0x0e))
	if len(out) != 38 || binary.LittleEndian.Uint16(out[21:]) != 0 {
		t.Fatal("unexpected Create/Set/Save response")
	}
	mid := binary.LittleEndian.Uint64(out[7:])
	if mid == 0 || binary.LittleEndian.Uint64(out[30:]) != mid {
		t.Fatal("local configuration save changed the reserved MID")
	}
	_, link := readLink(1, true)
	object, _ := f.state.mailbox.lookup(f.handles[1], 4)
	if !bytes.Equal(link, marshalMessageEntryID(object.binding.mailboxGUID, store.replica, freeBusyFID, mid)) {
		t.Fatal("Inbox link does not reference the saved object")
	}
	setLink := func(slot byte, data []byte) []byte {
		t.Helper()
		wire := binary.LittleEndian.AppendUint16([]byte{0x0a, 4, slot}, uint16(6+len(data)))
		wire = binary.LittleEndian.AppendUint16(wire, 1)
		wire = binary.LittleEndian.AppendUint32(wire, freeBusyEntryIDsTag)
		return faiRunOK(t, f, append(wire, data...))
	}
	faiRunOK(t, f, folderOperation(f.fids[MailboxRoot], 4, 0, 4))
	reference := marshalFreeBusyEntryIDs(object.binding, freeBusyFID, mid)
	for _, slot := range []byte{1, 4} {
		if result := setLink(slot, reference); len(result) != 8 || binary.LittleEndian.Uint16(result[6:]) != 0 {
			t.Fatal("canonical Freebusy links were rejected")
		}
	}
	foreign := bytes.Clone(reference)
	foreign[13] ^= 1
	for _, attempt := range []struct {
		slot byte
		data []byte
	}{{1, foreign}, {2, reference}} {
		result := setLink(attempt.slot, attempt.data)
		if len(result) != 18 || binary.LittleEndian.Uint32(result[10:]) != freeBusyEntryIDsTag || binary.LittleEndian.Uint32(result[14:]) != accessDenied {
			t.Fatal("an invalid Freebusy link or source folder was accepted")
		}
	}
	if _, reread := readLink(4, true); !bytes.Equal(reread, link) {
		t.Fatal("rejected link write changed the computed Root reference")
	}
	faiRunOK(t, f, []byte{1, 4, 3})
	faiRunOK(t, f, openMessageOperation(4, 2, 3, freeBusyFID, mid, 1252, 1))
	out = faiRunOK(t, f, getSpecificWire(3, 0x001a001f, delegatorWantsCopyTag, delegatorWantsInfoTag, 0x67aa000b))
	want, _ := propertyString(string(localFreeBusyClass), 0x001f, 1252)
	if !bytes.Equal(out[6:], append(append([]byte{0}, want...), 1, 0, 0)) {
		t.Fatal("private configuration values were not retained")
	}
	for _, flags := range []byte{0, 2} {
		wantCount := uint32(1)
		if flags == 2 {
			wantCount = 0
		}
		table := faiRunOK(t, f, []byte{5, 4, 2, 3, flags})
		if len(table) != 10 || binary.LittleEndian.Uint32(table[6:]) != wantCount {
			t.Fatal("configuration appeared in the wrong contents table")
		}
	}
	if _, err := store.readOpenedMessage(ctx, f.session, f.fids[Inbox], openedMessage{mid: mid}); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("private configuration opened through the Inbox")
	}
	saved := store.freeBusyMessage()
	first, err := store.editFreeBusyMessage(saved)
	if err != nil {
		t.Fatal(err)
	}
	defer first.close()
	stale, err := store.editFreeBusyMessage(saved)
	if err != nil {
		t.Fatal(err)
	}
	defer stale.close()
	markInfo := faiSetRequest{{delegatorWantsInfoTag, core.Bool(true)}}
	if problems, err := first.set(ctx, f.session, markInfo); err != nil || len(problems) != 0 {
		t.Fatalf("set local preference: %v %v", err, problems)
	}
	if row, err := first.save(ctx, f.session, 0x0e); err != nil || row.mid != mid || row.cn <= saved.cn {
		t.Fatalf("edit did not preserve the MID and advance the change number: %v", err)
	}
	if _, err := stale.set(ctx, f.session, markInfo); err != nil {
		t.Fatal(err)
	}
	if _, err := stale.save(ctx, f.session, 0x0e); !errors.Is(err, core.ErrConflict) {
		t.Fatal("stale private configuration overwrote a committed revision")
	}
	if box.sourceCalls != 0 || len(store.fai.records) != 0 {
		t.Fatal("private configuration accessed native contents or the associated namespace")
	}
}
