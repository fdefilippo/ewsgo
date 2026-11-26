// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const (
	maxMAPIFolderValue  = 128 * 1024
	maxMAPIFolderValues = 8 * 1024 * 1024
)

func loadMAPIFolderValues(ctx context.Context, tx *sql.Tx, mailbox string) ([]mapireplica.FolderValue, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT counter, kind, data FROM mapi_folder_values WHERE mailbox_id = ? ORDER BY kind, counter
	`, mailbox)
	if err != nil {
		return nil, fmt.Errorf("read MAPI folder values: %w", err)
	}
	defer rows.Close()
	total := 0
	var values []mapireplica.FolderValue
	for rows.Next() {
		var value mapireplica.FolderValue
		if err := rows.Scan(&value.Counter, &value.Kind, &value.Data); err != nil {
			return nil, fmt.Errorf("decode MAPI folder value: %w", err)
		}
		total += len(value.Data)
		if len(value.Data) > maxMAPIFolderValue || total > maxMAPIFolderValues {
			return nil, core.ErrLimit
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func writeMAPIFolderValues(ctx context.Context, tx *sql.Tx, mailbox string, values []mapireplica.FolderValue) error {
	if len(values) == 0 {
		return nil
	}
	for _, value := range values {
		if value.Counter == 0 || value.Counter > maxMAPICounter || value.Kind < mapireplica.FolderDefinition ||
			value.Kind > mapireplica.FolderRetiredMIDs || len(value.Data) > maxMAPIFolderValue {
			return core.ErrInvalid
		}
		if len(value.Data) == 0 {
			if _, err := tx.ExecContext(ctx, "DELETE FROM mapi_folder_values WHERE mailbox_id = ? AND counter = ? AND kind = ?",
				mailbox, value.Counter, value.Kind); err != nil {
				return fmt.Errorf("delete MAPI folder value: %w", err)
			}
			continue
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO mapi_folder_values (mailbox_id, counter, kind, data)
			SELECT mailbox_id, counter, ?, ? FROM mapi_object_ids WHERE mailbox_id = ? AND counter = ? AND namespace = ?
			ON CONFLICT (mailbox_id, counter, kind) DO UPDATE SET data = excluded.data
		`, value.Kind, value.Data, mailbox, value.Counter, MAPIFolder)
		if err != nil {
			return fmt.Errorf("write MAPI folder value: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check MAPI folder value: %w", err)
		}
		if count != 1 {
			return core.ErrInvalid
		}
	}
	var total int
	if err := tx.QueryRowContext(ctx, "SELECT COALESCE(SUM(length(data)), 0) FROM mapi_folder_values WHERE mailbox_id = ?", mailbox).Scan(&total); err != nil {
		return fmt.Errorf("measure MAPI folder values: %w", err)
	}
	if total > maxMAPIFolderValues {
		return core.ErrLimit
	}
	return nil
}
