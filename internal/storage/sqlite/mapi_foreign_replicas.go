// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"ewsgo/internal/core"
)

const maxMAPIForeignReplicas = 256

func loadMAPIForeignReplicas(ctx context.Context, tx *sql.Tx, mailbox string) ([][16]byte, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT replid, guid FROM mapi_foreign_replicas WHERE mailbox_id = ? ORDER BY replid LIMIT ?
	`, mailbox, maxMAPIForeignReplicas+1)
	if err != nil {
		return nil, fmt.Errorf("read MAPI foreign replicas: %w", err)
	}
	defer rows.Close()
	var result [][16]byte
	for rows.Next() {
		var id int
		var raw []byte
		if err := rows.Scan(&id, &raw); err != nil {
			return nil, fmt.Errorf("decode MAPI foreign replica: %w", err)
		}
		if id != len(result)+2 || len(raw) != 16 || len(result) == maxMAPIForeignReplicas {
			return nil, core.ErrInvalid
		}
		result = append(result, [16]byte(raw))
	}
	return result, rows.Err()
}

func writeMAPIForeignReplica(ctx context.Context, tx *sql.Tx, mailbox string, guid [16]byte) error {
	var next int
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(replid), 1) + 1 FROM mapi_foreign_replicas WHERE mailbox_id = ?
	`, mailbox).Scan(&next); err != nil {
		return fmt.Errorf("read last MAPI foreign replica: %w", err)
	}
	if next > maxMAPIForeignReplicas+1 {
		return core.ErrLimit
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO mapi_foreign_replicas VALUES (?, ?, ?)", mailbox, next, guid[:]); err != nil {
		return fmt.Errorf("write MAPI foreign replica: %w", err)
	}
	return nil
}
