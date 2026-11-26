// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"ewsgo/internal/core"
	"ewsgo/internal/mailmime"
)

func copyDraftPropertiesWire(logon, source, target, async, flags byte, tags ...uint32) []byte {
	wire := binary.LittleEndian.AppendUint16([]byte{0x67, logon, source, target, async, flags}, uint16(len(tags)))
	for _, tag := range tags {
		wire = binary.LittleEndian.AppendUint32(wire, tag)
	}
	return wire
}

func TestCopyPropertiesThroughExecute(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableMailDrafts(existingDraftOnly{})
	f.handles = append(f.handles, 0xffffffff)
	objects := f.state.mailbox
	folder := objects.objects[f.handles[1]]
	makeDraft := func(slot byte, mid uint64, subject string, raw []byte) *mailDraft {
		t.Helper()
		object := messageTestObject(core.ID(subject), folder.binding.store.byFID[folder.fid].native)
		object.MIME = raw
		object.Properties[core.Subject] = core.Text(subject)
		object.Properties[core.IsDraft] = core.Bool(true)
		draft := &mailDraft{store: folder.binding.store, fid: folder.fid, mid: mid, folder: object.Parent, staged: object}
		handle, err := objects.allocator.handle()
		if err != nil {
			t.Fatal("unable to allocate draft handle")
		}
		objects.objects[handle] = serverObject{logon: 4, fid: folder.fid, binding: folder.binding, message: &openedMessage{mailDraft: draft}}
		objects.allocated++
		f.handles[slot] = handle
		return draft
	}
	const source, target, missing = 2, 3, 4
	src := makeDraft(source, folderID(100), "synthetic", []byte(attachmentFixtureMIME))
	dst := makeDraft(target, folderID(101), "existing", []byte("Subject: existing\r\nContent-Type: text/plain\r\n\r\noriginal body"))
	srcMIME, dstMIME := bytes.Clone(src.staged.MIME), bytes.Clone(dst.staged.MIME)
	const subject, plain, html, files, sourceKey = 0x0037001e, 0x1000001f, 0x10130102, 0x0e13000d, 0x65e00102
	copyWire := copyDraftPropertiesWire(4, source, target, 1, 1, subject, html, sourceKey)
	body, _ := f.execute(append(bytes.Clone(copyWire), 0x67, 4, source, target, 0, 0, 1, 0), 65535)
	if !bytes.Equal(body, executeError(0x80070057, 65535)) || !bytes.Equal(dst.staged.MIME, dstMIME) {
		t.Fatal("a truncated suffix changed the destination")
	}
	body, _ = f.execute(copyWire, 36)
	if ops := storeResponse(t, body).Operations; len(ops) == 0 || ops[0] != 0xff || dst.dirty {
		t.Fatal("Execute did not reserve space for all reported PropertyProblems")
	}
	for _, tc := range []struct {
		wire []byte
		want []byte
	}{
		{copyDraftPropertiesWire(4, source, missing, 0, 0, subject), append32(ropError(0x67, source, 0x503), missing)},
		{copyDraftPropertiesWire(5, source, target, 0, 0, subject), ropError(0x67, source, accessDenied)},
		{copyDraftPropertiesWire(4, source, 1, 0, 0, subject), ropError(0x67, source, 0x80040306)},
		{copyDraftPropertiesWire(4, source, target, 0, 4, subject), ropError(0x67, source, 0x80070057)},
		{copyDraftPropertiesWire(4, source, target, 0, 2, subject, plain), append(ropError(0x67, source, 0), 0, 0)},
		{copyDraftPropertiesWire(4, source, target, 0, 0), append(ropError(0x67, source, 0), 0, 0)},
	} {
		got, _ := f.run(tc.wire)
		if !bytes.Equal(got, tc.want) || !bytes.Equal(dst.staged.MIME, dstMIME) || dst.dirty {
			t.Fatal("failed or no-op property copy modified the draft")
		}
	}
	want := append(ropError(0x67, source, 0), 2, 0, 1, 0)
	want = append32(want, html)
	want = append32(want, PropertyNotFound)
	want = append(want, 2, 0)
	want = append32(want, sourceKey)
	want = append32(want, accessDenied)
	if got := faiRunOK(t, f, copyWire); !bytes.Equal(got, want) {
		t.Fatal("CopyProperties did not return the original indexes and property tags")
	}
	mail, err := mailmime.ReadDraft(dst.staged.MIME)
	if err != nil || mail.Header.Get("Subject") != "synthetic" || string(mail.Body) != "original body" ||
		dst.staged.ID != "existing" || dst.staged.Properties[core.Subject] != core.Text("synthetic") ||
		!bytes.Equal(src.staged.MIME, srcMIME) || src.dirty || !dst.dirty || box.sourceCalls != 0 {
		t.Fatal("CopyProperties did not stage only the selected writable properties")
	}

	faiRunOK(t, f, copyDraftPropertiesWire(4, source, target, 0, 0, plain, files))
	projection, err := projectMailContents(dst.staged)
	if err != nil || projection.values[fixedContentsProperty(plain)] != core.Text("body") || len(projection.attachments) != 2 {
		t.Fatal("CopyProperties failed to stage the selected body and attachments")
	}
	data := projection.attachments[1][fixedContentsProperty(attachDataBinary)].(core.Binary)
	if !bytes.Equal(data, []byte{0, 127, 128, 255}) || dst.staged.Properties[core.HasAttachments] != core.Bool(true) {
		t.Fatal("CopyProperties changed the selected attachment payload")
	}
}

type existingDraftOnly struct{}

func (existingDraftOnly) CreateMailDraft(context.Context, core.Session, core.ID, string, []byte) (core.Object, error) {
	return core.Object{}, core.ErrUnsupported
}
