// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"ewsgo/internal/core"
)

const mapiStagedIDRetention = 24 * time.Hour

func validStagedNamespace(namespace MAPIObjectNamespace) bool {
	return namespace >= MAPIFolder && namespace <= MAPIAssociatedMessage
}

func applyMAPIStaging(ctx context.Context, tx *sql.Tx, mailbox string, change MAPIReplicaMutation) error {
	if change.Stage == 0 && len(change.StagedBindings) == 0 {
		return nil
	}
	now := time.Now().UTC()
	for _, binding := range change.StagedBindings {
		result, err := tx.ExecContext(ctx, `
			DELETE FROM mapi_staged_ids AS s WHERE mailbox_id = ? AND counter = ? AND namespace = ?
			AND (expires_at > ? OR EXISTS (
				SELECT 1 FROM mapi_draft_placements AS d
				WHERE d.mailbox_id = s.mailbox_id AND d.counter = s.counter AND d.native_id = ''
			))
		`, mailbox, binding.Counter, binding.Namespace, now.Unix())
		if err != nil {
			return fmt.Errorf("consume staged MAPI ID: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check consumed MAPI ID: %w", err)
		}
		if n != 1 {
			return core.ErrConflict
		}
	}
	if change.Stage == 0 {
		return nil
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM mapi_staged_ids AS s WHERE mailbox_id = ? AND expires_at <= ? AND NOT EXISTS (
			SELECT 1 FROM mapi_draft_placements AS d
			WHERE d.mailbox_id = s.mailbox_id AND d.counter = s.counter AND d.native_id = ''
		)
	`, mailbox, now.Unix()); err != nil {
		return fmt.Errorf("expire staged MAPI IDs: %w", err)
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM mapi_staged_ids WHERE mailbox_id = ?", mailbox).Scan(&count); err != nil {
		return fmt.Errorf("count staged MAPI IDs: %w", err)
	}
	if count >= maxMAPIObjectID {
		return core.ErrLimit
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO mapi_staged_ids VALUES (?, ?, ?, ?)", mailbox,
		change.HighWater, change.Stage, now.Add(mapiStagedIDRetention).Unix()); err != nil {
		return fmt.Errorf("issue staged MAPI ID: %w", err)
	}
	return nil
}
