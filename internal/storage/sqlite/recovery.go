// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"ewsgo/internal/core"
)

// Backup copies a consistent snapshot of a store, including commits in WAL,
// to a new file. The source can remain in use and is opened read-only.
func Backup(ctx context.Context, source, destination string) error {
	return copyStore(ctx, source, destination, false)
}

// Restore copies a backup to a new file and invalidates pre-restore journal
// cursors while preserving mailbox and replica identities. Restored ready
// sends cannot be dispatched because a newer copy may already have sent them.
func Restore(ctx context.Context, backup, destination string) error {
	return copyStore(ctx, backup, destination, true)
}

func copyStore(ctx context.Context, source, destination string, restore bool) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if source == "" || destination == "" || source == ":memory:" || destination == ":memory:" {
		return core.ErrInvalid
	}
	source, err := filepath.Abs(source)
	if err != nil {
		return fmt.Errorf("resolve source database path: %w", err)
	}
	destination, err = filepath.Abs(destination)
	if err != nil {
		return fmt.Errorf("resolve destination database path: %w", err)
	}
	if err := requireNewDatabasePath(destination); err != nil {
		return err
	}
	parent := filepath.Dir(destination)
	stagingDir, err := os.MkdirTemp(parent, ".ewsgo-store-")
	if err != nil {
		return fmt.Errorf("create private staging directory: %w", err)
	}
	defer os.RemoveAll(stagingDir)
	staged := filepath.Join(stagingDir, "store.sqlite")
	file, err := os.OpenFile(staged, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create private staged database: %w", err)
	}
	if err := file.Close(); err != nil {
		return fmt.Errorf("close empty staged database: %w", err)
	}
	if err := snapshotDatabase(ctx, source, staged); err != nil {
		return err
	}
	if restore {
		if err := prepareRestoredDatabase(ctx, staged); err != nil {
			return err
		}
	}
	if err := syncPath(staged); err != nil {
		return fmt.Errorf("sync staged database: %w", err)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := requireNewDatabasePath(destination); err != nil {
		return err
	}
	// The staging directory is on the destination filesystem. Linking publishes
	// a complete file atomically and refuses to overwrite any competing file.
	if err := os.Link(staged, destination); err != nil {
		return fmt.Errorf("publish database copy: %w", err)
	}
	if err := syncPath(parent); err != nil {
		return fmt.Errorf("database published but destination directory sync failed: %w", err)
	}
	return nil
}

func requireNewDatabasePath(path string) error {
	for _, suffix := range []string{"-wal", "-shm", "-journal"} {
		if strings.HasSuffix(path, suffix) {
			return fmt.Errorf("destination uses a SQLite sidecar suffix: %w", core.ErrInvalid)
		}
	}
	for _, suffix := range []string{"", "-wal", "-shm", "-journal"} {
		if _, err := os.Lstat(path + suffix); err == nil {
			return fmt.Errorf("destination database or sidecar exists: %w", os.ErrExist)
		} else if !os.IsNotExist(err) {
			return fmt.Errorf("inspect destination database: %w", err)
		}
	}
	return nil
}

func snapshotDatabase(ctx context.Context, source, staged string) error {
	db, err := openDatabase(source, "ro")
	if err != nil {
		return err
	}
	defer db.Close()
	var version int
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read source schema version: %w", err)
	}
	if version < 1 || version > schemaVersion {
		return fmt.Errorf("unsupported source schema version %d", version)
	}
	rows, err := db.QueryContext(ctx, "SELECT mailbox_id, replica_id FROM mailboxes LIMIT 0")
	if err != nil {
		return fmt.Errorf("verify source is an EWSGo store: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("close source schema check: %w", err)
	}
	if _, err := db.ExecContext(ctx, "VACUUM INTO ?", staged); err != nil {
		return fmt.Errorf("copy source database snapshot: %w", err)
	}
	return nil
}

func prepareRestoredDatabase(ctx context.Context, staged string) (result error) {
	db, err := openDatabase(staged, "rw")
	if err != nil {
		return err
	}
	defer func() {
		if err := db.Close(); result == nil && err != nil {
			result = fmt.Errorf("close staged restore: %w", err)
		}
	}()
	var journalMode string
	if err := db.QueryRowContext(ctx, "PRAGMA journal_mode = DELETE").Scan(&journalMode); err != nil {
		return fmt.Errorf("set standalone restore journal mode: %w", err)
	}
	if journalMode != "delete" {
		return fmt.Errorf("restore requires a standalone file, got journal mode %q", journalMode)
	}
	if err := migrate(ctx, db); err != nil {
		return err
	}
	if err := verifyRestoredDatabase(ctx, db); err != nil {
		return err
	}
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin restore invalidation: %w", err)
	}
	defer tx.Rollback()
	if _, err := tx.ExecContext(ctx, "UPDATE mailboxes SET journal_generation = lower(hex(randomblob(16)))"); err != nil {
		return fmt.Errorf("invalidate restored journal cursors: %w", err)
	}
	if err := rotateRestoredMAPIReplicas(ctx, tx); err != nil {
		return err
	}
	if _, err := tx.ExecContext(ctx, `UPDATE send_intents SET state = ? WHERE state = ?`, core.SendUncertain, core.SendReady); err != nil {
		return fmt.Errorf("invalidate dispatch of restored send intents: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit restore invalidation: %w", err)
	}
	return nil
}

func verifyRestoredDatabase(ctx context.Context, db *sql.DB) error {
	var integrity string
	if err := db.QueryRowContext(ctx, "PRAGMA integrity_check").Scan(&integrity); err != nil {
		return fmt.Errorf("check restored database integrity: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("restored database failed integrity check")
	}
	violations, err := db.QueryContext(ctx, "PRAGMA foreign_key_check")
	if err != nil {
		return fmt.Errorf("check restored database foreign keys: %w", err)
	}
	defer violations.Close()
	if violations.Next() {
		return fmt.Errorf("restored database failed foreign key check")
	}
	if err := violations.Err(); err != nil {
		return fmt.Errorf("read restored database foreign key check: %w", err)
	}
	return nil
}

func syncPath(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}
