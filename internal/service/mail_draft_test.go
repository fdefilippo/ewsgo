// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"bytes"
	"context"
	"errors"
	"path/filepath"
	"testing"

	"ewsgo/internal/core"
	"ewsgo/internal/mailmime"
	"ewsgo/internal/storage/sqlite"
)

type draftTestSession struct {
	core.Session
	box core.Mailbox
}

func (s draftTestSession) Mailbox() core.Mailbox { return s.box }

type droppedDraftReply struct {
	core.Mailbox
	core.MailDraftWriter
	core.MailDraftReferenceFinder
	dispatches int
	saved      core.Object
}

func (m *droppedDraftReply) SaveMailDraft(ctx context.Context, id core.ID, revision core.Revision, raw []byte) (core.Object, error) {
	m.dispatches++
	saved, err := m.MailDraftWriter.SaveMailDraft(ctx, id, revision, raw)
	if err != nil {
		return core.Object{}, err
	}
	m.saved = saved
	return core.Object{}, core.ErrOutcomeUnknown
}

func (m *droppedDraftReply) FindMailDraftsByReference(context.Context, core.ID, string) ([]core.Object, error) {
	return nil, core.ErrNotFound // Also lose discovery on the first request.
}

func verifyLostDraftReply(t *testing.T, ctx context.Context, session core.Session, folder core.ID, reference string, mime []byte) core.Object {
	t.Helper()
	path := filepath.Join(t.TempDir(), "draft.sqlite")
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = store.Close() }()
	backend := session.Mailbox()
	loss := &droppedDraftReply{
		Mailbox: backend, MailDraftWriter: backend.(core.MailDraftWriter),
		MailDraftReferenceFinder: backend.(core.MailDraftReferenceFinder),
	}
	creator := MailDraftCreator{store, "draft-test"}
	wrapped := draftTestSession{Session: session, box: loss}
	if _, err := creator.CreateMailDraft(ctx, wrapped, folder, reference, mime); !errors.Is(err, core.ErrOutcomeUnknown) {
		t.Fatalf("lost native reply: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	store, creator.store = reopened, reopened
	writer := &countedDraftWrites{MailDraftWriter: loss, Mailbox: backend, MailDraftReferenceFinder: backend.(core.MailDraftReferenceFinder)}
	account := draftTestSession{Session: session, box: writer}
	altered := append(bytes.Clone(mime), 'x')
	if _, err := creator.CreateMailDraft(ctx, account, folder, reference, altered); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("reused reference with different MIME: %v", err)
	}
	for range 2 {
		recovered, err := creator.CreateMailDraft(ctx, account, folder, reference, mime)
		if err != nil || recovered.ID == "" || recovered.ID != loss.saved.ID || recovered.Revision != loss.saved.Revision {
			t.Fatalf("draft did not reconcile after reopening the store: %v", err)
		}
	}
	if loss.dispatches != 1 {
		t.Fatalf("expected one native save, got %d", loss.dispatches)
	}
	return loss.saved
}

type countedDraftWrites struct {
	core.Mailbox
	core.MailDraftWriter
	core.MailDraftReferenceFinder
}

type draftFixtureMailbox struct {
	core.Mailbox
	match []core.Object
	raw   []byte
}

func (m *draftFixtureMailbox) Get(_ context.Context, id core.ID) (core.Object, error) {
	return core.Object{ID: id, Kind: core.Folder, Properties: core.Properties{core.WellKnown: core.Text("drafts")}}, nil
}

func (m *draftFixtureMailbox) SaveMailDraft(_ context.Context, id core.ID, revision core.Revision, raw []byte) (core.Object, error) {
	if id != "" || revision != 0 || mailmime.DraftReference(raw) == "" {
		return core.Object{}, core.ErrInvalid
	}
	m.raw = bytes.Clone(raw)
	saved := core.Object{ID: "first-draft", Kind: core.Message, Parent: "drafts", Revision: 1, Properties: core.Properties{core.IsDraft: core.Bool(true)}}
	m.match = append(m.match, saved)
	return saved, nil
}

func (m *draftFixtureMailbox) FindMailDraftsByReference(_ context.Context, folder core.ID, reference string) ([]core.Object, error) {
	if folder == "drafts" && mailmime.DraftReference(m.raw) == reference {
		return m.match, nil
	}
	return nil, nil
}

type draftFixtureSession struct {
	core.Session
	box *draftFixtureMailbox
}

func (s draftFixtureSession) Identity() core.Identity {
	return core.Identity{Scope: core.Scope{Backend: "fixture", Account: "a"}}
}

func (s draftFixtureSession) Mailbox() core.Mailbox { return s.box }

func TestMailDraftReconcilesAfterLostReplyAndRestart(t *testing.T) {
	session := draftFixtureSession{box: &draftFixtureMailbox{}}
	verifyLostDraftReply(t, t.Context(), session, "drafts", "000102030405060708090a0b0c0d0e0f", []byte("Subject: fixture\r\n\r\nPlain fixture\r\n"))
}

func TestConfirmedDraftCreationRetryAfterRestartAndRemoval(t *testing.T) {
	const reference = "100102030405060708090a0b0c0d0e0f"
	mime := []byte("Subject: fixture\r\n\r\nDraft\r\n")
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "retention.sqlite")
	store, err := sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	box := &draftFixtureMailbox{}
	account := draftFixtureSession{box: box}
	creator := MailDraftCreator{store, "retention-test"}
	saved, err := creator.CreateMailDraft(ctx, account, "drafts", reference, mime)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlite.Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	creator.store = store
	retry, err := creator.CreateMailDraft(ctx, account, "drafts", reference, mime)
	if err != nil || retry.ID != saved.ID || len(box.match) != 1 {
		t.Fatalf("confirmed draft retry returned %v with %d candidates", err, len(box.match))
	}
	box.match = nil
	if _, err := creator.CreateMailDraft(ctx, account, "drafts", reference, mime); !errors.Is(err, core.ErrOutcomeUnknown) || len(box.match) != 0 {
		t.Fatalf("removed draft retry returned %v with %d candidates", err, len(box.match))
	}
}
