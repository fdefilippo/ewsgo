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

func validMAPIDraftPlacement(d mapireplica.DraftPlacement, highWater uint64) bool {
	return d.Counter != 0 && d.Counter <= highWater && d.Folder != 0 && d.Folder <= highWater &&
		d.Folder != d.Counter && core.ValidDraftReference(d.Reference) && d.NativeFolder != "" &&
		len(d.NativeFolder) <= 4096 && len(d.NativeID) <= 4096 && len(d.CopyOf) <= 4096 &&
		(d.NativeID == "" || d.NativeID != d.CopyOf) &&
		(d.NativeID != "" || d.SubmissionRevision == 0) && (!d.TrashPending || d.CopyOf != "" && d.SubmissionRevision != 0)
}

func loadMAPIDraftPlacements(ctx context.Context, tx *sql.Tx, mailbox string, highWater uint64) ([]mapireplica.DraftPlacement, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT counter, folder, reference, native_folder, native_id, copy_of, submission_revision, trash_pending
		FROM mapi_draft_placements WHERE mailbox_id = ? ORDER BY counter LIMIT ?
	`, mailbox, maxMAPIObjectID+1)
	if err != nil {
		return nil, fmt.Errorf("read MAPI draft placements: %w", err)
	}
	defer rows.Close()
	var placements []mapireplica.DraftPlacement
	for rows.Next() {
		var draft mapireplica.DraftPlacement
		var revision []byte
		if err := rows.Scan(&draft.Counter, &draft.Folder, &draft.Reference, &draft.NativeFolder,
			&draft.NativeID, &draft.CopyOf, &revision, &draft.TrashPending); err != nil {
			return nil, fmt.Errorf("decode MAPI draft placement: %w", err)
		}
		if len(revision) != 8 {
			return nil, core.ErrInvalid
		}
		draft.SubmissionRevision = core.Revision(binary.LittleEndian.Uint64(revision))
		if !validMAPIDraftPlacement(draft, highWater) {
			return nil, core.ErrInvalid
		}
		placements = append(placements, draft)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate MAPI draft placements: %w", err)
	}
	if len(placements) > maxMAPIObjectID {
		return nil, core.ErrLimit
	}
	return placements, nil
}

func writeMAPIDraftPlacements(ctx context.Context, tx *sql.Tx, mailbox string, mutation MAPIReplicaMutation) error {
	for _, counter := range mutation.ForgetDrafts {
		if _, err := tx.ExecContext(ctx, "DELETE FROM mapi_draft_placements WHERE mailbox_id = ? AND counter = ?", mailbox, counter); err != nil {
			return fmt.Errorf("forget MAPI draft placement: %w", err)
		}
	}
	for _, d := range mutation.Drafts {
		if !validMAPIDraftPlacement(d, mutation.HighWater) {
			return core.ErrInvalid
		}
		result, err := tx.ExecContext(ctx, `
			INSERT INTO mapi_draft_placements (mailbox_id, counter, folder, reference, native_folder, native_id,
				copy_of, submission_revision, trash_pending)
			SELECT p.mailbox_id, ?, p.counter, ?, ?, ?, ?, ?, ? FROM mapi_object_ids AS p
			WHERE p.mailbox_id = ? AND p.counter = ? AND p.namespace = ?
			AND ((? = '' AND EXISTS (
				SELECT 1 FROM mapi_staged_ids AS x
				WHERE x.mailbox_id = p.mailbox_id AND x.counter = ? AND x.namespace = ?
			)) OR (? <> '' AND EXISTS (
				SELECT 1 FROM mapi_object_ids AS x
				WHERE x.mailbox_id = p.mailbox_id AND x.counter = ? AND x.namespace = ? AND x.object_key = ?
			)))
			ON CONFLICT (mailbox_id, counter) DO UPDATE SET
				native_id = excluded.native_id,
				submission_revision = excluded.submission_revision,
				trash_pending = excluded.trash_pending
			WHERE mapi_draft_placements.reference = excluded.reference
			  AND mapi_draft_placements.folder = excluded.folder
			  AND mapi_draft_placements.native_folder = excluded.native_folder
			  AND mapi_draft_placements.copy_of = excluded.copy_of
			  AND (mapi_draft_placements.native_id = '' OR mapi_draft_placements.native_id = excluded.native_id)
		`, d.Counter, d.Reference, d.NativeFolder, d.NativeID, d.CopyOf,
			binary.LittleEndian.AppendUint64(nil, uint64(d.SubmissionRevision)), d.TrashPending,
			mailbox, d.Folder, MAPIFolder, d.NativeID, d.Counter, MAPIMessage, d.NativeID, d.Counter,
			MAPIMessage, append([]byte{0}, d.NativeID...))
		if err != nil {
			return fmt.Errorf("write MAPI draft placement: %w", err)
		}
		count, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check MAPI draft placement: %w", err)
		}
		if count != 1 {
			return core.ErrConflict
		}
	}
	return nil
}
