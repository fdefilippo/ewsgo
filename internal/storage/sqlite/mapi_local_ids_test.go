// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"errors"
	"path/filepath"
	"slices"
	"testing"

	"ewsgo/internal/core"
)

func TestMAPIReservedIDsSurviveReopenAndBindOnce(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "reservations.sqlite")
	a, mailbox := openJournalFixture(t, path)
	snapshot, err := a.LoadMAPIReplica(ctx, mailbox)
	if err != nil {
		t.Fatal(err)
	}
	version := snapshot.Version
	if issued, err := a.ReserveMAPIReplicaIDs(ctx, mailbox, version, 3); err != nil || issued != (MAPIIDRange{First: 1, Last: 3}) {
		t.Fatalf("first issued interval: %v, %v", issued, err)
	}
	if _, err := a.ReserveMAPIReplicaIDs(ctx, mailbox, version, 1); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("stale reservation accepted: %v", err)
	}
	version.HighWater = 3
	if issued, err := a.ReserveMAPIReplicaIDs(ctx, mailbox, version, 2); err != nil || issued != (MAPIIDRange{First: 4, Last: 5}) {
		t.Fatalf("adjacent issued interval: %v, %v", issued, err)
	}
	version.HighWater = 5
	if err := a.AdvanceMAPIReplica(ctx, mailbox, version, 6, nil); err != nil {
		t.Fatal(err)
	}
	version.HighWater = 6
	if issued, err := a.ReserveMAPIReplicaIDs(ctx, mailbox, version, 1); err != nil || issued != (MAPIIDRange{First: 7, Last: 7}) {
		t.Fatalf("separate issued interval: %v, %v", issued, err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	b, _ := openJournalFixture(t, path)
	c, _ := openJournalFixture(t, path)
	snapshot, err = b.LoadMAPIReplica(ctx, mailbox)
	version.HighWater = 7
	if err != nil || snapshot.Version != version || !slices.Equal(snapshot.Reserved, []MAPIIDRange{{First: 1, Last: 5}, {First: 7, Last: 7}}) {
		t.Fatalf("reopened reservations: %v, %v", snapshot.Reserved, err)
	}
	gap := MAPIObjectID{Namespace: MAPIFolder, Key: []byte("gap"), Counter: 6}
	if err := b.ClaimMAPIReservedID(ctx, mailbox, version.Epoch, gap); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("unreserved gap accepted: %v", err)
	}
	if err := b.AdvanceMAPIReplica(ctx, mailbox, version, 8, []MAPIObjectID{{Namespace: MAPIFolder, Key: []byte("reserved"), Counter: 7}}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("ordinary allocation used previous counter: %v", err)
	}
	candidates := []struct {
		store   *Store
		binding MAPIObjectID
	}{
		{b, MAPIObjectID{Namespace: MAPIFolder, Key: []byte("folder"), Counter: 4}},
		{c, MAPIObjectID{Namespace: MAPIAssociatedMessage, Key: []byte("associated"), Counter: 4}},
	}
	type result struct {
		binding MAPIObjectID
		err     error
	}
	done := make(chan result, 2)
	for _, candidate := range candidates {
		go func() {
			done <- result{candidate.binding, candidate.store.ClaimMAPIReservedID(ctx, mailbox, version.Epoch, candidate.binding)}
		}()
	}
	winner, loser := <-done, <-done
	if winner.err != nil {
		winner, loser = loser, winner
	}
	if winner.err != nil || !errors.Is(loser.err, core.ErrConflict) {
		t.Fatalf("competing reservation claims: %v, %v", winner.err, loser.err)
	}
	if err := c.ClaimMAPIReservedID(ctx, mailbox, version.Epoch, winner.binding); err != nil {
		t.Fatalf("identical replay failed: %v", err)
	}
	snapshot, err = c.LoadMAPIReplica(ctx, mailbox)
	if err != nil || snapshot.Version != version || len(snapshot.Bindings) != 1 || snapshot.Bindings[0].Counter != 4 {
		t.Fatalf("replay unexpectedly changed allocation: %v, %v", snapshot.Version, err)
	}
}
