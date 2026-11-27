// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestJournalMaintenanceKeepsRecentWindow(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "retention.sqlite")
	store, mailbox := openJournalFixture(t, path)
	const headRevision = journalRetainedRevisions + journalCompactionBatch + 2
	if _, err := store.db.ExecContext(ctx, `
		WITH RECURSIVE sequence(n) AS (
			VALUES (2) UNION ALL SELECT n+1 FROM sequence WHERE n < ?
		)
		INSERT INTO journal (mailbox_id, revision, kind, object_id, parent_id, previous_parent_id)
		SELECT ?, n, ?, 'changing', 'inbox', '' FROM sequence
	`, headRevision, mailbox, core.Updated); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO journal (mailbox_id, revision, kind, object_id, parent_id, previous_parent_id)
		VALUES (?, 1, ?, 'untouched', 'inbox', '')
	`, mailbox, core.Created); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(ctx, "UPDATE mailboxes SET journal_revision = ? WHERE mailbox_id = ?", headRevision, mailbox); err != nil {
		t.Fatal(err)
	}
	head, err := store.JournalHead(ctx, mailbox)
	if err != nil {
		t.Fatal(err)
	}
	for _, cut := range []core.Revision{journalCompactionBatch, headRevision - journalRetainedRevisions} {
		if err := store.MaintainJournal(ctx); err != nil {
			t.Fatal(err)
		}
		if _, err := store.Journal(ctx, mailbox, JournalCursor{head.Generation, cut - 1}, 1); !errors.Is(err, core.ErrResync) {
			t.Fatalf("stale cursor at %d: %v", cut-1, err)
		}
		page, err := store.Journal(ctx, mailbox, JournalCursor{head.Generation, cut}, 1)
		if err != nil || len(page.Changes) != 1 || page.Next.Revision != cut+1 {
			t.Fatalf("resume at cutoff: %+v, %v", page, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, _ = openJournalFixture(t, path)
	if _, revision, err := store.ObjectProperties(ctx, mailbox, "untouched"); err != nil || revision != 1 {
		t.Fatalf("untouched object after restart: %d, %v", revision, err)
	}
	page, err := store.Journal(ctx, mailbox, head, 1)
	if err != nil || len(page.Changes) != 0 || page.Next != head {
		t.Fatalf("current cursor after restart: %+v, %v", page, err)
	}
}

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

	binding := NativeBinding{Kind: core.Message, Reference: "old-native"}
	create := DeltaEvent{Change: core.Change{Kind: core.Created, ID: "recreated", Parent: "inbox"}, Binding: &binding}
	if _, err := store.ApplyDelta(ctx, mailbox, native, "recreated", []DeltaEvent{create}); err != nil {
		t.Fatal(err)
	}
	if id, err := store.ResolveNativeBinding(ctx, mailbox, binding); err != nil || id != "recreated" {
		t.Fatalf("recycled native reference resolved to %s, %v", id, err)
	}
	if _, _, err := store.ObjectProperties(ctx, mailbox, "deleted"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("published old identity became live after recreation: %v", err)
	}
	replay, err := store.Journal(ctx, mailbox, cutoff, 10)
	if err != nil || len(replay.Changes) != 4 || replay.Changes[3].ID != "recreated" || replay.Changes[3].Kind != core.Created {
		t.Fatalf("unexpected journal replay after recreation: %+v, %v", replay, err)
	}
}

func TestNativeBindingTombstoneStorageGrowth(t *testing.T) {
	ctx := t.Context()
	store, mailbox := openJournalFixture(t, filepath.Join(t.TempDir(), "binding-growth.sqlite"))
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO native_bindings (mailbox_id, object_kind, native_ref, object_id)
		VALUES (?, ?, 'native:live', 'opaque:live')
	`, mailbox, core.Message); err != nil {
		t.Fatal(err)
	}
	var baseline, size int64
	sizeSQL := "SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()"
	if err := store.db.QueryRowContext(ctx, sizeSQL).Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	next := 1
	for _, total := range []int{0, 1000, 10000} {
		if total != 0 {
			if _, err := store.db.ExecContext(ctx, `
				WITH RECURSIVE seq(n) AS (VALUES (?) UNION ALL SELECT n+1 FROM seq WHERE n < ?)
				INSERT INTO native_bindings
				SELECT ?, ?, printf('native:%08x', n), printf('opaque:%032x', n), n FROM seq
			`, next, total, mailbox, core.Message); err != nil {
				t.Fatal(err)
			}
			next = total + 1
		}
		if err := store.db.QueryRowContext(ctx, sizeSQL).Scan(&size); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		const lookups = 200
		binding := NativeBinding{Kind: core.Message, Reference: "native:live"}
		for range lookups {
			id, err := store.ResolveNativeBinding(ctx, mailbox, binding)
			if err != nil || id != "opaque:live" {
				t.Fatalf("live binding changed: %s, %v", id, err)
			}
		}
		t.Logf("%d retired bindings: %d bytes above baseline, %v per live lookup", total, size-baseline, time.Since(start)/lookups)
		if total > 0 {
			binding.Reference = fmt.Sprintf("native:%08x", total)
			if _, err := store.ResolveNativeBinding(ctx, mailbox, binding); !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("retired binding resolved: %v", err)
			}
		}
	}
}
