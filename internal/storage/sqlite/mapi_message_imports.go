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

func validMAPIMessageImport(p mapireplica.MessageImport, highWater uint64) bool {
	return p.Counter != 0 && p.Parent != 0 && p.Counter != p.Parent && max(p.Counter, p.Parent) <= highWater &&
		p.Digest != ([32]byte{}) && (p.MIME == ([32]byte{})) == (p.Special() != 0) &&
		(p.ContactReference == ([16]byte{}) || p.AppointmentReference == ([16]byte{})) && len(p.NativeID) <= 4096
}

func loadMAPIMessageImports(ctx context.Context, tx *sql.Tx, mailbox string, highWater uint64) ([]mapireplica.MessageImport, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT counter, parent, digest, mime_digest, COALESCE(native_id, ''), contact_reference, appointment_reference FROM mapi_message_imports
		WHERE mailbox_id = ? ORDER BY counter LIMIT ?
	`, mailbox, maxMAPIObjectID+1)
	if err != nil {
		return nil, fmt.Errorf("read MAPI message imports: %w", err)
	}
	defer rows.Close()
	var imports []mapireplica.MessageImport
	for rows.Next() {
		var p mapireplica.MessageImport
		var digest, mime, contact, appointment []byte
		if err := rows.Scan(&p.Counter, &p.Parent, &digest, &mime, &p.NativeID, &contact, &appointment); err != nil {
			return nil, fmt.Errorf("decode MAPI message import: %w", err)
		}
		if len(imports) >= maxMAPIObjectID || len(digest) != len(p.Digest) || len(mime) != len(p.MIME) || len(contact) != len(p.ContactReference) || len(appointment) != len(p.AppointmentReference) {
			return nil, core.ErrInvalid
		}
		copy(p.Digest[:], digest)
		copy(p.MIME[:], mime)
		copy(p.ContactReference[:], contact)
		copy(p.AppointmentReference[:], appointment)
		if !validMAPIMessageImport(p, highWater) {
			return nil, core.ErrInvalid
		}
		imports = append(imports, p)
	}
	return imports, rows.Err()
}

func checkMAPIMessageImportReservation(ctx context.Context, tx *sql.Tx, mailbox string, counter uint64) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM mapi_message_imports WHERE mailbox_id = ? AND counter = ?)
	`, mailbox, counter).Scan(&exists); err != nil {
		return fmt.Errorf("check MAPI message import reservation: %w", err)
	}
	if exists {
		return core.ErrConflict
	}
	return nil
}

func writeMAPIMessageImport(ctx context.Context, tx *sql.Tx, mailbox string, previous uint64, mutation MAPIReplicaMutation) error {
	if mutation.MessageImport == nil {
		return core.ErrInvalid
	}
	p := *mutation.MessageImport
	if !validMAPIMessageImport(p, previous) {
		return core.ErrInvalid
	}
	if err := requireMAPIReservedID(ctx, tx, mailbox, previous, p.Counter); err != nil {
		return err
	}
	if mutation.ImportAction == mapireplica.ClaimMessageImport {
		if p.NativeID != "" {
			return core.ErrInvalid
		}
		if err := checkMAPIMessageMoveReservation(ctx, tx, mailbox, p.Counter); err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO mapi_message_imports (mailbox_id, counter, parent, digest, mime_digest, contact_reference, appointment_reference)
			SELECT ?, ?, ?, ?, ?, ?, ?
			WHERE NOT EXISTS (SELECT 1 FROM mapi_object_ids WHERE mailbox_id = ? AND counter = ?)
			AND NOT EXISTS (SELECT 1 FROM mapi_folder_creations WHERE mailbox_id = ? AND counter = ?)
			AND EXISTS (SELECT 1 FROM mapi_object_ids WHERE mailbox_id = ? AND counter = ? AND namespace = ?
				AND substr(object_key, 1, 1) = x'01')
			ON CONFLICT DO NOTHING
		`, mailbox, p.Counter, p.Parent, p.Digest[:], p.MIME[:], p.ContactReference[:], p.AppointmentReference[:], mailbox, p.Counter,
			mailbox, p.Counter, mailbox, p.Parent, MAPIFolder)
		return mapiMoveResult(result, err, "claim MAPI message import")
	}
	switch mutation.ImportAction {
	case mapireplica.RecordMessageImport:
		if p.NativeID == "" {
			return core.ErrInvalid
		}
		result, err := tx.ExecContext(ctx, `
			UPDATE mapi_message_imports SET native_id = ?
			WHERE mailbox_id = ? AND counter = ? AND parent = ? AND digest = ? AND mime_digest = ?
			AND contact_reference = ? AND appointment_reference = ? AND native_id IS NULL
			AND NOT EXISTS (SELECT 1 FROM mapi_object_ids WHERE mailbox_id = ? AND namespace = ? AND object_key = ?)
		`, p.NativeID, mailbox, p.Counter, p.Parent, p.Digest[:], p.MIME[:], p.ContactReference[:], p.AppointmentReference[:], mailbox, MAPIMessage,
			append([]byte{byte(p.Special())}, p.NativeID...))
		return mapiMoveResult(result, err, "record MAPI message import")
	case mapireplica.CancelMessageImport:
		result, err := tx.ExecContext(ctx, `
			DELETE FROM mapi_message_imports WHERE mailbox_id = ? AND counter = ? AND parent = ?
			AND digest = ? AND mime_digest = ? AND contact_reference = ? AND appointment_reference = ? AND native_id IS NULL
		`, mailbox, p.Counter, p.Parent, p.Digest[:], p.MIME[:], p.ContactReference[:], p.AppointmentReference[:])
		return mapiMoveResult(result, err, "cancel MAPI message import")
	case mapireplica.PublishMessageImport:
		return publishMAPIMessageImport(ctx, tx, mailbox, previous, mutation.Projection, p)
	default:
		return core.ErrInvalid
	}
}

func publishMAPIMessageImport(ctx context.Context, tx *sql.Tx, mailbox string, previous uint64, projection []MAPIProjectionVersion, p mapireplica.MessageImport) error {
	if len(projection) != 1 || p.NativeID == "" {
		return core.ErrInvalid
	}
	v := projection[0]
	if v.Counter != p.Counter || v.Parent != p.Parent || !v.Present || v.ChangeNumber <= previous || v.ReadNumber <= previous {
		return core.ErrInvalid
	}
	result, err := tx.ExecContext(ctx, `
		DELETE FROM mapi_message_imports WHERE mailbox_id = ? AND counter = ? AND parent = ?
		AND digest = ? AND mime_digest = ? AND contact_reference = ? AND appointment_reference = ? AND native_id = ?
	`, mailbox, p.Counter, p.Parent, p.Digest[:], p.MIME[:], p.ContactReference[:], p.AppointmentReference[:], p.NativeID)
	if err := mapiMoveResult(result, err, "publish MAPI message import"); err != nil {
		return err
	}
	return insertMAPIObjectIDs(ctx, tx, mailbox, []MAPIObjectID{{
		Namespace: MAPIMessage, Key: append([]byte{byte(p.Special())}, p.NativeID...), Counter: p.Counter,
	}})
}
