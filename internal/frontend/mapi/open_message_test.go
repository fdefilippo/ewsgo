// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"testing"

	"ewsgo/internal/core"
)

func TestSetMessageStatusForCompleteNativeMail(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &openMessageFixtureMailbox{messageFixtureMailbox: base}
	f.session = &testSession{box}
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	messages, err := store.observeMailMessages(t.Context(), f.session, f.fids[Inbox])
	if err != nil || len(messages.messages) == 0 {
		t.Fatal("could not observe native message", err)
	}
	mid := messages.messages[0].mid
	for _, tc := range []struct{ flags, mask, code uint32 }{
		{0, 0x3000, 0},
		{0x1000, 0, 0},
		{0x1000, 0x1000, NotSupported},
		{0x2000, 0x2000, NotSupported},
		{0x0800, 0x0800, accessDenied},
		{0, 1, 0x80070057},
	} {
		wire := binary.LittleEndian.AppendUint64([]byte{ropSetMessageStatus, 4, 1}, mid)
		wire = binary.LittleEndian.AppendUint32(wire, tc.flags)
		wire = binary.LittleEndian.AppendUint32(wire, tc.mask)
		out, result := f.run(wire)
		want := ropError(ropSetMessageStatus, 1, tc.code)
		if tc.code == 0 {
			want = append32(want, 0)
		}
		if !bytes.Equal(out, want) || result.errors != boolInt(tc.code != 0) {
			t.Fatalf("status flags=%#x mask=%#x: got %x want %x", tc.flags, tc.mask, out, want)
		}
	}
	if box.envelopeReads != 2 {
		t.Fatalf("expected two status reads, got %d", box.envelopeReads)
	}
}

func openMessageOperation(logon, input, output byte, fid, mid uint64, cp uint16, mode byte) []byte {
	wire := binary.LittleEndian.AppendUint16([]byte{3, logon, input, output}, cp)
	wire = binary.LittleEndian.AppendUint64(wire, fid)
	return binary.LittleEndian.AppendUint64(append(wire, mode), mid)
}

type openMessageFixtureMailbox struct {
	*messageFixtureMailbox
	envelopeReads int
	softDeleted   bool
}

type readOnlyDraftFixture struct {
	*openMessageFixtureMailbox
	core.MailDraftWriter
	folder     core.ID
	err        error
	draftReads int
}

func (b *readOnlyDraftFixture) Get(ctx context.Context, id core.ID) (core.Object, error) {
	object, err := b.openMessageFixtureMailbox.Get(ctx, id)
	if err == nil && id == b.folder {
		object.Properties = core.Properties{core.WellKnown: core.Text("drafts")}
	}
	return object, err
}

func (b *readOnlyDraftFixture) ReadMailMessageMIME(context.Context, core.ID, core.ID, bool) (core.Object, error) {
	b.draftReads++
	return core.Object{}, b.err
}

func TestOpenMessageTreatsWrappedUnsupportedLikeBareError(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	f.transport.EnableMailDrafts(existingDraftOnly{})
	base.objects[0].Properties[core.IsDraft] = core.Bool(true)
	box := &readOnlyDraftFixture{
		openMessageFixtureMailbox: &openMessageFixtureMailbox{messageFixtureMailbox: base},
		folder:                    base.objects[0].Parent,
	}
	f.session = &testSession{box}
	fid := f.fids[Inbox]
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	messages, err := store.observeMailMessages(t.Context(), f.session, fid)
	if err != nil || len(messages.messages) != 1 {
		t.Fatal("could not bind draft fixture", err)
	}
	mid := messages.messages[0].mid
	for _, source := range []error{core.ErrUnsupported, fmt.Errorf("fixture: %w", core.ErrUnsupported)} {
		box.err = source
		before := box.draftReads
		faiRunOK(t, f, openMessageOperation(4, 0, 2, fid, mid, 0x0fff, 3))
		if object := f.state.mailbox.objects[f.handles[2]]; object.message == nil || object.message.mailDraft != nil {
			t.Fatal("best-access open did not fall back to read-only")
		}
		faiRunOK(t, f, []byte{1, 4, 2})
		out, _ := f.run(openMessageOperation(4, 0, 2, fid, mid, 0x0fff, 1))
		if !bytes.Equal(out, ropError(3, 2, NotSupported)) || box.draftReads != before+2 {
			t.Fatal("explicit edit did not return the same backend error status")
		}
	}
}

func (b *openMessageFixtureMailbox) ReadMailMessageEnvelope(_ context.Context, folder, id core.ID, includeSoftDeleted bool) (core.Object, error) {
	b.envelopeReads++
	for _, object := range b.objects {
		if object.ID == id && object.Parent == folder && (!b.softDeleted || includeSoftDeleted) {
			object.MIME = nil
			return object, nil
		}
	}
	return core.Object{}, core.ErrNotFound
}

func TestOpenMessageBatchWireAuthorizationAndPreflight(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &openMessageFixtureMailbox{messageFixtureMailbox: base}
	f.session = &testSession{box}
	f.state.codePage = 1252
	box.objects[0].Properties[core.Subject] = core.Text("Re: Test")
	box.objects[0].Recipients = []core.Recipient{{Kind: core.RecipientCc, Address: "a@b"}}
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	fid := f.fids[Inbox]
	snapshot, err := store.observeMailMessages(context.Background(), f.session, fid)
	if err != nil || len(snapshot.messages) != 1 {
		t.Fatal("message fixture unavailable", err)
	}
	mid := snapshot.messages[0].mid
	open := openMessageOperation(4, 1, 2, fid, mid, 0x0fff, 3)
	props := folderPropertiesOperation(4, 2, 1, 0x0037001e, 0x674a0014, 0x0ff70003)
	// A malformed suffix must be rejected without processing a valid Open.
	for _, wire := range [][]byte{append(bytes.Clone(open), 0x07), openMessageOperation(4, 1, 255, fid, mid, 1252, 0)} {
		body, _ := f.execute(wire, 65535)
		if binary.LittleEndian.Uint32(body[4:]) != 0x80070057 || box.envelopeReads != 0 {
			t.Fatal("malformed request read the envelope")
		}
	}
	body, _ := f.execute(append([]byte{1, 4, 1}, open...), 30)
	if rops := storeResponse(t, body).Operations; len(rops) == 0 || rops[0] != 0xff || box.envelopeReads != 0 {
		t.Fatal("short output executed Release or Open")
	}
	out, counts := f.run(append(append(bytes.Clone(open), props...), 1, 4, 2))
	want := []byte{3, 2, 0, 0, 0, 0, 0,
		4, 'R', 0, 'e', 0, ':', 0, ' ', 0, 0, 0,
		4, 'T', 0, 'e', 0, 's', 0, 't', 0, 0, 0,
		1, 0, 0, 0, 1,
		2, 0xb0, 4, 0, 0, 13, 0,
		0x0b, 2, 'a', 0, '@', 0, 'b', 0, 0, 0, 0, 0, 0,
		7, 2, 0, 0, 0, 0, 0, 'R', 'e', ':', ' ', 'T', 'e', 's', 't', 0}
	want = binary.LittleEndian.AppendUint64(want, mid)
	want = append(want, 0, 0, 0, 0)
	if !bytes.Equal(out, want) || counts.errors != 0 || counts.properties.Values != 3 || !counts.properties.Message {
		t.Fatalf("synthetic OpenMessage batch: got %x want %x (errors=%d values=%d message=%v)", out, want, counts.errors, counts.properties.Values, counts.properties.Message)
	}
	failed, _ := f.run(props)
	if !bytes.Equal(failed, ropError(7, 2, invalidObject)) {
		t.Fatal("released Message was still readable")
	}

	for _, tc := range []struct {
		name string
		wire []byte
		code uint32
	}{
		{"wrong folder", openMessageOperation(4, 0, 2, f.fids[SentItems], mid, 0x0fff, 0), PropertyNotFound},
		{"unknown MID", openMessageOperation(4, 0, 2, fid, 0, 0x0fff, 0), PropertyNotFound},
		{"foreign logon", openMessageOperation(5, 0, 2, fid, mid, 0x0fff, 0), accessDenied},
		{"explicit write", openMessageOperation(4, 1, 2, fid, mid, 0x0fff, 1), accessDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before, reads := f.handles[2], box.envelopeReads
			// Independent ROP after a failed OpenMessage still executes.
			got, c := f.run(append(bytes.Clone(tc.wire), 0x49, 4, 0))
			expect := append(ropError(3, 2, tc.code), addressTypesResponse(0)...)
			if !bytes.Equal(got, expect) || c.errors != 1 || f.handles[2] != before || box.envelopeReads != reads {
				t.Fatal("failure mutated handles or stopped subsequent ROP")
			}
		})
	}

	box.softDeleted = true
	failed, _ = f.run(open)
	if !bytes.Equal(failed, ropError(3, 2, PropertyNotFound)) {
		t.Fatal("soft-deleted message opened without its flag")
	}
	softOpen := openMessageOperation(4, 1, 2, fid, mid, 0x0fff, 0x84)
	faiRunOK(t, f, softOpen)
	box.objects[0].Parent = store.byFID[f.fids[SentItems]].native
	failed, _ = f.run(props)
	if !bytes.Equal(failed, ropError(7, 2, PropertyNotFound)) {
		t.Fatal("open handle read a message moved outside its folder")
	}
}

func TestOpenSavedAssociatedMessageAndFitRecipients(t *testing.T) {
	f, box, store := newFAIExecuteFixture(t)
	request := syntheticFAIImport()
	faiRunOK(t, f, faiImportOperation(request))
	faiRunOK(t, f, faiSetOperation(3, faiCreateProperties()))
	save := faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 0})
	mid := binary.LittleEndian.Uint64(save[7:])
	before := store.allocator.contents.used
	open := openMessageOperation(4, 1, 4, f.fids[Inbox], mid, 0x0fff, 0)
	if raw := faiRunOK(t, f, open); !bytes.Equal(raw, append(ropError(3, 4, 0), 0, 1, 1, 0, 0, 0, 0, 0)) {
		t.Fatal("associated message envelope shape")
	}
	props := faiRunOK(t, f, folderPropertiesOperation(4, 4, 1, 0x65e20102))
	key := []byte(request.change)
	if len(props) != 9+len(key) || props[6] != 0 || !bytes.Equal(props[9:], key) {
		t.Fatal("associated message properties lost native FAI change key")
	}
	faiRunOK(t, f, []byte{1, 4, 4})
	if box.sourceCalls != 0 || store.allocator.contents.used != before {
		t.Fatal("FAI envelope read native contents or leaked memory")
	}

	message := core.Object{Recipients: []core.Recipient{{Kind: core.RecipientTo, Address: "a@b"}, {Kind: core.RecipientBcc, Address: "b@c"}}}
	got, code := openMessageResponse(3, 2, messageRow{object: message}, openMessageMinimum+20)
	if code != 0 || got[9] != 2 || got[13] != 1 || len(got) != openMessageMinimum+20 {
		t.Fatal("recipient count lost rows deferred by output budget")
	}
}
