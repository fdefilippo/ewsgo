// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"testing"

	"ewsgo/internal/core"
)

func readRecipientsOperation(logon, input byte, rowID uint32) []byte {
	return append(append32([]byte{0x0f, logon, input}, rowID), 0, 0)
}

func TestReadRecipientsThroughOpenedMessage(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &openMessageFixtureMailbox{messageFixtureMailbox: base}
	f.session = &testSession{box}
	message := &box.objects[0]
	message.Recipients = []core.Recipient{
		{Kind: core.RecipientTo, Address: "a@b"},
		{Kind: core.RecipientCc, Address: "c@d", DisplayName: "É"},
		{Kind: core.RecipientBcc, Address: "e@f"},
	}
	fid := f.fids[Inbox]
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	snapshot, err := store.observeMailMessages(t.Context(), f.session, fid)
	if err != nil || len(snapshot.messages) != 1 {
		t.Fatal("message fixture unavailable")
	}
	open := openMessageOperation(4, 1, 2, fid, snapshot.messages[0].mid, 0x0fff, 0)
	read := readRecipientsOperation(4, 2, 0)
	malformed := append(bytes.Clone(open), read...)
	malformed[len(malformed)-1] = 1
	body, _ := f.execute(malformed, 65535)
	if !bytes.Equal(body, executeError(0x80070057, 65535)) || box.envelopeReads != 0 {
		t.Fatal("invalid reserved field executed a valid prefix")
	}
	faiRunOK(t, f, open)
	retained := store.allocator.contents.used
	row0 := []byte{0, 0, 0, 0, 1, 0xb0, 4, 0, 0, 13, 0,
		0x0b, 2, 'a', 0, '@', 0, 'b', 0, 0, 0, 0, 0, 0}
	row1 := []byte{1, 0, 0, 0, 2, 0xb0, 4, 0, 0, 17, 0,
		0x1b, 2, 'c', 0, '@', 0, 'd', 0, 0, 0, 0xc9, 0, 0, 0, 0, 0, 0}
	row2 := []byte{2, 0, 0, 0, 3, 0xb0, 4, 0, 0, 13, 0,
		0x0b, 2, 'e', 0, '@', 0, 'f', 0, 0, 0, 0, 0, 0}
	maximum := uint32(10 + 4*len(f.handles) + readRecipientsMinimum + len(row0))
	body, _ = f.execute(read, maximum-1)
	if got := storeResponse(t, body).Operations; !bytes.Equal(got, ropError(0x0f, 2, tableBufferTooSmall)) {
		t.Fatal("a short buffer returned a partial row")
	}
	body, _ = f.execute(read, maximum)
	if got := storeResponse(t, body).Operations; !bytes.Equal(got, append(append(ropError(0x0f, 2, 0), 1), row0...)) {
		t.Fatal("first page did not contain exactly the first recipient")
	}
	if got := faiRunOK(t, f, readRecipientsOperation(4, 2, 1)); !bytes.Equal(got, append(append(append(ropError(0x0f, 2, 0), 2), row1...), row2...)) {
		t.Fatal("next page changed RowId, Unicode, recipient type or order")
	}
	for _, tc := range []struct {
		wire []byte
		code uint32
	}{
		{readRecipientsOperation(4, 2, 3), PropertyNotFound},
		{readRecipientsOperation(4, 2, 0xffffffff), PropertyNotFound},
		{readRecipientsOperation(4, 1, 0), nullObject},
		{readRecipientsOperation(5, 2, 0), accessDenied},
	} {
		got, _ := f.run(tc.wire)
		if !bytes.Equal(got, ropError(0x0f, tc.wire[2], tc.code)) {
			t.Fatalf("recipient error response %x, want code %x", got, tc.code)
		}
	}
	message.Recipients[0], message.Recipients[1] = message.Recipients[1], message.Recipients[0]
	if got, _ := f.run(read); !bytes.Equal(got, ropError(0x0f, 2, recipientListChanged)) {
		t.Fatal("an open handle returned RowIds from a different list")
	}
	faiRunOK(t, f, []byte{1, 4, 2})
	if got, _ := f.run(read); !bytes.Equal(got, ropError(0x0f, 2, invalidObject)) || store.allocator.contents.used != retained {
		t.Fatal("release left the recipient handle readable or reserved memory")
	}
	message.Recipients = nil
	faiRunOK(t, f, open)
	if got, _ := f.run(read); !bytes.Equal(got, ropError(0x0f, 2, PropertyNotFound)) {
		t.Fatal("recipient read on an empty list did not return NotFound")
	}
}
