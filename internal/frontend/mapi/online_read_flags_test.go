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

type onlineReadMailbox struct {
	*readStateMailbox
	suppressed  bool
	beforeWrite func() error
}

type readFlagActivity struct{ event core.Activity }

func (*readFlagActivity) Authenticated(string) {}
func (a *readFlagActivity) Record(event core.Activity) {
	a.event = event
}

func TestReadFlagsActivityReportsPartialCompletion(t *testing.T) {
	var observed readFlagActivity
	ctx := core.WithActivityObserver(context.Background(), &observed)
	response := append(ropError(ropSetReadFlags, 1, 0), 1)
	recordROPActivity(ctx, ropSetReadFlags, response, 0)
	if observed.event.Operation != "set_read_flags" ||
		observed.event.Result != core.ActivityFailure || observed.event.ROPReturnValue != 0 {
		t.Fatalf("unexpected partial completion activity: %+v", observed.event)
	}
	response[6] = 0
	recordROPActivity(ctx, ropSetReadFlags, response, 0)
	if observed.event.Result != core.ActivitySuccess {
		t.Fatalf("unexpected complete activity: %+v", observed.event)
	}
}

func (b *onlineReadMailbox) ReadMailMessageEnvelope(ctx context.Context, folder, id core.ID, includeSoftDeleted bool) (core.Object, error) {
	envelopes := openMessageFixtureMailbox{messageFixtureMailbox: b.messageFixtureMailbox}
	return envelopes.ReadMailMessageEnvelope(ctx, folder, id, includeSoftDeleted)
}

func (b *onlineReadMailbox) SetMailFlag(ctx context.Context, id core.ID, rev core.Revision, flag core.MailFlag, read bool) (core.Object, error) {
	if b.beforeWrite != nil {
		if err := b.beforeWrite(); err != nil {
			return core.Object{}, err
		}
	}
	current, err := b.Get(ctx, id)
	if err != nil {
		return core.Object{}, err
	}
	if flag != core.MailFlagRead || current.Revision != rev {
		return core.Object{}, core.ErrConflict
	}
	if current.Properties[core.IsRead] == core.Bool(read) {
		return current, nil
	}
	return b.SetRead(ctx, id, read)
}

func TestOnlineReadFlagsRechecksMembershipBeforeRetry(t *testing.T) {
	for _, tc := range []struct {
		name     string
		moved    bool
		firstErr error
		code     uint32
		attempts int
	}{
		{name: "unrelated metadata change", attempts: 2},
		{name: "moved out of folder", moved: true, code: PropertyNotFound, attempts: 1},
		{name: "unknown outcome", firstErr: core.ErrOutcomeUnknown, code: 0x80004005, attempts: 1},
		{name: "continuing contention", firstErr: core.ErrConflict, code: 0x80004005, attempts: 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, native, snapshot := readStateFixture(t)
			box := &onlineReadMailbox{readStateMailbox: native}
			f.session = &testSession{box}
			var attempts int
			box.beforeWrite = func() error {
				attempts++
				if tc.firstErr != nil {
					return tc.firstErr
				}
				if attempts != 1 {
					return nil
				}
				item := &native.objects[0]
				item.Revision++
				item.Properties[core.MetadataRevision] = core.Integer(item.Revision)
				item.Properties[core.MessageImportance] = core.Integer(2)
				if tc.moved {
					item.Parent = "different-folder"
				}
				return nil
			}
			mid := snapshot.messages[0].mid
			wire := binary.LittleEndian.AppendUint64([]byte{ropSetReadFlags, 4, 1, 0, readFlagSuppressReceipt, 1, 0}, mid)
			reply, _ := f.run(wire)
			want := append(ropError(ropSetReadFlags, 1, tc.code), byte(boolInt(tc.code != 0)))
			if attempts != tc.attempts || !bytes.Equal(reply, want) {
				t.Fatalf("read flag attempts = %d, response = %x, want %d and %x", attempts, reply, tc.attempts, want)
			}
			if read := native.objects[0].Properties[core.IsRead]; read != core.Bool(tc.code == 0) {
				t.Fatalf("unexpected read state: %v", read)
			}
			if tc.firstErr == nil && native.objects[0].Properties[core.MessageImportance] != core.Integer(2) {
				t.Fatal("lost concurrent metadata change")
			}
		})
	}
}

func (b *onlineReadMailbox) MarkMailRead(ctx context.Context, id core.ID, rev core.Revision, suppress bool) (core.Object, error) {
	b.suppressed = suppress
	return b.SetMailFlag(ctx, id, rev, core.MailFlagRead, true)
}

func TestOnlineReadFlagsThroughExecute(t *testing.T) {
	f, native, snapshot := readStateFixture(t)
	box := &onlineReadMailbox{readStateMailbox: native}
	f.session = &testSession{box}
	inbox := f.fids[Inbox]
	mid := snapshot.messages[0].mid
	faiRunOK(t, f, openMessageOperation(4, 1, 2, inbox, mid, 1252, 0))
	single := []byte{0x11, 4, 0, 2, 1}
	batch := func(flags byte, mids ...uint64) []byte {
		wire := binary.LittleEndian.AppendUint16([]byte{0x66, 4, 1, 0, flags}, uint16(len(mids)))
		for _, id := range mids {
			wire = binary.LittleEndian.AppendUint64(wire, id)
		}
		return wire
	}
	check := func(wire, want []byte) {
		t.Helper()
		reply, _ := f.run(wire)
		if !bytes.Equal(reply, want) {
			t.Fatalf("unexpected read flag response: %x, want %x", reply, want)
		}
	}
	for _, invalid := range [][]byte{single[:4], append(bytes.Clone(single), 0x66), batch(1, mid, mid)} {
		wire, _ := f.execute(invalid, 65535)
		if !bytes.Equal(wire, executeError(0x80070057, 65535)) || box.sets != 0 {
			t.Fatal("malformed Execute reached the write")
		}
	}
	f.execute(single, uint32(10+4*len(f.handles)+readFlagsResponseSize-1))
	if box.sets != 0 {
		t.Fatal("insufficient output budget allowed a write")
	}
	check([]byte{0x11, 4, 3, 2, 1}, ropError(0x11, 3, nullObject))
	check([]byte{0x11, 4, 0, 2, 0x10}, ropError(0x11, 0, NotSupported))
	check(single, append(ropError(0x11, 0, 0), 0))
	check(single, append(ropError(0x11, 0, 0), 0))
	if !box.suppressed || box.sets != 1 || box.objects[0].Properties[core.IsRead] != core.Bool(true) {
		t.Fatal("single-message mark and retry diverged")
	}
	faiRunOK(t, f, folderOperation(f.fids[SentItems], 4, 0, 3))
	outside := batch(5, mid)
	outside[2] = 3
	check(outside, append(ropError(0x66, 3, PropertyNotFound), 1))
	check(batch(5, 0, mid), append(ropError(0x66, 1, 0), 1))
	check(batch(0), append(ropError(0x66, 1, 0), 0))
	check(batch(5), append(ropError(0x66, 1, 0), 0))
	if box.suppressed || box.sets != 4 || box.objects[0].Properties[core.IsRead] != core.Bool(false) {
		t.Fatal("folder batch, partial response or mark-all diverged")
	}
}
