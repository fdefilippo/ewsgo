// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestBackupAndRestoreWithConcurrentWALWriter(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	source := filepath.Join(dir, "live.sqlite")
	archive := filepath.Join(dir, "archive.sqlite")
	restored := filepath.Join(dir, "restored.sqlite")
	store, mailbox := openJournalFixture(t, source)
	scope := core.Scope{Backend: "fixture", Account: "journal-test"}
	identity, err := store.BindMailbox(ctx, scope, "one")
	if err != nil {
		t.Fatal(err)
	}

	// A read snapshot taken before these writes pins the WAL so the committed
	// state used by the backup cannot all be present in the main file yet.
	pinned, err := store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer pinned.Rollback()
	var count int
	if err := pinned.QueryRowContext(ctx, "SELECT count(*) FROM journal").Scan(&count); err != nil || count != 0 {
		t.Fatalf("pin initial WAL position: %d, %v", count, err)
	}

	binding := NativeBinding{Kind: core.Message, Reference: "native"}
	created := core.Change{Kind: core.Created, ID: "message", Parent: "inbox"}
	first, err := store.ApplyDelta(ctx, mailbox, DeltaCheckpoint{}, "first", []DeltaEvent{{Change: created, Binding: &binding}})
	if err != nil {
		t.Fatal(err)
	}
	key := core.Key{Set: testPropertySet, Name: "custom"}
	ids, err := store.NamedPropertyIDs(ctx, mailbox, []core.Key{key}, true)
	if err != nil {
		t.Fatal(err)
	}
	patch := core.Patch{Set: core.Properties{key: core.Text("before backup")}}
	local, err := store.PatchObjectProperties(ctx, mailbox, created.ID, first.Revision, patch)
	if err != nil {
		t.Fatal(err)
	}
	moved := core.Change{Kind: core.Moved, ID: created.ID, Parent: "archive", PreviousParent: "inbox"}
	checkpoint, err := store.ApplyDelta(ctx, mailbox, first, "before-backup", []DeltaEvent{{Change: moved}})
	if err != nil {
		t.Fatal(err)
	}
	oldHead, err := store.JournalHead(ctx, mailbox)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompactJournal(ctx, mailbox, JournalCursor{Generation: oldHead.Generation, Revision: local}); err != nil {
		t.Fatal(err)
	}

	writer, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()
	uncommitted := core.Change{Kind: core.Created, ID: "live-only"}
	liveCheckpoint, err := writeDelta(ctx, writer, mailbox, checkpoint, "after-backup", []DeltaEvent{{Change: uncommitted}}, false)
	if err != nil {
		t.Fatal(err)
	}
	backupCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	// The writer remains open for the entire backup. Its uncommitted page
	// must be absent and the previously committed WAL pages must be present.
	if err := Backup(backupCtx, source, archive); err != nil {
		t.Fatal(err)
	}
	if err := writer.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := pinned.Rollback(); err != nil {
		t.Fatal(err)
	}
	liveHead, err := store.JournalHead(ctx, mailbox)
	if err != nil || liveHead.Revision != liveCheckpoint.Revision {
		t.Fatalf("original store stopped advancing: %+v, %v", liveHead, err)
	}

	beforeArchive, err := os.ReadFile(archive)
	if err != nil {
		t.Fatal(err)
	}
	if err := Restore(ctx, archive, restored); err != nil {
		t.Fatal(err)
	}
	afterArchive, err := os.ReadFile(archive)
	if err != nil || !bytes.Equal(beforeArchive, afterArchive) {
		t.Fatalf("restore modified archive: %v", err)
	}
	for _, path := range []string{archive, restored} {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0o600 {
			t.Fatalf("copy permission for %s: %s", path, info.Mode())
		}
		assertStoreFileAbsent(t, path+"-wal")
	}

	reopened, _ := openJournalFixture(t, restored)
	recoveredIdentity, err := reopened.BindMailbox(ctx, scope, "one")
	if err != nil || recoveredIdentity != identity {
		t.Fatalf("restore changed mailbox or replica identity: %+v, %v", recoveredIdentity, err)
	}
	if saved, err := reopened.DeltaCheckpoint(ctx, mailbox); err != nil || saved != checkpoint {
		t.Fatalf("restored native checkpoint: %+v, %v", saved, err)
	}
	if id, err := reopened.ResolveNativeBinding(ctx, mailbox, binding); err != nil || id != created.ID {
		t.Fatalf("restored native binding: %q, %v", id, err)
	}
	if assigned, err := reopened.NamedPropertyIDs(ctx, mailbox, []core.Key{key}, false); err != nil || !slices.Equal(assigned, ids) {
		t.Fatalf("restored named ID: %v, %v", assigned, err)
	}
	properties, revision, err := reopened.ObjectProperties(ctx, mailbox, created.ID)
	if err != nil || revision != checkpoint.Revision || properties[key] != core.Text("before backup") {
		t.Fatalf("restored property: %v at %d, %v", properties, revision, err)
	}
	if _, _, err := reopened.ObjectProperties(ctx, mailbox, uncommitted.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("uncommitted page escaped into backup: %v", err)
	}
	newHead, err := reopened.JournalHead(ctx, mailbox)
	if err != nil || newHead.Revision != oldHead.Revision || newHead.Generation == oldHead.Generation {
		t.Fatalf("restore must retain revision with new generation: %+v, %v", newHead, err)
	}
	for _, cursor := range []JournalCursor{oldHead, liveHead, {Generation: newHead.Generation, Revision: local - 1}} {
		if _, err := reopened.Journal(ctx, mailbox, cursor, 10); !errors.Is(err, core.ErrResync) {
			t.Fatalf("restored store accepted stale cursor %+v: %v", cursor, err)
		}
	}

	patch.Set[key] = core.Text("after restore")
	patched, err := reopened.PatchObjectProperties(ctx, mailbox, created.ID, revision, patch)
	if err != nil || patched != liveHead.Revision {
		t.Fatalf("write after restore with a reused numeric revision: %d, %v", patched, err)
	}
	if _, err := reopened.Journal(ctx, mailbox, liveHead, 10); !errors.Is(err, core.ErrResync) {
		t.Fatalf("old cursor accepted after its number was reached again: %v", err)
	}
	next, err := reopened.ApplyDelta(ctx, mailbox, checkpoint, "replayed-native-page", []DeltaEvent{{Change: uncommitted}})
	if err != nil || next.Revision != patched+1 {
		t.Fatalf("resume native feed from backup checkpoint: %+v, %v", next, err)
	}
	page, err := reopened.Journal(ctx, mailbox, newHead, 10)
	if err != nil || len(page.Changes) != 2 || page.Changes[0].Parent != moved.Parent || page.Next.Revision != next.Revision {
		t.Fatalf("restored write stream: %+v, %v", page, err)
	}
}

func TestDatabaseCopiesRejectExistingDestinationsAndInvalidSources(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	source := filepath.Join(dir, "source.sqlite")
	openJournalFixture(t, source)
	destination := filepath.Join(dir, "copy.sqlite")
	sidecar := destination + "-wal"
	if err := os.WriteFile(sidecar, []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Restore(ctx, source, destination); !errors.Is(err, os.ErrExist) {
		t.Fatalf("orphaned sidecar: %v", err)
	}
	if err := os.Remove(sidecar); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(destination, []byte("keep"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, source, destination); !errors.Is(err, os.ErrExist) {
		t.Fatalf("existing destination: %v", err)
	}
	data, err := os.ReadFile(destination)
	if err != nil || string(data) != "keep" {
		t.Fatalf("existing destination changed: %q, %v", data, err)
	}

	missing := filepath.Join(dir, "does-not-exist.sqlite")
	unpublished := filepath.Join(dir, "unpublished.sqlite")
	if err := Backup(ctx, missing, unpublished); err == nil {
		t.Fatal("backup of missing input succeeded")
	}
	assertStoreFileAbsent(t, missing)
	assertStoreFileAbsent(t, unpublished)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if err := Restore(canceled, source, unpublished); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled restore: %v", err)
	}
	assertStoreFileAbsent(t, unpublished)
	if copies, err := filepath.Glob(filepath.Join(dir, ".ewsgo-store-*")); err != nil || len(copies) != 0 {
		t.Fatalf("partial copies remained: %v, %v", copies, err)
	}
}

func TestRestoreRejectsInconsistentArchive(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	archive := filepath.Join(dir, "inconsistent.sqlite")
	store, _ := openJournalFixture(t, archive)
	conn, err := store.db.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := conn.ExecContext(ctx, `
		PRAGMA foreign_keys = OFF;
		INSERT INTO journal (mailbox_id, revision, kind, object_id, parent_id, previous_parent_id)
		VALUES ('missing-mailbox', 1, 1, 'orphan', '', '');
	`); err != nil {
		t.Fatal(err)
	}
	destination := filepath.Join(dir, "restored.sqlite")
	if err := Restore(ctx, archive, destination); err == nil || !strings.Contains(err.Error(), "failed foreign key check") {
		t.Fatalf("restore did not reject orphaned data: %v", err)
	}
	assertStoreFileAbsent(t, destination)
}

func assertStoreFileAbsent(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("expected no file at %s: %v", path, err)
	}
}
