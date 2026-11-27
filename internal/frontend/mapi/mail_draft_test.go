// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

type outboxPlacementFixture struct {
	*messageFixtureMailbox
	core.MailMessagePlacementWriter
	trash func(core.ID, core.Revision) (core.Object, error)
}

func (b *outboxPlacementFixture) Get(ctx context.Context, id core.ID) (core.Object, error) {
	for _, object := range b.objects {
		if object.ID == id {
			return object, nil
		}
	}
	return b.stateMailbox.Get(ctx, id)
}

func (b *outboxPlacementFixture) ReadMailMessageMIME(ctx context.Context, folder, id core.ID, _ bool) (core.Object, error) {
	object, err := b.Get(ctx, id)
	if err == nil && object.Parent != folder {
		err = core.ErrNotFound
	}
	return object, err
}

func (b *outboxPlacementFixture) TrashMailMessage(_ context.Context, id core.ID, revision core.Revision) (core.Object, error) {
	return b.trash(id, revision)
}

type outboxSendFunc func(core.Object) (core.SendObservation, error)

func (fn outboxSendFunc) SendDraft(_ context.Context, _ core.Session, draft core.Object) (core.SendObservation, error) {
	return fn(draft)
}

func TestOutboxTrashesOnlyConfirmedTransportCopy(t *testing.T) {
	f, mailbox := messageFixture(t)
	ctx := context.Background()
	store := f.store
	const drafts, trash core.ID = "native-drafts", "opaque-trash"
	mailbox.page.Objects = append(mailbox.page.Objects, core.Object{
		ID: drafts, Parent: mailbox.Root(), Kind: core.Folder,
		Properties: core.Properties{core.DisplayName: core.Text("Drafts"), core.WellKnown: core.Text("drafts")},
	})
	fids, err := store.assign(t.Context(), []folderKey{{native: drafts}})
	if err != nil {
		t.Fatal(err)
	}
	box := &outboxPlacementFixture{messageFixtureMailbox: mailbox}
	f.session = &testSession{box}
	original := messageTestObject("original", drafts)
	original.MIME = []byte("Content-Type: text/plain; charset=utf-8\r\n\r\nOriginal")
	original.Properties[core.IsDraft] = core.Bool(true)
	source := &mailDraft{store: store, fid: fids[0], folder: drafts, staged: original}
	const reference = "000102030405060708090a0b0c0d0e0f"
	store.next++
	fid, mid := f.binding.fids[Outbox], folderID(store.next)
	transport := messageTestObject("", drafts)
	transport.Properties[core.IsDraft] = core.Bool(true)
	transport.MIME = []byte("Content-Type: text/plain; charset=utf-8\r\n\r\n")
	d := &mailDraft{
		store: store, fid: fid, mid: mid, folder: drafts, outbox: true, staged: transport, ref: reference,
		rows: map[uint32]core.Recipient{0: {Address: "synthetic@example.invalid"}},
	}
	store.pendingMailDrafts = map[string]pendingMailDraft{reference: {fid: fid, mid: mid, folder: drafts}}
	if _, err := d.copyFrom(ctx, f.session, source, copyMessageToRequest{}); err != nil {
		t.Fatal(err)
	}
	d.staged.ID, d.dirty = "transport", false // Native creation has replied.
	if err := store.bindMailDraft(ctx, fid, mid, d.staged.ID, reference); err != nil {
		t.Fatal(err)
	}
	box.objects = []core.Object{original, d.staged}
	box.trash = func(core.ID, core.Revision) (core.Object, error) {
		t.Fatal("attempted to move a concurrently changed copy")
		return core.Object{}, nil
	}
	box.objects[1].Revision++
	if err := d.trashTransportCopy(ctx, box, original.ID); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("changed copy was not rejected: %v", err)
	}
	box.objects[1] = d.staged
	var sends, moves int
	send := outboxSendFunc(func(object core.Object) (core.SendObservation, error) {
		sends++
		if object.ID != d.staged.ID || object.Revision != d.staged.Revision {
			t.Fatal("wrong draft submitted")
		}
		if sends == 1 {
			return core.SendObservation{SentCandidate: "candidate"}, core.ErrOutcomeUnknown
		}
		return core.SendObservation{}, nil
	})
	box.trash = func(id core.ID, revision core.Revision) (core.Object, error) {
		moves++
		if sends != 2 || id != d.staged.ID || revision != d.staged.Revision {
			t.Fatal("move preceded acceptance or addressed the wrong draft/revision")
		}
		box.objects[1].Parent = trash
		box.objects[1].Revision++
		return core.Object{}, core.ErrOutcomeUnknown // Move committed, reply lost.
	}
	if evidence, err := d.submit(ctx, f.session, send); !errors.Is(err, core.ErrOutcomeUnknown) || evidence.SentCandidate == "" || moves != 0 {
		t.Fatalf("uncertain submission performed cleanup or lost evidence: %v", err)
	}
	if _, err := d.submit(ctx, f.session, send); err != nil || !store.outboxDrafts[d.staged.ID].trashPending || moves != 1 {
		t.Fatalf("lost move reply affected confirmed submission: %v", err)
	}
	if _, err := d.submit(ctx, f.session, send); err != nil || !d.submitted || sends != 2 || moves != 1 {
		t.Fatalf("retry did not reconcile the move by identity: %v", err)
	}
	if store.hasOutboxDrafts() || box.objects[0].Parent != drafts || box.objects[0].Revision != original.Revision {
		t.Fatal("Outbox remained queued or the original draft changed")
	}
}

func TestOutboxRetainsNativeDraftAndSubmissionIdentity(t *testing.T) {
	f, box := messageFixture(t)
	ctx := context.Background()
	store := f.store
	const drafts core.ID = "native-drafts"
	box.page.Objects = append(box.page.Objects, core.Object{
		ID: drafts, Parent: box.Root(), Kind: core.Folder,
		Properties: core.Properties{core.DisplayName: core.Text("Drafts"), core.WellKnown: core.Text("drafts")},
	})
	fid := f.binding.fids[Outbox]
	native, err := draftFolder(ctx, f.session, store, fid)
	if err != nil || native != drafts {
		t.Fatalf("Outbox did not resolve native Drafts: %v", err)
	}
	ids, err := store.assign(t.Context(), []folderKey{{native: drafts}})
	if err != nil {
		t.Fatal(err)
	}
	const reference = "000102030405060708090a0b0c0d0e0f"
	store.next++
	mid := folderID(store.next)
	store.pendingMailDrafts = map[string]pendingMailDraft{reference: {fid: fid, mid: mid, folder: drafts, dispatched: true}}
	a := messageTestObject("outbound", drafts)
	b := messageTestObject("unrelated", drafts)
	a.Properties[core.IsDraft], b.Properties[core.IsDraft] = core.Bool(true), core.Bool(true)
	a.MIME = append([]byte(core.DraftReferenceHeader+": "+reference+"\r\n"), a.MIME...)
	box.objects = []core.Object{a, b}
	normalView := func() []messageRow {
		t.Helper()
		view, err := store.observeMailMessages(ctx, f.session, ids[0])
		if err != nil {
			t.Fatal(err)
		}
		return view.messages
	}
	if view := normalView(); len(view) != 1 || view[0].object.ID != b.ID {
		t.Fatal("native Drafts projected an unresolved Outbox draft")
	}
	if view := messageObserve(t, f, Outbox).messages; len(view) != 1 || view[0].mid != mid || view[0].parent != fid {
		t.Fatal("Outbox did not reconcile its native draft with the provisional MID")
	}
	d := &mailDraft{store: store, fid: fid, mid: mid, folder: drafts, staged: a, outbox: true}
	prior, err := store.startOutboxSubmission(ctx, d)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.startOutboxSubmission(ctx, d); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("duplicate in-flight attempt: %v", err)
	}
	if err := store.finishOutboxSubmission(ctx, d, prior, outboxUncertain); err != nil {
		t.Fatal(err)
	}
	if err := store.editableOutboxDraft(a.ID, drafts); !errors.Is(err, core.ErrOutcomeUnknown) {
		t.Fatalf("submitted revision could be modified after an uncertain response: %v", err)
	}
	if view := messageObserve(t, f, Outbox).messages; len(view) != 1 || view[0].mid != mid {
		t.Fatal("uncertain result removed the draft from Outbox")
	}
	d.staged.Revision++
	if _, err := store.startOutboxSubmission(ctx, d); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("different revision could be sent after an uncertain response: %v", err)
	}
	d.staged.Revision = a.Revision
	prior, err = store.startOutboxSubmission(ctx, d)
	if err != nil {
		t.Fatalf("same revision could not query the durable submission intent: %v", err)
	}
	if err := store.finishOutboxSubmission(ctx, d, prior, outboxRetained); err != nil {
		t.Fatal(err)
	}
	if view := messageObserve(t, f, Outbox).messages; len(view) != 0 || !d.submitted {
		t.Fatal("confirmed draft remained queued")
	}
	for _, row := range normalView() {
		if row.object.ID == a.ID && row.mid == mid {
			return
		}
	}
	t.Fatal("confirmed submission failed to retain the original draft with the same MID")
}

func TestSQLiteOutboxResumesNativeAcknowledgmentAndSubmission(t *testing.T) {
	f, box := messageFixture(t)
	reopen := sqliteReplicaFixture(t, f)
	ctx := t.Context()
	reopen()
	const drafts = core.ID("native-drafts")
	const reference = "000102030405060708090a0b0c0d0e0f"
	box.page.Objects = append(box.page.Objects, core.Object{
		ID: drafts, Parent: box.Root(), Kind: core.Folder,
		Properties: core.Properties{core.DisplayName: core.Text("Drafts"), core.WellKnown: core.Text("drafts")},
	})
	fid := f.binding.fids[Outbox]
	store := f.store
	mid, err := store.stageIDLocked(ctx, mapireplica.Message)
	if err != nil {
		t.Fatal(err)
	}
	store.pendingMailDrafts = map[string]pendingMailDraft{reference: {fid: fid, mid: mid, folder: drafts, transportSource: "original"}}
	if err := store.setDraftDispatched(ctx, reference, true); err != nil {
		t.Fatal("durable dispatch:", err)
	}
	item := messageTestObject("outbox-native-id", drafts)
	item.Properties[core.IsDraft] = core.Bool(true)
	item.MIME = append([]byte(core.DraftReferenceHeader+": "+reference+"\r\n"), item.MIME...)
	box.objects = []core.Object{item}
	reopen()
	if rows := messageObserve(t, f, Outbox).messages; len(rows) != 1 || rows[0].mid != mid {
		t.Fatal("lost native acknowledgment was not reconciled to the original MID")
	}
	reopen()
	store = f.store
	d := &mailDraft{store: store, fid: fid, mid: mid, folder: drafts, staged: item, outbox: true}
	prior, err := store.startOutboxSubmission(ctx, d)
	if err != nil || prior.transportSource != "original" {
		t.Fatal("copy relationship or submission claim was lost:", err)
	}
	if err := store.finishOutboxSubmission(ctx, d, prior, outboxTrashPending); err != nil {
		t.Fatal("persist pending cleanup:", err)
	}
	reopen()
	store = f.store
	d.store = store
	if err := store.editableOutboxDraft(item.ID, drafts); !errors.Is(err, core.ErrOutcomeUnknown) {
		t.Fatal("acknowledged revision was editable after restart:", err)
	}
	prior, err = store.startOutboxSubmission(ctx, d)
	if err != nil || !prior.trashPending || prior.revision != item.Revision {
		t.Fatal("pending cleanup could not resume without resending:", err)
	}
	if err := store.finishOutboxSubmission(ctx, d, prior, outboxTrashed); err != nil {
		t.Fatal("persist cleanup:", err)
	}
	reopen()
	if f.store.hasOutboxDrafts() || f.store.byMessage[messageKey{id: item.ID}] != mid {
		t.Fatal("confirmed cleanup lost the MID or left the item queued")
	}
}

func TestObservedDraftAfterLostReplyUsesProvisionalMID(t *testing.T) {
	f, box := messageFixture(t)
	fid := f.binding.fids[Inbox]
	const reference = "000102030405060708090a0b0c0d0e0f"
	store := f.store
	store.next++
	mid := folderID(store.next)
	pending := pendingMailDraft{fid: fid, mid: mid, dispatched: true}
	store.pendingMailDrafts = map[string]pendingMailDraft{reference: pending}
	draft := &box.objects[0]
	draft.Properties[core.IsDraft] = core.Bool(true)
	draft.MIME = append([]byte(core.DraftReferenceHeader+": "+reference+"\r\n"), draft.MIME...)
	visible := map[core.ID]hierarchyNode{draft.Parent: {}}
	event := NotificationEvent{ID: draft.ID, Parent: draft.Parent, Kind: core.Created, ObjectKind: core.Message}
	if _, err := store.projectObjectNotification(t.Context(), event, visible, true); !errors.Is(err, core.ErrOutcomeUnknown) {
		t.Fatalf("notification assigned a MID before reconciliation: %v", err)
	}
	row := messageObserve(t, f, Inbox).messages[0]
	if row.mid != mid || len(store.pendingMailDrafts) != 0 {
		t.Fatal("the committed snapshot did not adopt the provisional MID")
	}
	n, err := store.projectObjectNotification(t.Context(), event, visible, true)
	if err != nil || n.mid != mid {
		t.Fatalf("notification lost the reconciled MID: %v", err)
	}
	if err := store.bindMailDraft(t.Context(), fid, mid, draft.ID, reference); err != nil {
		t.Fatalf("late acknowledgment changed the reconciled MID: %v", err)
	}
	view, err := projectMailContents(*draft)
	if err != nil {
		t.Fatal(err)
	}
	headers := view.values[fixedContentsProperty(0x007d001f)].(core.Text)
	if bytes.Contains([]byte(headers), []byte(reference)) {
		t.Fatal("MAPI exposed the private draft reference")
	}
}

type existingDraftOnly struct{}

func (existingDraftOnly) CreateMailDraft(context.Context, core.Session, core.ID, string, []byte) (core.Object, error) {
	return core.Object{}, core.ErrUnsupported
}
