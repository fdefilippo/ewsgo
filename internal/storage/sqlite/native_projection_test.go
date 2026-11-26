// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"errors"
	"path/filepath"
	"testing"

	"ewsgo/internal/core"
)

func TestProjectionAndJournalCommitOrRollbackTogether(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "projection.sqlite")
	store, mailbox := openJournalFixture(t, path)
	p, err := store.OpenNativeProjection(ctx, core.Scope{Backend: "fixture", Account: "journal-test"}, "one")
	if err != nil {
		t.Fatal(err)
	}
	first := core.NativeObject{ID: "first", Kind: core.Message, Parent: "inbox", Revision: 8}
	second := core.NativeObject{ID: "second", Kind: core.Message, Parent: "inbox", Revision: 8}
	if err := p.Apply(ctx, core.NativeCheckpoint{}, "seed", []core.NativeObject{first, second}, nil); err != nil {
		t.Fatal(err)
	}
	checkpoint, err := p.Checkpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	patch := core.Patch{Set: core.Properties{{ID: 1}: core.Bool(true)}}
	if _, err := store.PatchObjectProperties(ctx, mailbox, second.ID, checkpoint.Revision, patch); err != nil {
		t.Fatal(err)
	}
	head, err := store.JournalHead(ctx, mailbox)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER reject_deleted BEFORE INSERT ON journal
		WHEN NEW.kind = 4 BEGIN SELECT RAISE(ABORT, 'fixture rollback'); END;
	`); err != nil {
		t.Fatal(err)
	}
	moved := first
	moved.Parent, moved.Revision = "dest", 11
	if err := p.Apply(ctx, checkpoint, "next", []core.NativeObject{moved}, []core.ID{second.ID}); err == nil {
		t.Fatal("an injected journal failure must fail the whole page")
	}
	members, err := p.Members(ctx, checkpoint, []core.ID{first.ID, second.ID})
	if err != nil || members[first.ID] != first || members[second.ID] != second {
		t.Fatal("journal failure left a partial membership change")
	}
	if got, err := store.JournalHead(ctx, mailbox); err != nil || got != head {
		t.Fatal("journal failure advanced its position")
	}
	if props, _, err := store.ObjectProperties(ctx, mailbox, second.ID); err != nil || props[core.Key{ID: 1}] != core.Bool(true) {
		t.Fatal("journal failure lost gateway properties")
	}
	if _, err := store.db.ExecContext(ctx, "DROP TRIGGER reject_deleted"); err != nil {
		t.Fatal(err)
	}
	if err := p.Apply(ctx, checkpoint, "next", []core.NativeObject{moved}, []core.ID{second.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := p.Members(ctx, checkpoint, []core.ID{second.ID}); !errors.Is(err, core.ErrConflict) {
		t.Fatal("membership lookup accepted a stale checkpoint")
	}
	if err := p.Apply(ctx, checkpoint, "next", []core.NativeObject{moved}, nil); !errors.Is(err, core.ErrConflict) {
		t.Fatal("stale writer replaced a committed native page")
	}
	next, err := p.Checkpoint(ctx)
	if err != nil || next.Cursor != "next" || next.Revision != head.Revision+2 {
		t.Fatal("committed checkpoint does not match the journal")
	}
	if _, err := store.ApplyDelta(ctx, mailbox, next, "bypass", nil); !errors.Is(err, core.ErrConflict) {
		t.Fatal("legacy delta API bypassed membership")
	}
	events, err := store.Journal(ctx, mailbox, head, 10)
	if err != nil || len(events.Changes) != 2 ||
		events.Changes[0] != (core.Change{ID: first.ID, Kind: core.Moved, Parent: moved.Parent, PreviousParent: first.Parent, Revision: head.Revision + 1}) ||
		events.Changes[1] != (core.Change{ID: second.ID, Kind: core.Deleted, PreviousParent: second.Parent, Revision: next.Revision}) {
		t.Fatal("move and delete were not journaled with previous membership")
	}
	if err := p.Apply(ctx, next, "repeat", []core.NativeObject{moved}, []core.ID{second.ID}); err != nil {
		t.Fatal(err)
	}
	repeat, err := p.Checkpoint(ctx)
	if err != nil || repeat.Cursor != "repeat" || repeat.Revision != next.Revision {
		t.Fatal("duplicate observations advanced the local revision")
	}
	if err := p.Apply(ctx, repeat, "backward", []core.NativeObject{first}, nil); !errors.Is(err, core.ErrConflict) {
		t.Fatal("regressive native observation accepted")
	}
	archive := filepath.Join(t.TempDir(), "archive.sqlite")
	if err := Backup(ctx, path, archive); err != nil {
		t.Fatal(err)
	}
	restoredPath := filepath.Join(t.TempDir(), "restored.sqlite")
	if err := Restore(ctx, archive, restoredPath); err != nil {
		t.Fatal(err)
	}
	restored, restoredMailbox := openJournalFixture(t, restoredPath)
	if restoredMailbox != mailbox {
		t.Fatal("restore changed the mailbox scope")
	}
	p, err = restored.OpenNativeProjection(ctx, core.Scope{Backend: "fixture", Account: "journal-test"}, "one")
	if err != nil {
		t.Fatal(err)
	}
	members, err = p.Members(ctx, repeat, []core.ID{first.ID, second.ID})
	if err != nil || len(members) != 1 || members[first.ID] != moved {
		t.Fatal("restored checkpoint and membership disagree")
	}
}

func TestLegacyCheckpointRequiresProjectionReconciliation(t *testing.T) {
	ctx := t.Context()
	store, mailbox := openJournalFixture(t, filepath.Join(t.TempDir(), "legacy.sqlite"))
	if _, err := store.ApplyDelta(ctx, mailbox, DeltaCheckpoint{}, "legacy", nil); err != nil {
		t.Fatal(err)
	}
	p, err := store.OpenNativeProjection(ctx, core.Scope{Backend: "fixture", Account: "journal-test"}, "one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.Checkpoint(ctx); !errors.Is(err, core.ErrResync) {
		t.Fatal("legacy checkpoint accepted without baseline")
	}
	if err := p.Apply(ctx, core.NativeCheckpoint{}, "replaced", nil, nil); !errors.Is(err, core.ErrResync) {
		t.Fatal("legacy projection was silently replaced")
	}
	legacy, err := p.Baseline(ctx)
	if err != nil || !legacy.Legacy {
		t.Fatal("reconciliation did not read legacy state")
	}
	if _, err := store.ApplyDelta(ctx, mailbox, legacy.Checkpoint, "advanced", nil); err != nil {
		t.Fatal(err)
	}
	if err := p.Reconcile(ctx, legacy, "replacement", nil, nil); !errors.Is(err, core.ErrConflict) {
		t.Fatal("stale reconciliation overwrote a committed page")
	}
	current, err := p.Baseline(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// A quiet mailbox can be reconciled at the same cursor with no changes.
	if err := p.Reconcile(ctx, current, current.Checkpoint.Cursor, nil, nil); err != nil {
		t.Fatal(err)
	}
	if checkpoint, err := p.Checkpoint(ctx); err != nil || checkpoint != current.Checkpoint {
		t.Fatal("promoting a quiet legacy mailbox changed its checkpoint or revision")
	}
	if err := p.Reconcile(ctx, current, "stale", nil, nil); !errors.Is(err, core.ErrConflict) {
		t.Fatal("reconciliation failed to compare checkpoint mode")
	}
}
