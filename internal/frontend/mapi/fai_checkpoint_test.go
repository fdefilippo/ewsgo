// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"ewsgo/internal/core"
)

func collectorForTest(t *testing.T, f *hierarchyFixture, state contentsSyncState) *faiCollector {
	t.Helper()
	c, err := newFAICollector(context.Background(), f.session, f.store, f.binding.fids[Inbox])
	if err != nil {
		t.Fatal("new collector", err)
	}
	t.Cleanup(c.close)
	uploadContentsTestState(t, c.state, f.session, state)
	return c
}
func collectorFinalForTest(t *testing.T, f *hierarchyFixture, c *faiCollector, given idSet) (contentsSyncState, *faiStatePermit) {
	t.Helper()
	state, permit, _, err := c.transferState(context.Background(), f.session)
	if err != nil {
		t.Fatal("collector state", err)
	}
	t.Cleanup(func() { f.store.finishFAIStatePermit(permit, false) })
	if len(state.given.replicas) != 0 {
		t.Fatal("collector returned Given")
	}
	state.given = cloneIDSet(given) // The client retains its download Given.
	return state, permit
}
func TestFAICheckpointSavedImportPublicationReconnectAndDelete(t *testing.T) {
	f, box := messageFixture(t)
	config := testContentsStateConfig()
	s, p := preparedContentsForTest(t, f, config, contentsSyncState{})
	initial := finishPreparedForTest(t, s, p)
	c := collectorForTest(t, f, initial)
	d := newFAIDraftForTest(t, f, syntheticFAIImport())
	row, err := c.save(context.Background(), f.session, d, 2)
	if err != nil {
		t.Fatal("collector save", err)
	}
	after, permit := collectorFinalForTest(t, f, c, initial.given)
	if !after.seenFAI.contains(f.store.replica, row.cn) || !reflect.DeepEqual(after.seen, initial.seen) || !reflect.DeepEqual(after.read, initial.read) {
		t.Fatal("import advanced an unrelated set or omitted saved version")
	}
	if err := f.store.validateContentsCheckpointRoot(t.Context(), s.scope, after, s.root); !errors.Is(err, core.ErrResync) {
		t.Fatal("unpublished state accepted")
	}
	f.store.finishFAIStatePermit(permit, true)
	c.close() // Reconnect: only the published store permit survives.
	s, p = preparedContentsForTest(t, f, config, after)
	if len(p.delta.changes) != 1 || p.delta.changes[0].row.mid != row.mid {
		t.Fatal("new imported FAI not delivered despite absent Given")
	}
	checkpoint := finishPreparedForTest(t, s, p)
	c = collectorForTest(t, f, checkpoint)
	if err := c.delete(context.Background(), f.session, faiDeleteRequest{sources: [][22]byte{row.sourceKey}}); err != nil {
		t.Fatal("collector delete", err)
	}
	deleted, permit := collectorFinalForTest(t, f, c, checkpoint.given)
	f.store.finishFAIStatePermit(permit, true)
	c.close()
	s, p = preparedContentsForTest(t, f, config, deleted)
	if len(p.delta.deleted) != 1 || p.delta.deleted[0] != row.mid || len(p.delta.changes) != 0 {
		t.Fatal("deleted FAI checkpoint lost removal")
	}
	finishPreparedForTest(t, s, p)
	if box.sourceCalls != 3 {
		t.Fatal("unexpected native captures during collector operations")
	}
}
func TestFAICheckpointBindsScopeRootAndUnchangedSets(t *testing.T) {
	f, _ := messageFixture(t)
	config := testContentsStateConfig()
	s, p := preparedContentsForTest(t, f, config, contentsSyncState{})
	initial := finishPreparedForTest(t, s, p)
	c := collectorForTest(t, f, initial)
	d := newFAIDraftForTest(t, f, syntheticFAIImport())
	row, err := c.save(context.Background(), f.session, d, 0)
	if err != nil {
		t.Fatal(err)
	}
	state, permit := collectorFinalForTest(t, f, c, initial.given)
	f.store.finishFAIStatePermit(permit, true)
	for _, change := range []func(*contentsSyncState){
		func(v *contentsSyncState) { v.seen = idSet{} },
		func(v *contentsSyncState) { v.read = idSet{} },
		func(v *contentsSyncState) { v.given = idSet{} },
		func(v *contentsSyncState) {
			v.seenFAI = singleReplicaSet(f.store.replica, []idRange{{row.cn + 1, row.cn + 1}})
		},
	} {
		forged := cloneContentsState(state)
		change(&forged)
		if err := f.store.validateContentsCheckpointRoot(t.Context(), s.scope, forged, s.root); !errors.Is(err, core.ErrResync) {
			t.Fatal("collector grant promoted unrelated/unissued state")
		}
	}
	if err := f.store.validateContentsCheckpointRoot(t.Context(), s.scope, state, f.binding.fids[SentItems]); !errors.Is(err, core.ErrResync) {
		t.Fatal("permit rebound to another root")
	}
	modified := config
	modified.frame.flags |= 0x80
	modified.frame.tags = []uint32{0x0037001f}
	other := contentsStateForTest(t, f, modified)
	if err := f.store.validateContentsCheckpointRoot(t.Context(), other.scope, state, s.root); !errors.Is(err, core.ErrResync) {
		t.Fatal("permit bypassed projection scope")
	}
}
func TestFAICollectorGivenIgnoredPendingFailureAndAbort(t *testing.T) {
	f, box := messageFixture(t)
	c := collectorForTest(t, f, contentsSyncState{})
	// Even a foreign download Given is ignored. It cannot name writable FAI.
	uploadContentsTestSet(t, c.state, f.session, idsetGivenTag, singleReplicaSet([16]byte{77}, []idRange{{1, 9}}))
	if len(c.state.value.given.replicas) != 0 {
		t.Fatal("collector retained Given")
	}
	d := newFAIDraftForTest(t, f, syntheticFAIImport())
	if err := c.state.upload(context.Background(), f.session, hierarchyUpload{kind: 0x75, property: cnsetSeenFAITag, size: 1}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.save(context.Background(), f.session, d, 0); !errors.Is(err, core.ErrConflict) || len(f.store.fai.records) != 0 {
		t.Fatal("pending upload allowed save")
	}
	c.state.pending = nil // Drop deliberately unfinished synthetic framing.
	before := f.stores.contents.used
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.save(ctx, f.session, d, 0); !errors.Is(err, context.Canceled) || c.changes != 0 {
		t.Fatal("canceled save acknowledged")
	}
	f.box.state.Owner = false
	if _, err := c.save(context.Background(), f.session, d, 0); !errors.Is(err, core.ErrAccessDenied) || c.changes != 0 {
		t.Fatal("revoked owner saved")
	}
	f.box.state.Owner = true
	if _, err := c.save(context.Background(), f.session, d, 0); err != nil {
		t.Fatal(err)
	}
	if len(f.store.faiStatePermits) != 1 {
		t.Fatal("missing mutation reservation")
	}
	c.close()
	d.close()
	if len(f.store.faiStatePermits) != 0 || f.stores.contents.used >= before {
		t.Fatal("aborted unpublished permit leaked")
	}
	if box.sourceCalls != 0 {
		t.Fatal("collector read native content")
	}
}
func TestFAIStatePermitChainsPublicationQuotaAndNoEviction(t *testing.T) {
	f, _ := messageFixture(t)
	c := collectorForTest(t, f, contentsSyncState{})
	d := newFAIDraftForTest(t, f, syntheticFAIImport())
	if _, err := c.save(context.Background(), f.session, d, 2); err != nil {
		t.Fatal(err)
	}
	first, a := collectorFinalForTest(t, f, c, idSet{})
	if _, err := d.set(context.Background(), f.session, faiSetRequest{{0x7006001f, core.Text("next")}}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.save(context.Background(), f.session, d, 0); err != nil {
		t.Fatal(err)
	}
	second, b := collectorFinalForTest(t, f, c, idSet{})
	f.store.finishFAIStatePermit(b, true)
	scope := [32]byte{1}
	if err := f.store.validateContentsCheckpointRoot(t.Context(), scope, second, c.state.root); err != nil {
		t.Fatal("later complete state depended on unfinished earlier export")
	}
	f.store.finishFAIStatePermit(a, false)
	if err := f.store.validateContentsCheckpointRoot(t.Context(), scope, first, c.state.root); !errors.Is(err, core.ErrResync) {
		t.Fatal("aborted earlier export accepted")
	}
	// Every subsequent collector starts from a previously published state.
	prev := second.seenFAI
	var pending []*faiStatePermit
	for len(f.store.faiStatePermits) < maxFAIStatePermits {
		p, err := f.store.reserveFAIStatePermit(c.state.root, prev)
		if errors.Is(err, core.ErrUnsupported) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		pending = append(pending, p)
	}
	before := f.stores.contents.used
	if _, err := f.store.reserveFAIStatePermit(c.state.root, prev); !errors.Is(err, core.ErrUnsupported) || f.stores.contents.used != before {
		t.Fatal("permit quota evicted or allocated")
	}
	var wg sync.WaitGroup
	for _, p := range pending {
		wg.Add(1)
		go func() { defer wg.Done(); f.store.finishFAIStatePermit(p, false) }()
	}
	wg.Wait()
	if len(f.store.faiStatePermits) != 0 || len(f.store.faiUploadProofs) != 1 {
		t.Fatal("quota abort retained reservations or removed a published proof")
	}
	f.store.finishFAIStatePermit(b, false)
	if err := f.store.validateContentsCheckpointRoot(t.Context(), scope, second, c.state.root); err != nil {
		t.Fatal("published permit revoked by repeated cleanup")
	}
}

func TestFAIUploadPermitsAreReusableAcrossCollectorSessions(t *testing.T) {
	f, _ := messageFixture(t)
	root, otherRoot := f.binding.fids[Inbox], f.binding.fids[SentItems]
	scope := [32]byte{1}
	var expired, last contentsSyncState
	for i := range maxFAIStatePermits + 1 {
		c := collectorForTest(t, f, contentsSyncState{})
		if err := c.prepareMutation(t.Context(), f.session, 1); err != nil {
			t.Fatalf("session %d: %v", i, err)
		}
		c.acknowledge([]uint64{uint64(i + 1)})
		state, permit := collectorFinalForTest(t, f, c, idSet{})
		f.store.finishFAIStatePermit(permit, true)
		c.close()
		if len(f.store.faiStatePermits) != 0 || permit.lease.size() >= faiStatePermitMemory {
			t.Fatal("published upload retained an active reservation")
		}
		if i == 0 {
			expired = state
		}
		last = state
	}
	for range 2 {
		if err := f.store.validateContentsCheckpointRoot(t.Context(), scope, last, root); err != nil {
			t.Fatal("recent upload cannot be replayed", err)
		}
	}
	if err := f.store.validateContentsCheckpointRoot(t.Context(), scope, expired, root); !errors.Is(err, core.ErrResync) {
		t.Fatal("oldest upload proof remained past the retention limit", err)
	}
	if err := f.store.validateContentsCheckpointRoot(t.Context(), scope, last, otherRoot); !errors.Is(err, core.ErrResync) {
		t.Fatal("upload proof crossed folders", err)
	}
	a, b := collectorForTest(t, f, contentsSyncState{}), collectorForTest(t, f, contentsSyncState{})
	for _, c := range []*faiCollector{a, b} {
		if err := c.prepareMutation(t.Context(), f.session, 1); err != nil {
			t.Fatal(err)
		}
	}
	var canceled sync.WaitGroup
	canceled.Go(a.close)
	b.acknowledge([]uint64{100})
	state, permit := collectorFinalForTest(t, f, b, idSet{})
	f.store.finishFAIStatePermit(permit, true)
	b.close()
	canceled.Wait()
	if err := f.store.validateContentsCheckpointRoot(t.Context(), scope, state, root); err != nil {
		t.Fatal("closing one collector invalidated another collector's proof", err)
	}
	if len(f.store.faiStatePermits) != 0 || len(f.store.faiUploadProofs) != maxFAIStatePermits {
		t.Fatal("collector closure leaked beyond the two quotas")
	}
}
