// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"fmt"

	"ewsgo/internal/core"
)

// EWSSigningKey is provisioned once for the store, including when two gateway
// processes open it concurrently. The key is copied with the store backup.
func (s *Store) EWSSigningKey(ctx context.Context) ([32]byte, error) {
	var key [32]byte
	if _, err := rand.Read(key[:]); err != nil {
		return key, err
	}
	if _, err := s.db.ExecContext(ctx, `INSERT INTO store_keys (name, material) VALUES ('ews', ?) ON CONFLICT(name) DO NOTHING`, key[:]); err != nil {
		return [32]byte{}, fmt.Errorf("initialize EWS signing key: %w", err)
	}
	var stored []byte
	if err := s.db.QueryRowContext(ctx, `SELECT material FROM store_keys WHERE name = 'ews'`).Scan(&stored); err != nil {
		return [32]byte{}, fmt.Errorf("load EWS signing key: %w", err)
	}
	if len(stored) != len(key) {
		return [32]byte{}, core.ErrInvalid
	}
	copy(key[:], stored)
	return key, nil
}

// CollectionCursor either walks a snapshot pinned to Position or reads changes
// after Position. Mid-snapshot, any advance of the retention floor can discard
// a historical row still needed by that snapshot.
type CollectionCursor struct {
	Position JournalCursor
	Snapshot bool          `json:",omitempty"`
	AfterID  core.ID       `json:",omitempty"`
	Floor    core.Revision `json:",omitempty"`
}

type CollectionPage struct {
	Changes []core.Change
	Next    CollectionCursor
	More    bool
}

// Collection reads the folder hierarchy or one message collection. The caller
// must retain the collection identity with its cursor. Native and core changes
// to other collections advance the journal without occupying response slots.
func (s *Store) Collection(ctx context.Context, mailboxID string, kind core.Kind, parent core.ID, after CollectionCursor, limit int) (CollectionPage, error) {
	if limit < 1 || limit > 512 || kind != core.Folder && kind != core.Message || (kind == core.Message) != (parent != "") {
		return CollectionPage{}, core.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return CollectionPage{}, fmt.Errorf("begin collection read: %w", err)
	}
	defer tx.Rollback()
	head, floor, err := readJournalWindow(ctx, tx, mailboxID)
	if err != nil {
		return CollectionPage{}, err
	}
	if after == (CollectionCursor{}) {
		after = CollectionCursor{Position: head, Snapshot: true, Floor: floor}
	} else if after.Position.Generation != head.Generation || after.Position.Revision < floor || after.Position.Revision > head.Revision ||
		after.Snapshot && floor != after.Floor {
		return CollectionPage{}, core.ErrResync
	}
	query := `SELECT kind, object_id, parent_id, previous_parent_id, revision FROM journal AS j
		WHERE mailbox_id = ? AND object_kind = ?`
	args := []any{mailboxID, kind}
	if kind == core.Folder {
		query += ` AND (parent_id != '' OR previous_parent_id != '')`
	} else {
		query += ` AND (parent_id = ? OR previous_parent_id = ?)`
		args = append(args, parent, parent)
	}
	if after.Snapshot {
		query += ` AND object_id > ? AND revision <= ? AND kind != ? AND NOT EXISTS (
			SELECT 1 FROM journal AS newer WHERE newer.mailbox_id = j.mailbox_id AND newer.object_id = j.object_id
			AND newer.revision > j.revision AND newer.revision <= ?
		) ORDER BY object_id LIMIT ?`
		args = append(args, after.AfterID, after.Position.Revision, core.Deleted, after.Position.Revision, limit+1)
	} else {
		query += ` AND revision > ? ORDER BY revision LIMIT ?`
		args = append(args, after.Position.Revision, limit+1)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return CollectionPage{}, fmt.Errorf("read collection: %w", err)
	}
	defer rows.Close()
	page := CollectionPage{Next: CollectionCursor{Position: head}}
	for rows.Next() {
		var change core.Change
		if err := rows.Scan(&change.Kind, &change.ID, &change.Parent, &change.PreviousParent, &change.Revision); err != nil {
			return CollectionPage{}, fmt.Errorf("scan collection event: %w", err)
		}
		if after.Snapshot {
			change.Kind, change.PreviousParent = core.Created, ""
		}
		page.Changes = append(page.Changes, change)
	}
	if err := rows.Err(); err != nil {
		return CollectionPage{}, fmt.Errorf("iterate collection: %w", err)
	}
	if len(page.Changes) > limit {
		page.More = true
		page.Changes = page.Changes[:limit]
		last := page.Changes[limit-1]
		if after.Snapshot {
			page.Next = after
			page.Next.AfterID = last.ID
		} else {
			page.Next.Position.Revision = last.Revision
		}
	} else if after.Snapshot {
		page.Next.Position = after.Position
		page.More = after.Position != head
	}
	return page, nil
}
