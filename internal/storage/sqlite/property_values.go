// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"
	"unicode/utf8"

	"ewsgo/internal/core"
)

const (
	propertyBool = iota + 1
	propertyInteger
	propertyText
	propertyBinary
	propertyTime
	propertyStrings
)

type encodedProperty struct {
	kind int
	data []byte
}

func encodeProperty(value core.Value) (encodedProperty, error) {
	var kind int
	switch v := value.(type) {
	case core.Bool:
		kind = propertyBool
	case core.Integer:
		kind = propertyInteger
	case core.Text:
		if !utf8.ValidString(string(v)) {
			return encodedProperty{}, core.ErrInvalid
		}
		kind = propertyText
	case core.Binary:
		kind = propertyBinary
	case core.Timestamp:
		data, err := time.Time(v).UTC().MarshalBinary()
		return encodedProperty{propertyTime, data}, err
	case core.Strings:
		for _, item := range v {
			if !utf8.ValidString(item) {
				return encodedProperty{}, core.ErrInvalid
			}
		}
		kind = propertyStrings
	default:
		return encodedProperty{}, core.ErrInvalid
	}
	data, err := json.Marshal(value)
	return encodedProperty{kind, data}, err
}

func decodeProperty(kind int, data []byte) (core.Value, error) {
	switch kind {
	case propertyBool:
		return decodePropertyJSON[core.Bool](data)
	case propertyInteger:
		return decodePropertyJSON[core.Integer](data)
	case propertyText:
		return decodePropertyJSON[core.Text](data)
	case propertyBinary:
		return decodePropertyJSON[core.Binary](data)
	case propertyTime:
		var instant time.Time
		err := instant.UnmarshalBinary(data)
		return core.Timestamp(instant), err
	case propertyStrings:
		return decodePropertyJSON[core.Strings](data)
	default:
		return nil, core.ErrInvalid
	}
}

func decodePropertyJSON[T core.Value](data []byte) (core.Value, error) {
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		return nil, err
	}
	return value, nil
}

func readProjectedObject(ctx context.Context, tx *sql.Tx, mailboxID string, objectID core.ID) (core.Change, error) {
	latest := core.Change{ID: objectID}
	err := tx.QueryRowContext(ctx, `
		SELECT kind, parent_id, revision FROM journal WHERE mailbox_id = ? AND object_id = ?
		ORDER BY revision DESC LIMIT 1
	`, mailboxID, objectID).Scan(&latest.Kind, &latest.Parent, &latest.Revision)
	if errors.Is(err, sql.ErrNoRows) {
		return core.Change{}, core.ErrNotFound
	}
	if err != nil {
		return core.Change{}, fmt.Errorf("read projected object revision: %w", err)
	}
	if latest.Kind == core.Deleted {
		return core.Change{}, core.ErrNotFound
	}
	return latest, nil
}

// ObjectProperties reads the gateway-owned properties and the current local
// object revision from one snapshot. The caller owns mailbox authorization.
func (s *Store) ObjectProperties(ctx context.Context, mailboxID string, objectID core.ID) (core.Properties, core.Revision, error) {
	tx, err := s.db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return nil, 0, fmt.Errorf("begin object properties read: %w", err)
	}
	defer tx.Rollback()
	latest, err := readProjectedObject(ctx, tx, mailboxID, objectID)
	if err != nil {
		return nil, 0, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT property_set, property_id, property_name, empty_name, value_type, value
		FROM object_properties WHERE mailbox_id = ? AND object_id = ?
	`, mailboxID, objectID)
	if err != nil {
		return nil, 0, fmt.Errorf("read object properties: %w", err)
	}
	defer rows.Close()
	properties := core.Properties{}
	for rows.Next() {
		var key core.Key
		var value encodedProperty
		if err := rows.Scan(&key.Set, &key.ID, &key.Name, &key.EmptyName, &value.kind, &value.data); err != nil {
			return nil, 0, fmt.Errorf("read stored property: %w", err)
		}
		decoded, err := decodeProperty(value.kind, value.data)
		if err != nil || !key.Valid() {
			return nil, 0, errors.New("invalid stored property")
		}
		properties[key] = decoded
	}
	if err := rows.Err(); err != nil {
		return nil, 0, fmt.Errorf("iterate stored properties: %w", err)
	}
	return properties, latest.Revision, nil
}

// PatchObjectProperties applies the property subset of a core patch. It
// records one update for an effective edit, using the object's existing parent.
// Its revision is independent of the last native delta checkpoint.
func (s *Store) PatchObjectProperties(ctx context.Context, mailboxID string, objectID core.ID, expected core.Revision, patch core.Patch) (core.Revision, error) {
	sets, removals, err := encodePropertyPatch(patch)
	if err != nil {
		return 0, err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("begin object properties patch: %w", err)
	}
	defer tx.Rollback()
	latest, err := readProjectedObject(ctx, tx, mailboxID, objectID)
	if err != nil {
		return 0, err
	}
	if latest.Revision != expected {
		return 0, core.ErrConflict
	}
	effective, err := writePropertyPatch(ctx, tx, mailboxID, objectID, sets, removals)
	if err != nil {
		return 0, err
	}
	if !effective {
		return latest.Revision, nil
	}
	revision, err := reserveJournalRevisions(ctx, tx, mailboxID, 1)
	if err != nil {
		return 0, err
	}
	update := core.Change{Kind: core.Updated, ID: objectID, Parent: latest.Parent, Revision: revision}
	if err := appendJournalChange(ctx, tx, mailboxID, update, 0); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("commit object properties patch: %w", err)
	}
	return revision, nil
}

func encodePropertyPatch(patch core.Patch) (map[core.Key]encodedProperty, []core.Key, error) {
	if patch.Parent != nil || patch.MIME != nil || patch.Recipients != nil || patch.Attachments != nil {
		return nil, nil, core.ErrInvalid
	}
	sets := make(map[core.Key]encodedProperty, len(patch.Set))
	for key, value := range patch.Set {
		key, err := normalizePropertyKey(key)
		if _, duplicate := sets[key]; err != nil || duplicate || key.Set == standardMAPISet {
			return nil, nil, core.ErrInvalid
		}
		sets[key], err = encodeProperty(value)
		if err != nil {
			return nil, nil, err
		}
	}
	removals := make([]core.Key, 0, len(patch.Remove))
	for _, key := range patch.Remove {
		key, err := normalizePropertyKey(key)
		if _, overlap := sets[key]; err != nil || overlap || key.Set == standardMAPISet {
			return nil, nil, core.ErrInvalid
		}
		removals = append(removals, key)
	}
	return sets, removals, nil
}

func writePropertyPatch(ctx context.Context, tx *sql.Tx, mailboxID string, objectID core.ID, sets map[core.Key]encodedProperty, removals []core.Key) (bool, error) {
	var effective bool
	for key, next := range sets {
		var old encodedProperty
		err := tx.QueryRowContext(ctx, `
			SELECT value_type, value FROM object_properties WHERE mailbox_id = ? AND object_id = ?
			AND property_set = ? AND property_id = ? AND property_name = ? AND empty_name = ?
		`, mailboxID, objectID, key.Set, key.ID, key.Name, key.EmptyName).Scan(&old.kind, &old.data)
		if err == nil {
			if old.kind != next.kind {
				return false, core.ErrConflict
			}
			if bytes.Equal(old.data, next.data) {
				continue
			}
		} else if !errors.Is(err, sql.ErrNoRows) {
			return false, fmt.Errorf("read previous property value: %w", err)
		}
		if _, err := tx.ExecContext(ctx, `
			INSERT INTO object_properties (mailbox_id, object_id, property_set, property_id, property_name, empty_name, value_type, value)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (mailbox_id, object_id, property_set, property_id, property_name, empty_name)
			DO UPDATE SET value = excluded.value
		`, mailboxID, objectID, key.Set, key.ID, key.Name, key.EmptyName, next.kind, next.data); err != nil {
			return false, fmt.Errorf("write property value: %w", err)
		}
		effective = true
	}
	for _, key := range removals {
		result, err := tx.ExecContext(ctx, `
			DELETE FROM object_properties WHERE mailbox_id = ? AND object_id = ?
			AND property_set = ? AND property_id = ? AND property_name = ? AND empty_name = ?
		`, mailboxID, objectID, key.Set, key.ID, key.Name, key.EmptyName)
		if err != nil {
			return false, fmt.Errorf("remove property value: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return false, fmt.Errorf("count removed property values: %w", err)
		}
		effective = effective || n > 0
	}
	return effective, nil
}
