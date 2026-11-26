// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"ewsgo/internal/core"
)

const maxMAPIContentsProofs = 256

// ValidateMAPIContentsProof checks a completed checkpoint in the current wire
// namespace. The digest includes the folder, synchronization options, filter
// and canonical state supplied by the frontend.
func (s *Store) ValidateMAPIContentsProof(ctx context.Context, mailboxID string, epoch [16]byte, digest [32]byte) error {
	var found int
	err := s.db.QueryRowContext(ctx, `
		SELECT 1 FROM mapi_contents_proofs AS proof
		JOIN mapi_replicas AS replica USING (mailbox_id)
		WHERE proof.mailbox_id = ? AND replica.epoch = ? AND proof.digest = ?
	`, mailboxID, epoch[:], digest[:]).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ErrResync
	}
	if err != nil {
		return fmt.Errorf("read MAPI Contents proof: %w", err)
	}
	return nil
}

// PublishMAPIContentsProof admits a complete checkpoint and evicts older
// proofs atomically. Call this just before returning the final transfer bytes.
func (s *Store) PublishMAPIContentsProof(ctx context.Context, mailboxID string, epoch [16]byte, digest [32]byte) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin MAPI Contents proof: %w", err)
	}
	defer tx.Rollback()
	version, err := readMAPIReplica(ctx, tx, mailboxID)
	if errors.Is(err, core.ErrNotFound) || err == nil && version.Epoch != epoch {
		return core.ErrResync
	}
	if err != nil {
		return err
	}
	// Replacement gets a fresh sequence, so retransmission renews recency.
	if _, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO mapi_contents_proofs (mailbox_id, digest) VALUES (?, ?)
	`, mailboxID, digest[:]); err != nil {
		return fmt.Errorf("write MAPI Contents proof: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM mapi_contents_proofs WHERE mailbox_id = ? AND sequence <= (
			SELECT sequence FROM mapi_contents_proofs WHERE mailbox_id = ?
			ORDER BY sequence DESC LIMIT 1 OFFSET ?
		)
	`, mailboxID, mailboxID, maxMAPIContentsProofs); err != nil {
		return fmt.Errorf("compact MAPI Contents proofs: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit MAPI Contents proof: %w", err)
	}
	return nil
}
