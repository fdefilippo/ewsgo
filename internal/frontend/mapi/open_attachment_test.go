// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"testing"

	"ewsgo/internal/core"
)

func attachmentOpenWire(logon, input, output, mode byte, number uint32) []byte {
	return append32([]byte{0x22, logon, input, output, mode}, number)
}

type attachmentFixtureMailbox struct {
	*openMessageFixtureMailbox
	mimeReads int
}

func (b *attachmentFixtureMailbox) ReadMailMessageMIME(_ context.Context, folder, id core.ID, includeSoftDeleted bool) (core.Object, error) {
	b.mimeReads++
	for _, object := range b.objects {
		if object.ID == id && object.Parent == folder && (!b.softDeleted || includeSoftDeleted) {
			return object, nil
		}
	}
	return core.Object{}, core.ErrNotFound
}

const attachmentFixtureMIME = "Subject: synthetic\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n" +
	"--x\r\nContent-Type: text/plain\r\n\r\nbody\r\n" +
	"--x\r\nContent-Type: text/plain\r\nContent-Disposition: attachment; filename=first.txt\r\n\r\nfirst\r\n" +
	"--x\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename*=UTF-8''%C3%A9.bin\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\nAH+A/w==\r\n--x--\r\n"

func TestOpenedAttachmentsThroughExecute(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &attachmentFixtureMailbox{openMessageFixtureMailbox: &openMessageFixtureMailbox{messageFixtureMailbox: base}}
	f.session = &testSession{box}
	f.state.codePage = 1252
	message := &box.objects[0]
	withoutAttachments := message.MIME
	message.MIME = []byte(attachmentFixtureMIME)
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	fid := f.fids[Inbox]
	snapshot, err := store.observeMailMessages(t.Context(), f.session, fid)
	if err != nil || len(snapshot.messages) != 1 {
		t.Fatal("fixture unavailable", err)
	}
	open := openMessageOperation(4, 1, 2, fid, snapshot.messages[0].mid, 0x0fff, 0)
	list := []byte{0x52, 4, 2}
	attach := attachmentOpenWire(4, 2, 3, 3, 1)
	properties := folderPropertiesOperation(4, 3, 1, 0x0e210003, 0x3707001f, 0x37010102, 0x37050003, 0x0ffe0003)
	// Invalid suffix validates the whole batch before OpenMessage can read.
	body, _ := f.execute(append(bytes.Clone(open), attachmentOpenWire(4, 2, 255, 0, 1)...), 65535)
	if !bytes.Equal(body, executeError(0x80070057, 65535)) || box.envelopeReads != 0 {
		t.Fatal("malformed open attachment executed the batch prefix")
	}
	faiRunOK(t, f, open)
	before := store.allocator.contents.used
	wantIDs := append(ropError(0x52, 2, 0), 2, 0, 0, 0, 0, 0, 1, 0, 0, 0)
	maxOutput := uint32(10 + 4*len(f.handles) + len(wantIDs))
	body, _ = f.execute(list, maxOutput-1)
	if got := storeResponse(t, body).Operations; !bytes.Equal(got, ropError(0x52, 2, tableBufferTooSmall)) {
		t.Fatal("short output did not reject the entire attachment list")
	}
	body, _ = f.execute(list, maxOutput)
	if got := storeResponse(t, body).Operations; !bytes.Equal(got, wantIDs) {
		t.Fatal("attachment numbers differ from MIME order", got)
	}
	beforeReads := box.mimeReads
	for _, tc := range []struct {
		wire []byte
		code uint32
	}{
		{attachmentOpenWire(4, 1, 3, 0, 0), nullObject},
		{attachmentOpenWire(5, 2, 3, 0, 0), accessDenied},
		{attachmentOpenWire(4, 2, 3, 1, 0), accessDenied},
		{attachmentOpenWire(4, 2, 3, 2, 0), 0x80070057},
	} {
		got, _ := f.run(tc.wire)
		if !bytes.Equal(got, ropError(0x22, 3, tc.code)) || box.mimeReads != beforeReads {
			t.Fatal("invalid open reached MIME or returned wrong status", got)
		}
	}
	got, _ := f.run(attachmentOpenWire(4, 2, 3, 0, 2))
	if !bytes.Equal(got, ropError(0x22, 3, PropertyNotFound)) {
		t.Fatal("out-of-range attachment was opened", got)
	}
	got = faiRunOK(t, f, append(bytes.Clone(attach), properties...))
	// On a by-value attachment, the absent embedded-message request
	// preserves the output slot and the remaining batch.
	embedded := []byte{ropOpenEmbeddedMessage, 4, 3, 1, 0xe4, 4, 0}
	saved := f.handles[1]
	next := []byte{ropTellVersion, 4, 0, 1, 0, 0, 0, 0, 0}
	embeddedReply, result := f.run(append(embedded, next...))
	expected := append(ropError(ropOpenEmbeddedMessage, 1, PropertyNotFound), ropError(ropTellVersion, 0, 0)...)
	if !bytes.Equal(embeddedReply, expected) || result.errors != 1 || f.handles[1] != saved {
		t.Fatalf("OpenEmbeddedMessage changed an existing handle or the batch: %x", embeddedReply)
	}
	want := append(ropError(0x22, 3, 0), ropError(0x07, 3, 0)...)
	want = append(want, 0, 1, 0, 0, 0, 0xe9, 0, '.', 0, 'b', 0, 'i', 0, 'n', 0, 0, 0,
		4, 0, 0, 0x7f, 0x80, 0xff, 1, 0, 0, 0, 7, 0, 0, 0)
	if !bytes.Equal(got, want) {
		t.Fatalf("attachment properties wire: got %x want %x", got, want)
	}
	short := folderPropertiesOperation(4, 3, 1, 0x37010102)
	body, _ = f.execute(short, uint32(10+4*len(f.handles)+12))
	propertyError := append32(append(ropError(7, 3, 0), 1, 0x0a), 0x8007000e)
	if got := storeResponse(t, body).Operations; !bytes.Equal(got, propertyError) {
		t.Fatal("short property output contained a partial blob", got)
	}
	faiRunOK(t, f, []byte{1, 4, 2})
	if got := faiRunOK(t, f, properties); !bytes.Equal(got, want[len(ropError(0x22, 3, 0)):]) {
		t.Fatal("releasing the Message invalidated an independent Attachment handle")
	}
	faiRunOK(t, f, open)
	message.MIME = withoutAttachments
	message.Properties[core.ContentRevision] = message.Properties[core.ContentRevision].(core.Integer) + 1
	for _, wire := range [][]byte{list, properties} {
		got, _ := f.run(wire)
		if !bytes.Equal(got, ropError(wire[0], wire[2], 0x80040109)) {
			t.Fatal("stale ContentRevision read an attachment", got)
		}
	}
	faiRunOK(t, f, []byte{1, 4, 3})
	if got, _ := f.run(properties); !bytes.Equal(got, ropError(7, 3, invalidObject)) {
		t.Fatal("released Attachment was used", got)
	}
	faiRunOK(t, f, open)
	if got := faiRunOK(t, f, list); !bytes.Equal(got, append(ropError(0x52, 2, 0), 0, 0)) {
		t.Fatal("attachment-free message did not return an empty list", got)
	}
	if store.allocator.contents.used != before {
		t.Fatal("attachment operations retained memory")
	}
}
