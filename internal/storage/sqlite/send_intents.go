// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"strings"

	"ewsgo/internal/core"
)

// PrepareSend records a fresh intentional send and returns its own receipt.
// A retry of a submission must use the original receipt.
func (s *Store) PrepareSend(ctx context.Context, mailboxID string, input core.SendInput) (core.SendIntent, error) {
	return s.prepareSend(ctx, mailboxID, input, nil)
}

// PrepareCorrelatedSend reuses the receipt for a message key in the current
// mailbox, provided that the caller supplied the original message bytes.
func (s *Store) PrepareCorrelatedSend(ctx context.Context, mailboxID string, input core.SendInput, key [32]byte) (core.SendIntent, error) {
	return s.prepareSend(ctx, mailboxID, input, key[:])
}

func (s *Store) prepareSend(ctx context.Context, mailboxID string, input core.SendInput, key []byte) (core.SendIntent, error) {
	if mailboxID == "" || (input.DraftID == "") != (input.Revision == 0) ||
		input.Revision > math.MaxInt64 || len(input.DraftID) > 1024 || strings.ContainsRune(string(input.DraftID), 0) {
		return core.SendIntent{}, core.ErrInvalid
	}
	intent := core.SendIntent{ID: strings.ToLower(rand.Text()), Input: input, State: core.SendReady}
	result, err := s.db.ExecContext(ctx, `
		INSERT INTO send_intents (mailbox_id, intent_id, mime_digest, draft_id, draft_revision, state, request_key)
		SELECT mailbox_id, ?, ?, ?, ?, ?, ? FROM mailboxes WHERE mailbox_id = ?
		ON CONFLICT(mailbox_id, request_key) DO NOTHING
	`, intent.ID, input.MIMEDigest[:], input.DraftID, input.Revision, intent.State, key, mailboxID)
	if err != nil {
		return core.SendIntent{}, fmt.Errorf("record send intent: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return core.SendIntent{}, fmt.Errorf("verify send intent was recorded: %w", err)
	} else if n > 0 {
		return intent, nil
	}
	if key == nil {
		return core.SendIntent{}, core.ErrNotFound
	}
	var id string
	err = s.db.QueryRowContext(ctx, `
		SELECT intent_id FROM send_intents WHERE mailbox_id = ? AND request_key = ?
	`, mailboxID, key).Scan(&id)
	if errors.Is(err, sql.ErrNoRows) {
		return core.SendIntent{}, core.ErrNotFound
	}
	if err != nil {
		return core.SendIntent{}, fmt.Errorf("read correlated send receipt: %w", err)
	}
	intent, err = readSendIntent(ctx, s.db, mailboxID, id)
	if err == nil && intent.Input != input {
		return core.SendIntent{}, core.ErrConflict
	}
	return intent, err
}

func (s *Store) SendIntent(ctx context.Context, mailboxID, id string) (core.SendIntent, error) {
	if !core.ValidSendIntentID(id) || mailboxID == "" {
		return core.SendIntent{}, core.ErrInvalid
	}
	return readSendIntent(ctx, s.db, mailboxID, id)
}

func readSendIntent(ctx context.Context, db rowQueryer, mailboxID, id string) (core.SendIntent, error) {
	intent := core.SendIntent{ID: id}
	var digest []byte
	err := db.QueryRowContext(ctx, `
		SELECT mime_digest, draft_id, draft_revision, state, sent_id
		FROM send_intents WHERE mailbox_id = ? AND intent_id = ?
	`, mailboxID, id).Scan(&digest, &intent.Input.DraftID, &intent.Input.Revision, &intent.State, &intent.SentID)
	if errors.Is(err, sql.ErrNoRows) {
		return core.SendIntent{}, core.ErrNotFound
	}
	if err != nil {
		return core.SendIntent{}, fmt.Errorf("read send intent: %w", err)
	}
	if len(digest) != len(intent.Input.MIMEDigest) {
		return core.SendIntent{}, fmt.Errorf("invalid stored send digest")
	}
	copy(intent.Input.MIMEDigest[:], digest)
	return intent, nil
}

// ClaimSend durably enters uncertainty before the caller issues SendMsg. Only
// one caller can win. A crash after this point requires observation rather than
// another dispatch, even if the process died before sending the HTTP request.
func (s *Store) ClaimSend(ctx context.Context, mailboxID, id string, input core.SendInput) (core.SendIntent, bool, error) {
	if !core.ValidSendIntentID(id) || mailboxID == "" {
		return core.SendIntent{}, false, core.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return core.SendIntent{}, false, fmt.Errorf("begin send claim: %w", err)
	}
	defer tx.Rollback()
	intent, err := readSendIntent(ctx, tx, mailboxID, id)
	if err != nil {
		return core.SendIntent{}, false, err
	}
	if intent.Input != input {
		return core.SendIntent{}, false, core.ErrConflict
	}
	if intent.State != core.SendReady {
		return intent, false, nil
	}
	if _, err := tx.ExecContext(ctx, `
		UPDATE send_intents SET state = ? WHERE mailbox_id = ? AND intent_id = ?
	`, core.SendUncertain, mailboxID, id); err != nil {
		return core.SendIntent{}, false, fmt.Errorf("claim send intent: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return core.SendIntent{}, false, fmt.Errorf("commit send claim: %w", err)
	}
	intent.State = core.SendUncertain
	return intent, true, nil
}

// CompleteSend records definitive evidence held by the submitting caller.
func (s *Store) CompleteSend(ctx context.Context, mailboxID, id string, state core.SendState, sentID core.ID) error {
	if !core.ValidSendIntentID(id) || mailboxID == "" ||
		state != core.SendConfirmed && state != core.SendFailed ||
		state == core.SendFailed && sentID != "" || len(sentID) > 1024 || strings.ContainsRune(string(sentID), 0) {
		return core.ErrInvalid
	}
	result, err := s.db.ExecContext(ctx, `
		UPDATE send_intents SET state = ?, sent_id = ?
		WHERE mailbox_id = ? AND intent_id = ? AND state = ?
	`, state, sentID, mailboxID, id, core.SendUncertain)
	if err != nil {
		return fmt.Errorf("record send outcome: %w", err)
	}
	if n, err := result.RowsAffected(); err != nil {
		return fmt.Errorf("verify send outcome was recorded: %w", err)
	} else if n == 0 {
		return core.ErrConflict
	}
	return nil
}
