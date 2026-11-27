// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const (
	maxMAPIFAIObjects     = 64
	maxMAPIFAIBytes       = 4 << 20
	maxMAPIFAIObject      = 128 << 10
	maxMAPIFAIPayload     = 1 << 20
	mapiFAITombstoneBytes = 128
)

type MAPIFAIObject = mapireplica.FAIObject
type MAPIFAITombstone = mapireplica.FAITombstone
type MAPIFAIState = mapireplica.FAIState
type MAPIFAIChanges = mapireplica.FAIChanges

type storedFAIProperty struct {
	Key   core.Key
	Kind  int
	Value []byte
}

func encodeFAIProperties(properties core.Properties) ([]byte, error) {
	if len(properties) > 64 {
		return nil, core.ErrUnsupported
	}
	values := make([]storedFAIProperty, 0, len(properties))
	for key, value := range properties {
		if !key.Valid() || len(key.Name) > maxMAPIObjectKey {
			return nil, core.ErrInvalid
		}
		encoded, err := encodeProperty(value)
		if err != nil {
			return nil, err
		}
		values = append(values, storedFAIProperty{key, encoded.kind, encoded.data})
	}
	data, err := json.Marshal(values)
	if err == nil && len(data) > maxMAPIFAIPayload {
		return nil, core.ErrUnsupported
	}
	return data, err
}

func decodeFAIProperties(data []byte) (core.Properties, error) {
	if len(data) > maxMAPIFAIPayload {
		return nil, core.ErrUnsupported
	}
	var values []storedFAIProperty
	if err := json.Unmarshal(data, &values); err != nil || len(values) > 64 {
		return nil, core.ErrInvalid
	}
	properties := make(core.Properties, len(values))
	for _, field := range values {
		if !field.Key.Valid() || len(field.Key.Name) > maxMAPIObjectKey || properties[field.Key] != nil {
			return nil, core.ErrInvalid
		}
		value, err := decodeProperty(field.Kind, field.Value)
		if err != nil {
			return nil, core.ErrInvalid
		}
		properties[field.Key] = value
	}
	return properties, nil
}

func loadMAPIFAI(ctx context.Context, tx *sql.Tx, mailboxID string) (MAPIFAIState, error) {
	if err := checkMAPIFAIQuota(ctx, tx, mailboxID); err != nil {
		return MAPIFAIState{}, err
	}
	objects, err := loadMAPIFAIObjects(ctx, tx, mailboxID)
	if err != nil {
		return MAPIFAIState{}, err
	}
	rows, err := tx.QueryContext(ctx, `
		SELECT source_key, folder FROM mapi_fai_tombstones WHERE mailbox_id = ? ORDER BY source_key
	`, mailboxID)
	if err != nil {
		return MAPIFAIState{}, fmt.Errorf("read MAPI FAI tombstones: %w", err)
	}
	defer rows.Close()
	state := MAPIFAIState{Objects: objects}
	for rows.Next() {
		var tombstone MAPIFAITombstone
		var source []byte
		if err := rows.Scan(&source, &tombstone.Folder); err != nil {
			return MAPIFAIState{}, fmt.Errorf("decode MAPI FAI tombstone: %w", err)
		}
		if len(source) != len(tombstone.Source) {
			return MAPIFAIState{}, core.ErrInvalid
		}
		copy(tombstone.Source[:], source)
		state.Tombstones = append(state.Tombstones, tombstone)
	}
	return state, rows.Err()
}

func loadMAPIFAIObjects(ctx context.Context, tx *sql.Tx, mailboxID string) ([]MAPIFAIObject, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT counter, source_key, private_config, cache_bytes, properties
		FROM mapi_fai_objects WHERE mailbox_id = ? ORDER BY counter
	`, mailboxID)
	if err != nil {
		return nil, fmt.Errorf("read MAPI FAI objects: %w", err)
	}
	defer rows.Close()
	var objects []MAPIFAIObject
	for rows.Next() {
		var object MAPIFAIObject
		var source, data []byte
		if err := rows.Scan(&object.Counter, &source, &object.PrivateConfig, &object.CacheBytes, &data); err != nil {
			return nil, fmt.Errorf("decode MAPI FAI object: %w", err)
		}
		if len(source) != len(object.Source) {
			return nil, core.ErrInvalid
		}
		copy(object.Source[:], source)
		object.Properties, err = decodeFAIProperties(data)
		if err != nil {
			return nil, err
		}
		objects = append(objects, object)
	}
	return objects, rows.Err()
}

// CommitMAPIFAI writes private objects and their exact projected versions
// together. Forget operations accompany the removal of an entire folder.
func (s *Store) CommitMAPIFAI(ctx context.Context, mailboxID string, expected MAPIReplicaVersion, highWater uint64, mappings []MAPIObjectID, versions []MAPIProjectionVersion, changes MAPIFAIChanges) error {
	return s.CommitMAPIReplica(ctx, mailboxID, expected, MAPIReplicaMutation{
		HighWater: highWater, Bindings: mappings, Projection: versions, FAI: &changes,
	})
}

func boundedMAPIFAIChanges(changes MAPIFAIChanges) bool {
	return len(changes.Objects) <= maxMAPIFAIObjects+1 && len(changes.Tombstones) <= maxMAPIFAIObjects &&
		len(changes.ForgetObjects) <= maxMAPIFAIObjects+mapireplica.MaxFAITombstones+1 &&
		len(changes.ForgetTombstones) <= mapireplica.MaxFAITombstones
}

func writeMAPIFAI(ctx context.Context, tx *sql.Tx, mailboxID string, versions []MAPIProjectionVersion, changes MAPIFAIChanges) error {
	for _, counter := range changes.ForgetObjects {
		if _, err := tx.ExecContext(ctx, "DELETE FROM mapi_fai_objects WHERE mailbox_id = ? AND counter = ?", mailboxID, counter); err != nil {
			return fmt.Errorf("remove private MAPI object: %w", err)
		}
	}
	for _, source := range changes.ForgetTombstones {
		if _, err := tx.ExecContext(ctx, "DELETE FROM mapi_fai_tombstones WHERE mailbox_id = ? AND source_key = ?", mailboxID, source[:]); err != nil {
			return fmt.Errorf("remove private MAPI tombstone: %w", err)
		}
	}
	byCounter := make(map[uint64]MAPIProjectionVersion, len(versions))
	for _, version := range versions {
		byCounter[version.Counter] = version
	}
	for _, object := range changes.Objects {
		version, ok := byCounter[object.Counter]
		if !ok || version.Parent == 0 || (!version.Present && len(object.Properties) != 0) ||
			object.Source == ([22]byte{}) || object.CacheBytes <= 0 || object.CacheBytes > maxMAPIFAIObject {
			return core.ErrInvalid
		}
		delete(byCounter, object.Counter)
		if err := saveMAPIFAIObject(ctx, tx, mailboxID, object); err != nil {
			return err
		}
	}
	for _, tombstone := range changes.Tombstones {
		if err := saveMAPIFAITombstone(ctx, tx, mailboxID, tombstone); err != nil {
			return err
		}
	}
	return checkMAPIFAIQuota(ctx, tx, mailboxID)
}

func saveMAPIFAIObject(ctx context.Context, tx *sql.Tx, mailboxID string, object MAPIFAIObject) error {
	properties, err := encodeFAIProperties(object.Properties)
	if err != nil {
		return err
	}
	namespace := MAPIAssociatedMessage
	if object.PrivateConfig {
		namespace = MAPIMessage
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO mapi_fai_objects (mailbox_id, counter, source_key, private_config, cache_bytes, properties)
		SELECT id.mailbox_id, id.counter, ?, ?, ?, ? FROM mapi_object_ids AS id
		WHERE id.mailbox_id = ? AND id.counter = ? AND id.namespace = ?
		  AND NOT EXISTS (SELECT 1 FROM mapi_fai_tombstones WHERE mailbox_id = id.mailbox_id AND source_key = ?)
		ON CONFLICT DO UPDATE SET cache_bytes = excluded.cache_bytes, properties = excluded.properties
		WHERE mapi_fai_objects.counter = excluded.counter
		  AND mapi_fai_objects.source_key = excluded.source_key
		  AND mapi_fai_objects.private_config = excluded.private_config
	`, object.Source[:], object.PrivateConfig, object.CacheBytes, properties, mailboxID, object.Counter, namespace, object.Source[:])
	return checkMAPIFAIWrite(result, err)
}

func saveMAPIFAITombstone(ctx context.Context, tx *sql.Tx, mailboxID string, tombstone MAPIFAITombstone) error {
	if tombstone.Source == ([22]byte{}) {
		return core.ErrInvalid
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO mapi_fai_tombstones (mailbox_id, source_key, folder)
		SELECT id.mailbox_id, ?, id.counter FROM mapi_object_ids AS id
		WHERE id.mailbox_id = ? AND id.counter = ? AND id.namespace = ?
		  AND NOT EXISTS (SELECT 1 FROM mapi_fai_objects WHERE mailbox_id = id.mailbox_id AND source_key = ?)
		ON CONFLICT (mailbox_id, source_key) DO UPDATE SET folder = excluded.folder
		WHERE mapi_fai_tombstones.folder = excluded.folder
	`, tombstone.Source[:], mailboxID, tombstone.Folder, MAPIFolder, tombstone.Source[:])
	return checkMAPIFAIWrite(result, err)
}

func checkMAPIFAIWrite(result sql.Result, err error) error {
	if err != nil {
		return fmt.Errorf("write private MAPI data: %w", err)
	}
	count, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check private MAPI write: %w", err)
	}
	if count != 1 {
		return core.ErrConflict
	}
	return nil
}

func checkMAPIFAIQuota(ctx context.Context, tx *sql.Tx, mailboxID string) error {
	var active, config, absent, deleted, activeBytes, deletedBytes int
	err := tx.QueryRowContext(ctx, `
		SELECT count(*) FILTER (WHERE o.private_config = 0 AND v.is_present = 1),
		       count(*) FILTER (WHERE o.private_config = 1),
		       (SELECT count(*) FROM mapi_fai_tombstones WHERE mailbox_id = ?),
		       count(*) FILTER (WHERE o.private_config = 0 AND v.is_present = 0),
		       COALESCE(sum(o.cache_bytes) FILTER (WHERE o.private_config = 0 AND v.is_present = 1), 0),
		       COALESCE(sum(o.cache_bytes) FILTER (WHERE o.private_config = 0 AND v.is_present = 0), 0)
		FROM mapi_fai_objects AS o
		JOIN mapi_projection_versions AS v USING (mailbox_id, counter)
		WHERE o.mailbox_id = ?
	`, mailboxID, mailboxID).Scan(&active, &config, &absent, &deleted, &activeBytes, &deletedBytes)
	if err != nil {
		return fmt.Errorf("check private MAPI quota: %w", err)
	}
	if active > maxMAPIFAIObjects || config > 1 || activeBytes > maxMAPIFAIBytes ||
		deleted+absent > mapireplica.MaxFAITombstones ||
		deletedBytes+absent*mapiFAITombstoneBytes > mapireplica.MaxFAITombstoneBytes {
		return core.ErrUnsupported
	}
	return nil
}
