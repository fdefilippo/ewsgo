// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"

	"ewsgo/internal/core"
)

const (
	standardMAPISet    = "00020328-0000-0000-c000-000000000046"
	internetHeadersSet = "00020386-0000-0000-c000-000000000046"
	lastNamedID        = 0xfffe
)

var ErrNamedPropertyQuota = errors.New("mailbox named property ID space exhausted")

func normalizePropertyKey(key core.Key) (core.Key, error) {
	if !key.Valid() {
		return core.Key{}, core.ErrInvalid
	}
	if key.Set == internetHeadersSet {
		key.Name = strings.ToLower(key.Name)
	}
	return key, nil
}

func requireMailbox(ctx context.Context, db rowQueryer, mailboxID string) error {
	var found int
	err := db.QueryRowContext(ctx, `SELECT 1 FROM mailboxes WHERE mailbox_id = ?`, mailboxID).Scan(&found)
	if errors.Is(err, sql.ErrNoRows) {
		return core.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("verify mailbox scope: %w", err)
	}
	return nil
}

// NamedPropertyIDs resolves a batch in input order. Zero denotes an unknown
// name. An empty batch lists registered IDs. Creation requires authorization
// from the caller and publishes all new assignments together.
func (s *Store) NamedPropertyIDs(ctx context.Context, mailboxID string, names []core.Key, create bool) ([]uint16, error) {
	return s.namedPropertyIDs(ctx, mailboxID, names, create, lastNamedID)
}

// RegisterNamedProperties applies a frontend's wire-ID limit to the whole
// assignment transaction. Previously registered names remain readable.
func (s *Store) RegisterNamedProperties(ctx context.Context, mailboxID string, names []core.Key, lastID uint16) ([]uint16, error) {
	if len(names) == 0 || lastID < 0x8001 || lastID > lastNamedID {
		return nil, core.ErrInvalid
	}
	return s.namedPropertyIDs(ctx, mailboxID, names, true, lastID)
}

func (s *Store) namedPropertyIDs(ctx context.Context, mailboxID string, names []core.Key, create bool, lastID uint16) ([]uint16, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: !create || len(names) == 0})
	if err != nil {
		return nil, fmt.Errorf("begin named property lookup: %w", err)
	}
	defer tx.Rollback()
	if err := requireMailbox(ctx, tx, mailboxID); err != nil {
		return nil, err
	}
	if len(names) == 0 {
		return listNamedPropertyIDs(ctx, tx, mailboxID)
	}
	ids := make([]uint16, len(names))
	for i, name := range names {
		key, err := normalizePropertyKey(name)
		if err != nil || key.Set == "" {
			return nil, core.ErrInvalid
		}
		if key.Set == standardMAPISet {
			if !key.IsName() && key.ID <= 0xffff {
				ids[i] = uint16(key.ID)
			}
			continue
		}
		ids[i], err = findNamedPropertyID(ctx, tx, mailboxID, key, create, lastID)
		if err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit named property assignments: %w", err)
	}
	return ids, nil
}

func findNamedPropertyID(ctx context.Context, tx *sql.Tx, mailboxID string, key core.Key, create bool, lastID uint16) (uint16, error) {
	var id uint16
	err := tx.QueryRowContext(ctx, `
		SELECT wire_id FROM named_property_ids WHERE mailbox_id = ? AND property_set = ?
		AND property_id = ? AND property_name = ? AND empty_name = ?
	`, mailboxID, key.Set, key.ID, key.Name, key.EmptyName).Scan(&id)
	if !errors.Is(err, sql.ErrNoRows) {
		if err != nil {
			return 0, fmt.Errorf("read named property ID: %w", err)
		}
		return id, nil
	}
	if !create {
		return 0, nil
	}
	if err := tx.QueryRowContext(ctx, `
		SELECT COALESCE(MAX(wire_id), 32768) FROM named_property_ids WHERE mailbox_id = ?
	`, mailboxID).Scan(&id); err != nil {
		return 0, fmt.Errorf("find last named property ID: %w", err)
	}
	if id >= lastID {
		return 0, ErrNamedPropertyQuota
	}
	id++
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO named_property_ids (mailbox_id, property_set, property_id, property_name, empty_name, wire_id)
		VALUES (?, ?, ?, ?, ?, ?)
	`, mailboxID, key.Set, key.ID, key.Name, key.EmptyName, id); err != nil {
		return 0, fmt.Errorf("assign named property ID: %w", err)
	}
	return id, nil
}

func listNamedPropertyIDs(ctx context.Context, tx *sql.Tx, mailboxID string) ([]uint16, error) {
	rows, err := tx.QueryContext(ctx, `SELECT wire_id FROM named_property_ids WHERE mailbox_id = ? ORDER BY wire_id`, mailboxID)
	if err != nil {
		return nil, fmt.Errorf("list named property IDs: %w", err)
	}
	defer rows.Close()
	var ids []uint16
	for rows.Next() {
		var id uint16
		if err := rows.Scan(&id); err != nil {
			return nil, fmt.Errorf("read registered named property ID: %w", err)
		}
		ids = append(ids, id)
	}
	return ids, rows.Err()
}

// LoadNamedPropertyBindings loads a bounded, consistent mailbox registry.
func (s *Store) LoadNamedPropertyBindings(ctx context.Context, mailboxID string, maximum int) (map[uint16]core.Key, error) {
	if maximum <= 0 || maximum > lastNamedID-0x8000 {
		return nil, core.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if err := requireMailbox(ctx, tx, mailboxID); err != nil {
		return nil, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT wire_id, property_set, property_id, property_name, empty_name
		FROM named_property_ids WHERE mailbox_id = ? ORDER BY wire_id LIMIT ?
	`, mailboxID, maximum+1)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	names := make(map[uint16]core.Key)
	for rows.Next() {
		if len(names) == maximum {
			return nil, ErrNamedPropertyQuota
		}
		var id uint16
		var key core.Key
		if err := rows.Scan(&id, &key.Set, &key.ID, &key.Name, &key.EmptyName); err != nil {
			return nil, err
		}
		names[id] = key
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return names, nil
}

// NamedPropertyKeys resolves the inverse mapping. Unassigned IDs are absent.
func (s *Store) NamedPropertyKeys(ctx context.Context, mailboxID string, ids []uint16) (map[uint16]core.Key, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, fmt.Errorf("begin reverse named property lookup: %w", err)
	}
	defer tx.Rollback()
	if err := requireMailbox(ctx, tx, mailboxID); err != nil {
		return nil, err
	}
	names := make(map[uint16]core.Key, len(ids))
	for _, id := range ids {
		if id > 0 && id < 0x8000 {
			names[id] = core.Key{Set: standardMAPISet, ID: uint32(id)}
			continue
		}
		var key core.Key
		err := tx.QueryRowContext(ctx, `
			SELECT property_set, property_id, property_name, empty_name
			FROM named_property_ids WHERE mailbox_id = ? AND wire_id = ?
		`, mailboxID, id).Scan(&key.Set, &key.ID, &key.Name, &key.EmptyName)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("read name for property ID: %w", err)
		}
		names[id] = key
	}
	return names, nil
}
