// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package sqlite stores the gateway's persistent mailbox state.
package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"
	"net/url"
	"os"
	"path/filepath"

	"ewsgo/internal/core"

	_ "modernc.org/sqlite"
)

const schemaVersion = 28

type Store struct {
	db *sql.DB
}

// Open opens a local database file. Its parent directory must already exist.
func Open(ctx context.Context, path string) (*Store, error) {
	if path == "" || path == ":memory:" {
		return nil, fmt.Errorf("SQLite requires a persistent database path: %w", core.ErrInvalid)
	}
	absolutePath, err := filepath.Abs(path)
	if err != nil {
		return nil, fmt.Errorf("resolve SQLite database path: %w", err)
	}
	file, err := os.OpenFile(absolutePath, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open SQLite database file: %w", err)
	}
	if err := file.Close(); err != nil {
		return nil, fmt.Errorf("close SQLite database file: %w", err)
	}

	db, err := openDatabase(absolutePath, "rw")
	if err != nil {
		return nil, err
	}
	if err := initialize(ctx, db); err != nil {
		_ = db.Close()
		return nil, err
	}
	return &Store{db: db}, nil
}

func openDatabase(absolutePath, mode string) (*sql.DB, error) {
	options := url.Values{
		"mode":    {mode},
		"_pragma": {"busy_timeout(5000)", "foreign_keys(ON)", "synchronous(FULL)"},
		"_txlock": {"immediate"},
	}
	dsn := url.URL{Scheme: "file", Path: filepath.ToSlash(absolutePath), RawQuery: options.Encode()}
	db, err := sql.Open("sqlite", dsn.String())
	if err != nil {
		return nil, fmt.Errorf("open SQLite store: %w", err)
	}
	return db, nil
}

func (s *Store) Close() error {
	return s.db.Close()
}

func initialize(ctx context.Context, db *sql.DB) error {
	if err := migrate(ctx, db); err != nil {
		return err
	}
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		return fmt.Errorf("set SQLite journal mode: %w", err)
	}
	if journalMode != "wal" {
		return fmt.Errorf("SQLite requires WAL journal mode, got %q", journalMode)
	}
	return nil
}

func migrate(ctx context.Context, db *sql.DB) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin SQLite migration: %w", err)
	}
	defer tx.Rollback()

	var version int
	if err := tx.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read SQLite schema version: %w", err)
	}
	switch version {
	case 0:
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE mailboxes (
				backend_kind TEXT NOT NULL,
				backend_instance TEXT NOT NULL,
				account TEXT NOT NULL,
				mailbox_id TEXT NOT NULL UNIQUE,
				replica_id TEXT NOT NULL UNIQUE,
				PRIMARY KEY (backend_kind, backend_instance, account)
			) STRICT;
			PRAGMA user_version = 1;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 1: %w", err)
		}
		fallthrough
	case 1:
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE delta_checkpoints (
				mailbox_id TEXT PRIMARY KEY REFERENCES mailboxes(mailbox_id),
				cursor TEXT NOT NULL,
				revision INTEGER NOT NULL CHECK (revision > 0)
			) STRICT;
			CREATE TABLE journal (
				mailbox_id TEXT NOT NULL REFERENCES mailboxes(mailbox_id),
				revision INTEGER NOT NULL CHECK (revision > 0),
				kind INTEGER NOT NULL CHECK (kind BETWEEN 1 AND 4),
				object_id TEXT NOT NULL,
				parent_id TEXT NOT NULL,
				previous_parent_id TEXT NOT NULL,
				PRIMARY KEY (mailbox_id, revision)
			) STRICT;
			CREATE TABLE native_bindings (
				mailbox_id TEXT NOT NULL REFERENCES mailboxes(mailbox_id),
				object_kind INTEGER NOT NULL CHECK (object_kind IN (1, 2)),
				native_ref TEXT NOT NULL,
				object_id TEXT NOT NULL,
				deleted_revision INTEGER,
				PRIMARY KEY (mailbox_id, object_id)
			) STRICT;
			CREATE UNIQUE INDEX native_binding_live_ref
				ON native_bindings (mailbox_id, object_kind, native_ref)
				WHERE deleted_revision IS NULL;
			PRAGMA user_version = 2;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 2: %w", err)
		}
		fallthrough
	case 2:
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE mailboxes ADD COLUMN journal_revision INTEGER NOT NULL DEFAULT 0 CHECK (journal_revision >= 0);
			UPDATE mailboxes SET journal_revision = COALESCE((
				SELECT revision FROM delta_checkpoints WHERE mailbox_id = mailboxes.mailbox_id
			), 0);
			CREATE INDEX journal_object_revision ON journal (mailbox_id, object_id, revision DESC);
			CREATE TABLE named_property_ids (
				mailbox_id TEXT NOT NULL REFERENCES mailboxes(mailbox_id),
				property_set TEXT NOT NULL,
				property_id INTEGER NOT NULL,
				property_name TEXT NOT NULL,
				empty_name INTEGER NOT NULL CHECK (empty_name IN (0, 1)),
				wire_id INTEGER NOT NULL CHECK (wire_id BETWEEN 32769 AND 65534),
				PRIMARY KEY (mailbox_id, property_set, property_id, property_name, empty_name),
				UNIQUE (mailbox_id, wire_id)
			) STRICT;
			CREATE TABLE object_properties (
				mailbox_id TEXT NOT NULL REFERENCES mailboxes(mailbox_id),
				object_id TEXT NOT NULL,
				property_set TEXT NOT NULL,
				property_id INTEGER NOT NULL,
				property_name TEXT NOT NULL,
				empty_name INTEGER NOT NULL CHECK (empty_name IN (0, 1)),
				value_type INTEGER NOT NULL CHECK (value_type BETWEEN 1 AND 6),
				value BLOB NOT NULL,
				PRIMARY KEY (mailbox_id, object_id, property_set, property_id, property_name, empty_name)
			) STRICT;
			PRAGMA user_version = 3;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 3: %w", err)
		}
		fallthrough
	case 3:
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE mailboxes ADD COLUMN journal_generation TEXT NOT NULL DEFAULT '';
			ALTER TABLE mailboxes ADD COLUMN journal_floor INTEGER NOT NULL DEFAULT 0
				CHECK (journal_floor >= 0 AND journal_floor <= journal_revision);
			UPDATE mailboxes SET journal_generation = replica_id;
			PRAGMA user_version = 4;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 4: %w", err)
		}
		fallthrough
	case 4:
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE delta_checkpoints ADD COLUMN projected INTEGER NOT NULL DEFAULT 0 CHECK (projected IN (0, 1));
			CREATE TABLE native_projection (
				mailbox_id TEXT NOT NULL REFERENCES mailboxes(mailbox_id),
				object_id TEXT NOT NULL CHECK (object_id != ''),
				object_kind INTEGER NOT NULL CHECK (object_kind IN (1, 2)),
				parent_id TEXT NOT NULL,
				native_revision INTEGER NOT NULL CHECK (native_revision > 0),
				PRIMARY KEY (mailbox_id, object_id)
			) STRICT;
			PRAGMA user_version = 5;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 5: %w", err)
		}
		fallthrough
	case 5:
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE send_intents (
				mailbox_id TEXT NOT NULL REFERENCES mailboxes(mailbox_id),
				intent_id TEXT NOT NULL CHECK (intent_id != ''),
				mime_digest BLOB NOT NULL CHECK (length(mime_digest) = 32),
				draft_id TEXT NOT NULL,
				draft_revision INTEGER NOT NULL CHECK (draft_revision >= 0),
				state INTEGER NOT NULL CHECK (state BETWEEN 1 AND 4),
				sent_id TEXT NOT NULL DEFAULT '',
				CHECK ((draft_id = '') = (draft_revision = 0)),
				CHECK (state = 3 OR sent_id = ''),
				PRIMARY KEY (mailbox_id, intent_id)
			) STRICT;
			PRAGMA user_version = 6;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 6: %w", err)
		}
		fallthrough
	case 6:
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE journal ADD COLUMN object_kind INTEGER NOT NULL DEFAULT 0 CHECK (object_kind IN (0, 1, 2));
			UPDATE journal SET object_kind = COALESCE(
				(SELECT object_kind FROM native_projection WHERE mailbox_id = journal.mailbox_id AND object_id = journal.object_id),
				(SELECT object_kind FROM native_bindings WHERE mailbox_id = journal.mailbox_id AND object_id = journal.object_id), 0
			);
			CREATE INDEX journal_kind_revision ON journal (mailbox_id, object_kind, revision);
			CREATE TABLE store_keys (
				name TEXT PRIMARY KEY,
				material BLOB NOT NULL CHECK (length(material) = 32)
			) STRICT;
			PRAGMA user_version = 7;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 7: %w", err)
		}
		fallthrough
	case 7:
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE draft_string_properties (
				mailbox_id TEXT NOT NULL REFERENCES mailboxes(mailbox_id),
				reference TEXT NOT NULL CHECK (length(reference) = 32),
				created_at INTEGER NOT NULL,
				property_set TEXT NOT NULL,
				property_name TEXT NOT NULL,
				value TEXT NOT NULL,
				PRIMARY KEY (mailbox_id, reference, property_set, property_name)
			) STRICT;
			CREATE INDEX draft_string_property_search
				ON draft_string_properties (mailbox_id, property_set, property_name, value, reference);
			PRAGMA user_version = 8;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 8: %w", err)
		}
		fallthrough
	case 8:
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE send_intents ADD COLUMN request_key BLOB CHECK (length(request_key) = 32);
			CREATE UNIQUE INDEX send_intents_request_key ON send_intents (mailbox_id, request_key);
			PRAGMA user_version = 9;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 9: %w", err)
		}
		fallthrough
	case 9:
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE journal ADD COLUMN observed_at INTEGER;
			PRAGMA user_version = 10;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 10: %w", err)
		}
		fallthrough
	case 10:
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE mail_draft_creations (
				mailbox_id TEXT NOT NULL REFERENCES mailboxes(mailbox_id),
				reference TEXT NOT NULL CHECK (length(reference) = 32),
				folder_id TEXT NOT NULL,
				mime_digest BLOB NOT NULL CHECK (length(mime_digest) = 32),
				acknowledged_id TEXT NOT NULL DEFAULT '',
				PRIMARY KEY (mailbox_id, reference)
			) STRICT;
			PRAGMA user_version = 11;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 11: %w", err)
		}
		fallthrough
	case 11:
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE mapi_replicas (
				mailbox_id TEXT PRIMARY KEY REFERENCES mailboxes(mailbox_id),
				epoch BLOB NOT NULL UNIQUE CHECK (length(epoch) = 16),
				high_water INTEGER NOT NULL CHECK (high_water BETWEEN 0 AND 281474976710655)
			) STRICT;
			CREATE TABLE mapi_object_ids (
				mailbox_id TEXT NOT NULL REFERENCES mapi_replicas(mailbox_id) ON DELETE CASCADE,
				namespace INTEGER NOT NULL CHECK (namespace BETWEEN 1 AND 4),
				object_key BLOB NOT NULL CHECK (length(object_key) BETWEEN 1 AND 1024),
				counter INTEGER NOT NULL CHECK (counter BETWEEN 1 AND 281474976710655),
				PRIMARY KEY (mailbox_id, namespace, object_key),
				UNIQUE (mailbox_id, counter)
			) STRICT;
			PRAGMA user_version = 12;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 12: %w", err)
		}
		fallthrough
	case 12:
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE mapi_local_id_ranges (
				mailbox_id TEXT NOT NULL REFERENCES mapi_replicas(mailbox_id) ON DELETE CASCADE,
				first_id INTEGER NOT NULL CHECK (first_id BETWEEN 1 AND 281474976710655),
				last_id INTEGER NOT NULL CHECK (last_id BETWEEN first_id AND 281474976710655),
				PRIMARY KEY (mailbox_id, first_id)
			) STRICT;
			PRAGMA user_version = 13;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 13: %w", err)
		}
		fallthrough
	case 13:
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE mapi_contents_proofs (
				sequence INTEGER PRIMARY KEY,
				mailbox_id TEXT NOT NULL REFERENCES mapi_replicas(mailbox_id) ON DELETE CASCADE,
				digest BLOB NOT NULL CHECK (length(digest) = 32),
				UNIQUE (mailbox_id, digest)
			) STRICT;
			CREATE INDEX mapi_contents_proofs_recent ON mapi_contents_proofs (mailbox_id, sequence DESC);
			PRAGMA user_version = 14;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 14: %w", err)
		}
		fallthrough
	case 14:
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE mapi_object_ids RENAME TO mapi_object_ids_v14;
			CREATE TABLE mapi_object_ids (
				mailbox_id TEXT NOT NULL REFERENCES mapi_replicas(mailbox_id) ON DELETE CASCADE,
				namespace INTEGER NOT NULL CHECK (namespace BETWEEN 1 AND 4),
				object_key BLOB NOT NULL CHECK (length(object_key) BETWEEN 1 AND 4104),
				counter INTEGER NOT NULL CHECK (counter BETWEEN 1 AND 281474976710655),
				PRIMARY KEY (mailbox_id, namespace, object_key),
				UNIQUE (mailbox_id, counter)
			) STRICT;
			INSERT INTO mapi_object_ids SELECT mailbox_id, namespace, object_key, counter FROM mapi_object_ids_v14;
			DROP TABLE mapi_object_ids_v14;
			CREATE TABLE mapi_projection_versions (
				mailbox_id TEXT NOT NULL,
				counter INTEGER NOT NULL,
				parent INTEGER,
				change_number INTEGER NOT NULL CHECK (change_number BETWEEN 1 AND 281474976710655),
				read_number INTEGER NOT NULL CHECK (read_number BETWEEN 0 AND 281474976710655),
				fingerprint BLOB NOT NULL CHECK (length(fingerprint) = 32),
				native_revision BLOB NOT NULL CHECK (length(native_revision) = 8),
				modified BLOB NOT NULL CHECK (length(modified) BETWEEN 15 AND 16),
				change_key BLOB NOT NULL CHECK (length(change_key) = 0 OR length(change_key) BETWEEN 17 AND 24),
				predecessors BLOB NOT NULL CHECK (length(predecessors) <= 1600),
				is_present INTEGER NOT NULL CHECK (is_present IN (0, 1)),
				is_read INTEGER NOT NULL CHECK (is_read IN (0, 1)),
				PRIMARY KEY (mailbox_id, counter),
				FOREIGN KEY (mailbox_id, counter) REFERENCES mapi_object_ids(mailbox_id, counter) ON DELETE CASCADE,
				FOREIGN KEY (mailbox_id, parent) REFERENCES mapi_object_ids(mailbox_id, counter)
			) STRICT;
			PRAGMA user_version = 15;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 15: %w", err)
		}
		fallthrough
	case 15:
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE mapi_fai_objects (
				mailbox_id TEXT NOT NULL,
				counter INTEGER NOT NULL,
				source_key BLOB NOT NULL CHECK (length(source_key) = 22),
				private_config INTEGER NOT NULL CHECK (private_config IN (0, 1)),
				cache_bytes INTEGER NOT NULL CHECK (cache_bytes BETWEEN 1 AND 131072),
				properties BLOB NOT NULL CHECK (length(properties) <= 1048576),
				PRIMARY KEY (mailbox_id, counter),
				UNIQUE (mailbox_id, source_key),
				FOREIGN KEY (mailbox_id, counter) REFERENCES mapi_projection_versions(mailbox_id, counter) ON DELETE CASCADE
			) STRICT;
			CREATE TABLE mapi_fai_tombstones (
				mailbox_id TEXT NOT NULL,
				source_key BLOB NOT NULL CHECK (length(source_key) = 22),
				folder INTEGER NOT NULL,
				PRIMARY KEY (mailbox_id, source_key),
				FOREIGN KEY (mailbox_id, folder) REFERENCES mapi_object_ids(mailbox_id, counter) ON DELETE CASCADE
			) STRICT;
			PRAGMA user_version = 16;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 16: %w", err)
		}
		fallthrough
	case 16:
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE mapi_staged_ids (
				mailbox_id TEXT NOT NULL REFERENCES mapi_replicas(mailbox_id) ON DELETE CASCADE,
				counter INTEGER NOT NULL CHECK (counter BETWEEN 1 AND 281474976710655),
				namespace INTEGER NOT NULL CHECK (namespace BETWEEN 1 AND 3),
				expires_at INTEGER NOT NULL,
				PRIMARY KEY (mailbox_id, counter)
			) STRICT;
			PRAGMA user_version = 17;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 17: %w", err)
		}
		fallthrough
	case 17:
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE mapi_draft_placements (
				mailbox_id TEXT NOT NULL REFERENCES mapi_replicas(mailbox_id) ON DELETE CASCADE,
				counter INTEGER NOT NULL CHECK (counter BETWEEN 1 AND 281474976710655),
				folder INTEGER NOT NULL,
				reference TEXT NOT NULL CHECK (length(reference) = 32),
				native_folder TEXT NOT NULL,
				native_id TEXT NOT NULL,
				copy_of TEXT NOT NULL,
				submission_revision BLOB NOT NULL CHECK (length(submission_revision) = 8),
				trash_pending INTEGER NOT NULL CHECK (trash_pending IN (0, 1)),
				PRIMARY KEY (mailbox_id, counter),
				UNIQUE (mailbox_id, reference),
				FOREIGN KEY (mailbox_id, folder) REFERENCES mapi_object_ids(mailbox_id, counter)
			) STRICT;
			PRAGMA user_version = 18;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 18: %w", err)
		}
		fallthrough
	case 18:
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE mapi_folder_values (
				mailbox_id TEXT NOT NULL,
				counter INTEGER NOT NULL,
				kind INTEGER NOT NULL CHECK (kind BETWEEN 1 AND 4),
				data BLOB NOT NULL CHECK (length(data) BETWEEN 1 AND 131072),
				PRIMARY KEY (mailbox_id, counter, kind),
				FOREIGN KEY (mailbox_id, counter) REFERENCES mapi_object_ids(mailbox_id, counter)
			) STRICT;
			PRAGMA user_version = 19;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 19: %w", err)
		}
		fallthrough
	case 19:
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE mapi_folder_creations (
				mailbox_id TEXT NOT NULL REFERENCES mapi_replicas(mailbox_id) ON DELETE CASCADE,
				counter INTEGER NOT NULL CHECK (counter BETWEEN 1 AND 281474976710655),
				parent TEXT NOT NULL,
				name TEXT NOT NULL,
				change_key BLOB NOT NULL CHECK (length(change_key) BETWEEN 17 AND 24),
				native_id TEXT NOT NULL,
				PRIMARY KEY (mailbox_id, counter)
			) STRICT;
			CREATE TABLE mapi_foreign_replicas (
				mailbox_id TEXT NOT NULL REFERENCES mapi_replicas(mailbox_id) ON DELETE CASCADE,
				replid INTEGER NOT NULL CHECK (replid BETWEEN 2 AND 257),
				guid BLOB NOT NULL CHECK (length(guid) = 16),
				PRIMARY KEY (mailbox_id, replid),
				UNIQUE (mailbox_id, guid)
			) STRICT;
			CREATE TABLE mapi_upload_proofs (
				sequence INTEGER PRIMARY KEY AUTOINCREMENT,
				mailbox_id TEXT NOT NULL REFERENCES mapi_replicas(mailbox_id) ON DELETE CASCADE,
				root INTEGER NOT NULL,
				digest BLOB NOT NULL CHECK (length(digest) = 32),
				seen_before BLOB NOT NULL CHECK (length(seen_before) <= 65536),
				seen_after BLOB NOT NULL CHECK (length(seen_after) = 32),
				read_before BLOB NOT NULL CHECK (length(read_before) <= 65536),
				read_after BLOB NOT NULL CHECK (length(read_after) = 32),
				created BLOB NOT NULL CHECK (length(created) <= 768),
				UNIQUE (mailbox_id, digest),
				FOREIGN KEY (mailbox_id, root) REFERENCES mapi_object_ids(mailbox_id, counter)
			) STRICT;
			CREATE INDEX mapi_upload_proofs_mailbox_sequence ON mapi_upload_proofs (mailbox_id, sequence);
			PRAGMA user_version = 20;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 20: %w", err)
		}
		fallthrough
	case 20:
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE mapi_upload_proofs ADD COLUMN normal_before BLOB NOT NULL DEFAULT x''
				CHECK (length(normal_before) <= 65536);
			ALTER TABLE mapi_upload_proofs ADD COLUMN normal_after BLOB NOT NULL
				DEFAULT x'0000000000000000000000000000000000000000000000000000000000000000'
				CHECK (length(normal_after) = 32);
			PRAGMA user_version = 21;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 21: %w", err)
		}
		fallthrough
	case 21:
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE mapi_message_moves (
				mailbox_id TEXT NOT NULL REFERENCES mapi_replicas(mailbox_id) ON DELETE CASCADE,
				source INTEGER NOT NULL CHECK (source BETWEEN 1 AND 281474976710655),
				destination INTEGER NOT NULL CHECK (destination BETWEEN 1 AND 281474976710655),
				from_folder INTEGER NOT NULL CHECK (from_folder BETWEEN 1 AND 281474976710655),
				to_folder INTEGER NOT NULL CHECK (to_folder BETWEEN 1 AND 281474976710655),
				native_id TEXT NOT NULL CHECK (length(native_id) BETWEEN 1 AND 4096),
				revision BLOB NOT NULL CHECK (length(revision) = 8),
				request BLOB NOT NULL CHECK (length(request) = 32),
				result INTEGER NOT NULL CHECK (result BETWEEN 0 AND 281474976710655),
				newer_client INTEGER NOT NULL CHECK (newer_client IN (0, 1)),
				PRIMARY KEY (mailbox_id, source),
				UNIQUE (mailbox_id, destination)
			) STRICT;
			PRAGMA user_version = 22;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 22: %w", err)
		}
		fallthrough
	case 22:
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE mapi_message_imports (
				mailbox_id TEXT NOT NULL REFERENCES mapi_replicas(mailbox_id) ON DELETE CASCADE,
				counter INTEGER NOT NULL CHECK (counter BETWEEN 1 AND 281474976710655),
				parent INTEGER NOT NULL CHECK (parent BETWEEN 1 AND 281474976710655),
				digest BLOB NOT NULL CHECK (length(digest) = 32),
				mime_digest BLOB NOT NULL CHECK (length(mime_digest) = 32),
				native_id TEXT CHECK (length(native_id) BETWEEN 1 AND 4096),
				PRIMARY KEY (mailbox_id, counter),
				UNIQUE (mailbox_id, native_id)
			) STRICT;
			PRAGMA user_version = 23;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 23: %w", err)
		}
		fallthrough
	case 23:
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE mapi_folder_values RENAME TO mapi_folder_values_v23;
			CREATE TABLE mapi_folder_values (
				mailbox_id TEXT NOT NULL,
				counter INTEGER NOT NULL,
				kind INTEGER NOT NULL CHECK (kind BETWEEN 1 AND 5),
				data BLOB NOT NULL CHECK (length(data) BETWEEN 1 AND 131072),
				PRIMARY KEY (mailbox_id, counter, kind),
				FOREIGN KEY (mailbox_id, counter) REFERENCES mapi_object_ids(mailbox_id, counter)
			) STRICT;
			INSERT INTO mapi_folder_values SELECT mailbox_id, counter, kind, data FROM mapi_folder_values_v23;
			DROP TABLE mapi_folder_values_v23;
			PRAGMA user_version = 24;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 24: %w", err)
		}
		fallthrough
	case 24:
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE mapi_spooler (
				mailbox_id TEXT NOT NULL,
				counter INTEGER NOT NULL,
				folder INTEGER NOT NULL,
				revision BLOB NOT NULL CHECK (length(revision) = 8),
				state INTEGER NOT NULL CHECK (state BETWEEN 1 AND 3),
				PRIMARY KEY (mailbox_id, counter),
				FOREIGN KEY (mailbox_id, counter) REFERENCES mapi_object_ids(mailbox_id, counter),
				FOREIGN KEY (mailbox_id, folder) REFERENCES mapi_object_ids(mailbox_id, counter)
			) STRICT;
			PRAGMA user_version = 25;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 25: %w", err)
		}
		fallthrough
	case 25:
		if _, err := tx.ExecContext(ctx, `
			CREATE TABLE mapi_public_read_state (
				user_mailbox TEXT NOT NULL REFERENCES mailboxes(mailbox_id),
				store_key TEXT NOT NULL,
				folder BLOB NOT NULL CHECK (length(folder) = 24),
				private INTEGER NOT NULL CHECK (private BETWEEN 0 AND 1),
				replica BLOB NOT NULL CHECK (length(replica) = 16),
				read_state BLOB NOT NULL CHECK (length(read_state) <= 65536),
				revision INTEGER NOT NULL CHECK (revision > 0),
				updated_at INTEGER NOT NULL,
				PRIMARY KEY (user_mailbox, store_key, folder, private)
			) STRICT;
			PRAGMA user_version = 26;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 26: %w", err)
		}
		fallthrough
	case 26:
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE mapi_message_imports ADD COLUMN contact_reference BLOB NOT NULL
				DEFAULT x'00000000000000000000000000000000' CHECK (length(contact_reference) = 16);
			CREATE UNIQUE INDEX mapi_contact_import_reference ON mapi_message_imports (mailbox_id, contact_reference)
				WHERE contact_reference <> x'00000000000000000000000000000000';
			PRAGMA user_version = 27;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 27: %w", err)
		}
		fallthrough
	case 27:
		if _, err := tx.ExecContext(ctx, `
			ALTER TABLE mapi_message_imports ADD COLUMN appointment_reference BLOB NOT NULL
				DEFAULT x'00000000000000000000000000000000' CHECK (length(appointment_reference) = 16);
			CREATE UNIQUE INDEX mapi_appointment_import_reference ON mapi_message_imports (mailbox_id, appointment_reference)
				WHERE appointment_reference <> x'00000000000000000000000000000000';
			PRAGMA user_version = 28;
		`); err != nil {
			return fmt.Errorf("create SQLite schema version 28: %w", err)
		}
	case schemaVersion:
	default:
		return fmt.Errorf("unsupported SQLite schema version %d (supported: %d)", version, schemaVersion)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit SQLite migration: %w", err)
	}
	return nil
}

// MailboxIdentity contains opaque core identifiers that survive store reopen.
type MailboxIdentity struct {
	MailboxID string
	ReplicaID string
}

// BindMailbox resolves the canonical account within a configured backend instance.
// The caller supplies the scope after authentication and authorization.
func (s *Store) BindMailbox(ctx context.Context, scope core.Scope, backendInstance string) (MailboxIdentity, error) {
	if scope.Backend == "" || backendInstance == "" || scope.Account == "" {
		return MailboxIdentity{}, core.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MailboxIdentity{}, fmt.Errorf("begin mailbox binding: %w", err)
	}
	defer tx.Rollback()

	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mailboxes (backend_kind, backend_instance, account, mailbox_id, replica_id, journal_generation)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (backend_kind, backend_instance, account) DO NOTHING
	`, scope.Backend, backendInstance, scope.Account, rand.Text(), rand.Text(), rand.Text()); err != nil {
		return MailboxIdentity{}, fmt.Errorf("insert mailbox binding: %w", err)
	}
	var identity MailboxIdentity
	if err := tx.QueryRowContext(ctx, `
		SELECT mailbox_id, replica_id FROM mailboxes
		WHERE backend_kind = ? AND backend_instance = ? AND account = ?
	`, scope.Backend, backendInstance, scope.Account).Scan(&identity.MailboxID, &identity.ReplicaID); err != nil {
		return MailboxIdentity{}, fmt.Errorf("read mailbox binding: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return MailboxIdentity{}, fmt.Errorf("commit mailbox binding: %w", err)
	}
	return identity, nil
}
