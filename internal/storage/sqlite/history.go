// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"fmt"

	"ewsgo/internal/core"
)

// CompactJournal discards history through the given revision. It retains the
// latest row for each live object so property reads and edits still see its
// parent and revision. Clients below the cutoff must fully synchronize again.
func (s *Store) CompactJournal(ctx context.Context, mailboxID string, through JournalCursor) error {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin journal compaction: %w", err)
	}
	defer tx.Rollback()
	head, floor, err := readJournalWindow(ctx, tx, mailboxID)
	if err != nil {
		return err
	}
	if through.Generation != head.Generation || through.Revision > head.Revision {
		return core.ErrResync
	}
	if through.Revision <= floor {
		return nil
	}

	// The maximum revision of each object is kept by the first delete. Keep
	// those rows until the second delete so their presence reliably identifies
	// all earlier rows, including rows of deleted objects.
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM journal WHERE mailbox_id = ? AND revision <= ?
		AND EXISTS (
			SELECT 1 FROM journal AS newer
			WHERE newer.mailbox_id = journal.mailbox_id AND newer.object_id = journal.object_id
			AND newer.revision > journal.revision
		)
	`, mailboxID, through.Revision); err != nil {
		return fmt.Errorf("prune superseded journal changes: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM journal WHERE mailbox_id = ? AND revision <= ? AND kind = ?
	`, mailboxID, through.Revision, core.Deleted); err != nil {
		return fmt.Errorf("prune expired deletion events: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE mailboxes SET journal_floor = ? WHERE mailbox_id = ?
	`, through.Revision, mailboxID); err != nil {
		return fmt.Errorf("advance journal retention boundary: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit journal compaction: %w", err)
	}
	return nil
}
