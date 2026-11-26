// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"ewsgo/internal/core"
)

var _ core.PublicReadStateStore = (*Store)(nil)

func (s *Store) PublicReadState(ctx context.Context, user, store string, folder [24]byte, private bool) (core.PublicReadState, error) {
	if user == "" || store == "" || folder == ([24]byte{}) {
		return core.PublicReadState{}, core.ErrInvalid
	}
	var state core.PublicReadState
	var replica []byte
	err := s.db.QueryRowContext(ctx, `
		SELECT revision, replica, read_state FROM mapi_public_read_state
		WHERE user_mailbox = ? AND store_key = ? AND folder = ? AND private = ?
	`, user, store, folder[:], private).Scan(&state.Revision, &replica, &state.CNSet)
	if errors.Is(err, sql.ErrNoRows) {
		return core.PublicReadState{}, nil
	}
	if err != nil {
		return core.PublicReadState{}, fmt.Errorf("read MAPI public folder read state: %w", err)
	}
	if len(replica) != 16 {
		return core.PublicReadState{}, core.ErrInvalid
	}
	copy(state.Replica[:], replica)
	return state, nil
}

// The caller passes the revision of the last committed state observed before
// the first segment. A newer completed upload wins over an older staged one.
func (s *Store) SavePublicReadState(ctx context.Context, user, store string, folder [24]byte, private bool, state core.PublicReadState) error {
	if user == "" || store == "" || folder == ([24]byte{}) || state.Replica == ([16]byte{}) || len(state.CNSet) > 65536 {
		return core.ErrInvalid
	}
	var result sql.Result
	var err error
	now := time.Now().UTC().UnixMilli()
	if state.Revision == 0 {
		result, err = s.db.ExecContext(ctx, `
			INSERT INTO mapi_public_read_state (user_mailbox, store_key, folder, private, replica, read_state, revision, updated_at)
			VALUES (?, ?, ?, ?, ?, coalesce(?, X''), 1, ?)
			ON CONFLICT (user_mailbox, store_key, folder, private) DO NOTHING
		`, user, store, folder[:], private, state.Replica[:], state.CNSet, now)
	} else {
		result, err = s.db.ExecContext(ctx, `
			UPDATE mapi_public_read_state SET replica = ?, read_state = coalesce(?, X''), revision = revision + 1, updated_at = ?
			WHERE user_mailbox = ? AND store_key = ? AND folder = ? AND private = ? AND revision = ?
		`, state.Replica[:], state.CNSet, now, user, store, folder[:], private, state.Revision)
	}
	if err != nil {
		return fmt.Errorf("write MAPI public folder read state: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("check MAPI public folder read state write: %w", err)
	}
	if n != 1 {
		return core.ErrConflict
	}
	return nil
}

func (s *Store) PrivatePublicReadFolders(ctx context.Context, user, store string, limit int) ([][24]byte, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT folder FROM mapi_public_read_state
		WHERE user_mailbox = ? AND store_key = ? AND private = 1 ORDER BY folder LIMIT ?
	`, user, store, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var folders [][24]byte
	for rows.Next() {
		var value []byte
		if err := rows.Scan(&value); err != nil {
			return nil, err
		}
		if len(value) != 24 {
			return nil, core.ErrInvalid
		}
		folders = append(folders, [24]byte(value))
	}
	return folders, rows.Err()
}
