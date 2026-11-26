// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func qualifyNativeDraftCopyProperties(ctx context.Context, t *testing.T, f *ostFixture, verifier core.Mailbox, source core.Object, label string, folder byte) {
	t.Helper()
	box := f.session.Mailbox()
	destinationLabel := label + " destination"
	raw := []byte("Subject: " + destinationLabel + "\r\nContent-Type: text/plain; charset=UTF-8\r\n\r\nOriginal destination body.")
	destination, err := box.(core.MailDraftWriter).SaveMailDraft(ctx, "", 0, raw)
	if err != nil {
		t.Fatal("create synthetic destination draft:", err)
	}
	defer func() {
		cleanup, stop := context.WithTimeout(context.Background(), 10*time.Second)
		defer stop()
		current, err := box.Get(cleanup, destination.ID)
		if err == nil && current.Properties[core.Subject] == core.Text(destinationLabel) {
			_, err = box.(core.MailMessagePlacementWriter).TrashMailMessage(cleanup, current.ID, current.Revision)
		} else if err == nil {
			err = core.ErrConflict
		}
		if err != nil {
			t.Error("could not move synthetic destination to Trash:", err)
		}
	}()

	read := func(item core.Object) core.Object {
		t.Helper()
		got, err := verifier.(core.MailMessageMIMEReader).ReadMailMessageMIME(ctx, item.Parent, item.ID, false)
		if err != nil {
			t.Fatal("read synthetic CopyProperties fixture:", err)
		}
		return got
	}
	originalSource, originalTarget := read(source), read(destination)
	object := f.state.mailbox.objects[f.handles[folder]]
	snapshot, err := object.binding.store.observeMailMessages(ctx, f.session, object.fid)
	if err != nil {
		t.Fatal("observe synthetic CopyProperties fixtures:", err)
	}
	ids := map[core.ID]uint64{}
	for _, row := range snapshot.messages {
		ids[row.object.ID] = row.mid
	}
	if ids[source.ID] == 0 || ids[destination.ID] == 0 {
		t.Fatal("synthetic CopyProperties draft was not observed")
	}

	const src, dst, reply = 2, 3, 4
	faiRunOK(t, f, openMessageOperation(4, folder, src, object.fid, ids[source.ID], 0x0fff, 1))
	faiRunOK(t, f, openMessageOperation(4, folder, dst, object.fid, ids[destination.ID], 0x0fff, 1))
	defer func() {
		faiRunOK(t, f, []byte{1, 4, dst, 1, 4, src})
	}()
	const plain, files = 0x1000001f, 0x0e13000d
	got := faiRunOK(t, f, copyDraftPropertiesWire(4, src, dst, 0, 0, plain, files))
	if !bytes.Equal(got, append(ropError(0x67, src, 0), 0, 0)) {
		t.Fatal("CopyProperties returned an error for a native text or attachment property")
	}
	if staged := read(destination); staged.Revision != originalTarget.Revision || !bytes.Equal(staged.MIME, originalTarget.MIME) {
		t.Fatal("CopyProperties wrote to Zimbra before SaveChangesMessage")
	}
	faiRunOK(t, f, []byte{0x0c, 4, reply, dst, 1})
	saved := read(destination)
	payload, err := projectMailContents(saved)
	if err != nil || len(payload.attachments) != 2 || saved.Revision <= originalTarget.Revision ||
		saved.Properties[core.Subject] != core.Text(destinationLabel) || payload.values[fixedContentsProperty(plain)] != core.Text("Synthetic draft.") {
		t.Fatal("SaveChangesMessage failed to persist the selected properties:", err)
	}
	for i, want := range [][]byte{{0, 1, 2}, {0xff, 0xff, 0}} {
		data, ok := payload.attachments[i][fixedContentsProperty(attachDataBinary)].(core.Binary)
		if !ok || !bytes.Equal(data, want) {
			t.Fatal("CopyProperties changed native by-value attachment bytes")
		}
	}
	if current := read(source); current.Revision != originalSource.Revision || !bytes.Equal(current.MIME, originalSource.MIME) {
		t.Fatal("CopyProperties changed the native source draft")
	}
	t.Log("CopyProperties preserved the source and committed the selected body and attachment bytes on SaveChangesMessage")
}
