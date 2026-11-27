// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"fmt"
	"path/filepath"
	"testing"
	"time"
)

func TestConfirmedDraftCreationStorageGrowth(t *testing.T) {
	ctx := t.Context()
	store, mailbox := openJournalFixture(t, filepath.Join(t.TempDir(), "growth.sqlite"))
	var digest [32]byte
	digest[0] = 1
	var baseline, size int64
	sizeSQL := "SELECT page_count * page_size FROM pragma_page_count(), pragma_page_size()"
	if err := store.db.QueryRowContext(ctx, sizeSQL).Scan(&baseline); err != nil {
		t.Fatal(err)
	}
	next := 1
	for _, total := range []int{1000, 10000} {
		if _, err := store.db.ExecContext(ctx, `
			WITH RECURSIVE seq(n) AS (VALUES (?) UNION ALL SELECT n+1 FROM seq WHERE n < ?)
			INSERT INTO mail_draft_creations
			SELECT ?, printf('%032x', n), 'f:6', ?, printf('m:%d', n) FROM seq
		`, next, total, mailbox, digest[:]); err != nil {
			t.Fatal(err)
		}
		next = total + 1
		if err := store.db.QueryRowContext(ctx, sizeSQL).Scan(&size); err != nil {
			t.Fatal(err)
		}
		start := time.Now()
		const replays = 100
		for i := range replays {
			n := i*(total/replays) + 1
			claim, id, err := store.ClaimMailDraftCreation(ctx, mailbox, "f:6", fmt.Sprintf("%032x", n), digest)
			if err != nil || claim || string(id) != fmt.Sprintf("m:%d", n) {
				t.Fatalf("claim %d did not retain its acknowledgement: %s, %v", n, id, err)
			}
		}
		t.Logf("%d confirmed drafts: %d bytes above empty store, %v per serial replay", total, size-baseline, time.Since(start)/replays)
	}
}
