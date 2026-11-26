// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const maxMAPIFolderCreations = 32

func validMAPIFolderCreation(p mapireplica.FolderCreation, highWater uint64) bool {
	return p.Counter > 0 && p.Counter <= highWater &&
		len(p.Parent) > 0 && len(p.Parent) <= maxMAPIObjectKey &&
		len(p.Name) > 0 && len(p.Name) <= maxMAPIObjectKey &&
		len(p.ChangeKey) >= 17 && len(p.ChangeKey) <= 24 && len(p.NativeID) <= maxMAPIObjectKey
}

func loadMAPIFolderCreations(ctx context.Context, tx *sql.Tx, mailbox string, highWater uint64) ([]mapireplica.FolderCreation, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT counter, parent, name, change_key, native_id FROM mapi_folder_creations
		WHERE mailbox_id = ? ORDER BY counter LIMIT ?
	`, mailbox, maxMAPIFolderCreations+1)
	if err != nil {
		return nil, fmt.Errorf("read MAPI folder creations: %w", err)
	}
	defer rows.Close()
	var result []mapireplica.FolderCreation
	for rows.Next() {
		var p mapireplica.FolderCreation
		if err := rows.Scan(&p.Counter, &p.Parent, &p.Name, &p.ChangeKey, &p.NativeID); err != nil {
			return nil, fmt.Errorf("decode MAPI folder creation: %w", err)
		}
		if len(result) >= maxMAPIFolderCreations || !validMAPIFolderCreation(p, highWater) {
			return nil, core.ErrInvalid
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func checkMAPIFolderClaims(ctx context.Context, tx *sql.Tx, mailbox string, bindings []mapireplica.Binding, creation *mapireplica.FolderCreation) error {
	for _, binding := range bindings {
		var claimed bool
		err := tx.QueryRowContext(ctx, `SELECT EXISTS (
			SELECT 1 FROM mapi_folder_creations WHERE mailbox_id = ? AND counter = ?
		)`, mailbox, binding.Counter).Scan(&claimed)
		if err != nil {
			return fmt.Errorf("read MAPI folder claim: %w", err)
		}
		if claimed && (creation == nil || creation.Counter != binding.Counter || creation.NativeID == "" || binding.Namespace != mapireplica.Folder) {
			return core.ErrConflict
		}
	}
	return nil
}

func writeMAPIFolderCreation(ctx context.Context, tx *sql.Tx, mailbox string, highWater uint64, m mapireplica.Mutation) error {
	if m.ForgetFolderCreation != 0 {
		if m.FolderCreation != nil {
			return core.ErrInvalid
		}
		_, err := tx.ExecContext(ctx, "DELETE FROM mapi_folder_creations WHERE mailbox_id = ? AND counter = ?", mailbox, m.ForgetFolderCreation)
		return err
	}
	p := m.FolderCreation
	if p == nil {
		return nil
	}
	if !validMAPIFolderCreation(*p, highWater) {
		return core.ErrInvalid
	}
	if err := requireMAPIReservedID(ctx, tx, mailbox, highWater, p.Counter); err != nil {
		return err
	}
	if err := checkMAPIMessageMoveReservation(ctx, tx, mailbox, p.Counter); err != nil {
		return err
	}
	if err := checkMAPIMessageImportReservation(ctx, tx, mailbox, p.Counter); err != nil {
		return err
	}
	var namespace mapireplica.Namespace
	err := tx.QueryRowContext(ctx, "SELECT namespace FROM mapi_object_ids WHERE mailbox_id = ? AND counter = ?", mailbox, p.Counter).Scan(&namespace)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read MAPI folder creation binding: %w", err)
	}
	if p.NativeID == "" && err == nil || p.NativeID != "" && (err != nil || namespace != mapireplica.Folder) {
		return core.ErrConflict
	}
	if p.NativeID == "" {
		var count int
		if err := tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM mapi_folder_creations WHERE mailbox_id = ?", mailbox).Scan(&count); err != nil {
			return err
		}
		if count >= maxMAPIFolderCreations {
			return core.ErrLimit
		}
		_, err = tx.ExecContext(ctx, `
			INSERT INTO mapi_folder_creations (mailbox_id, counter, parent, name, change_key, native_id)
			VALUES (?, ?, ?, ?, ?, '')
		`, mailbox, p.Counter, p.Parent, p.Name, p.ChangeKey)
		return err
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE mapi_folder_creations SET native_id = ? WHERE mailbox_id = ? AND counter = ? AND
		parent = ? AND name = ? AND change_key = ? AND native_id IN ('', ?)
	`, p.NativeID, mailbox, p.Counter, p.Parent, p.Name, p.ChangeKey, p.NativeID)
	if err != nil {
		return fmt.Errorf("acknowledge MAPI folder creation: %w", err)
	}
	n, err := result.RowsAffected()
	if err == nil && n != 1 {
		return core.ErrConflict
	}
	return err
}
