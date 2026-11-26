// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"slices"
	"testing"

	"ewsgo/internal/core"
)

func TestHardDeleteLocalMessagesExecute(t *testing.T) {
	f, _, mail := readStateFixture(t)
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	inbox := f.fids[Inbox]
	deleted := createDeletionTestFAI(t, store, f.session, inbox, 1)
	kept := createDeletionTestFAI(t, store, f.session, inbox, 2)
	other := createDeletionTestFAI(t, store, f.session, f.fids[SentItems], 3)
	native := mail.messages[0].mid
	before := store.localNotifications.head
	for _, tc := range []struct {
		async, notify byte
		mids          []uint64
		code          uint32
		partial       byte
	}{
		{0, 0, []uint64{deleted.mid, native}, NotSupported, 0},
		{1, 0, []uint64{deleted.mid}, NotSupported, 0},
		{0, 1, []uint64{deleted.mid}, NotSupported, 0},
		{0, 0, []uint64{deleted.mid, other.mid}, 0, 1},
		{0, 0, []uint64{deleted.mid}, 0, 0},
	} {
		wire := deleteMessagesWire(4, 1, tc.async, tc.notify, tc.mids...)
		wire[0] = ropHardDeleteMessages
		wire = append(wire, folderOperation(inbox, 4, 0, 2)...)
		out, _ := f.run(wire)
		want := append(ropError(ropHardDeleteMessages, 1, tc.code), tc.partial)
		want = append(want, 2, 2, 0, 0, 0, 0, 0, 0)
		if !bytes.Equal(out, want) {
			t.Fatalf("hard delete followed by OpenFolder: %x, want %x", out, want)
		}
		if store.fai.records[deleted.object.ID].deleted != (tc.code == 0) ||
			store.fai.records[other.object.ID].deleted || store.fai.records[kept.object.ID].deleted {
			t.Fatal("hard delete changed an item outside the requested scope")
		}
	}
	events, _, err := store.localNotificationPage(before)
	if err != nil || len(events) != 2 || events[0].flag != notifyDeleted|notifyMessage || events[0].mid != deleted.mid ||
		events[1].flag != notifyModified || events[1].fid != inbox {
		t.Fatal("hard delete did not publish exactly one change", err)
	}
}

func TestHardDeleteLocalMessagesSQLiteReopen(t *testing.T) {
	f, _ := messageFixture(t)
	reopen := sqliteReplicaFixture(t, f)
	reopen()
	inbox := f.binding.fids[Inbox]
	row := createDeletionTestFAI(t, f.store, f.session, inbox, 1)
	if partial, err := f.store.hardDeleteLocalMessages(t.Context(), f.session, inbox, []uint64{row.mid}); err != nil || partial {
		t.Fatal("hard delete:", partial, err)
	}
	reopen()
	record := f.store.fai.records[row.object.ID]
	version := f.store.messages[f.store.byMID[row.mid]]
	if !record.deleted || len(record.object.Properties) != 0 || version.present || version.cn <= row.cn {
		t.Fatal("payload or active version survived hard delete")
	}
	before := f.store.replicaPosition()
	if partial, err := f.store.hardDeleteLocalMessages(t.Context(), f.session, inbox, []uint64{row.mid}); err != nil || partial || f.store.replicaPosition() != before {
		t.Fatal("a retry changed the replica", partial, err)
	}
}

func deleteMessagesWire(logon, folder, asynchronous, notifyNonRead byte, mids ...uint64) []byte {
	wire := binary.LittleEndian.AppendUint16([]byte{0x1e, logon, folder, asynchronous, notifyNonRead}, uint16(len(mids)))
	for _, mid := range mids {
		wire = binary.LittleEndian.AppendUint64(wire, mid)
	}
	return wire
}

func TestUnsupportedDeletionBatch(t *testing.T) {
	f := newCreateFixture(t)
	openInbox := folderOperation(f.fids[Inbox], 4, 0, 2)
	hardDelete := deleteMessagesWire(4, 1, 0, 0, 0x10001)
	hardDelete[0] = ropHardDeleteMessages
	for _, request := range [][]byte{
		hardDelete, {ropEmptyFolder, 4, 1, 0, 0}, {ropHardDeleteFolderContents, 4, 1, 0, 1},
	} {
		for size := 1; size < len(request); size++ {
			if _, err := (ROPBuffer{Operations: request[:size], Handles: f.handles}).DecodeOperations(); err == nil {
				t.Fatalf("truncated 0x%02x accepted at byte %d", request[0], size)
			}
		}
		out, result := f.run(append(bytes.Clone(request), openInbox...))
		want := append(ropError(request[0], 1, NotSupported), 0, 2, 2, 0, 0, 0, 0, 0, 0)
		if !bytes.Equal(out, want) || result.errors != 1 {
			t.Fatalf("0x%02x followed by OpenFolder = %x, want %x", request[0], out, want)
		}
	}
}

type softDeleteMailbox struct {
	*readStateMailbox
	calls  int
	result error
}

func (b *softDeleteMailbox) SoftDeleteMailMessage(_ context.Context, id core.ID, rev core.Revision) error {
	b.calls++
	if b.result != nil {
		return b.result
	}
	for i, obj := range b.objects {
		if obj.ID == id && obj.Revision == rev {
			b.objects = append(b.objects[:i], b.objects[i+1:]...)
			return nil
		}
	}
	return core.ErrConflict
}

type receiptSoftDeleteMailbox struct {
	*softDeleteMailbox
	eligible core.ID
}

type failingSyncDeleteMailbox struct {
	*softDeleteMailbox
	failID core.ID
}

func (b *failingSyncDeleteMailbox) SoftDeleteMailMessage(ctx context.Context, id core.ID, rev core.Revision) error {
	if id == b.failID {
		return core.ErrOutcomeUnknown
	}
	return b.softDeleteMailbox.SoftDeleteMailMessage(ctx, id, rev)
}

func TestContentsImportSoftDeletesRetryThroughExecute(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &failingSyncDeleteMailbox{softDeleteMailbox: &softDeleteMailbox{
		readStateMailbox: &readStateMailbox{messageFixtureMailbox: base},
	}}
	box.objects = append(box.objects, messageTestObject("second", box.objects[0].Parent))
	f.session = &testSession{box}
	f.transport.EnableFAICollectorSpike()
	reopen := sqliteExecuteFixture(t, f)
	db := reopen()
	store := replicaStore(t, f)
	snapshot, err := store.observeMailMessages(t.Context(), f.session, f.fids[Inbox])
	if err != nil || len(snapshot.messages) != 2 {
		t.Fatal("source fixture unavailable")
	}
	first, second := snapshot.messages[0], snapshot.messages[1]
	softDelete := func(sources ...[22]byte) []byte {
		wire := faiDeleteOperation(sources...)
		wire[3] = 0
		return wire
	}
	openCollector := func() { faiRunOK(t, f, []byte{0x7e, 4, 1, 2, 1}) }
	requireCode := func(wire []byte, want uint32) {
		t.Helper()
		response, _ := f.run(wire)
		if !bytes.Equal(response, ropError(0x74, 2, want)) {
			t.Fatalf("unexpected collector response: %x, want code %x", response, want)
		}
	}
	openCollector()
	requireCode(faiDeleteOperation(first.sourceKey), accessDenied)
	invalid := store.hierarchyXID(f.fids[SentItems])
	requireCode(softDelete(first.sourceKey, invalid), accessDenied)
	if box.calls != 0 {
		t.Fatal("unsupported options or invalid subsequent key caused a native write")
	}
	box.failID = second.object.ID
	wire := softDelete(first.sourceKey, second.sourceKey)
	requireCode(wire, faiError(core.ErrOutcomeUnknown, false))
	if box.calls != 1 || len(box.objects) != 1 || box.objects[0].ID != box.failID {
		t.Fatal("the failed request did not preserve the verified native prefix")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopen()
	openCollector()
	box.failID = ""
	requireCode(wire, 0)
	requireCode(wire, 0)
	if box.calls != 2 || len(box.objects) != 0 {
		t.Fatal("retry applied the original native deletion again or left a target behind")
	}
	loaded := replicaStore(t, f)
	removed, err := loaded.observeMailMessages(t.Context(), f.session, f.fids[Inbox])
	if err != nil || len(removed.messages) != 0 || len(loaded.fai.absent) != 0 {
		t.Fatal("native deletions created an FAI tombstone or remained visible")
	}
}

func (b *receiptSoftDeleteMailbox) SoftDeleteMailMessageWithNonReadReceipt(ctx context.Context, id core.ID, rev core.Revision) error {
	if id != b.eligible {
		return core.ErrUnsupported
	}
	return b.SoftDeleteMailMessage(ctx, id, rev)
}

func TestDeleteMessagesThroughExecute(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &softDeleteMailbox{readStateMailbox: &readStateMailbox{messageFixtureMailbox: base}}
	f.session = &testSession{box}
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	inbox := f.fids[Inbox]
	box.objects = append(box.objects, messageTestObject("second", box.objects[0].Parent))
	snapshot, err := store.observeMailMessages(t.Context(), f.session, inbox)
	if err != nil || len(snapshot.messages) != 2 {
		t.Fatal("source fixture unavailable")
	}
	mid, second := snapshot.messages[0].mid, snapshot.messages[1].mid
	faiRunOK(t, f, folderOperation(f.fids[SentItems], 4, 0, 2))

	valid := deleteMessagesWire(4, 1, 0, 0, mid)
	for _, tc := range []struct {
		name string
		wire []byte
		code uint32
	}{
		{"truncated", valid[:len(valid)-1], 0x80070057},
		{"duplicate", deleteMessagesWire(4, 1, 0, 0, mid, mid), 0x80070057},
		{"outside handle table", deleteMessagesWire(4, 4, 0, 0, mid), 0x80070057},
		{"malformed trailing ROP", append(bytes.Clone(valid), 0x1e), 0x80070057},
		{"over quota", deleteMessagesWire(4, 1, 0, 0, make([]uint64, core.MaxMailContentMessages+1)...), NotSupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			wire, _ := f.execute(tc.wire, 65535)
			if !bytes.Equal(wire, executeError(tc.code, 65535)) || box.calls != 0 || box.gets != 0 {
				t.Fatalf("unexpected response or backend effect: %x", wire)
			}
		})
	}
	hard := append([]byte{ropHardDeleteMessages}, valid[1:]...)
	hardReply, hardResult := f.run(hard)
	if !bytes.Equal(hardReply, append(ropError(ropHardDeleteMessages, 1, NotSupported), 0)) ||
		hardResult.errors != 1 || box.calls != 0 || box.gets != 0 {
		t.Fatalf("hard delete returned %x or invoked the backend", hardReply)
	}
	f.execute(valid, uint32(10+4*len(f.handles)+deleteMessagesResponseSize-1))
	if box.calls != 0 || box.gets != 0 {
		t.Fatal("insufficient output capacity reached the backend")
	}
	check := func(wire []byte, code uint32, partial byte) {
		t.Helper()
		want := append(ropError(0x1e, wire[2], code), partial)
		reply, counts := f.run(wire)
		if !bytes.Equal(reply, want) || counts.errors != boolInt(code != 0 || partial != 0) {
			t.Fatalf("unexpected delete response: %x, want %x, errors=%d", reply, want, counts.errors)
		}
	}
	check(deleteMessagesWire(4, 1, 0xff, 0, mid), NotSupported, 0)
	check(deleteMessagesWire(4, 1, 0, 0x01, mid), NotSupported, 0)
	check(deleteMessagesWire(4, 0, 0, 0, mid), NotSupported, 0)
	check(deleteMessagesWire(4, 3, 0, 0, mid), nullObject, 0)
	check(deleteMessagesWire(5, 1, 0, 0, mid), accessDenied, 0)
	check(deleteMessagesWire(4, 1, 0, 0), 0, 0)
	if box.calls != 0 || box.gets != 0 {
		t.Fatal("handle or option rejection attempted a message read or write")
	}
	withOptions := deleteMessagesWire(4, 1, 0xff, 1, mid)
	_, counts := f.run(withOptions)
	if len(counts.execution.DeleteMessageOptions()) != 0 {
		t.Fatal("delete options recorded without tracing")
	}
	f.transport.EnableSynchronizationOptionsTrace()
	_, counts = f.run(append(withOptions, deleteMessagesWire(4, 1, 0, 0, 0)...))
	if !slices.Equal(counts.execution.DeleteMessageOptions(), []string{"synchronous", "asynchronous_notify_non_read"}) {
		t.Fatal("delete options were not diagnosed per request")
	}
	check(deleteMessagesWire(4, 2, 0, 0, mid), PropertyNotFound, 1)
	if box.calls != 0 {
		t.Fatal("folder handle was used to delete a message in another folder")
	}

	box.result = core.ErrOutcomeUnknown
	check(deleteMessagesWire(4, 1, 0, 0, mid, second), 0x80004005, 1)
	if box.calls != 1 || len(box.objects) != 2 {
		t.Fatal("execution continued after an uncertain mutation")
	}
	box.result = nil
	check(deleteMessagesWire(4, 1, 0, 0, 0, mid), 0, 1)
	check(valid, PropertyNotFound, 1)
	check(deleteMessagesWire(4, 1, 0, 0, second), 0, 0)
	if box.calls != 3 || len(box.objects) != 0 {
		t.Fatal("delete did not apply exactly once to each valid target")
	}
}

func TestDeleteMessagesWithNonReadReceiptThroughExecute(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &receiptSoftDeleteMailbox{softDeleteMailbox: &softDeleteMailbox{
		readStateMailbox: &readStateMailbox{messageFixtureMailbox: base},
	}}
	f.session = &testSession{box}
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	inbox := f.fids[Inbox]
	first, second := base.objects[0], messageTestObject("second", base.objects[0].Parent)
	box.eligible = second.ID
	base.objects = append(base.objects, second)
	snapshot, err := store.observeMailMessages(t.Context(), f.session, inbox)
	if err != nil || len(snapshot.messages) != 2 {
		t.Fatal("source fixture unavailable", err)
	}
	wire := deleteMessagesWire(4, 1, 0, 1, snapshot.messages[0].mid, snapshot.messages[1].mid)
	reply, counts := f.run(wire)
	if want := append(ropError(ropDeleteMessages, 1, 0), 1); !bytes.Equal(reply, want) || counts.errors != 1 {
		t.Fatalf("unexpected partial result: %x", reply)
	}
	if box.calls != 1 || len(base.objects) != 1 || base.objects[0].ID != first.ID {
		t.Fatal("deletion proceeded for a message whose receipt could not be handled")
	}
}
