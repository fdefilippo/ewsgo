// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"sync"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func openJournalFixture(t *testing.T, path string) (*Store, string) {
	t.Helper()
	store, err := Open(t.Context(), path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	scope := core.Scope{Backend: "fixture", Account: "journal-test"}
	mailbox, err := store.BindMailbox(t.Context(), scope, "one")
	if err != nil {
		t.Fatal(err)
	}
	return store, mailbox.MailboxID
}

func TestJournalCommitsOrderedChangesAndCheckpoint(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "journal.sqlite")
	store, mailbox := openJournalFixture(t, path)
	changes := []core.Change{
		{Kind: core.Created, ID: "message-old", Parent: "inbox"},
		{Kind: core.Moved, ID: "message-old", Parent: "archive", PreviousParent: "inbox"},
		{Kind: core.Created, ID: "copied-message", Parent: "drafts"},
		{Kind: core.Deleted, ID: "message-old", PreviousParent: "archive"},
		{Kind: core.Created, ID: "new-generation", Parent: "inbox"},
	}
	binding := NativeBinding{Kind: core.Message, Reference: "backend-item"}
	events := []DeltaEvent{
		{Change: changes[0], Binding: &binding},
		{Change: changes[1]},
		{Change: changes[2]},
	}
	var zero DeltaCheckpoint
	initial, err := store.ApplyDelta(ctx, mailbox, zero, "first-page", events)
	if err != nil || initial != (DeltaCheckpoint{Cursor: "first-page", Revision: 3}) {
		t.Fatalf("initial checkpoint: %+v, %v", initial, err)
	}
	if id, err := store.ResolveNativeBinding(ctx, mailbox, binding); err != nil || id != "message-old" {
		t.Fatalf("move must preserve native binding: %s, %v", id, err)
	}
	if _, err := store.ApplyDelta(ctx, mailbox, zero, "first-page", events); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("retry from stale checkpoint: %v", err)
	}
	broken := []DeltaEvent{{Change: changes[3]}, {Change: core.Change{Kind: core.Created}}}
	if _, err := store.ApplyDelta(ctx, mailbox, initial, "broken-page", broken); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("invalid partially normalized page: %v", err)
	}
	if id, err := store.ResolveNativeBinding(ctx, mailbox, binding); err != nil || id != "message-old" {
		t.Fatalf("rollback must undo alias retirement: %s, %v", id, err)
	}
	reused := []DeltaEvent{{Change: changes[4], Binding: &binding}}
	if _, err := store.ApplyDelta(ctx, mailbox, initial, "collision", reused); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("cannot reuse a live native reference: %v", err)
	}
	deleted, err := store.ApplyDelta(ctx, mailbox, initial, "delete-page", []DeltaEvent{{Change: changes[3]}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ResolveNativeBinding(ctx, mailbox, binding); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("deleted binding remains live: %v", err)
	}
	second, err := store.ApplyDelta(ctx, mailbox, deleted, "recreated-page", reused)
	if err != nil {
		t.Fatal(err)
	}
	noChange, err := store.ApplyDelta(ctx, mailbox, second, second.Cursor, nil)
	if err != nil || noChange != second {
		t.Fatalf("unchanged native cursor: %+v, %v", noChange, err)
	}
	advanced, err := store.ApplyDelta(ctx, mailbox, second, "empty-page", nil)
	if err != nil || advanced.Revision != 6 {
		t.Fatalf("empty advancing page: %+v, %v", advanced, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, reopenedMailbox := openJournalFixture(t, path)
	actual, err := store.DeltaCheckpoint(ctx, mailbox)
	if err != nil || reopenedMailbox != mailbox || actual != advanced {
		t.Fatalf("reopened checkpoint: %+v, %v", actual, err)
	}
	if id, err := store.ResolveNativeBinding(ctx, mailbox, binding); err != nil || id != "new-generation" {
		t.Fatalf("reused native reference after reopening: %s, %v", id, err)
	}
	for i := range changes {
		changes[i].Revision = core.Revision(i + 1)
	}
	first, err := store.Journal(ctx, mailbox, JournalCursor{}, 2)
	if err != nil || !slices.Equal(first.Changes, changes[:2]) {
		t.Fatalf("first journal page: %+v, %v", first, err)
	}
	rest, err := store.Journal(ctx, mailbox, first.Next, 20)
	if err != nil || !slices.Equal(rest.Changes, changes[2:]) || rest.Next.Revision != advanced.Revision {
		t.Fatalf("resumed journal page: %+v, %v", rest, err)
	}
	other, err := store.BindMailbox(ctx, core.Scope{Backend: "fixture", Account: "other"}, "one")
	if err != nil {
		t.Fatal(err)
	}
	if events, err := store.Journal(ctx, other.MailboxID, JournalCursor{}, 20); err != nil || len(events.Changes) != 0 {
		t.Fatalf("unexpected journal from another mailbox: %+v, %v", events, err)
	}
	if _, err := store.ResolveNativeBinding(ctx, other.MailboxID, binding); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("binding escaped mailbox scope: %v", err)
	}
	if _, err := store.ApplyDelta(ctx, "unknown", zero, "unexpected", nil); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("unknown mailbox: %v", err)
	}
}

func TestConcurrentDeltaWritersRejectStaleCheckpoint(t *testing.T) {
	store, mailbox := openJournalFixture(t, filepath.Join(t.TempDir(), "concurrent.sqlite"))
	ctx := t.Context()
	var wg sync.WaitGroup
	var outcomes [2]error
	for i := range outcomes {
		wg.Go(func() {
			_, outcomes[i] = store.ApplyDelta(ctx, mailbox, DeltaCheckpoint{}, "page", []DeltaEvent{{Change: core.Change{Kind: core.Created, ID: "one"}}})
		})
	}
	wg.Wait()
	if !((outcomes[0] == nil && errors.Is(outcomes[1], core.ErrConflict)) || (outcomes[1] == nil && errors.Is(outcomes[0], core.ErrConflict))) {
		t.Fatalf("expected one success and one stale checkpoint: %v", outcomes)
	}
	if events, err := store.Journal(ctx, mailbox, JournalCursor{}, 2); err != nil || len(events.Changes) != 1 {
		t.Fatalf("expected one committed event: %+v, %v", events, err)
	}
}

func TestJournalSurvivesProcessTermination(t *testing.T) {
	const childPhase = "EWSGO_SQLITE_TEST_JOURNAL_PHASE"
	const childDatabase = "EWSGO_SQLITE_TEST_JOURNAL_DATABASE"
	binding := NativeBinding{Kind: core.Message, Reference: "fixture-reference"}
	if phase := os.Getenv(childPhase); phase != "" {
		store, mailbox := openJournalFixture(t, os.Getenv(childDatabase))
		ctx := t.Context()
		page := []DeltaEvent{{Change: core.Change{Kind: core.Created, ID: "synthetic-item"}, Binding: &binding}}
		if phase == "before" {
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				t.Fatal(err)
			}
			defer tx.Rollback()
			if _, err := writeDelta(ctx, tx, mailbox, DeltaCheckpoint{}, "committed-page", page, false); err != nil {
				t.Fatal(err)
			}
		} else if _, err := store.ApplyDelta(ctx, mailbox, DeltaCheckpoint{}, "committed-page", page); err != nil {
			t.Fatal(err)
		}
		fmt.Println("transaction barrier reached")
		// The parent terminates this process at the barrier. Keep the pending
		// write transaction open until then, with a bounded fallback on failure.
		time.Sleep(20 * time.Second)
		t.Fatal("child was not terminated at the transaction barrier")
	}
	for _, phase := range []string{"before", "after"} {
		t.Run(phase, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "crash.sqlite")
			binary, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(t.Context(), 20*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, binary, "-test.run=^TestJournalSurvivesProcessTermination$")
			cmd.Env = append(os.Environ(), childPhase+"="+phase, childDatabase+"="+path)
			stdout, err := cmd.StdoutPipe()
			if err != nil {
				t.Fatal(err)
			}
			if err := cmd.Start(); err != nil {
				t.Fatal(err)
			}
			line, readErr := bufio.NewReader(stdout).ReadString('\n')
			killErr := cmd.Process.Kill()
			waitErr := cmd.Wait()
			if readErr != nil || line != "transaction barrier reached\n" || killErr != nil || waitErr == nil {
				t.Fatalf("transaction barrier %q: read=%v kill=%v wait=%v", line, readErr, killErr, waitErr)
			}
			store, mailbox := openJournalFixture(t, path)
			checkpoint, err := store.DeltaCheckpoint(t.Context(), mailbox)
			if err != nil {
				t.Fatal(err)
			}
			events, err := store.Journal(t.Context(), mailbox, JournalCursor{}, 2)
			if err != nil {
				t.Fatal(err)
			}
			id, lookupErr := store.ResolveNativeBinding(t.Context(), mailbox, binding)
			if phase == "before" && (checkpoint != (DeltaCheckpoint{}) || len(events.Changes) != 0) {
				t.Fatalf("uncommitted write survived: %+v, %+v", checkpoint, events)
			}
			if phase == "after" && (checkpoint != (DeltaCheckpoint{Cursor: "committed-page", Revision: 1}) || len(events.Changes) != 1 || events.Changes[0].ID != "synthetic-item") {
				t.Fatalf("committed write lost: %+v, %+v", checkpoint, events)
			}
			if phase == "before" && !errors.Is(lookupErr, core.ErrNotFound) || phase == "after" && (lookupErr != nil || id != "synthetic-item") {
				t.Fatalf("unexpected binding across transaction boundary: %s, %v", id, lookupErr)
			}
		})
	}
}
