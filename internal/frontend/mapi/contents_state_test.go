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

func contentsStateForTest(t *testing.T, f *hierarchyFixture, c contentsConfigure) *contentsState {
	t.Helper()
	plan, err := compileContentsFilter(c, &f.store.named, func(contentsProperty) bool { return true })
	if err != nil {
		t.Fatal("compile Contents", err)
	}
	s, err := newContentsState(context.Background(), f.session, f.store, f.binding.fids[Inbox], c, plan)
	if err != nil {
		t.Fatal("new Contents state", err)
	}
	t.Cleanup(s.close)
	return s
}
func testContentsStateConfig() contentsConfigure {
	return contentsConfigure{frame: synchronizationConfigure{kind: 1, flags: 0x39, sendOptions: 0x1d, extraFlags: 5}}
}
func uploadContentsTestSet(t *testing.T, s *contentsState, session core.Session, tag uint32, set idSet) {
	t.Helper()
	raw, err := set.marshal()
	if err != nil {
		t.Fatal("marshal synthetic state")
	}
	if err = s.upload(context.Background(), session, hierarchyUpload{kind: 0x75, property: tag, size: uint32(len(raw))}); err != nil {
		t.Fatal("upload begin", err)
	}
	for at := 0; at < len(raw); {
		end := min(at+11, len(raw))
		if err = s.upload(context.Background(), session, hierarchyUpload{kind: 0x76, size: uint32(end - at), data: raw[at:end]}); err != nil {
			t.Fatal("upload continue", err)
		}
		at = end
	}
	if err = s.upload(context.Background(), session, hierarchyUpload{kind: 0x77}); err != nil {
		t.Fatal("upload end", err)
	}
}
func uploadContentsTestState(t *testing.T, s *contentsState, session core.Session, state contentsSyncState) {
	t.Helper()
	for _, item := range []struct {
		tag uint32
		set idSet
	}{{idsetGivenTag, state.given}, {cnsetSeenTag, state.seen}, {cnsetSeenFAITag, state.seenFAI}, {cnsetReadTag, state.read}} {
		uploadContentsTestSet(t, s, session, item.tag, item.set)
	}
}
func syntheticMessageState(row messageRow, replica [16]byte) contentsSyncState {
	state := contentsSyncState{given: singleReplicaSet(replica, []idRange{{folderGlobalCount(row.mid), folderGlobalCount(row.mid)}}), read: singleReplicaSet(replica, []idRange{{row.readCN, row.readCN}})}
	if row.associated {
		state.seenFAI = singleReplicaSet(replica, []idRange{{row.cn, row.cn}})
	} else {
		state.seen = singleReplicaSet(replica, []idRange{{row.cn, row.cn}})
	}
	return state
}

func TestContentsStateFourSetsPublishedCheckpointReplayAndMixedSetRejection(t *testing.T) {
	f, _ := messageFixture(t)
	row := messageObserve(t, f, Inbox).messages[0]
	c := testContentsStateConfig()
	s := contentsStateForTest(t, f, c)
	initial, err := s.beginPlanning(context.Background(), f.session)
	if err != nil || !reflect.DeepEqual(initial, contentsSyncState{}) {
		t.Fatal("empty initial state not accepted")
	}
	state := syntheticMessageState(row, f.store.replica)
	ticket, err := f.store.stageContentsCheckpoint(s.scope, state)
	if err != nil {
		t.Fatal(err)
	}
	next := contentsStateForTest(t, f, c)
	uploadContentsTestState(t, next, f.session, state)
	if _, err := next.beginPlanning(context.Background(), f.session); !errors.Is(err, core.ErrResync) {
		t.Fatal("pending checkpoint authorized replay")
	}
	ticket.finish(t.Context(), true)
	got, err := next.beginPlanning(context.Background(), f.session)
	if err != nil || !reflect.DeepEqual(got, state) {
		t.Fatal("published checkpoint replay rejected", err)
	}
	got.given.replicas[0].ranges[0].low = 0
	if next.value.given.replicas[0].ranges[0].low == 0 {
		t.Fatal("planning result aliases committed state")
	}
	if err := next.upload(context.Background(), f.session, hierarchyUpload{kind: 0x75, property: idsetGivenTag}); !errors.Is(err, core.ErrConflict) {
		t.Fatal("upload after planning accepted")
	}
	mixed := contentsStateForTest(t, f, c)
	state.read = idSet{}
	uploadContentsTestState(t, mixed, f.session, state)
	if _, err := mixed.beginPlanning(context.Background(), f.session); !errors.Is(err, core.ErrResync) {
		t.Fatal("four individually valid but mixed sets accepted")
	}
}

func TestContentsStateAtomicUploadTruncationQuotaCancellationAndInvalidIdentity(t *testing.T) {
	f, _ := messageFixture(t)
	row := messageObserve(t, f, Inbox).messages[0]
	s := contentsStateForTest(t, f, testContentsStateConfig())
	state := syntheticMessageState(row, f.store.replica)
	uploadContentsTestSet(t, s, f.session, cnsetSeenTag, state.seen)
	before := cloneContentsState(s.value)
	if err := s.upload(context.Background(), f.session, hierarchyUpload{kind: 0x75, property: cnsetSeenTag, size: maxIDSetBytes + 1}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal("oversize state accepted")
	}
	if err := s.upload(context.Background(), f.session, hierarchyUpload{kind: 0x75, property: 0x12340102}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal("unknown state kind accepted")
	}
	if err := s.upload(context.Background(), f.session, hierarchyUpload{kind: 0x75, property: cnsetSeenTag, size: 2}); err != nil {
		t.Fatal(err)
	}
	if err := s.upload(context.Background(), f.session, hierarchyUpload{kind: 0x76, size: 1, data: []byte{1}}); err != nil {
		t.Fatal(err)
	}
	if err := s.upload(context.Background(), f.session, hierarchyUpload{kind: 0x77}); !errors.Is(err, core.ErrConflict) || !reflect.DeepEqual(before, s.value) {
		t.Fatal("truncated upload replaced committed set")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.upload(ctx, f.session, hierarchyUpload{kind: 0x76, size: 1, data: []byte{2}}); !errors.Is(err, context.Canceled) || len(s.pending.data) != 1 {
		t.Fatal("canceled continuation advanced")
	}
	s.close()
	s.close()
	if f.stores.contents.used != 0 {
		t.Fatal("state/pending memory leaked")
	}
	if err := s.upload(context.Background(), f.session, hierarchyUpload{kind: 0x77}); !errors.Is(err, core.ErrClosed) {
		t.Fatal("closed context accepted upload")
	}
	for _, set := range []idSet{singleReplicaSet([16]byte{99}, []idRange{{1, 1}}), singleReplicaSet(f.store.replica, []idRange{{0, 0}}), singleReplicaSet(f.store.replica, []idRange{{1, maxGlobalCount}}), singleReplicaSet(f.store.replica, []idRange{{folderGlobalCount(f.binding.fids[Inbox]), folderGlobalCount(f.binding.fids[Inbox])}})} {
		fresh := contentsStateForTest(t, f, testContentsStateConfig())
		raw, _ := set.marshal()
		fresh.upload(context.Background(), f.session, hierarchyUpload{kind: 0x75, property: idsetGivenTag, size: uint32(len(raw))})
		if len(raw) > 0 {
			fresh.upload(context.Background(), f.session, hierarchyUpload{kind: 0x76, size: uint32(len(raw)), data: raw})
		}
		if err := fresh.upload(context.Background(), f.session, hierarchyUpload{kind: 0x77}); !errors.Is(err, core.ErrResync) || !reflect.DeepEqual(fresh.value, contentsSyncState{}) {
			t.Fatal("foreign/stale/unknown message identity accepted", err)
		}
		fresh.close()
	}
}

func TestContentsStateScopeFilterProjectionAndEpochBinding(t *testing.T) {
	f, _ := messageFixture(t)
	row := messageObserve(t, f, Inbox).messages[0]
	c := testContentsStateConfig()
	s := contentsStateForTest(t, f, c)
	state := syntheticMessageState(row, f.store.replica)
	ticket, err := f.store.stageContentsCheckpoint(s.scope, state)
	if err != nil {
		t.Fatal(err)
	}
	ticket.finish(t.Context(), true)
	for _, change := range []func(*contentsConfigure){
		func(c *contentsConfigure) { c.frame.flags |= 0x80; c.frame.tags = []uint32{0x0037001f} },
		func(c *contentsConfigure) { c.frame.extraFlags |= 8 },
		func(c *contentsConfigure) { c.restriction = &contentsRestriction{op: 8, tag: 0x0037001f} },
	} {
		modified := c
		change(&modified)
		other := contentsStateForTest(t, f, modified)
		uploadContentsTestState(t, other, f.session, state)
		if _, err := other.beginPlanning(context.Background(), f.session); !errors.Is(err, core.ErrResync) {
			t.Fatal("state rebound across filter/projection")
		}
		other.close()
	}
	otherScope, err := contentsScopeFingerprint(f.binding.fids[SentItems], c.frame, s.plan)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.store.validateContentsCheckpoint(t.Context(), otherScope, state); !errors.Is(err, core.ErrResync) {
		t.Fatal("state rebound to another folder")
	}
	otherFlags := c.frame
	otherFlags.sendOptions |= 0x60
	otherFlags.extraFlags |= 0x80000000
	ignoredScope, err := contentsScopeFingerprint(s.root, otherFlags, s.plan)
	if err != nil || ignoredScope != s.scope {
		t.Fatal("ignored protocol bits changed scope")
	}
	otherIdentity := f.session.Identity()
	otherIdentity.Scope.Account = "another-account"
	if _, err = s.beginPlanning(context.Background(), scopedSession{Session: f.session, identity: otherIdentity}); !errors.Is(err, core.ErrAccessDenied) {
		t.Fatal("foreign authenticated scope accepted")
	}
	catalog, _ := privateFolders(context.Background(), f.session)
	restarted, err := newRAMStores().bind(context.Background(), catalog)
	if err != nil {
		t.Fatal(err)
	}
	if err = restarted.store.validateContentsCheckpoint(t.Context(), s.scope, state); !errors.Is(err, core.ErrResync) {
		t.Fatal("checkpoint survived restart")
	}
}

func TestContentsCheckpointReservationsConcurrentPublishRollbackAndLimit(t *testing.T) {
	f, _ := messageFixture(t)
	row := messageObserve(t, f, Inbox).messages[0]
	s := contentsStateForTest(t, f, testContentsStateConfig())
	state := syntheticMessageState(row, f.store.replica)
	base := f.stores.contents.used
	a, err := f.store.stageContentsCheckpoint(s.scope, state)
	if err != nil {
		t.Fatal(err)
	}
	b, err := f.store.stageContentsCheckpoint(s.scope, state)
	if err != nil || f.stores.contents.used != base+contentsProofMemory {
		t.Fatal("duplicate checkpoint not shared")
	}
	a.finish(t.Context(), false)
	if len(f.store.contentsProofs) != 1 {
		t.Fatal("one cancellation invalidated other pending ticket")
	}
	b.finish(t.Context(), false)
	b.finish(t.Context(), false)
	if len(f.store.contentsProofs) != 0 || f.stores.contents.used != base {
		t.Fatal("unpublished checkpoint leaked quota")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ticket, e := f.store.stageContentsCheckpoint(s.scope, state)
			if e != nil {
				t.Error("concurrent stage failed")
				return
			}
			ticket.finish(t.Context(), true)
		}()
	}
	wg.Wait()
	if err = f.store.validateContentsCheckpoint(t.Context(), s.scope, state); err != nil || len(f.store.contentsProofs) != 1 || f.stores.contents.used != base+contentsProofMemory {
		t.Fatal("concurrent publication inconsistent")
	}
	for i := 1; i < maxContentsStateProofs; i++ {
		scope := s.scope
		scope[0] ^= byte(i)
		ticket, e := f.store.stageContentsCheckpoint(scope, state)
		if e != nil {
			t.Fatal("checkpoint limit premature", i)
		}
		ticket.finish(t.Context(), true)
	}
	if len(f.store.contentsProofs) != maxContentsStateProofs {
		t.Fatal("checkpoint count incorrect")
	}
	scope := s.scope
	scope[2] ^= 0xff
	replay, err := f.store.stageContentsCheckpoint(s.scope, state)
	if err != nil {
		t.Fatal(err)
	}
	newer, err := f.store.stageContentsCheckpoint(scope, state)
	if err != nil {
		t.Fatal("a full retained window blocked a new transfer", err)
	}
	newer.finish(t.Context(), false)
	if err := f.store.validateContentsCheckpoint(t.Context(), s.scope, state); err != nil || len(f.store.contentsProofs) != maxContentsStateProofs {
		t.Fatal("abandoned transfer displaced a usable checkpoint")
	}
	newer, err = f.store.stageContentsCheckpoint(scope, state)
	if err != nil {
		t.Fatal(err)
	}
	newer.finish(t.Context(), true)
	if err := f.store.validateContentsCheckpoint(t.Context(), scope, state); err != nil {
		t.Fatal("most recent completed checkpoint cannot resume", err)
	}
	s.value = state
	if _, err = s.beginPlanning(t.Context(), f.session); hierarchyError(err) != 0x80040805 {
		t.Fatal("expired state did not report NotSynchronized", err)
	}
	if len(f.store.contentsProofs) != maxContentsStateProofs+1 {
		t.Fatal("eviction discarded the pending retransmission's reservation")
	}
	replay.finish(t.Context(), true)
	if err := f.store.validateContentsCheckpoint(t.Context(), s.scope, state); err != nil || len(f.store.contentsProofs) != maxContentsStateProofs {
		t.Fatal("completed retransmission failed to republish its proof", err)
	}
	second := s.scope
	second[0] ^= 1
	if err := f.store.validateContentsCheckpoint(t.Context(), second, state); !errors.Is(err, core.ErrResync) {
		t.Fatal("completed retransmission did not expire the oldest retained proof", err)
	}
	s.close()
	if f.stores.contents.used != maxContentsStateProofs*contentsProofMemory {
		t.Fatal("published proof or state lease accounting inconsistent")
	}
}

func TestContentsStateAccessRevokedDuringUploadAndMemoryQuota(t *testing.T) {
	for _, phase := range []byte{0x75, 0x76, 0x77} {
		f, _ := messageFixture(t)
		row := messageObserve(t, f, Inbox).messages[0]
		s := contentsStateForTest(t, f, testContentsStateConfig())
		raw, _ := syntheticMessageState(row, f.store.replica).given.marshal()
		if phase != 0x75 {
			if err := s.upload(context.Background(), f.session, hierarchyUpload{kind: 0x75, property: idsetGivenTag, size: uint32(len(raw))}); err != nil {
				t.Fatal(err)
			}
		}
		if phase == 0x77 {
			if err := s.upload(context.Background(), f.session, hierarchyUpload{kind: 0x76, size: uint32(len(raw)), data: raw}); err != nil {
				t.Fatal(err)
			}
		}
		var before []byte
		if s.pending != nil {
			before = append([]byte(nil), s.pending.data...)
		}
		f.box.state.Owner = false
		if err := s.upload(context.Background(), f.session, hierarchyUpload{kind: phase, property: idsetGivenTag, size: uint32(len(raw)), data: raw}); !errors.Is(err, core.ErrAccessDenied) || !reflect.DeepEqual(s.value, contentsSyncState{}) {
			t.Fatal("revoked account committed state")
		}
		if s.pending != nil && !reflect.DeepEqual(before, s.pending.data) {
			t.Fatal("revoked continuation changed pending buffer")
		}
		if _, err := s.beginPlanning(context.Background(), f.session); !errors.Is(err, core.ErrAccessDenied) || s.started {
			t.Fatal("revoked account began download")
		}
		s.close()
		if f.stores.contents.used != 0 {
			t.Fatal("revoked state leaked memory")
		}
	}
	f, _ := messageFixture(t)
	c := testContentsStateConfig()
	plan, err := compileContentsFilter(c, &f.store.named, func(contentsProperty) bool { return true })
	if err != nil {
		t.Fatal(err)
	}
	lease, err := f.store.reserveContentsMemory(maxStoreContentsMemory)
	if err != nil {
		t.Fatal(err)
	}
	if s, err := newContentsState(context.Background(), f.session, f.store, f.binding.fids[Inbox], c, plan); err == nil || s != nil {
		t.Fatal("state allocation bypassed shared quota")
	}
	lease.release()
	if f.stores.contents.used != 0 {
		t.Fatal("failed construction leaked reservation")
	}
}
