// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"ewsgo/internal/core"
)

func moveCopyWire(logon, from, to, asynchronous, copyFlag byte, mids ...uint64) []byte {
	wire := binary.LittleEndian.AppendUint16([]byte{0x33, logon, from, to}, uint16(len(mids)))
	for _, mid := range mids {
		wire = binary.LittleEndian.AppendUint64(wire, mid)
	}
	return append(wire, asynchronous, copyFlag)
}

type moveCopyMailbox struct {
	*readStateMailbox
	core.MailMessagePlacementWriter
	calls  int
	result error
}

func (b *moveCopyMailbox) MoveMailMessage(_ context.Context, id core.ID, revision core.Revision, to core.ID) (core.Object, error) {
	b.calls++
	if b.result != nil {
		return core.Object{}, b.result
	}
	for i := range b.objects {
		object := &b.objects[i]
		if object.ID == id && object.Revision == revision {
			object.Parent = to
			return *object, nil
		}
	}
	return core.Object{}, core.ErrConflict
}

func (b *moveCopyMailbox) CopyMailMessage(_ context.Context, id core.ID, revision core.Revision, to core.ID) (core.Object, error) {
	b.calls++
	if b.result != nil {
		return core.Object{}, b.result
	}
	for _, object := range b.objects {
		if object.ID == id && object.Revision == revision {
			object.ID, object.Parent = "copy-"+id, to
			return object, nil
		}
	}
	return core.Object{}, core.ErrConflict
}

func TestMoveCopyMessagesThroughExecute(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &moveCopyMailbox{readStateMailbox: &readStateMailbox{messageFixtureMailbox: base}}
	f.session = &testSession{box}
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	inbox := f.fids[Inbox]
	snapshot, err := store.observeMailMessages(t.Context(), f.session, inbox)
	if err != nil || len(snapshot.messages) != 1 {
		t.Fatal("source fixture unavailable")
	}
	mid := snapshot.messages[0].mid
	faiRunOK(t, f, folderOperation(f.fids[SentItems], 4, 0, 2))
	copyOne := moveCopyWire(4, 1, 2, 0, 1, mid)
	bad := []struct {
		name, kind string
		wire       []byte
	}{
		{"truncated", "malformed", copyOne[:len(copyOne)-1]},
		{"duplicate", "malformed", moveCopyWire(4, 1, 2, 0, 1, mid, mid)},
		{"invalid flags", "malformed", moveCopyWire(4, 1, 2, 0, 2, mid)},
		{"invalid slot", "malformed", moveCopyWire(4, 1, 4, 0, 1, mid)},
		{"trailing invalid ROP", "malformed", append(bytes.Clone(copyOne), 0x33)},
		{"batch beyond limit", "unsupported", moveCopyWire(4, 1, 2, 0, 1, make([]uint64, core.MaxMailContentMessages+1)...)},
	}
	for _, tc := range bad {
		t.Run(tc.name, func(t *testing.T) {
			wire, _ := f.execute(tc.wire, 65535)
			code := uint32(0x80070057)
			if tc.kind == "unsupported" {
				code = NotSupported
			}
			if !bytes.Equal(wire, executeError(code, 65535)) || box.calls != 0 || box.gets != 0 {
				t.Fatal("malformed or unsupported Execute reached the backend")
			}
		})
	}
	f.execute(copyOne, uint32(10+4*len(f.handles)+moveCopyMessagesMaxResponse-1))
	if box.calls != 0 || box.gets != 0 {
		t.Fatal("insufficient output buffer had native effects")
	}
	check := func(wire, want []byte) {
		t.Helper()
		reply, _ := f.run(wire)
		if !bytes.Equal(reply, want) {
			t.Fatalf("unexpected move/copy response: %x, want %x", reply, want)
		}
	}
	check(moveCopyWire(4, 1, 3, 0, 1, mid), []byte{0x33, 1, 3, 5, 0, 0, 3, 0, 0, 0, 0})
	check(moveCopyWire(4, 1, 2, 1, 1, mid), append(ropError(0x33, 1, NotSupported), 0))
	check(moveCopyWire(5, 1, 2, 0, 1, mid), append(ropError(0x33, 1, accessDenied), 0))
	check(moveCopyWire(4, 1, 0, 0, 1, mid), append(ropError(0x33, 1, NotSupported), 0))
	check(moveCopyWire(4, 1, 2, 0, 1), append(ropError(0x33, 1, 0), 0))
	if box.calls != 0 || box.gets != 0 {
		t.Fatal("rejected or empty request touched messages")
	}

	check(moveCopyWire(4, 1, 2, 0, 1, 0, mid), append(ropError(0x33, 1, 0), 1))
	if box.calls != 1 || box.objects[0].Parent != store.byFID[inbox].native {
		t.Fatal("batch copy did not preserve source or skipped the valid member")
	}

	box.result = core.ErrOutcomeUnknown
	check(moveCopyWire(4, 1, 2, 0, 1, mid, 0), append(ropError(0x33, 1, 0x80004005), 1))
	if box.calls != 2 {
		t.Fatal("an uncertain copy was retried")
	}
	box.result = nil
	check(moveCopyWire(4, 1, 2, 0, 0, mid), append(ropError(0x33, 1, 0), 0))
	check(copyOne, append(ropError(0x33, 1, PropertyNotFound), 1))
	if box.calls != 3 || box.objects[0].ID != snapshot.messages[0].object.ID ||
		box.objects[0].Parent != store.byFID[f.fids[SentItems]].native {
		t.Fatal("move changed identity or allowed the old folder to mutate the message")
	}
}
