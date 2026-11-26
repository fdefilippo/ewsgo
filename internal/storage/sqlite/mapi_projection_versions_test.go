// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"bytes"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestMAPIProjectionVersionsSurviveMoveReadAndDelete(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "projection.sqlite")
	store, mailbox := openJournalFixture(t, path)
	snapshot, err := store.LoadMAPIReplica(ctx, mailbox)
	if err != nil {
		t.Fatal(err)
	}
	initial := snapshot.Version
	mappings := []MAPIObjectID{
		{Namespace: MAPIFolder, Key: []byte("inbox"), Counter: 1},
		{Namespace: MAPIFolder, Key: []byte("archive"), Counter: 2},
		{Namespace: MAPIMessage, Key: bytes.Repeat([]byte("m"), 4097), Counter: 3},
	}
	now := time.Date(2026, 11, 24, 10, 2, 0, 765432100, time.UTC)
	message := MAPIProjectionVersion{
		Counter: 3, Parent: 1, ChangeNumber: 4, ReadNumber: 5,
		Fingerprint: [32]byte{6}, NativeRevision: ^core.Revision(0), Modified: now, Present: true,
		ChangeKey: bytes.Repeat([]byte{0x10}, 22), Predecessors: bytes.Repeat([]byte{0x20}, 23),
	}
	if err := store.CommitMAPIProjection(ctx, mailbox, initial, 5, mappings, []MAPIProjectionVersion{message}); err != nil {
		t.Fatal(err)
	}
	head := MAPIReplicaVersion{Epoch: initial.Epoch, HighWater: 5}
	moved := message
	moved.Parent, moved.ChangeNumber = 2, 6
	if err := store.CommitMAPIProjection(ctx, mailbox, head, 6, nil, []MAPIProjectionVersion{moved}); err != nil {
		t.Fatal(err)
	}
	head.HighWater = 6
	read := moved
	read.ReadNumber, read.Read = 7, true
	if err := store.CommitMAPIProjection(ctx, mailbox, head, 7, nil, []MAPIProjectionVersion{read}); err != nil {
		t.Fatal(err)
	}
	head.HighWater = 7
	unmapped := MAPIObjectID{Namespace: MAPIMessage, Key: []byte("rollback"), Counter: 8}
	stale := read
	stale.ChangeNumber, stale.ReadNumber = 4, 9
	if err := store.CommitMAPIProjection(ctx, mailbox, head, 9, []MAPIObjectID{unmapped}, []MAPIProjectionVersion{stale}); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("regressed change number: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _ := openJournalFixture(t, path)
	snapshot, err = reopened.LoadMAPIReplica(ctx, mailbox)
	if err != nil || snapshot.Version != head || len(snapshot.Bindings) != 3 || len(snapshot.Projection) != 1 {
		t.Fatalf("reopened move or rolled-back update: %v", err)
	}
	got := snapshot.Projection[0]
	if got.Parent != 2 || got.ChangeNumber != 6 || got.ReadNumber != 7 || !got.Present || !got.Read ||
		got.NativeRevision != message.NativeRevision || !got.Modified.Equal(now) ||
		got.Fingerprint != message.Fingerprint || !bytes.Equal(got.ChangeKey, message.ChangeKey) ||
		!bytes.Equal(got.Predecessors, message.Predecessors) {
		t.Fatal("message version changed across reopen")
	}
	deleted := read
	deleted.Present, deleted.ChangeNumber = false, 8
	if err := reopened.CommitMAPIProjection(ctx, mailbox, head, 8, nil, []MAPIProjectionVersion{deleted}); err != nil {
		t.Fatal(err)
	}
	another, _ := openJournalFixture(t, path)
	snapshot, err = another.LoadMAPIReplica(ctx, mailbox)
	if err != nil || snapshot.Version.HighWater != 8 || len(snapshot.Projection) != 1 ||
		snapshot.Projection[0].Present || snapshot.Projection[0].ChangeNumber != 8 {
		t.Fatalf("durable deletion version: %v", err)
	}
}
