// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"math"

	"ewsgo/internal/core"
)

type nativeProjection struct {
	store     *Store
	mailboxID string
}

func (s *Store) OpenNativeProjection(ctx context.Context, scope core.Scope, backendInstance string) (core.NativeProjection, error) {
	identity, err := s.BindMailbox(ctx, scope, backendInstance)
	if err != nil {
		return nil, err
	}
	return &nativeProjection{s, identity.MailboxID}, nil
}

func (p *nativeProjection) Checkpoint(ctx context.Context) (core.NativeCheckpoint, error) {
	return p.readCheckpoint(ctx, p.store.db)
}

func (p *nativeProjection) readCheckpoint(ctx context.Context, db rowQueryer) (core.NativeCheckpoint, error) {
	checkpoint, projected, err := readDeltaState(ctx, db, p.mailboxID)
	if err != nil {
		return core.NativeCheckpoint{}, err
	}
	if checkpoint.Cursor != "" && !projected {
		return core.NativeCheckpoint{}, core.ErrResync
	}
	return checkpoint, nil
}

func (p *nativeProjection) Members(ctx context.Context, expected core.NativeCheckpoint, ids []core.ID) (map[core.ID]core.NativeObject, error) {
	tx, err := p.store.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin native membership read: %w", err)
	}
	defer tx.Rollback()
	if err := p.checkCheckpoint(ctx, tx, expected); err != nil {
		return nil, err
	}
	return p.readMembers(ctx, tx, ids)
}

func (p *nativeProjection) checkCheckpoint(ctx context.Context, tx *sql.Tx, expected core.NativeCheckpoint) error {
	current, err := p.readCheckpoint(ctx, tx)
	if err == nil && current != expected {
		return core.ErrConflict
	}
	return err
}

func (p *nativeProjection) readMembers(ctx context.Context, tx *sql.Tx, ids []core.ID) (map[core.ID]core.NativeObject, error) {
	members := make(map[core.ID]core.NativeObject)
	if len(ids) == 0 {
		return members, nil
	}
	// One bound argument keeps the lookup independent of the SQL variable limit.
	encoded, err := json.Marshal(ids)
	if err != nil {
		return nil, fmt.Errorf("encode native membership lookup: %w", err)
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT object_id, object_kind, parent_id, native_revision
		FROM native_projection
		WHERE mailbox_id = ? AND object_id IN (SELECT value FROM json_each(?))
	`, p.mailboxID, string(encoded))
	if err != nil {
		return nil, fmt.Errorf("read native membership: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var member core.NativeObject
		if err := rows.Scan(&member.ID, &member.Kind, &member.Parent, &member.Revision); err != nil {
			return nil, fmt.Errorf("scan native membership: %w", err)
		}
		members[member.ID] = member
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate native membership: %w", err)
	}
	return members, nil
}

func (p *nativeProjection) Apply(ctx context.Context, expected core.NativeCheckpoint, next core.Cursor, observed []core.NativeObject, removed []core.ID) error {
	if next == "" {
		return core.ErrInvalid
	}
	ids, err := projectionIDs(observed, removed)
	if err != nil {
		return err
	}
	tx, err := p.store.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin native projection: %w", err)
	}
	defer tx.Rollback()
	if err := p.checkCheckpoint(ctx, tx, expected); err != nil {
		return err
	}
	members, err := p.readMembers(ctx, tx, ids)
	if err != nil {
		return err
	}
	events, err := p.project(ctx, tx, observed, removed, members)
	if err != nil {
		return err
	}
	if next == expected.Cursor {
		if len(events) != 0 {
			return core.ErrInvalid
		}
		return nil
	}
	if _, err := writeDelta(ctx, tx, p.mailboxID, expected, next, events, true); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit native projection: %w", err)
	}
	return nil
}

func projectionIDs(observed []core.NativeObject, removed []core.ID) ([]core.ID, error) {
	ids := make([]core.ID, 0, len(observed)+len(removed))
	seen := make(map[core.ID]bool, cap(ids)) // true for a removal, false for an observation
	for _, id := range removed {
		if id == "" || seen[id] {
			return nil, core.ErrInvalid
		}
		seen[id] = true
		ids = append(ids, id)
	}
	for _, object := range observed {
		_, duplicate := seen[object.ID]
		if object.ID == "" || duplicate || seen[object.Parent] ||
			object.ID == object.Parent || object.Revision == 0 || object.Revision > math.MaxInt64 ||
			object.Kind != core.Folder && (object.Kind != core.Message || object.Parent == "") {
			return nil, core.ErrInvalid
		}
		seen[object.ID] = false
		ids = append(ids, object.ID)
	}
	return ids, nil
}

func (p *nativeProjection) project(ctx context.Context, tx *sql.Tx, observed []core.NativeObject, removed []core.ID, members map[core.ID]core.NativeObject) ([]DeltaEvent, error) {
	var events []DeltaEvent
	for _, object := range observed {
		before, exists := members[object.ID]
		if exists && before.Kind != 0 && (before.Kind != object.Kind || before.Revision > object.Revision ||
			before.Revision == object.Revision && before.Parent != object.Parent) {
			return nil, core.ErrConflict
		}
		if exists && before == object {
			continue
		}
		change := core.Change{ID: object.ID, Parent: object.Parent, Kind: core.Created}
		if exists {
			change.Kind = core.Updated
			if before.Parent != object.Parent {
				change.Kind = core.Moved
				change.PreviousParent = before.Parent
			}
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO native_projection (mailbox_id, object_id, object_kind, parent_id, native_revision)
			VALUES (?, ?, ?, ?, ?)
			ON CONFLICT (mailbox_id, object_id) DO UPDATE SET parent_id = excluded.parent_id, native_revision = excluded.native_revision
		`, p.mailboxID, object.ID, object.Kind, object.Parent, object.Revision); err != nil {
			return nil, fmt.Errorf("write native membership: %w", err)
		}
		events = append(events, DeltaEvent{Change: change, ObjectKind: object.Kind})
	}
	for _, id := range removed {
		if before, exists := members[id]; exists {
			if _, err := tx.ExecContext(ctx, `DELETE FROM native_projection WHERE mailbox_id = ? AND object_id = ?`, p.mailboxID, id); err != nil {
				return nil, fmt.Errorf("remove native membership: %w", err)
			}
			events = append(events, DeltaEvent{Change: core.Change{Kind: core.Deleted, ID: id, PreviousParent: before.Parent}, ObjectKind: before.Kind})
		}
	}
	return events, nil
}
