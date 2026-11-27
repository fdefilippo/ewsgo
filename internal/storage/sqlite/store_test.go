// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestBindMailboxSurvivesReopenAndIsolatesScopes(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "mailbox #1?.sqlite")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	scope := core.Scope{Backend: "fixture", Account: "native-account"}
	initial, bindErr := store.BindMailbox(ctx, scope, "primary")
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if bindErr != nil || initial.MailboxID == "" || initial.ReplicaID == "" || initial.MailboxID == initial.ReplicaID {
		t.Fatalf("initial binding: %+v, %v", initial, bindErr)
	}
	store, err = Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	reopened, err := store.BindMailbox(ctx, scope, "primary")
	if err != nil || reopened != initial {
		t.Fatalf("reopened binding: %+v, %v, want %+v", reopened, err, initial)
	}
	for _, candidate := range []struct {
		scope    core.Scope
		instance string
	}{
		{scope, "secondary"},
		{core.Scope{Backend: scope.Backend, Account: "another-account"}, "primary"},
		{core.Scope{Backend: "another-backend", Account: scope.Account}, "primary"},
	} {
		other, err := store.BindMailbox(ctx, candidate.scope, candidate.instance)
		if err != nil || other.MailboxID == initial.MailboxID || other.ReplicaID == initial.ReplicaID {
			t.Fatalf("independent binding for %+v: %+v, %v", candidate, other, err)
		}
	}
	for _, invalid := range []struct {
		scope    core.Scope
		instance string
	}{
		{scope, ""},
		{core.Scope{Backend: scope.Backend}, "primary"},
		{core.Scope{Account: scope.Account}, "primary"},
	} {
		if _, err := store.BindMailbox(ctx, invalid.scope, invalid.instance); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("invalid binding for %+v: got %v, want ErrInvalid", invalid, err)
		}
	}
}

func TestConcurrentBinding(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "concurrent.sqlite")
	var stores [2]*Store
	for i := range stores {
		store, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		stores[i] = store
	}
	const callers = 12
	var identities [callers]MailboxIdentity
	var errs [callers]error
	var wg sync.WaitGroup
	start := make(chan struct{})
	for i := range identities {
		wg.Go(func() {
			<-start
			scope := core.Scope{Backend: "fixture", Account: "same-account"}
			identities[i], errs[i] = stores[i%len(stores)].BindMailbox(ctx, scope, "shared")
		})
	}
	close(start)
	wg.Wait()
	for i, identity := range identities {
		if errs[i] != nil || identity.MailboxID == "" || identity.ReplicaID == "" || identity != identities[0] {
			t.Errorf("binding %d: %+v, %v, want %+v", i, identity, errs[i], identities[0])
		}
	}
}

func TestKnownMailboxBindingWhileAnotherTransactionOwnsWriter(t *testing.T) {
	ctx := t.Context()
	store, err := Open(ctx, filepath.Join(t.TempDir(), "concurrent-read.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := core.Scope{Backend: "fixture", Account: "known"}
	want, err := store.BindMailbox(ctx, scope, "primary")
	if err != nil {
		t.Fatal(err)
	}
	writer, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer writer.Rollback()

	completed := make(chan bool, 1)
	go func() {
		identity, err := store.BindMailbox(ctx, scope, "primary")
		completed <- err == nil && identity == want
	}()
	timeout, cancel := context.WithTimeout(ctx, 2*time.Second)
	defer cancel()
	select {
	case ok := <-completed:
		if !ok {
			t.Fatal("read did not resolve the existing identity")
		}
	case <-timeout.Done():
		t.Fatal("read waited for the writer")
	}
}

func TestSchemaVersionAndConnectionSettings(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "versioned.sqlite")
	store, err := Open(ctx, path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	// Reserve both connections together so the settings are checked on a new
	// pool connection as well as the one that ran the migration.
	for range 2 {
		conn, err := store.db.Conn(ctx)
		if err != nil {
			t.Fatal(err)
		}
		defer conn.Close()
		var version, foreignKeys, synchronous int
		var journalMode string
		err = conn.QueryRowContext(ctx, `
			SELECT user_version, foreign_keys, synchronous, journal_mode
			FROM pragma_user_version(), pragma_foreign_keys(),
			     pragma_synchronous(), pragma_journal_mode()
		`).Scan(&version, &foreignKeys, &synchronous, &journalMode)
		if err != nil || version != schemaVersion || foreignKeys != 1 || synchronous != 2 || journalMode != "wal" {
			t.Fatalf("SQLite settings: schema=%d foreign_keys=%d synchronous=%d journal_mode=%s, %v",
				version, foreignKeys, synchronous, journalMode, err)
		}
	}
	if _, err := store.db.ExecContext(ctx, "PRAGMA user_version = 99"); err != nil {
		t.Fatal(err)
	}
	other, err := Open(ctx, path)
	if other != nil {
		_ = other.Close()
	}
	if err == nil || !strings.Contains(err.Error(), "unsupported SQLite schema version 99") {
		t.Fatalf("opening future schema: %v", err)
	}
	var version int
	if err := store.db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil || version != 99 {
		t.Fatalf("future schema after refused open: %d, %v", version, err)
	}
}

func TestOpenMigratesVersionOneAndPreservesBindings(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "version-one.sqlite")
	store, mailbox := openJournalFixture(t, path)
	// Leave only the tables and columns that existed at v1.
	if _, err := store.db.ExecContext(ctx, `
		DROP TABLE mapi_message_moves;
		DROP TABLE mapi_message_imports;
		DROP TABLE mapi_upload_proofs;
		DROP TABLE mapi_foreign_replicas;
		DROP TABLE mapi_folder_creations;
		DROP TABLE mapi_folder_values;
		DROP TABLE mapi_draft_placements;
		DROP TABLE mapi_staged_ids;
		DROP TABLE mapi_fai_tombstones;
		DROP TABLE mapi_fai_objects;
		DROP TABLE mapi_projection_versions;
		DROP TABLE mapi_contents_proofs;
		DROP TABLE mapi_local_id_ranges;
		DROP TABLE mapi_object_ids;
		DROP TABLE mapi_replicas;
		DROP TABLE mail_draft_creations;
		DROP TABLE draft_string_properties;
		DROP TABLE store_keys;
		DROP TABLE send_intents;
		DROP TABLE native_projection;
		DROP TABLE named_property_ids;
		DROP TABLE object_properties;
		ALTER TABLE mailboxes DROP COLUMN journal_floor;
		ALTER TABLE mailboxes DROP COLUMN journal_generation;
		ALTER TABLE mailboxes DROP COLUMN journal_revision;
		DROP TABLE journal;
		DROP TABLE delta_checkpoints;
		DROP TABLE native_bindings;
		DROP TABLE mapi_spooler;
		DROP TABLE mapi_public_read_state;
		DROP TABLE mapi_permission_members;
		PRAGMA user_version = 1;
	`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	migrated, migratedMailbox := openJournalFixture(t, path)
	if migratedMailbox != mailbox {
		t.Fatal("version one mailbox binding changed during migration")
	}
	checkpoint, err := migrated.DeltaCheckpoint(ctx, mailbox)
	if err != nil || checkpoint != (DeltaCheckpoint{}) {
		t.Fatalf("expected usable journal after migration: %+v, %v", checkpoint, err)
	}
}
