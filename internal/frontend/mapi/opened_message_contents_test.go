// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"slices"
	"strings"
	"testing"

	"ewsgo/internal/core"
)

func TestOpenedMessageBodyThroughExecute(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &attachmentFixtureMailbox{openMessageFixtureMailbox: &openMessageFixtureMailbox{messageFixtureMailbox: base}}
	f.session = &testSession{box}
	f.state.codePage = 1252
	object := &box.objects[0]
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	fid := f.fids[Inbox]
	snapshot, err := store.observeMailMessages(t.Context(), f.session, fid)
	if err != nil || len(snapshot.messages) != 1 {
		t.Fatal("fixture unavailable", err)
	}
	open := openMessageOperation(4, 1, 2, fid, snapshot.messages[0].mid, 0x0fff, 0)

	const body, html, rtf = 0x1000001f, 0x10130102, 0x10090102
	const plain = "Café"
	const markup = "<p>Café</p>"
	object.MIME = []byte("Subject: synthetic\r\nContent-Type: multipart/alternative; boundary=x\r\n\r\n" +
		"--x\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + plain +
		"\r\n--x\r\nContent-Type: text/html; charset=utf-8\r\n\r\n" + markup + "\r\n--x--\r\n")
	object.Properties[core.IsRead] = core.Bool(true)
	object.Properties[core.HasAttachments] = core.Bool(false)
	object.Properties[core.MessageSize] = core.Integer(len(object.MIME))
	faiRunOK(t, f, open)
	faiRunOK(t, f, folderPropertiesOperation(4, 2, 1, 0x0037001f))
	got := faiRunOK(t, f, folderPropertiesOperation(4, 2, 1,
		0x0e070003, 0x0e170003, 0x0e69000b, 0x0e1b000b, 0x0e080003, folderEntryIDTag, parentFolderEntryIDTag,
		0x0ff90102, 0x0ffa0102, 0x65e00102, 0x65e20102, 0x0ffe0003))
	binding := f.state.mailbox.objects[f.handles[0]].binding
	want := append32(append(ropError(7, 2, 0), 0), 1)
	want = append32(want, 0)
	want = append32(append(want, 1, 0), uint32(len(object.MIME)))
	for _, entry := range [][]byte{
		marshalMessageEntryID(binding.mailboxGUID, store.replica, fid, snapshot.messages[0].mid),
		marshalFolderEntryID(binding.mailboxGUID, store.replica, fid),
		marshalFolderEntryID(binding.mailboxGUID, store.replica, snapshot.messages[0].mid),
		binding.mailboxGUID[:],
		snapshot.messages[0].sourceKey[:],
		[]byte(snapshot.messages[0].changeKey),
	} {
		want = append(binary.LittleEndian.AppendUint16(want, uint16(len(entry))), entry...)
	}
	want = append32(want, 5)
	if !bytes.Equal(got, want) {
		t.Fatal("opened message metadata did not match its native envelope and identifiers")
	}
	object.Revision++
	got = faiRunOK(t, f, folderPropertiesOperation(4, 2, 1, 0x65e20102))
	want = append32(append(ropError(7, 2, 0), 1, 0x0a), PropertyNotFound)
	if !bytes.Equal(got, want) {
		t.Fatal("an old change key was exposed after the native revision changed")
	}
	object.Revision--
	if box.mimeReads != 0 {
		t.Fatal("envelope lookup read MIME")
	}
	got = faiRunOK(t, f, folderPropertiesOperation(4, 2, 1, body, html, messageNativeBody, messageInternetCP, rtf))
	textBody := []byte{'C', 0, 'a', 0, 'f', 0, 0xe9, 0, 0, 0}
	want = append(ropError(7, 2, 0), 1, 0)
	want = append(want, textBody...)
	want = binary.LittleEndian.AppendUint16(append(want, 0), uint16(len(markup)))
	want = append(want, markup...)
	want = append32(append(want, 0), 3)
	want = append32(append(want, 0), 65001)
	want = append32(append(want, 0x0a), PropertyNotFound)
	if !bytes.Equal(got, want) || box.mimeReads != 1 {
		t.Fatal("mixed property row did not project the original body formats")
	}
	for _, stream := range []struct {
		tag  uint32
		data []byte
	}{
		{body, textBody},
		{0x1000001e, []byte{'C', 'a', 'f', 0xe9, 0}},
		{html, []byte(markup)},
	} {
		got := faiRunOK(t, f, streamOpenWire(2, 3, 0, stream.tag))
		if !bytes.Equal(got, append32(ropError(0x2b, 3, 0), uint32(len(stream.data)))) ||
			!bytes.Equal(decodeStreamRead(t, faiRunOK(t, f, streamReadWire(3, 100)), 3), stream.data) {
			t.Fatal("stream bytes differed from MIME")
		}
		faiRunOK(t, f, []byte{1, 4, 3})
	}

	// Change the backend revision and reopen so the next two reads use the
	// same long, plain-only body. The line breaks are part of the fixture.
	object.Properties[core.ContentRevision] = object.Properties[core.ContentRevision].(core.Integer) + 1
	longPlain := strings.Repeat(strings.Repeat("x", 500)+"\r\n", 70)
	object.MIME = []byte("Subject: long synthetic\r\nContent-Type: text/plain; charset=utf-8\r\n\r\n" + longPlain)
	faiRunOK(t, f, open)
	got = faiRunOK(t, f, folderPropertiesOperation(4, 2, 1, body, html, messageNativeBody))
	want = append32(append(ropError(7, 2, 0), 1, 0x0a), 0x8007000e)
	want = append32(append(want, 0x0a), PropertyNotFound)
	want = append32(append(want, 0), 1)
	if !bytes.Equal(got, want) {
		t.Fatal("large body failed the row or returned the wrong property status")
	}
	got, _ = f.run(streamOpenWire(2, 3, 0, html))
	if !bytes.Equal(got, ropError(0x2b, 3, PropertyNotFound)) {
		t.Fatal("absent HTML opened a stream")
	}
	got = faiRunOK(t, f, streamOpenWire(2, 3, 0, body))
	expected := contentsUnicode(longPlain)
	if !bytes.Equal(got, append32(ropError(0x2b, 3, 0), uint32(len(expected)))) {
		t.Fatal("large body stream length")
	}
	read := streamReadWire(3, 15000)
	var data []byte
	before := box.mimeReads
	for {
		chunk := decodeStreamRead(t, faiRunOK(t, f, read), 3)
		if len(chunk) == 0 {
			break
		}
		if len(data)+len(chunk) > len(expected) {
			t.Fatal("stream returned data beyond the declared size")
		}
		data = append(data, chunk...)
	}
	if !bytes.Equal(data, expected) || box.mimeReads != before {
		t.Fatal("chunked body did not reproduce the original content")
	}
	tags, rest := listedPropertyTags(t, faiRunOK(t, f, []byte{0x09, 4, 2}), 0x09, 2)
	if len(rest) != 0 || !slices.Contains(tags, uint32(body)) || !slices.Contains(tags, uint32(folderEntryIDTag)) || slices.Contains(tags, uint32(html)) {
		t.Fatal("property list did not describe the native message formats")
	}
	object.Properties[core.ContentRevision] = object.Properties[core.ContentRevision].(core.Integer) + 1
	got, _ = f.run(read)
	if !bytes.Equal(got, append(ropError(0x2c, 3, 0x80040109), 0, 0)) {
		t.Fatal("stale stream remained readable")
	}
}

func TestNativeMailTableServerID(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	openTableTest(t, f, 0)
	tags := []uint32{instanceServerIDTag, 0x674a0014, folderEntryIDTag, parentFolderEntryIDTag, 0x0ff60102}
	faiRunOK(t, f, tableColumnsWire(2, tags...))
	rows := decodeTableRows(t, faiRunOK(t, f, tableQueryWire(2, 0, 1, 1)), tags)
	if len(rows) != 1 {
		t.Fatal("missing fixture row")
	}
	row := rows[0]
	fid, mid := f.fids[Inbox], row[1].(uint64)
	server := row[0].([]byte)
	if len(server) != 21 || server[0] != 1 || binary.LittleEndian.Uint64(server[1:]) != fid ||
		binary.LittleEndian.Uint64(server[9:]) != mid || binary.LittleEndian.Uint32(server[17:]) != 0 {
		t.Fatal("row ServerId did not identify the actual message and its parent")
	}
	binding := f.state.mailbox.objects[f.handles[0]].binding
	if !bytes.Equal(row[2].([]byte), marshalMessageEntryID(binding.mailboxGUID, binding.store.replica, fid, mid)) ||
		!bytes.Equal(row[3].([]byte), marshalFolderEntryID(binding.mailboxGUID, binding.store.replica, fid)) ||
		row[4] != uint32(PropertyNotFound) {
		t.Fatal("row EntryIDs or the distinct InstanceKey did not match the reference")
	}
}
