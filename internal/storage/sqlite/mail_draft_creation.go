// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"bytes"
	"context"

	"ewsgo/internal/core"
)

// ClaimMailDraftCreation reserves one native creation for this exact MIME and
// mailbox. A previously claimed request can only be observed. Keep confirmed
// claims after native deletion: the caller can replay a reference without an
// expiry and the missing draft alone cannot distinguish a retry from creation.
func (s *Store) ClaimMailDraftCreation(ctx context.Context, mailbox string, folder core.ID, reference string, digest [32]byte) (bool, core.ID, error) {
	if mailbox == "" || folder == "" || !core.ValidDraftReference(reference) {
		return false, "", core.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return false, "", err
	}
	defer tx.Rollback()
	result, err := tx.ExecContext(ctx, `
		INSERT INTO mail_draft_creations (mailbox_id, reference, folder_id, mime_digest)
		VALUES (?, ?, ?, ?) ON CONFLICT (mailbox_id, reference) DO NOTHING
	`, mailbox, reference, folder, digest[:])
	if err != nil {
		return false, "", err
	}
	inserted, err := result.RowsAffected()
	if err != nil {
		return false, "", err
	}
	var acknowledged core.ID
	if inserted == 0 {
		var storedFolder core.ID
		var storedDigest []byte
		err = tx.QueryRowContext(ctx, `
			SELECT folder_id, mime_digest, acknowledged_id FROM mail_draft_creations
			WHERE mailbox_id = ? AND reference = ?
		`, mailbox, reference).Scan(&storedFolder, &storedDigest, &acknowledged)
		if err != nil {
			return false, "", err
		}
		if storedFolder != folder || !bytes.Equal(storedDigest, digest[:]) {
			return false, "", core.ErrConflict
		}
	}
	err = tx.Commit()
	return inserted == 1 && err == nil, acknowledged, err
}

func (s *Store) AcknowledgeMailDraftCreation(ctx context.Context, mailbox, reference string, id core.ID) error {
	if id == "" {
		return core.ErrInvalid
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE mail_draft_creations SET acknowledged_id = ?
		WHERE mailbox_id = ? AND reference = ? AND acknowledged_id IN ('', ?)
	`, id, mailbox, reference, id)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return core.ErrConflict
	}
	return err
}

// ForgetRejectedMailDraftCreation releases a claim after a definite failure.
func (s *Store) ForgetRejectedMailDraftCreation(ctx context.Context, mailbox, reference string) error {
	result, err := s.db.ExecContext(ctx, `
		DELETE FROM mail_draft_creations WHERE mailbox_id = ? AND reference = ? AND acknowledged_id = ''
	`, mailbox, reference)
	if err != nil {
		return err
	}
	count, err := result.RowsAffected()
	if err == nil && count != 1 {
		return core.ErrConflict
	}
	return err
}
