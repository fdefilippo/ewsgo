// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/binary"
	"errors"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

// Keep persistent IDs outside the range used by the legacy RAM allocator.
const firstPersistentPermissionMember = 1 << 62

func (s *Store) BindMAPIPermissionMembers(ctx context.Context, mailboxID string, accounts []string) (map[string]uint64, error) {
	if len(accounts) > mapireplica.MaxPermissionMembers {
		return nil, core.ErrLimit
	}
	ids := make(map[string]uint64, len(accounts))
	if len(accounts) == 0 {
		return ids, nil
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("begin MAPI permission member binding: %w", err)
	}
	defer tx.Rollback()
	if err := requireMailbox(ctx, tx, mailboxID); err != nil {
		return nil, err
	}
	var count int
	if err := tx.QueryRowContext(ctx, `
		SELECT count(*) FROM mapi_permission_members WHERE mailbox_id = ?
	`, mailboxID).Scan(&count); err != nil {
		return nil, fmt.Errorf("read MAPI permission member allocation: %w", err)
	}
	for _, account := range accounts {
		if account == "" || len(account) > 512 || !utf8.ValidString(account) || strings.ContainsRune(account, 0) {
			return nil, core.ErrInvalid
		}
		if ids[account] != 0 {
			continue
		}
		var id uint64
		err := tx.QueryRowContext(ctx, `
			SELECT member_id FROM mapi_permission_members WHERE mailbox_id = ? AND account = ?
		`, mailboxID, account).Scan(&id)
		if errors.Is(err, sql.ErrNoRows) {
			if count >= mapireplica.MaxPermissionMembers {
				return nil, core.ErrUnsupported
			}
			// A recovered backup might predate later assignments. Avoid
			// issuing the same sequence again after such a restore.
			var token [8]byte
			_, _ = rand.Read(token[:])
			id = binary.LittleEndian.Uint64(token[:])&(firstPersistentPermissionMember-1) | firstPersistentPermissionMember
			if _, err := tx.ExecContext(ctx, `
				INSERT INTO mapi_permission_members (mailbox_id, account, member_id) VALUES (?, ?, ?)
			`, mailboxID, account, id); err != nil {
				return nil, fmt.Errorf("record MAPI permission member: %w", err)
			}
			count++
		} else if err != nil {
			return nil, fmt.Errorf("read MAPI permission member: %w", err)
		}
		ids[account] = id
	}
	if err := tx.Commit(); err != nil {
		return nil, fmt.Errorf("commit MAPI permission member binding: %w", err)
	}
	return ids, nil
}

func (s *Store) MAPIPermissionMember(ctx context.Context, mailboxID string, id uint64) (string, error) {
	if id < firstPersistentPermissionMember || id > math.MaxInt64 {
		return "", core.ErrNotFound
	}
	var account string
	err := s.db.QueryRowContext(ctx, `
		SELECT account FROM mapi_permission_members WHERE mailbox_id = ? AND member_id = ?
	`, mailboxID, id).Scan(&account)
	if errors.Is(err, sql.ErrNoRows) {
		return "", core.ErrNotFound
	}
	if err != nil {
		return "", fmt.Errorf("resolve MAPI permission member: %w", err)
	}
	return account, nil
}
