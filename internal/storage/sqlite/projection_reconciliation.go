// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"fmt"

	"ewsgo/internal/core"
)

func (p *nativeProjection) Baseline(ctx context.Context) (core.NativeBaseline, error) {
	tx, err := p.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return core.NativeBaseline{}, fmt.Errorf("begin native baseline read: %w", err)
	}
	defer tx.Rollback()
	checkpoint, projected, err := readDeltaState(ctx, tx, p.mailboxID)
	if err != nil {
		return core.NativeBaseline{}, err
	}
	baseline := core.NativeBaseline{Checkpoint: checkpoint, Legacy: !projected && checkpoint.Cursor != ""}
	baseline.Objects, err = p.baselineObjects(ctx, tx, baseline.Legacy)
	if err != nil {
		return core.NativeBaseline{}, err
	}
	return baseline, nil
}

func (p *nativeProjection) baselineObjects(ctx context.Context, tx *sql.Tx, legacy bool) ([]core.NativeObject, error) {
	query := `
		SELECT object_id, object_kind, parent_id, native_revision
		FROM native_projection WHERE mailbox_id = ?`
	args := []any{p.mailboxID}
	if legacy {
		// Latest live journal rows also survive compaction. Their native
		// metadata is unknown until the backend observes the object.
		query = `
			SELECT j.object_id, 0, j.parent_id, 0
			FROM journal AS j
			WHERE j.mailbox_id = ? AND j.kind != ? AND NOT EXISTS (
				SELECT 1 FROM journal AS newer
				WHERE newer.mailbox_id = j.mailbox_id AND newer.object_id = j.object_id
					AND newer.revision > j.revision
			)`
		args = append(args, core.Deleted)
	}
	rows, err := tx.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("read native baseline: %w", err)
	}
	defer rows.Close()
	var objects []core.NativeObject
	for rows.Next() {
		var object core.NativeObject
		if err := rows.Scan(&object.ID, &object.Kind, &object.Parent, &object.Revision); err != nil {
			return nil, fmt.Errorf("scan native baseline object: %w", err)
		}
		objects = append(objects, object)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate native baseline: %w", err)
	}
	return objects, nil
}

func (p *nativeProjection) Reconcile(ctx context.Context, baseline core.NativeBaseline, next core.Cursor, observed []core.NativeObject, removed []core.ID) error {
	if next == "" {
		return core.ErrInvalid
	}
	if _, err := projectionIDs(observed, removed); err != nil {
		return err
	}
	tx, err := p.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin native reconciliation: %w", err)
	}
	defer tx.Rollback()
	current, projected, err := readDeltaState(ctx, tx, p.mailboxID)
	if err != nil {
		return err
	}
	if current != baseline.Checkpoint || baseline.Legacy != (!projected && current.Cursor != "") {
		return core.ErrConflict
	}

	before, err := p.baselineObjects(ctx, tx, baseline.Legacy)
	if err != nil {
		return err
	}
	members := make(map[core.ID]core.NativeObject, len(before))
	for _, object := range before {
		members[object.ID] = object
	}
	events, err := p.project(ctx, tx, observed, removed, members)
	if err != nil {
		return err
	}
	if _, err := writeDelta(ctx, tx, p.mailboxID, current, next, events, true); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit native reconciliation: %w", err)
	}
	return nil
}
