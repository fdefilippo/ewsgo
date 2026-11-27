// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"testing"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
	"ewsgo/internal/storage/sqlite"
)

type spoolerRecoveryFixture struct {
	outboxSendFunc
	db      *sqlite.Store
	mailbox string
}

func (f spoolerRecoveryFixture) RecoverDraftSend(ctx context.Context, _ core.Session, draft core.ID, revision core.Revision) (core.SendState, error) {
	return f.db.RecoverCorrelatedDraftSend(ctx, f.mailbox, draft, revision, core.DraftSendKey(draft, revision))
}

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
	item.MIME = []byte("From: lab@example.invalid\r\nTo: synthetic@example.invalid\r\n\r\nFixture\r\n")
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
	check(transport.executeSpooler(ctx, f.session, spooler, Operation{ID: ropSetSpooler}, spoolerHandles), 0)
	check(transport.executeSpoolerQueue(ctx, f.session, spooler, lock, spoolerHandles), 0)
	check(transport.executeSpoolerQueue(ctx, f.session, ordinary, abort, ordinaryHandles), unableToAbort)
	check(transport.executeSpooler(ctx, f.session, ordinary, Operation{ID: ropSetSpooler}, ordinaryHandles), 0)
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
	check(transport.executeSpooler(ctx, f.session, ordinary, Operation{ID: ropSetSpooler}, ordinaryHandles), 0)
	check(transport.executeSpoolerQueue(ctx, f.session, ordinary, lock, ordinaryHandles), unableToAbort)
	check(transport.executeSpoolerQueue(ctx, f.session, ordinary, abort, ordinaryHandles), unableToAbort)
	if submissions != 2 {
		t.Fatalf("%d native sends after uncertain result", submissions)
	}

	db := reopen()
	sender := spoolerRecoveryFixture{db: db, mailbox: f.store.durable.mailbox,
		outboxSendFunc: func(core.Object) (core.SendObservation, error) {
			t.Fatal("recovery dispatched a draft")
			return core.SendObservation{}, core.ErrUnsupported
		},
	}
	transport.EnableMailDraftSending(sender)
	ordinary, ordinaryHandles = connect()
	check(transport.executeSpooler(ctx, f.session, ordinary, Operation{ID: ropSetSpooler}, ordinaryHandles), 0)
	if f.store.isQueuedSubmission(mid) {
		t.Fatal("legacy Dispatching entry without a receipt retained its queue slot")
	}
	for _, tc := range []struct {
		name   string
		state  core.SendState
		queued bool
	}{
		{name: "safe retry", state: core.SendReady, queued: true},
		{name: "positive SOAP", state: core.SendConfirmed},
		{name: "terminal rejection", state: core.SendFailed},
		{name: "lost SOAP reply", state: core.SendUncertain},
	} {
		t.Run(tc.name, func(t *testing.T) {
			item.Revision++
			box.objects[0] = item
			ordinary, ordinaryHandles := connect()
			check(transport.executeSubmission(ctx, f.session, ordinary, submit, ordinaryHandles), 0)
			check(transport.executeSpooler(ctx, f.session, ordinary, Operation{ID: ropSetSpooler}, ordinaryHandles), 0)
			check(transport.executeSpoolerQueue(ctx, f.session, ordinary, lock, ordinaryHandles), 0)
			draft := ordinary.objects[ordinaryHandles[1]].message.mailDraft
			if _, err := draft.claimSpoolerDispatch(ctx, ordinaryHandles[0]); err != nil {
				t.Fatal(err)
			}
			// Simulate interruption before the queue can record the outcome.
			input := core.SendInput{DraftID: item.ID, Revision: item.Revision, MIMEDigest: sha256.Sum256(item.MIME)}
			intent, err := db.PrepareCorrelatedSend(ctx, sender.mailbox, input, core.DraftSendKey(item.ID, item.Revision))
			if err != nil {
				t.Fatal(err)
			}
			if _, claimed, err := db.ClaimSend(ctx, sender.mailbox, intent.ID, input); !claimed || err != nil {
				t.Fatalf("claim: %v", err)
			}
			if tc.state != core.SendUncertain {
				if err := db.CompleteSend(ctx, sender.mailbox, intent.ID, tc.state, ""); err != nil {
					t.Fatal(err)
				}
			}
			db = reopen()
			sender.db = db
			transport.EnableMailDraftSending(sender)
			ordinary, ordinaryHandles = connect()
			check(transport.executeSpooler(ctx, f.session, ordinary, Operation{ID: ropSetSpooler}, ordinaryHandles), 0)
			if got := f.store.isQueuedSubmission(mid); got != tc.queued {
				t.Fatalf("queued=%v, want %v", got, tc.queued)
			}
			if tc.queued {
				check(transport.executeSpoolerQueue(ctx, f.session, ordinary, abort, ordinaryHandles), 0)
				return
			}
			if stored, err := db.SendIntent(ctx, sender.mailbox, intent.ID); err != nil || stored.State != tc.state {
				t.Fatalf("receipt lost after recovery: %v, %v", stored.State, err)
			}
		})
	}
}
