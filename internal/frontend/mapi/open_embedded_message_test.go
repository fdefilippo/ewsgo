// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"testing"

	"ewsgo/internal/core"
)

func TestOpenEmbeddedMessageThroughExecute(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &attachmentFixtureMailbox{openMessageFixtureMailbox: &openMessageFixtureMailbox{messageFixtureMailbox: base}}
	f.session = &testSession{box}
	f.state.codePage = 1252
	f.handles = append(f.handles, 0xffffffff, 0xffffffff, 0xffffffff)
	message := &box.objects[0]
	message.MIME = []byte(embeddedMailFixture)
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	fid := f.fids[Inbox]
	snapshot, err := store.observeMailMessages(t.Context(), f.session, fid)
	if err != nil || len(snapshot.messages) != 1 {
		t.Fatal("fixture unavailable", err)
	}
	rootMID := snapshot.messages[0].mid
	outer := openMessageOperation(4, 1, 2, fid, rootMID, 0x0fff, 0)
	faiRunOK(t, f, outer)
	faiRunOK(t, f, attachmentOpenWire(4, 2, 3, 0, 0))
	open := []byte{ropOpenEmbeddedMessage, 4, 3, 4, 0xff, 0x0f, 0}
	reads := box.mimeReads
	for _, test := range []struct {
		input, mode byte
		cp          uint16
		code        uint32
	}{
		{2, 0, 1252, nullObject},
		{3, 0, 0xffff, 0x03ef},
		{3, 1, 1252, accessDenied},
		{3, 2, 1252, accessDenied},
		{3, 3, 1252, 0x80070057},
	} {
		wire := bytes.Clone(open)
		wire[2], wire[6] = test.input, test.mode
		binary.LittleEndian.PutUint16(wire[4:], test.cp)
		got, _ := f.run(wire)
		if !bytes.Equal(got, ropError(open[0], 4, test.code)) || box.mimeReads != reads || f.handles[4] != 0xffffffff {
			t.Fatalf("rejected open changed state or returned a different error: %x", got)
		}
	}
	body, _ := f.execute(open, uint32(10+4*len(f.handles)+openMessageMinimum+embeddedResponseFields))
	if got := storeResponse(t, body).Operations; !bytes.Equal(got, ropError(open[0], 4, tableBufferTooSmall)) || len(store.embeddedMIDs) != 0 {
		t.Fatal("short response allocated an embedded ID or returned a partial header", got)
	}
	props := folderPropertiesOperation(4, 4, 1, 0x0037001f, 0x1000001f)
	reply := faiRunOK(t, f, append(bytes.Clone(open), props...))
	mid := binary.LittleEndian.Uint64(reply[7:15])
	if mid == 0 || mid == rootMID || reply[6] != 0 || reply[15] != 0 ||
		!bytes.HasPrefix(reply[16:], append([]byte{1, 4}, contentsUnicode("Attached subject")...)) ||
		!bytes.Contains(reply[16:], contentsUnicode("inner@example.invalid")) {
		t.Fatalf("incorrect embedded open response: %x", reply)
	}
	wantProps := append(ropError(7, 4, 0), 0)
	wantProps = append(wantProps, contentsUnicode("Attached subject")...)
	wantProps = append(wantProps, contentsUnicode("Attached body")...)
	if !bytes.HasSuffix(reply, wantProps) {
		t.Fatalf("properties did not resolve to the embedded handle in the batch: %x", reply)
	}
	if got := faiRunOK(t, f, readRecipientsOperation(4, 4, 0)); got[6] != 1 || !bytes.Contains(got, contentsUnicode("inner@example.invalid")) {
		t.Fatal("embedded recipient missing")
	}
	faiRunOK(t, f, attachmentOpenWire(4, 4, 5, 0, 0))
	data := folderPropertiesOperation(4, 5, 1, 0x37010102)
	if got := faiRunOK(t, f, data); !bytes.Equal(got, append(ropError(7, 5, 0), 0, 3, 0, 1, 2, 3)) {
		t.Fatal("inner attachment data differs", got)
	}
	bodyStream := streamOpenWire(4, 6, 0, 0x1000001f)
	faiRunOK(t, f, bodyStream)
	if got := decodeStreamRead(t, faiRunOK(t, f, streamReadWire(6, 128)), 6); !bytes.Equal(got, contentsUnicode("Attached body")) {
		t.Fatal("embedded body stream differs")
	}
	getStandalone := openMessageOperation(4, 1, 6, fid, mid, 0x0fff, 0)
	if got, _ := f.run(getStandalone); !bytes.Equal(got, ropError(3, 6, PropertyNotFound)) {
		t.Fatal("embedded ID became a standalone mailbox message", got)
	}

	faiRunOK(t, f, []byte{1, 4, 4})
	if again := faiRunOK(t, f, open); binary.LittleEndian.Uint64(again[7:15]) != mid {
		t.Fatal("same MIME generation changed MID")
	}
	message.Properties[core.ContentRevision] = message.Properties[core.ContentRevision].(core.Integer) + 1
	for _, wire := range [][]byte{open, props, data, streamReadWire(6, 10)} {
		slot := wire[2]
		if wire[0] == open[0] {
			slot = wire[3]
		}
		want := ropError(wire[0], slot, 0x80040109)
		if wire[0] == 0x2c {
			want = append(want, 0, 0) // ReadStream includes a zero byte count on failure.
		}
		if got, _ := f.run(wire); !bytes.Equal(got, want) {
			t.Fatalf("stale root revision accepted by ROP %#x: %x", wire[0], got)
		}
	}
	faiRunOK(t, f, outer)
	faiRunOK(t, f, attachmentOpenWire(4, 2, 3, 0, 0))
	if fresh := faiRunOK(t, f, open); binary.LittleEndian.Uint64(fresh[7:15]) == mid {
		t.Fatal("a new MIME generation reused an old MID")
	}
}

func TestSQLiteEmbeddedMIDReopens(t *testing.T) {
	f, _ := messageFixture(t)
	reopen := sqliteReplicaFixture(t, f)
	db := reopen()
	ref := embeddedMessageRef{rootMID: folderID(23), path: [maxMailPartDepth]uint32{4, 1}, depth: 2}
	first, err := f.store.bindEmbeddedMessage(t.Context(), ref, 1)
	if err != nil || first == 0 || f.store.byMID[first] != (messageKey{}) {
		t.Fatal("failed to allocate a separate embedded ID", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopen()
	again, err := f.store.bindEmbeddedMessage(t.Context(), ref, 1)
	if err != nil || first != again || len(f.store.embeddedMIDs) != 1 || f.store.byMID[first] != (messageKey{}) {
		t.Fatal("reopening changed the embedded ID or added a standalone message", err)
	}
	ref.depth--
	parent, err := f.store.bindEmbeddedMessage(t.Context(), ref, 1)
	if err != nil || parent == first {
		t.Fatal("different attachment paths share an identifier", err)
	}
}
