// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"errors"
	"path/filepath"
	"testing"

	"ewsgo/internal/core"
)

func TestJournalCompactionPreservesLiveObjectsAndResyncBoundary(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "history.sqlite")
	store, mailbox := openJournalFixture(t, path)
	before, err := store.JournalHead(ctx, mailbox)
	if err != nil {
		t.Fatal(err)
	}
	events := []DeltaEvent{
		{Change: core.Change{Kind: core.Created, ID: "live", Parent: "inbox"}},
		{Change: core.Change{Kind: core.Moved, ID: "live", Parent: "archive", PreviousParent: "inbox"}},
		{Change: core.Change{Kind: core.Created, ID: "deleted", Parent: "inbox"}, Binding: &NativeBinding{Kind: core.Message, Reference: "old-native"}},
		{Change: core.Change{Kind: core.Deleted, ID: "deleted", PreviousParent: "inbox"}},
		{Change: core.Change{Kind: core.Created, ID: "retained", Parent: "inbox"}},
	}
	native, err := store.ApplyDelta(ctx, mailbox, DeltaCheckpoint{}, "initial", events)
	if err != nil {
		t.Fatal(err)
	}
	cutoff := JournalCursor{Generation: before.Generation, Revision: 4}
	key := core.Key{ID: 10}
	patch := core.Patch{Set: core.Properties{key: core.Bool(true)}}
	if _, err := store.PatchObjectProperties(ctx, mailbox, "retained", native.Revision, patch); err != nil {
		t.Fatal(err)
	}
	other, err := store.BindMailbox(ctx, core.Scope{Backend: "fixture", Account: "another"}, "one")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyDelta(ctx, other.MailboxID, DeltaCheckpoint{}, "initial", events); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Journal(ctx, other.MailboxID, cutoff, 10); !errors.Is(err, core.ErrResync) {
		t.Fatalf("accepted cursor belonging to another mailbox: %v", err)
	}
	if _, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER fail_floor BEFORE UPDATE OF journal_floor ON mailboxes
		BEGIN SELECT RAISE(ABORT, 'fixture compaction failure'); END;
	`); err != nil {
		t.Fatal(err)
	}
	if err := store.CompactJournal(ctx, mailbox, cutoff); err == nil {
		t.Fatal("expected compaction to fail")
	}
	if page, err := store.Journal(ctx, mailbox, before, 10); err != nil || len(page.Changes) != 6 {
		t.Fatalf("failed compaction changed history or floor: %+v, %v", page, err)
	}
	if _, err := store.db.ExecContext(ctx, "DROP TRIGGER fail_floor"); err != nil {
		t.Fatal(err)
	}
	if err := store.CompactJournal(ctx, mailbox, cutoff); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, _ = openJournalFixture(t, path)
	if err := store.CompactJournal(ctx, mailbox, cutoff); err != nil {
		t.Fatalf("repeated compaction: %v", err)
	}
	for _, stale := range []JournalCursor{{}, before, {Generation: before.Generation, Revision: 7}} {
		if _, err := store.Journal(ctx, mailbox, stale, 10); !errors.Is(err, core.ErrResync) {
			t.Fatalf("unusable cursor accepted: %+v: %v", stale, err)
		}
	}
	page, err := store.Journal(ctx, mailbox, cutoff, 10)
	if err != nil || len(page.Changes) != 2 || page.Changes[0].Revision != 5 || page.Next.Revision != 6 {
		t.Fatalf("resume exactly at floor: %+v, %v", page, err)
	}
	if page, err := store.Journal(ctx, other.MailboxID, JournalCursor{}, 10); err != nil || len(page.Changes) != 5 {
		t.Fatalf("other mailbox lost history: %+v, %v", page, err)
	}
	var retainedRows, deletedBindings int
	if err := store.db.QueryRowContext(ctx, "SELECT count(*) FROM journal WHERE mailbox_id = ?", mailbox).Scan(&retainedRows); err != nil || retainedRows != 3 {
		t.Fatalf("compaction retained obsolete rows: %d, %v", retainedRows, err)
	}
	if err := store.db.QueryRowContext(ctx, "SELECT count(*) FROM native_bindings WHERE mailbox_id = ? AND deleted_revision = 4", mailbox).Scan(&deletedBindings); err != nil || deletedBindings != 1 {
		t.Fatalf("compaction discarded native binding tombstone: %d, %v", deletedBindings, err)
	}
	if _, _, err := store.ObjectProperties(ctx, mailbox, "deleted"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("compaction resurrected deleted object: %v", err)
	}
	rev, err := store.PatchObjectProperties(ctx, mailbox, "live", 2, patch)
	if err != nil || rev != 7 {
		t.Fatalf("compaction lost live object revision: %d, %v", rev, err)
	}
	next, err := store.Journal(ctx, mailbox, page.Next, 10)
	if err != nil || len(next.Changes) != 1 || next.Changes[0].Parent != "archive" {
		t.Fatalf("compaction lost current parent: %+v, %v", next, err)
	}
	if after, err := store.DeltaCheckpoint(ctx, mailbox); err != nil || after != native {
		t.Fatalf("compaction changed native checkpoint: %+v, %v", after, err)
	}
}
