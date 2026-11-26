// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

func TestDurableSpoolerQueueLifecycle(t *testing.T) {
	f, mailbox := messageFixture(t)
	reopen := sqliteReplicaFixture(t, f)
	reopen()
	ctx := t.Context()
	const nativeFolder = "native-drafts"
	mailbox.page.Objects = append(mailbox.page.Objects, core.Object{
		ID: nativeFolder, Parent: mailbox.Root(), Kind: core.Folder,
		Properties: core.Properties{core.DisplayName: core.Text("Drafts"), core.WellKnown: core.Text("drafts")},
	})
	item := messageTestObject("spool-draft", nativeFolder)
	item.Properties[core.IsDraft] = core.Bool(true)
	item.Properties[core.ContentRevision] = core.Integer(item.Revision)
	box := &outboxPlacementFixture{messageFixtureMailbox: mailbox}
	box.objects = []core.Object{item}
	f.session = &testSession{box}
	fids, err := f.store.assign(ctx, []folderKey{{native: nativeFolder}})
	if err != nil {
		t.Fatal(err)
	}
	fid := fids[0]
	view, err := f.store.observeMailMessages(ctx, f.session, fid)
	if err != nil || len(view.messages) != 1 {
		t.Fatalf("observe draft: %v", err)
	}
	mid := view.messages[0].mid
	transport := NewSessionTransport(Namespace{})
	submissions := 0
	var sendErr error
	transport.EnableMailDraftSending(outboxSendFunc(func(o core.Object) (core.SendObservation, error) {
		if o.ID != item.ID || o.Revision != item.Revision {
			t.Fatalf("send target = %v, revision %v", o.ID, o.Revision)
		}
		submissions++
		return core.SendObservation{}, sendErr
	}))
	connect := func() (*mailboxObjects, []uint32) {
		t.Helper()
		objects := newMailboxObjects(f.stores)
		logon, err := objects.addLogon(0, f.binding)
		if err != nil {
			t.Fatal(err)
		}
		handle, err := f.stores.handle()
		if err != nil {
			t.Fatal(err)
		}
		draft := &mailDraft{store: f.store, fid: fid, mid: mid, folder: nativeFolder, staged: item,
			rows: map[uint32]core.Recipient{0: {Address: "synthetic@example.invalid"}}}
		objects.objects[handle] = serverObject{binding: f.binding, fid: fid,
			message: &openedMessage{mid: mid, mailDraft: draft}}
		t.Cleanup(objects.close)
		return objects, []uint32{logon, handle}
	}
	ordinary, ordinaryHandles := connect()
	submit := Operation{ID: ropSubmitMessage, Input: 1, Flags: needsSpooler}
	send := Operation{ID: ropTransportSend, Input: 1}
	abort := Operation{ID: ropAbortSubmit, Folder: fid, SubmissionMID: mid}
	check := func(response []byte, expected uint32) {
		t.Helper()
		if len(response) < 6 || binary.LittleEndian.Uint32(response[2:]) != expected {
			t.Fatalf("ROP response %x, expected status %08x", response, expected)
		}
	}
	check(transport.executeSubmission(ctx, f.session, ordinary, submit, ordinaryHandles), 0)
	if submissions != 0 || !f.store.isQueuedSubmission(mid) {
		t.Fatal("enqueue performed an immediate send")
	}
	check(transport.executeSubmission(ctx, f.session, ordinary, send, ordinaryHandles), accessDenied)
	badFID := abort
	badFID.Folder = f.binding.fids[Inbox]
	check(transport.executeSpoolerQueue(ctx, f.session, ordinary, badFID, ordinaryHandles), PropertyNotFound)

	reopen()
	ordinary, ordinaryHandles = connect()
	if !f.store.isQueuedSubmission(mid) {
		t.Fatal("queue lost across restart")
	}
	spooler, spoolerHandles := connect()
	spoolCtx := t.Context()
	lock := Operation{ID: ropSpoolerLockMessage, SubmissionMID: mid}
	check(transport.executeSpoolerQueue(ctx, f.session, spooler, lock, spoolerHandles), accessDenied)
	check(executeSpooler(ctx, f.session, spooler, Operation{ID: ropSetSpooler}, spoolerHandles), 0)
	check(transport.executeSpoolerQueue(ctx, f.session, spooler, lock, spoolerHandles), 0)
	check(transport.executeSpoolerQueue(ctx, f.session, ordinary, abort, ordinaryHandles), unableToAbort)
	check(executeSpooler(ctx, f.session, ordinary, Operation{ID: ropSetSpooler}, ordinaryHandles), 0)
	check(transport.executeSpoolerQueue(ctx, f.session, ordinary, lock, ordinaryHandles), unableToAbort)
	spooler.release(spoolerHandles[0])
	check(transport.executeSpoolerQueue(ctx, f.session, ordinary, abort, ordinaryHandles), 0)
	check(transport.executeSpoolerQueue(ctx, f.session, ordinary, abort, ordinaryHandles), notInQueue)
	if submissions != 0 {
		t.Fatal("aborted message was submitted")
	}

	check(transport.executeSubmission(ctx, f.session, ordinary, submit, ordinaryHandles), 0)
	check(transport.executeSpoolerQueue(ctx, f.session, ordinary, lock, ordinaryHandles), 0)
	finish := lock
	finish.Flags = spoolerFinished
	check(transport.executeSpoolerQueue(ctx, f.session, ordinary, finish, ordinaryHandles), unableToAbort)
	spoolCtx = context.WithValue(spoolCtx, spoolerLogonKey{}, ordinaryHandles[0])
	sent := transport.executeSubmission(spoolCtx, f.session, ordinary, send, ordinaryHandles)
	if !bytes.Equal(sent, submitSuccess(send)) {
		t.Fatalf("send %x", sent)
	}
	check(transport.executeSubmission(spoolCtx, f.session, ordinary, send, ordinaryHandles), 0)
	if submissions != 1 {
		t.Fatalf("%d native sends after retry", submissions)
	}
	check(transport.executeSpoolerQueue(ctx, f.session, ordinary, finish, ordinaryHandles), 0)
	if f.store.isQueuedSubmission(mid) {
		t.Fatal("finish did not remove queue entry")
	}

	check(transport.executeSubmission(ctx, f.session, ordinary, submit, ordinaryHandles), 0)
	check(transport.executeSpoolerQueue(ctx, f.session, ordinary, lock, ordinaryHandles), 0)
	sendErr = core.ErrOutcomeUnknown
	response := transport.executeSubmission(spoolCtx, f.session, ordinary, send, ordinaryHandles)
	if binary.LittleEndian.Uint32(response[2:6]) == 0 {
		t.Fatal("ambiguous submission returned success")
	}
	reopen()
	ordinary, ordinaryHandles = connect()
	if f.store.spoolerQueue[mid].State != mapireplica.SpoolerDispatching {
		t.Fatal("indeterminate attempt lost across restart")
	}
	check(executeSpooler(ctx, f.session, ordinary, Operation{ID: ropSetSpooler}, ordinaryHandles), 0)
	check(transport.executeSpoolerQueue(ctx, f.session, ordinary, lock, ordinaryHandles), unableToAbort)
	check(transport.executeSpoolerQueue(ctx, f.session, ordinary, abort, ordinaryHandles), unableToAbort)
	if submissions != 2 {
		t.Fatalf("%d native sends after uncertain result", submissions)
	}
}
