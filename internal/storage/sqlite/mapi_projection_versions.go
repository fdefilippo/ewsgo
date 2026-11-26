// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"encoding/binary"
	"fmt"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

type MAPIProjectionVersion = mapireplica.Projection

func validMAPIProjection(version MAPIProjectionVersion, highWater uint64) bool {
	return version.Counter > 0 && version.Counter <= highWater && version.Parent <= highWater &&
		version.Parent != version.Counter && version.ChangeNumber > 0 && version.ChangeNumber <= highWater &&
		version.ReadNumber <= highWater && version.Modified.Year() >= 1601 && version.Modified.Year() <= 9999 &&
		(len(version.ChangeKey) == 0 || len(version.ChangeKey) >= 17 && len(version.ChangeKey) <= 24) &&
		len(version.Predecessors) <= 1600
}

func validateMAPIProjectionBatch(versions []MAPIProjectionVersion, previous, next uint64) error {
	if len(versions) > maxMAPIObjectID {
		return core.ErrUnsupported
	}
	for _, version := range versions {
		if !validMAPIProjection(version, next) || max(version.ChangeNumber, version.ReadNumber) <= previous {
			return core.ErrInvalid
		}
	}
	return nil
}

func loadMAPIProjection(ctx context.Context, tx *sql.Tx, mailboxID string, highWater uint64) ([]MAPIProjectionVersion, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT counter, COALESCE(parent, 0), change_number, read_number, fingerprint,
			native_revision, modified, change_key, predecessors, is_present, is_read
		FROM mapi_projection_versions WHERE mailbox_id = ? ORDER BY counter LIMIT ?
	`, mailboxID, maxMAPIObjectID+1)
	if err != nil {
		return nil, fmt.Errorf("read MAPI projection versions: %w", err)
	}
	defer rows.Close()
	var versions []MAPIProjectionVersion
	for rows.Next() {
		var version MAPIProjectionVersion
		var fingerprint, native, modified []byte
		if err := rows.Scan(&version.Counter, &version.Parent, &version.ChangeNumber, &version.ReadNumber,
			&fingerprint, &native, &modified, &version.ChangeKey, &version.Predecessors, &version.Present, &version.Read); err != nil {
			return nil, fmt.Errorf("decode MAPI projection version: %w", err)
		}
		if len(fingerprint) != len(version.Fingerprint) || len(native) != 8 ||
			version.Modified.UnmarshalBinary(modified) != nil || !validMAPIProjection(version, highWater) {
			return nil, core.ErrInvalid
		}
		copy(version.Fingerprint[:], fingerprint)
		version.NativeRevision = core.Revision(binary.LittleEndian.Uint64(native))
		versions = append(versions, version)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate MAPI projection versions: %w", err)
	}
	if len(versions) > maxMAPIObjectID {
		return nil, core.ErrUnsupported
	}
	return versions, nil
}

func writeMAPIProjection(ctx context.Context, tx *sql.Tx, mailboxID string, versions []MAPIProjectionVersion) error {
	written := make(map[uint64]struct{}, len(versions))
	for _, version := range versions {
		if _, duplicate := written[version.Counter]; duplicate {
			return core.ErrInvalid
		}
		written[version.Counter] = struct{}{}
		modified, err := version.Modified.UTC().MarshalBinary()
		if err != nil {
			return core.ErrInvalid
		}
		native := binary.LittleEndian.AppendUint64(nil, uint64(version.NativeRevision))
		result, err := tx.ExecContext(ctx, `
			INSERT INTO mapi_projection_versions (mailbox_id, counter, parent, change_number, read_number,
				fingerprint, native_revision, modified, change_key, predecessors, is_present, is_read)
			VALUES (?, ?, NULLIF(?, 0), ?, ?, ?, ?, ?, COALESCE(?, X''), COALESCE(?, X''), ?, ?)
			ON CONFLICT (mailbox_id, counter) DO UPDATE SET
				parent = excluded.parent,
				change_number = excluded.change_number,
				read_number = excluded.read_number,
				fingerprint = excluded.fingerprint,
				native_revision = excluded.native_revision,
				modified = excluded.modified,
				change_key = excluded.change_key,
				predecessors = excluded.predecessors,
				is_present = excluded.is_present,
				is_read = excluded.is_read
			WHERE excluded.change_number >= mapi_projection_versions.change_number
			  AND excluded.read_number >= mapi_projection_versions.read_number
		`, mailboxID, version.Counter, version.Parent, version.ChangeNumber, version.ReadNumber,
			version.Fingerprint[:], native, modified, version.ChangeKey, version.Predecessors, version.Present, version.Read)
		if err != nil {
			return fmt.Errorf("write MAPI projection version: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check MAPI projection version: %w", err)
		}
		if n != 1 {
			return core.ErrConflict
		}
	}
	return nil
}
