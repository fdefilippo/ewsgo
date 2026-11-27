// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"crypto/sha256"
	"errors"
	"path/filepath"
	"testing"

	"ewsgo/internal/core"
)

func TestCorrelatedSendRetentionAcrossJournalMaintenanceAndRestart(t *testing.T) {
	for _, state := range []core.SendState{core.SendConfirmed, core.SendUncertain} {
		name := "confirmed"
		if state == core.SendUncertain {
			name = "uncertain"
		}
		t.Run(name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "retention.sqlite")
			store, mailbox := openJournalFixture(t, path)
			input := core.SendInput{MIMEDigest: sha256.Sum256([]byte("original MIME"))}
			key := sha256.Sum256([]byte("message-id"))
			intent, err := store.PrepareCorrelatedSend(ctx, mailbox, input, key)
			if err != nil {
				t.Fatal(err)
			}
			if _, won, err := store.ClaimSend(ctx, mailbox, intent.ID, input); err != nil || !won {
				t.Fatalf("claim: %t, %v", won, err)
			}
			var sent core.ID
			if state == core.SendConfirmed {
				sent = "sent-copy"
				if err := store.CompleteSend(ctx, mailbox, intent.ID, state, sent); err != nil {
					t.Fatal(err)
				}
			}
			if _, err := store.db.ExecContext(ctx, "UPDATE mailboxes SET journal_revision = ? WHERE mailbox_id = ?", journalRetainedRevisions+1, mailbox); err != nil {
				t.Fatal(err)
			}
			if err := store.MaintainJournal(ctx); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, _ = openJournalFixture(t, path)
			replay, err := store.PrepareCorrelatedSend(ctx, mailbox, input, key)
			if err != nil || replay.ID != intent.ID || replay.State != state || replay.SentID != sent {
				t.Fatalf("replay lost receipt: %+v, %v", replay, err)
			}
			if _, won, err := store.ClaimSend(ctx, mailbox, replay.ID, input); err != nil || won {
				t.Fatalf("replay claimed another dispatch: %t, %v", won, err)
			}
			input.MIMEDigest = sha256.Sum256([]byte("different MIME"))
			if _, err := store.PrepareCorrelatedSend(ctx, mailbox, input, key); !errors.Is(err, core.ErrConflict) {
				t.Fatalf("replay with different MIME: %v", err)
			}
		})
	}
}

func TestSendIntentClaimSurvivesRetryAndRestore(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	path := filepath.Join(dir, "store.db")
	open := func(path string) *Store {
		t.Helper()
		store, err := Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	original := open(path)
	bind := func(account string) string {
		t.Helper()
		m, err := original.BindMailbox(ctx, core.Scope{Backend: "test", Account: account}, "instance")
		if err != nil {
			t.Fatal(err)
		}
		return m.MailboxID
	}
	mailbox, otherMailbox := bind("account-a"), bind("account-b")
	input := core.SendInput{MIMEDigest: sha256.Sum256([]byte("fixture")), DraftID: "draft-1", Revision: 2}
	intent, err := original.PrepareSend(ctx, mailbox, input)
	if err != nil || intent.ID == "" || intent.State != core.SendReady {
		t.Fatalf("prepare send intent: %v", err)
	}
	if _, err := original.SendIntent(ctx, otherMailbox, intent.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("intent leaked across mailboxes: %v", err)
	}
	different := input
	different.Revision++
	if _, won, err := original.ClaimSend(ctx, mailbox, intent.ID, different); !errors.Is(err, core.ErrConflict) || won {
		t.Fatalf("different inputs must not claim an intent: %v", err)
	}

	backup := filepath.Join(dir, "backup.db")
	if err := Backup(ctx, path, backup); err != nil {
		t.Fatal(err)
	}
	competing := open(path)
	type result struct {
		won bool
		err error
	}
	results := make(chan result, 2)
	for _, store := range []*Store{original, competing} {
		go func() {
			_, won, err := store.ClaimSend(ctx, mailbox, intent.ID, input)
			results <- result{won, err}
		}()
	}
	winners := 0
	for range 2 {
		result := <-results
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.won {
			winners++
		}
	}
	if winners != 1 {
		t.Fatalf("expected one dispatch owner, got %d", winners)
	}
	if err := original.Close(); err != nil {
		t.Fatal(err)
	}
	if err := competing.Close(); err != nil {
		t.Fatal(err)
	}

	checkState := func(store *Store, state core.SendState, sentID core.ID) {
		t.Helper()
		got, won, err := store.ClaimSend(ctx, mailbox, intent.ID, input)
		if err != nil || won || got.ID != intent.ID || got.Input != input || got.State != state || got.SentID != sentID {
			t.Fatalf("receipt could be dispatched again or state changed: %v", err)
		}
	}
	reopened := open(path)
	checkState(reopened, core.SendUncertain, "")
	if err := reopened.CompleteSend(ctx, mailbox, intent.ID, core.SendConfirmed, "sent-item"); err != nil {
		t.Fatal(err)
	}
	if err := reopened.CompleteSend(ctx, mailbox, intent.ID, core.SendFailed, ""); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("terminal outcome was overwritten: %v", err)
	}
	checkState(open(path), core.SendConfirmed, "sent-item")

	restored := filepath.Join(dir, "restored.db")
	if err := Restore(ctx, backup, restored); err != nil {
		t.Fatal(err)
	}
	checkState(open(restored), core.SendUncertain, "")
}
