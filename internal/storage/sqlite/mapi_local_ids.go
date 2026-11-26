// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const (
	maxMAPIReservationSize = 0x10000
	maxMAPIReservations    = 4096
)

type MAPIIDRange = mapireplica.IDRange

func loadMAPIIDRanges(ctx context.Context, tx *sql.Tx, mailboxID string, highWater uint64) ([]MAPIIDRange, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT first_id, last_id FROM mapi_local_id_ranges
		WHERE mailbox_id = ? ORDER BY first_id LIMIT ?
	`, mailboxID, maxMAPIReservations+1)
	if err != nil {
		return nil, fmt.Errorf("read MAPI ID reservations: %w", err)
	}
	defer rows.Close()
	var ranges []MAPIIDRange
	for rows.Next() {
		var current MAPIIDRange
		if err := rows.Scan(&current.First, &current.Last); err != nil {
			return nil, fmt.Errorf("decode MAPI ID reservation: %w", err)
		}
		if current.Last > highWater || len(ranges) > 0 && ranges[len(ranges)-1].Last+1 >= current.First {
			return nil, core.ErrInvalid
		}
		ranges = append(ranges, current)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate MAPI ID reservations: %w", err)
	}
	if len(ranges) > maxMAPIReservations {
		return nil, core.ErrUnsupported
	}
	return ranges, nil
}

// ReserveMAPIReplicaIDs commits an inclusive interval before exposing it to a
// client. Retrying from a newer high-water position issues a different interval.
func (s *Store) ReserveMAPIReplicaIDs(ctx context.Context, mailboxID string, expected MAPIReplicaVersion, count uint32) (MAPIIDRange, error) {
	if expected.Epoch == ([16]byte{}) || count == 0 || count > maxMAPIReservationSize {
		return MAPIIDRange{}, core.ErrInvalid
	}
	if expected.HighWater > maxMAPICounter || uint64(count) > maxMAPICounter-expected.HighWater {
		return MAPIIDRange{}, core.ErrUnsupported
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MAPIIDRange{}, fmt.Errorf("begin MAPI ID reservation: %w", err)
	}
	defer tx.Rollback()
	if err := expectMAPIReplica(ctx, tx, mailboxID, expected); err != nil {
		return MAPIIDRange{}, err
	}
	issued := MAPIIDRange{First: expected.HighWater + 1, Last: expected.HighWater + uint64(count)}
	if err := insertMAPIIDRange(ctx, tx, mailboxID, issued); err != nil {
		return MAPIIDRange{}, err
	}
	if err := updateMAPICounter(ctx, tx, mailboxID, issued.Last); err != nil {
		return MAPIIDRange{}, err
	}
	if err := tx.Commit(); err != nil {
		return MAPIIDRange{}, fmt.Errorf("commit MAPI ID reservation: %w", err)
	}
	return issued, nil
}

func insertMAPIIDRange(ctx context.Context, tx *sql.Tx, mailboxID string, issued MAPIIDRange) error {
	result, err := tx.ExecContext(ctx, "UPDATE mapi_local_id_ranges SET last_id = ? WHERE mailbox_id = ? AND last_id = ?", issued.Last, mailboxID, issued.First-1)
	if err != nil {
		return fmt.Errorf("merge MAPI ID reservation: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check MAPI ID reservation merge: %w", err)
	}
	if n == 1 {
		return nil
	}
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM mapi_local_id_ranges WHERE mailbox_id = ?", mailboxID).Scan(&count); err != nil {
		return fmt.Errorf("count MAPI ID reservations: %w", err)
	}
	if count >= maxMAPIReservations {
		return core.ErrUnsupported
	}
	if _, err := tx.ExecContext(ctx, "INSERT INTO mapi_local_id_ranges VALUES (?, ?, ?)", mailboxID, issued.First, issued.Last); err != nil {
		return fmt.Errorf("write MAPI ID reservation: %w", err)
	}
	return nil
}

// ClaimMAPIReservedID binds a counter already issued to a client. It leaves
// the interval available as evidence and uses the unique counter as proof of use.
// Retrying an identical binding succeeds, including after reopening the store.
func (s *Store) ClaimMAPIReservedID(ctx context.Context, mailboxID string, epoch [16]byte, binding MAPIObjectID) error {
	if epoch == ([16]byte{}) || !validMAPIReservedBinding(binding) {
		return core.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin MAPI reserved ID binding: %w", err)
	}
	defer tx.Rollback()
	current, err := readMAPIReplica(ctx, tx, mailboxID)
	if errors.Is(err, core.ErrNotFound) || err == nil && current.Epoch != epoch {
		return core.ErrResync
	}
	if err != nil {
		return err
	}
	if err := requireMAPIReservedID(ctx, tx, mailboxID, current.HighWater, binding.Counter); err != nil {
		return err
	}
	var present MAPIObjectID
	err = tx.QueryRowContext(ctx, "SELECT namespace, object_key FROM mapi_object_ids WHERE mailbox_id = ? AND counter = ?", mailboxID, binding.Counter).Scan(&present.Namespace, &present.Key)
	if err == nil {
		if present.Namespace == binding.Namespace && bytes.Equal(present.Key, binding.Key) {
			return nil
		}
		return core.ErrConflict
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("read claimed MAPI reserved ID: %w", err)
	}
	if err := checkMAPIMessageMoveReservation(ctx, tx, mailboxID, binding.Counter); err != nil {
		return err
	}
	if err := checkMAPIMessageImportReservation(ctx, tx, mailboxID, binding.Counter); err != nil {
		return err
	}
	if err := insertMAPIObjectIDs(ctx, tx, mailboxID, []MAPIObjectID{binding}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit MAPI reserved ID binding: %w", err)
	}
	return nil
}

func validMAPIReservedBinding(binding MAPIObjectID) bool {
	return validMAPIObjectID(binding) && (binding.Namespace == MAPIFolder || binding.Namespace == MAPIAssociatedMessage)
}

func requireMAPIReservedID(ctx context.Context, tx *sql.Tx, mailboxID string, highWater, counter uint64) error {
	var last uint64
	err := tx.QueryRowContext(ctx, `
		SELECT last_id FROM mapi_local_id_ranges WHERE mailbox_id = ? AND first_id <= ?
		ORDER BY first_id DESC LIMIT 1
	`, mailboxID, counter).Scan(&last)
	if errors.Is(err, sql.ErrNoRows) || err == nil && (last < counter || last > highWater) {
		return core.ErrConflict
	}
	if err != nil {
		return fmt.Errorf("read MAPI reserved ID interval: %w", err)
	}
	return nil
}
