// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"time"

	"ewsgo/internal/core"
)

func (s *Store) RegisterDraftProperties(ctx context.Context, mailboxID string, fields map[core.Key]core.Text) (string, error) {
	if mailboxID == "" || len(fields) == 0 || len(fields) > 32 {
		return "", core.ErrInvalid
	}
	for key, value := range fields {
		if !validDraftStringProperty(key, value) {
			return "", core.ErrInvalid
		}
	}
	var random [16]byte
	if _, err := rand.Read(random[:]); err != nil {
		return "", err
	}
	ref := hex.EncodeToString(random[:])
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return "", err
	}
	defer tx.Rollback()
	createdAt := time.Now().UnixNano()
	for key, value := range fields {
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO draft_string_properties (mailbox_id, reference, created_at, property_set, property_name, value)
			VALUES (?, ?, ?, ?, ?, ?)
		`, mailboxID, ref, createdAt, key.Set, key.Name, value); err != nil {
			return "", err
		}
	}
	return ref, tx.Commit()
}

func (s *Store) DiscardDraftProperties(ctx context.Context, mailboxID, ref string) error {
	_, err := s.db.ExecContext(ctx, "DELETE FROM draft_string_properties WHERE mailbox_id = ? AND reference = ?", mailboxID, ref)
	return err
}

type DraftReference struct {
	Token     string
	CreatedAt int64
}

func (s *Store) FindDraftReferences(ctx context.Context, mailboxID string, key core.Key, value core.Text) ([]DraftReference, error) {
	if mailboxID == "" || !validDraftStringProperty(key, value) {
		return nil, core.ErrInvalid
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT reference, created_at FROM draft_string_properties
		WHERE mailbox_id = ? AND property_set = ? AND property_name = ? AND value = ?
		ORDER BY reference LIMIT 101
	`, mailboxID, key.Set, key.Name, value)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var refs []DraftReference
	for rows.Next() {
		var ref DraftReference
		if err := rows.Scan(&ref.Token, &ref.CreatedAt); err != nil {
			return nil, err
		}
		refs = append(refs, ref)
	}
	if len(refs) > 100 {
		return nil, core.ErrLimit
	}
	return refs, rows.Err()
}

func (s *Store) DraftProperties(ctx context.Context, mailboxID, ref string) (map[core.Key]core.Text, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT property_set, property_name, value FROM draft_string_properties
		WHERE mailbox_id = ? AND reference = ?
	`, mailboxID, ref)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	fields := make(map[core.Key]core.Text)
	for rows.Next() {
		var key core.Key
		var value core.Text
		if err := rows.Scan(&key.Set, &key.Name, &value); err != nil {
			return nil, err
		}
		fields[key] = value
	}
	return fields, rows.Err()
}

func validDraftStringProperty(key core.Key, value core.Text) bool {
	return key.Valid() && key.Set != "" && key.Name != "" && len(key.Name) <= 255 && len(value) <= 4096
}

// SetDraftStringProperty updates one gateway-owned field for an existing
// opaque draft reference. The reference can be registered before SOAP writes.
func (s *Store) SetDraftStringProperty(ctx context.Context, mailboxID, ref string, key core.Key, value core.Text) error {
	if mailboxID == "" || !core.ValidDraftReference(ref) || !validDraftStringProperty(key, value) {
		return core.ErrInvalid
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO draft_string_properties (mailbox_id, reference, created_at, property_set, property_name, value)
		VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (mailbox_id, reference, property_set, property_name) DO UPDATE SET value=excluded.value
	`, mailboxID, ref, time.Now().UnixNano(), key.Set, key.Name, value)
	return err
}
