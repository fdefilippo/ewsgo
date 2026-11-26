// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"time"

	"ewsgo/internal/core"
)

type DeltaCheckpoint = core.NativeCheckpoint

// NativeBinding is needed when the backend reference cannot be converted to
// the logical ID without retaining a mapping. The reference is backend data.
type NativeBinding struct {
	Kind      core.Kind
	Reference string
}

type DeltaEvent struct {
	Change     core.Change
	ObjectKind core.Kind
	Binding    *NativeBinding // optional on creation
}

type rowQueryer interface {
	QueryRowContext(context.Context, string, ...any) *sql.Row
}

func readDeltaCheckpoint(ctx context.Context, db rowQueryer, mailboxID string) (DeltaCheckpoint, error) {
	checkpoint, _, err := readDeltaState(ctx, db, mailboxID)
	return checkpoint, err
}

func readDeltaState(ctx context.Context, db rowQueryer, mailboxID string) (DeltaCheckpoint, bool, error) {
	var checkpoint DeltaCheckpoint
	var projected bool
	err := db.QueryRowContext(ctx, `
		SELECT COALESCE(d.cursor, ''), COALESCE(d.revision, 0), COALESCE(d.projected, 0)
		FROM mailboxes AS m LEFT JOIN delta_checkpoints AS d ON m.mailbox_id = d.mailbox_id
		WHERE m.mailbox_id = ?
	`, mailboxID).Scan(&checkpoint.Cursor, &checkpoint.Revision, &projected)
	if errors.Is(err, sql.ErrNoRows) {
		return DeltaCheckpoint{}, false, core.ErrNotFound
	}
	if err != nil {
		return DeltaCheckpoint{}, false, fmt.Errorf("read mailbox delta checkpoint: %w", err)
	}
	return checkpoint, projected, nil
}

func (s *Store) DeltaCheckpoint(ctx context.Context, mailboxID string) (DeltaCheckpoint, error) {
	return readDeltaCheckpoint(ctx, s.db, mailboxID)
}

// ApplyDelta commits one validated page from the normalized mailbox feed. The
// caller must resolve native references and membership first and only supply a
// resumable cursor. The store assigns local revisions and commits optional
// mappings for logical identities that cannot be derived from native references.
// A stale checkpoint must be recovered by reading the committed position.
func (s *Store) ApplyDelta(ctx context.Context, mailboxID string, expected DeltaCheckpoint, next core.Cursor, events []DeltaEvent) (DeltaCheckpoint, error) {
	if next == "" || next == expected.Cursor && len(events) != 0 {
		return DeltaCheckpoint{}, core.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return DeltaCheckpoint{}, fmt.Errorf("begin mailbox delta: %w", err)
	}
	defer tx.Rollback()
	checkpoint, projected, err := readDeltaState(ctx, tx, mailboxID)
	if err != nil {
		return DeltaCheckpoint{}, err
	}
	if checkpoint != expected || projected {
		return DeltaCheckpoint{}, core.ErrConflict
	}
	if next == checkpoint.Cursor {
		return checkpoint, nil
	}
	checkpoint, err = writeDelta(ctx, tx, mailboxID, checkpoint, next, events, false)
	if err != nil {
		return DeltaCheckpoint{}, err
	}
	if err := tx.Commit(); err != nil {
		return DeltaCheckpoint{}, fmt.Errorf("commit mailbox delta: %w", err)
	}
	return checkpoint, nil
}

// The caller must validate the expected checkpoint in this write transaction.
func writeDelta(ctx context.Context, tx *sql.Tx, mailboxID string, current DeltaCheckpoint, next core.Cursor, events []DeltaEvent, projected bool) (DeltaCheckpoint, error) {
	advance := len(events)
	if !projected || current.Revision == 0 {
		advance = max(1, advance)
	}
	last := current.Revision
	if advance > 0 {
		var err error
		last, err = reserveJournalRevisions(ctx, tx, mailboxID, advance)
		if err != nil {
			return DeltaCheckpoint{}, err
		}
	}
	for i, event := range events {
		change := event.Change
		if change.ID == "" || change.Kind < core.Created || change.Kind > core.Deleted || change.Revision != 0 {
			return DeltaCheckpoint{}, core.ErrInvalid
		}
		revision := last - core.Revision(advance-i-1)
		if err := updateNativeBinding(ctx, tx, mailboxID, event, revision); err != nil {
			return DeltaCheckpoint{}, err
		}
		if change.Kind == core.Deleted {
			if _, err := tx.ExecContext(ctx, `DELETE FROM object_properties WHERE mailbox_id = ? AND object_id = ?`, mailboxID, change.ID); err != nil {
				return DeltaCheckpoint{}, fmt.Errorf("remove deleted object properties: %w", err)
			}
		}
		change.Revision = revision
		if err := appendJournalChange(ctx, tx, mailboxID, change, event.ObjectKind); err != nil {
			return DeltaCheckpoint{}, err
		}
	}
	checkpoint := DeltaCheckpoint{Cursor: next, Revision: last}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO delta_checkpoints (mailbox_id, cursor, revision, projected) VALUES (?, ?, ?, ?)
		ON CONFLICT (mailbox_id) DO UPDATE SET
			cursor = excluded.cursor, revision = excluded.revision, projected = excluded.projected
	`, mailboxID, checkpoint.Cursor, checkpoint.Revision, projected); err != nil {
		return DeltaCheckpoint{}, fmt.Errorf("advance mailbox delta checkpoint: %w", err)
	}
	return checkpoint, nil
}

// The write transaction serializes native ingestion and local property edits.
// Reserve only after validating the mailbox. A rollback returns the range.
func reserveJournalRevisions(ctx context.Context, tx *sql.Tx, mailboxID string, count int) (core.Revision, error) {
	var last core.Revision
	err := tx.QueryRowContext(ctx, `
		UPDATE mailboxes SET journal_revision = journal_revision + ?
		WHERE mailbox_id = ? AND journal_revision <= ? RETURNING journal_revision
	`, count, mailboxID, math.MaxInt64-int64(count)).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) {
		return 0, fmt.Errorf("mailbox journal revision exhausted: %w", core.ErrInvalid)
	}
	if err != nil {
		return 0, fmt.Errorf("reserve mailbox journal revisions: %w", err)
	}
	return last, nil
}

func appendJournalChange(ctx context.Context, tx *sql.Tx, mailboxID string, change core.Change, kind core.Kind) error {
	_, err := tx.ExecContext(ctx, `
		INSERT INTO journal (mailbox_id, revision, kind, object_id, parent_id, previous_parent_id, object_kind, observed_at)
		VALUES (?, ?, ?, ?, ?, ?, COALESCE(NULLIF(?, 0),
			(SELECT object_kind FROM native_bindings WHERE mailbox_id = ? AND object_id = ?),
			(SELECT object_kind FROM journal WHERE mailbox_id = ? AND object_id = ? ORDER BY revision DESC LIMIT 1), 0), ?)
	`, mailboxID, change.Revision, change.Kind, change.ID, change.Parent, change.PreviousParent,
		kind, mailboxID, change.ID, mailboxID, change.ID, time.Now().UnixMilli())
	if err != nil {
		return fmt.Errorf("append mailbox change: %w", err)
	}
	return nil
}

func updateNativeBinding(ctx context.Context, tx *sql.Tx, mailboxID string, event DeltaEvent, revision core.Revision) error {
	change := event.Change
	if binding := event.Binding; binding != nil {
		if change.Kind != core.Created || binding.Reference == "" || (binding.Kind != core.Folder && binding.Kind != core.Message) {
			return core.ErrInvalid
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO native_bindings (mailbox_id, object_kind, native_ref, object_id)
			VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING
		`, mailboxID, binding.Kind, binding.Reference, change.ID)
		if err != nil {
			return fmt.Errorf("insert native object binding: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("count inserted native object bindings: %w", err)
		}
		if n == 0 {
			return core.ErrConflict
		}
	}
	if change.Kind == core.Deleted {
		if _, err := tx.ExecContext(ctx, `
			UPDATE native_bindings SET deleted_revision = ?
			WHERE mailbox_id = ? AND object_id = ? AND deleted_revision IS NULL
		`, revision, mailboxID, change.ID); err != nil {
			return fmt.Errorf("retire native object binding: %w", err)
		}
	}
	return nil
}

// ResolveNativeBinding looks up a live persisted mapping. A caller can use a
// provably derivable logical identity directly without creating a binding.
func (s *Store) ResolveNativeBinding(ctx context.Context, mailboxID string, binding NativeBinding) (core.ID, error) {
	var id core.ID
	err := s.db.QueryRowContext(ctx, `
		SELECT object_id FROM native_bindings
		WHERE mailbox_id = ? AND object_kind = ? AND native_ref = ? AND deleted_revision IS NULL
	`, mailboxID, binding.Kind, binding.Reference).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return "", core.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("read native object binding: %w", err)
	}
	return id, nil
}

// JournalCursor is internal to the persistent core. The generation scopes a
// position to one mailbox and invalidates it after restore.
type JournalCursor struct {
	Generation string
	Revision   core.Revision
}

type JournalPage struct {
	Changes []core.Change
	Next    JournalCursor
}

func readJournalWindow(ctx context.Context, db rowQueryer, mailboxID string) (JournalCursor, core.Revision, error) {
	var head JournalCursor
	var floor core.Revision
	err := db.QueryRowContext(ctx, `
		SELECT journal_generation, journal_revision, journal_floor FROM mailboxes WHERE mailbox_id = ?
	`, mailboxID).Scan(&head.Generation, &head.Revision, &floor)
	if errors.Is(err, sql.ErrNoRows) {
		return JournalCursor{}, 0, core.ErrNotFound
	}
	if err != nil {
		return JournalCursor{}, 0, fmt.Errorf("read mailbox journal window: %w", err)
	}
	return head, floor, nil
}

// JournalHead captures the current position. A consumer using it to
// bootstrap must coordinate its full object enumeration with that position.
func (s *Store) JournalHead(ctx context.Context, mailboxID string) (JournalCursor, error) {
	head, _, err := readJournalWindow(ctx, s.db, mailboxID)
	return head, err
}

// Journal reads the retained window and changes from one snapshot. The zero
// cursor starts at the original beginning, if still retained.
func (s *Store) Journal(ctx context.Context, mailboxID string, after JournalCursor, limit int) (JournalPage, error) {
	entries, next, err := s.JournalEntries(ctx, mailboxID, after, limit)
	if err != nil {
		return JournalPage{}, err
	}
	page := JournalPage{Next: next}
	for _, entry := range entries {
		page.Changes = append(page.Changes, entry.Change)
	}
	return page, nil
}

type JournalEntry struct {
	core.Change
	ObjectKind core.Kind
	ObservedAt time.Time
}

// JournalEntries adds the object type and projection time. Entries from
// before schema 10 do not have an observation time.
func (s *Store) JournalEntries(ctx context.Context, mailboxID string, after JournalCursor, limit int) ([]JournalEntry, JournalCursor, error) {
	if limit <= 0 {
		return nil, JournalCursor{}, core.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, JournalCursor{}, fmt.Errorf("begin mailbox journal read: %w", err)
	}
	defer tx.Rollback()
	head, floor, err := readJournalWindow(ctx, tx, mailboxID)
	if err != nil {
		return nil, JournalCursor{}, err
	}
	if (after != (JournalCursor{}) && after.Generation != head.Generation) || after.Revision < floor || after.Revision > head.Revision {
		return nil, JournalCursor{}, core.ErrResync
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT kind, object_id, parent_id, previous_parent_id, revision, object_kind, observed_at
		FROM journal WHERE mailbox_id = ? AND revision > ? ORDER BY revision LIMIT ?
	`, mailboxID, after.Revision, limit)
	if err != nil {
		return nil, JournalCursor{}, fmt.Errorf("read mailbox journal: %w", err)
	}
	defer rows.Close()
	var changes []JournalEntry
	for rows.Next() {
		var change JournalEntry
		var observed sql.NullInt64
		if err := rows.Scan(&change.Kind, &change.ID, &change.Parent, &change.PreviousParent, &change.Revision, &change.ObjectKind, &observed); err != nil {
			return nil, JournalCursor{}, fmt.Errorf("read mailbox journal row: %w", err)
		}
		if observed.Valid {
			change.ObservedAt = time.UnixMilli(observed.Int64).UTC()
		}
		changes = append(changes, change)
	}
	if err := rows.Err(); err != nil {
		return nil, JournalCursor{}, fmt.Errorf("iterate mailbox journal: %w", err)
	}
	// At the end of the window, cross gaps left by empty native pages too.
	if len(changes) == limit {
		head.Revision = changes[len(changes)-1].Revision
	}
	return changes, head, nil
}
