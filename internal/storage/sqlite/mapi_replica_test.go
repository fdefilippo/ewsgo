// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"errors"
	"fmt"
	"path/filepath"
	"testing"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

func TestMAPIReplicaAtomicBindingsAndReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "replica.sqlite")
	a, mailbox := openJournalFixture(t, path)
	b, _ := openJournalFixture(t, path)
	if _, err := a.LoadMAPIReplica(ctx, "absent"); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("missing mailbox: %v", err)
	}
	snapshot, err := a.LoadMAPIReplica(ctx, mailbox)
	start := snapshot.Version
	if err != nil || start.HighWater != 0 || len(snapshot.Bindings) != 0 ||
		start.Epoch[7]>>4 != 4 || start.Epoch[8]>>6 != 2 {
		t.Fatalf("initial MAPI namespace: %+v, %v", start, err)
	}

	folder := MAPIObjectID{Namespace: MAPIFolder, Key: []byte("native:2"), Counter: 1}
	message := MAPIObjectID{Namespace: MAPIMessage, Key: []byte("native:2"), Counter: 2}
	if err := a.AdvanceMAPIReplica(ctx, mailbox, start, 3, []MAPIObjectID{folder, message}); err != nil {
		t.Fatal(err)
	}
	head := MAPIReplicaVersion{Epoch: start.Epoch, HighWater: 3}
	associated := MAPIObjectID{Namespace: MAPIAssociatedMessage, Key: []byte("native:2"), Counter: 4}
	conflicting := MAPIObjectID{Namespace: MAPIFolder, Key: []byte("native:2"), Counter: 5}
	if err := b.AdvanceMAPIReplica(ctx, mailbox, head, 5, []MAPIObjectID{associated, conflicting}); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("remap accepted: %v", err)
	}
	snapshot, err = b.LoadMAPIReplica(ctx, mailbox)
	if err != nil || snapshot.Version != head || len(snapshot.Bindings) != 2 {
		t.Fatalf("partial transaction survived: %+v, %d mappings, %v", snapshot.Version, len(snapshot.Bindings), err)
	}

	// A separate SQLite handle races from the same committed allocation.
	results := make(chan error, 2)
	for _, store := range []*Store{a, b} {
		go func() {
			results <- store.AdvanceMAPIReplica(ctx, mailbox, head, 5, []MAPIObjectID{associated})
		}()
	}
	first, second := <-results, <-results
	if first != nil {
		first, second = second, first
	}
	if first != nil || !errors.Is(second, core.ErrConflict) {
		t.Fatalf("concurrent allocations: %v, %v", first, second)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	c, _ := openJournalFixture(t, path)
	snapshot, err = c.LoadMAPIReplica(ctx, mailbox)
	got := snapshot.Version
	if err != nil || got != (MAPIReplicaVersion{Epoch: start.Epoch, HighWater: 5}) || len(snapshot.Bindings) != 3 {
		t.Fatalf("reopened allocation: %+v, %d, %v", got, len(snapshot.Bindings), err)
	}
	for i, want := range []MAPIObjectID{folder, message, associated} {
		binding := snapshot.Bindings[i]
		if binding.Namespace != want.Namespace || string(binding.Key) != string(want.Key) || binding.Counter != want.Counter {
			t.Fatalf("binding %d changed across reopen", i)
		}
	}
	another, err := c.BindMailbox(ctx, core.Scope{Backend: "fixture", Account: "journal-test"}, "two")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = c.LoadMAPIReplica(ctx, another.MailboxID)
	independent := snapshot.Version
	if err != nil || independent.Epoch == got.Epoch || independent.HighWater != 0 || len(snapshot.Bindings) != 0 {
		t.Fatalf("mailbox namespace leaked: %+v, %d, %v", independent, len(snapshot.Bindings), err)
	}
	if err := c.AdvanceMAPIReplica(ctx, another.MailboxID, got, 6, nil); !errors.Is(err, core.ErrResync) {
		t.Fatalf("foreign epoch accepted: %v", err)
	}
}

func TestMAPIReplicaRestoreRejectsPreviousEpoch(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	live := filepath.Join(dir, "live.sqlite")
	archive := filepath.Join(dir, "archive.sqlite")
	recovered := filepath.Join(dir, "recovered.sqlite")
	store, mailbox := openJournalFixture(t, live)
	snapshot, err := store.LoadMAPIReplica(ctx, mailbox)
	if err != nil {
		t.Fatal(err)
	}
	start := snapshot.Version
	ids := []MAPIObjectID{
		{Namespace: MAPIFolder, Key: []byte{0, 'f', ':', '2'}, Counter: 1},
		{Namespace: MAPIMessage, Key: []byte{0, 'm', ':', '1'}, Counter: 2},
	}
	versions := []MAPIProjectionVersion{{Counter: 2, Parent: 1, ChangeNumber: 3, Modified: time.Now(), Present: true}}
	if err := store.CommitMAPIProjection(ctx, mailbox, start, 3, ids, versions); err != nil {
		t.Fatal(err)
	}
	afterFolder := MAPIReplicaVersion{Epoch: start.Epoch, HighWater: 3}
	reservation, err := store.ReserveMAPIReplicaIDs(ctx, mailbox, afterFolder, 2)
	if err != nil {
		t.Fatal(err)
	}
	proof := [32]byte{5}
	if err := store.PublishMAPIContentsProof(ctx, mailbox, start.Epoch, proof); err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, live, archive); err != nil {
		t.Fatal(err)
	}
	beforeBackup := MAPIReplicaVersion{Epoch: start.Epoch, HighWater: reservation.Last}
	if err := store.AdvanceMAPIReplica(ctx, mailbox, beforeBackup, beforeBackup.HighWater+1, nil); err != nil {
		t.Fatal(err)
	}
	if err := Restore(ctx, archive, recovered); err != nil {
		t.Fatal(err)
	}
	restored, restoredMailbox := openJournalFixture(t, recovered)
	if mailbox != restoredMailbox {
		t.Fatal("core mailbox identity changed")
	}
	if err := restored.AdvanceMAPIReplica(ctx, mailbox, beforeBackup, beforeBackup.HighWater+1, nil); !errors.Is(err, core.ErrResync) {
		t.Fatalf("stale allocation accepted on restored store: %v", err)
	}
	snapshot, err = restored.LoadMAPIReplica(ctx, mailbox)
	current := snapshot.Version
	if err != nil || current.Epoch == start.Epoch || current.HighWater != beforeBackup.HighWater || len(snapshot.Bindings) != 2 ||
		len(snapshot.Reserved) != 0 || len(snapshot.Projection) != 1 {
		t.Fatalf("invalid restored wire namespace: %+v, %d bindings, %d ranges, %v", current, len(snapshot.Bindings), len(snapshot.Reserved), err)
	}
	if err := restored.AdvanceMAPIReplica(ctx, mailbox, beforeBackup, beforeBackup.HighWater+1, nil); !errors.Is(err, core.ErrResync) {
		t.Fatalf("previous epoch accepted after recreation: %v", err)
	}
	previousID := MAPIObjectID{Namespace: MAPIFolder, Key: []byte("stale"), Counter: reservation.First}
	if err := restored.ClaimMAPIReservedID(ctx, mailbox, start.Epoch, previousID); !errors.Is(err, core.ErrResync) {
		t.Fatalf("previous reservation accepted after recreation: %v", err)
	}
	if err := restored.ValidateMAPIContentsProof(ctx, mailbox, current.Epoch, proof); !errors.Is(err, core.ErrResync) {
		t.Fatalf("stale proof survived into the new epoch: %v", err)
	}
	if err := restored.AdvanceMAPIReplica(ctx, mailbox, current, maxMAPICounter, nil); err != nil {
		t.Fatal(err)
	}
	current.HighWater = maxMAPICounter
	if err := restored.AdvanceMAPIReplica(ctx, mailbox, current, maxMAPICounter+1, nil); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("48-bit counter overflow: %v", err)
	}
}

func TestMAPIStagedIDProvenanceAndRollback(t *testing.T) {
	ctx := t.Context()
	store, mailbox := openJournalFixture(t, filepath.Join(t.TempDir(), "staged.sqlite"))
	snapshot, err := store.LoadMAPIReplica(ctx, mailbox)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CommitMAPIReplica(ctx, mailbox, snapshot.Version, MAPIReplicaMutation{HighWater: 1, Stage: MAPIAssociatedMessage}); err != nil {
		t.Fatal(err)
	}
	current := MAPIReplicaVersion{Epoch: snapshot.Version.Epoch, HighWater: 1}
	binding := MAPIObjectID{Namespace: MAPIAssociatedMessage, Counter: 1, Key: []byte("associated")}
	commit := func(mutation MAPIReplicaMutation) error {
		mutation.HighWater = 2
		return store.CommitMAPIReplica(ctx, mailbox, current, mutation)
	}
	if err := commit(MAPIReplicaMutation{ReservedBindings: []MAPIObjectID{binding}}); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("server-issued ID accepted as client reservation: %v", err)
	}
	folder := binding
	folder.Namespace = MAPIFolder
	if err := commit(MAPIReplicaMutation{StagedBindings: []MAPIObjectID{folder}}); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("staged ID accepted across namespaces: %v", err)
	}
	projection := MAPIProjectionVersion{Counter: 1, Parent: 2, ChangeNumber: 2, Modified: time.Now(), Present: true}
	if err := commit(MAPIReplicaMutation{StagedBindings: []MAPIObjectID{binding}, Projection: []MAPIProjectionVersion{projection}}); err == nil {
		t.Fatal("commit accepted a missing parent")
	}
	snapshot, err = store.LoadMAPIReplica(ctx, mailbox)
	if err != nil || snapshot.Version != current || len(snapshot.Bindings) != 0 {
		t.Fatal("failed save exposed a partial mapping:", err)
	}
	if err := commit(MAPIReplicaMutation{StagedBindings: []MAPIObjectID{binding}}); err != nil {
		t.Fatal("rollback lost usable reservation:", err)
	}
	current.HighWater = 2
	if err := store.CommitMAPIReplica(ctx, mailbox, current, MAPIReplicaMutation{HighWater: 3, StagedBindings: []MAPIObjectID{binding}}); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("staged ID consumed twice: %v", err)
	}
}

func TestMAPIReplicaReopenIgnoresRetiredSpoolerEntries(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "retired-spooler.sqlite")
	store, mailbox := openJournalFixture(t, path)
	snapshot, err := store.LoadMAPIReplica(ctx, mailbox)
	if err != nil {
		t.Fatal(err)
	}
	ids := []MAPIObjectID{
		{Namespace: MAPIFolder, Key: []byte("drafts"), Counter: 1},
		{Namespace: MAPIFolder, Key: []byte("destination"), Counter: 2},
		{Namespace: MAPIMessage, Key: mapireplica.RetiredMessageKey(3), Counter: 3},
		{Namespace: MAPIMessage, Key: mapireplica.RetiredMessageKey(4), Counter: 4},
		{Namespace: MAPIMessage, Key: mapireplica.RetiredMessageKey(5), Counter: 5},
		{Namespace: MAPIMessage, Key: []byte("\x00m:1"), Counter: 6},
		{Namespace: MAPIMessage, Key: []byte("\x00m:2"), Counter: 7},
		{Namespace: MAPIMessage, Key: []byte("\x00m:3"), Counter: 8},
		{Namespace: MAPIMessage, Key: []byte("\x00draft"), Counter: 9},
	}
	if err := store.AdvanceMAPIReplica(ctx, mailbox, snapshot.Version, 12, ids); err != nil {
		t.Fatal(err)
	}
	for i, state := range []mapireplica.SpoolerState{mapireplica.SpoolerQueued, mapireplica.SpoolerDispatching, mapireplica.SpoolerAccepted} {
		if _, err := store.db.ExecContext(ctx, `
			INSERT INTO mapi_message_moves VALUES (?, ?, ?, 1, 2, ?, X'0100000000000000', ?, ?, 0);
		`, mailbox, 3+i, 6+i, fmt.Sprintf("m:%d", i+1), append([]byte{1}, make([]byte, 31)...), 10+i); err != nil {
			t.Fatal(err)
		}
		if _, err := store.db.ExecContext(ctx, `
			INSERT INTO mapi_spooler VALUES (?, ?, 1, X'0100000000000000', ?);
		`, mailbox, 3+i, state); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO mapi_spooler VALUES (?, 9, 1, X'0100000000000000', 2);
	`, mailbox); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _ := openJournalFixture(t, path)
	snapshot, err = reopened.LoadMAPIReplica(ctx, mailbox)
	want := mapireplica.SpoolerEntry{Counter: 9, Folder: 1, Revision: 1, State: mapireplica.SpoolerDispatching}
	if err != nil || len(snapshot.Spooler) != 1 || snapshot.Spooler[0] != want || len(snapshot.MessageMoves) != 3 {
		t.Fatalf("retired spooler recovery: queue %v, moves %d, err %v", snapshot.Spooler, len(snapshot.MessageMoves), err)
	}
}
