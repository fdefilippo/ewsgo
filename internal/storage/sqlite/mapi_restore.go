// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"fmt"

	"ewsgo/internal/core"
)

func rotateRestoredMAPIReplicas(ctx context.Context, tx *sql.Tx) error {
	for _, table := range []string{"mapi_contents_proofs", "mapi_upload_proofs", "mapi_local_id_ranges", "mapi_folder_creations", "mapi_message_moves", "mapi_message_imports"} {
		if _, err := tx.ExecContext(ctx, "DELETE FROM "+table); err != nil {
			return fmt.Errorf("invalidate restored MAPI proofs and reservations: %w", err)
		}
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM mapi_staged_ids AS s WHERE NOT EXISTS (
			SELECT 1 FROM mapi_draft_placements AS d
			WHERE d.mailbox_id = s.mailbox_id AND d.counter = s.counter AND d.native_id = ''
		)
	`); err != nil {
		return fmt.Errorf("invalidate restored MAPI staged IDs: %w", err)
	}
	nextMailbox := ""
	for {
		var mailboxID string
		var previous []byte
		err := tx.QueryRowContext(ctx, `
			SELECT mailbox_id, epoch FROM mapi_replicas WHERE mailbox_id > ? ORDER BY mailbox_id LIMIT 1
		`, nextMailbox).Scan(&mailboxID, &previous)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read restored MAPI replica: %w", err)
		}
		if len(previous) != 16 {
			return core.ErrInvalid
		}
		current := newMAPIReplicaEpoch()
		if err := rebaseRestoredMAPIHistory(ctx, tx, mailboxID, previous, current[:]); err != nil {
			return err
		}
		for _, table := range []string{"mapi_fai_objects", "mapi_fai_tombstones"} {
			if _, err := tx.ExecContext(ctx, "UPDATE "+table+`
				SET source_key = CAST(? || substr(source_key, 17) AS BLOB)
				WHERE mailbox_id = ? AND substr(source_key, 1, 16) = ?
			`, current[:], mailboxID, previous); err != nil {
				return fmt.Errorf("rebase restored private MAPI source key: %w", err)
			}
		}
		if _, err := tx.ExecContext(ctx, "UPDATE mapi_replicas SET epoch = ? WHERE mailbox_id = ?", current[:], mailboxID); err != nil {
			return fmt.Errorf("rotate restored MAPI replica epoch: %w", err)
		}
		nextMailbox = mailboxID
	}
}

func rebaseRestoredMAPIHistory(ctx context.Context, tx *sql.Tx, mailboxID string, previous, current []byte) error {
	var after uint64
	for {
		var counter uint64
		var change, predecessors []byte
		err := tx.QueryRowContext(ctx, `
			SELECT counter, change_key, predecessors FROM mapi_projection_versions
			WHERE mailbox_id = ? AND counter > ? AND (length(change_key) > 0 OR length(predecessors) > 0)
			ORDER BY counter LIMIT 1
		`, mailboxID, after).Scan(&counter, &change, &predecessors)
		if err == sql.ErrNoRows {
			return nil
		}
		if err != nil {
			return fmt.Errorf("read restored MAPI history: %w", err)
		}
		changed := rebaseMAPIXID(change, previous, current)
		for entries := predecessors; len(entries) != 0; {
			size := int(entries[0])
			if size < 17 || size > 24 || size >= len(entries) {
				return fmt.Errorf("malformed restored MAPI predecessor list: %w", core.ErrInvalid)
			}
			changed = rebaseMAPIXID(entries[1:1+size], previous, current) || changed
			entries = entries[1+size:]
		}
		if changed {
			if _, err := tx.ExecContext(ctx, `
				UPDATE mapi_projection_versions SET change_key = ?, predecessors = ? WHERE mailbox_id = ? AND counter = ?
			`, change, predecessors, mailboxID, counter); err != nil {
				return fmt.Errorf("write restored MAPI history: %w", err)
			}
		}
		after = counter
	}
}

func rebaseMAPIXID(xid, previous, current []byte) bool {
	if len(xid) < 17 || !bytes.HasPrefix(xid, previous) {
		return false
	}
	copy(xid, current)
	return true
}
