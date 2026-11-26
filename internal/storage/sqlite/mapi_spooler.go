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

func loadMAPISpooler(ctx context.Context, tx *sql.Tx, mailbox string) ([]mapireplica.SpoolerEntry, error) {
	rows, err := tx.QueryContext(ctx, "SELECT counter, folder, revision, state FROM mapi_spooler WHERE mailbox_id = ? LIMIT ?", mailbox, mapireplica.MaxSpoolerEntries+1)
	if err != nil {
		return nil, fmt.Errorf("read MAPI spooler: %w", err)
	}
	defer rows.Close()
	var queue []mapireplica.SpoolerEntry
	for rows.Next() {
		var entry mapireplica.SpoolerEntry
		var revision []byte
		if err := rows.Scan(&entry.Counter, &entry.Folder, &revision, &entry.State); err != nil {
			return nil, fmt.Errorf("decode MAPI spooler entry: %w", err)
		}
		if len(revision) != 8 {
			return nil, core.ErrInvalid
		}
		entry.Revision = core.Revision(binary.LittleEndian.Uint64(revision))
		queue = append(queue, entry)
	}
	if len(queue) > mapireplica.MaxSpoolerEntries {
		return nil, core.ErrLimit
	}
	return queue, rows.Err()
}

func writeMAPISpooler(ctx context.Context, tx *sql.Tx, mailbox string, mutation MAPIReplicaMutation) error {
	if mutation.ForgetSpooler != 0 {
		if mutation.Spooler != nil {
			return core.ErrInvalid
		}
		result, err := tx.ExecContext(ctx, "DELETE FROM mapi_spooler WHERE mailbox_id = ? AND counter = ? AND state != ?", mailbox, mutation.ForgetSpooler, mapireplica.SpoolerDispatching)
		return spoolerRowsAffected(result, err)
	}
	d := mutation.Spooler
	if d == nil {
		return nil
	}
	if d.Counter == 0 || d.Folder == 0 || d.Counter == d.Folder || d.Revision == 0 ||
		d.State < mapireplica.SpoolerQueued || d.State > mapireplica.SpoolerAccepted {
		return core.ErrInvalid
	}
	result, err := tx.ExecContext(ctx, `
		INSERT INTO mapi_spooler (mailbox_id, counter, folder, revision, state)
		SELECT m.mailbox_id, m.counter, f.counter, ?, ?
		FROM mapi_object_ids m JOIN mapi_object_ids f ON f.mailbox_id = m.mailbox_id
		WHERE m.mailbox_id = ? AND m.counter = ? AND m.namespace = ? AND f.counter = ? AND f.namespace = ?
		  AND (EXISTS (SELECT 1 FROM mapi_spooler q WHERE q.mailbox_id = m.mailbox_id AND q.counter = m.counter)
		    OR (? = 1 AND (SELECT count(*) FROM mapi_spooler q WHERE q.mailbox_id = m.mailbox_id) < ?))
		ON CONFLICT (mailbox_id, counter) DO UPDATE SET state = excluded.state
		WHERE mapi_spooler.folder = excluded.folder AND mapi_spooler.revision = excluded.revision
		  AND ((mapi_spooler.state = 1 AND excluded.state = 2)
		    OR (mapi_spooler.state = 2 AND excluded.state IN (1, 3)))
	`, binary.LittleEndian.AppendUint64(nil, uint64(d.Revision)), d.State, mailbox, d.Counter, MAPIMessage, d.Folder, MAPIFolder, d.State, mapireplica.MaxSpoolerEntries)
	return spoolerRowsAffected(result, err)
}

func spoolerRowsAffected(result sql.Result, err error) error {
	if err != nil {
		return fmt.Errorf("write MAPI spooler entry: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check MAPI spooler write: %w", err)
	}
	if n != 1 {
		return core.ErrConflict
	}
	return nil
}
