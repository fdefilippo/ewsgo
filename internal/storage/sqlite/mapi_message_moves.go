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

func validMAPIMessageMove(move mapireplica.MessageMove, highWater uint64) bool {
	return move.Source != 0 && move.Destination != 0 && move.From != 0 && move.To != 0 &&
		move.Source != move.Destination && move.From != move.To &&
		max(move.Source, move.Destination, move.From, move.To, move.Result) <= highWater &&
		len(move.NativeID) > 0 && len(move.NativeID) <= 4096 &&
		move.Revision != 0 && move.Request != ([32]byte{})
}

func loadMAPIMessageMoves(ctx context.Context, tx *sql.Tx, mailbox string, highWater uint64) ([]mapireplica.MessageMove, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT source, destination, from_folder, to_folder, native_id, revision, request, result, newer_client
		FROM mapi_message_moves WHERE mailbox_id = ? ORDER BY source LIMIT ?
	`, mailbox, maxMAPIObjectID+1)
	if err != nil {
		return nil, fmt.Errorf("read MAPI message moves: %w", err)
	}
	defer rows.Close()
	var moves []mapireplica.MessageMove
	for rows.Next() {
		var move mapireplica.MessageMove
		var revision, request []byte
		if err := rows.Scan(&move.Source, &move.Destination, &move.From, &move.To, &move.NativeID,
			&revision, &request, &move.Result, &move.NewerClient); err != nil {
			return nil, fmt.Errorf("decode MAPI message move: %w", err)
		}
		if len(revision) != 8 || len(request) != len(move.Request) || len(moves) >= maxMAPIObjectID {
			return nil, core.ErrInvalid
		}
		move.Revision = core.Revision(binary.LittleEndian.Uint64(revision))
		copy(move.Request[:], request)
		if !validMAPIMessageMove(move, highWater) {
			return nil, core.ErrInvalid
		}
		moves = append(moves, move)
	}
	return moves, rows.Err()
}

func checkMAPIMessageMoveReservation(ctx context.Context, tx *sql.Tx, mailbox string, target uint64) error {
	var exists bool
	if err := tx.QueryRowContext(ctx, `
		SELECT EXISTS(SELECT 1 FROM mapi_message_moves WHERE mailbox_id = ? AND destination = ?)
	`, mailbox, target).Scan(&exists); err != nil {
		return fmt.Errorf("check MAPI message move reservation: %w", err)
	}
	if exists {
		return core.ErrConflict
	}
	return nil
}

func writeMAPIMessageMove(ctx context.Context, tx *sql.Tx, mailbox string, previous uint64, mutation MAPIReplicaMutation) error {
	move := *mutation.MessageMove
	if !validMAPIMessageMove(move, mutation.HighWater) ||
		max(move.Source, move.Destination, move.From, move.To) > previous {
		return core.ErrInvalid
	}
	if err := requireMAPIReservedID(ctx, tx, mailbox, previous, move.Destination); err != nil {
		return err
	}
	if err := checkMAPIMessageImportReservation(ctx, tx, mailbox, move.Destination); err != nil {
		return err
	}
	nativeKey := append([]byte{byte(move.Special())}, move.NativeID...)
	revision := binary.LittleEndian.AppendUint64(nil, uint64(move.Revision))
	if move.Result == 0 {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO mapi_message_moves
			SELECT ?, ?, ?, ?, ?, ?, ?, ?, 0, ?
			WHERE EXISTS (SELECT 1 FROM mapi_object_ids WHERE mailbox_id = ? AND counter = ? AND namespace = ? AND object_key = ?)
				AND NOT EXISTS (SELECT 1 FROM mapi_object_ids WHERE mailbox_id = ? AND counter = ?)
				AND NOT EXISTS (SELECT 1 FROM mapi_folder_creations WHERE mailbox_id = ? AND counter = ?)
			ON CONFLICT DO NOTHING
		`, mailbox, move.Source, move.Destination, move.From, move.To, move.NativeID, revision, move.Request[:], move.NewerClient,
			mailbox, move.Source, MAPIMessage, nativeKey, mailbox, move.Destination, mailbox, move.Destination)
		return mapiMoveResult(result, err, "claim MAPI message move")
	}
	return completeMAPIMessageMove(ctx, tx, mailbox, previous, mutation, nativeKey, revision)
}

func completeMAPIMessageMove(ctx context.Context, tx *sql.Tx, mailbox string, previous uint64, mutation MAPIReplicaMutation, nativeKey, revision []byte) error {
	move := *mutation.MessageMove
	var removed, added bool
	for _, p := range mutation.Projection {
		removed = removed || p.Counter == move.Source && p.Parent == move.From && !p.Present && p.ChangeNumber > previous
		added = added || p.Counter == move.Destination && p.Parent == move.To && p.Present && p.ChangeNumber == move.Result
	}
	if !removed || !added || move.Result <= previous {
		return core.ErrInvalid
	}
	result, err := tx.ExecContext(ctx, `
		UPDATE mapi_message_moves SET result = ?
		WHERE mailbox_id = ? AND source = ? AND destination = ? AND from_folder = ? AND to_folder = ?
			AND native_id = ? AND revision = ? AND request = ? AND newer_client = ? AND result = 0
	`, move.Result, mailbox, move.Source, move.Destination, move.From, move.To, move.NativeID, revision, move.Request[:], move.NewerClient)
	if err := mapiMoveResult(result, err, "complete MAPI message move"); err != nil {
		return err
	}
	result, err = tx.ExecContext(ctx, `
		UPDATE mapi_object_ids SET object_key = ?
		WHERE mailbox_id = ? AND namespace = ? AND counter = ? AND object_key = ?
	`, mapireplica.RetiredMessageKey(move.Source), mailbox, MAPIMessage, move.Source, nativeKey)
	if err := mapiMoveResult(result, err, "retire MAPI message binding"); err != nil {
		return err
	}
	return insertMAPIObjectIDs(ctx, tx, mailbox, []MAPIObjectID{{Namespace: MAPIMessage, Key: nativeKey, Counter: move.Destination}})
}

func mapiMoveResult(result sql.Result, err error, operation string) error {
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: %w", operation, err)
	}
	if n != 1 {
		return core.ErrConflict
	}
	return nil
}
