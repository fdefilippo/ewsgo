// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"ewsgo/internal/core"
)

func reservedFAIImportForTest(t *testing.T, f *hierarchyFixture) (faiImportRequest, uint64) {
	t.Helper()
	first, code := f.store.reserveLocalIDs(context.Background(), 1)
	if code != 0 {
		t.Fatal("reserve local identity")
	}
	request := syntheticFAIImport()
	copy(request.source[:16], f.store.replica[:])
	copy(request.source[16:], first[:])
	var counter uint64
	for _, b := range first {
		counter = counter<<8 | uint64(b)
	}
	return request, folderID(counter)
}

func TestFAIOwnReplicaMIDMatchesReservedSourceGID(t *testing.T) {
	f, _ := messageFixture(t)
	request, expectedMID := reservedFAIImportForTest(t, f)
	d := newFAIDraftForTest(t, f, request)
	row := saveFAIDraftForTest(t, f, d, 0)
	if row.mid != expectedMID || row.sourceKey != f.store.hierarchyXID(row.mid) {
		t.Fatal("imported own-replica SourceKey and MID identify different objects")
	}
	s := contentsStateForTest(t, f, testContentsStateConfig())
	uploadContentsTestSet(t, s, f.session, idsetGivenTag, singleReplicaSet(f.store.replica, []idRange{{folderGlobalCount(expectedMID), folderGlobalCount(expectedMID)}}))
}

func TestFAINewImportClientGivenRequiresPublishedCollectorGrant(t *testing.T) {
	f, _ := messageFixture(t)
	config := testContentsStateConfig()
	s, p := preparedContentsForTest(t, f, config, contentsSyncState{})
	initial := finishPreparedForTest(t, s, p)
	c := collectorForTest(t, f, initial)
	request, expectedMID := reservedFAIImportForTest(t, f)
	d := newFAIDraftForTest(t, f, request)
	row, err := c.save(context.Background(), f.session, d, 0)
	if err != nil {
		t.Fatal(err)
	}
	if row.mid != expectedMID {
		t.Fatal("reserved identity was replaced")
	}
	state, permit := collectorFinalForTest(t, f, c, initial.given)
	ranges := append(contentsRanges(state.given), idRange{folderGlobalCount(row.mid), folderGlobalCount(row.mid)})
	state.given = singleReplicaSet(f.store.replica, ranges)
	if err := f.store.validateContentsCheckpointRoot(t.Context(), s.scope, state, s.root); !errors.Is(err, core.ErrResync) {
		t.Fatal("unpublished grant accepted client Given")
	}
	f.store.finishFAIStatePermit(permit, true)
	if err := f.store.validateContentsCheckpointRoot(t.Context(), s.scope, state, s.root); err != nil {
		t.Fatal("published committed import does not authorize client Given", err)
	}
	// Same imported SeenFAI still permits a client that has not added the MID.
	withoutGiven := state
	withoutGiven.given = initial.given
	if err := f.store.validateContentsCheckpointRoot(t.Context(), s.scope, withoutGiven, s.root); err != nil {
		t.Fatal("existing Given behavior changed", err)
	}
	next := contentsStateForTest(t, f, config)
	uploadContentsTestState(t, next, f.session, state)
	if _, err := next.beginPlanning(context.Background(), f.session); err != nil {
		t.Fatal("client checkpoint cannot start planning", err)
	}
	// An unrelated committed FAI is known, but this collector never imported
	// it. A grant cannot bless arbitrary additions just because they exist.
	unrelated := newFAIDraftForTest(t, f, syntheticFAIImport())
	otherRow := saveFAIDraftForTest(t, f, unrelated, 0)
	forged := cloneContentsState(state)
	n := folderGlobalCount(otherRow.mid)
	forged.given = singleReplicaSet(f.store.replica, append(contentsRanges(forged.given), idRange{n, n}))
	if err := f.store.validateContentsCheckpointRoot(t.Context(), s.scope, forged, s.root); !errors.Is(err, core.ErrResync) {
		t.Fatal("grant authorized unrelated known message")
	}
	if err := f.store.validateContentsCheckpointRoot(t.Context(), s.scope, state, f.binding.fids[SentItems]); !errors.Is(err, core.ErrResync) {
		t.Fatal("Given grant crossed root")
	}
	for _, change := range []func(*contentsSyncState){
		func(v *contentsSyncState) { v.seen = idSet{} },
		func(v *contentsSyncState) { v.read = idSet{} },
		func(v *contentsSyncState) { v.given = singleReplicaSet([16]byte{99}, contentsRanges(v.given)) },
	} {
		forged := cloneContentsState(state)
		change(&forged)
		if err := f.store.validateContentsCheckpointRoot(t.Context(), s.scope, forged, s.root); !errors.Is(err, core.ErrResync) {
			t.Fatal("Given grant promoted unrelated state")
		}
	}
}

func TestFAIReservedMIDCollisionRollsBack(t *testing.T) {
	f, _ := messageFixture(t)
	messageObserve(t, f, Inbox)
	request, mid := reservedFAIImportForTest(t, f)
	d := newFAIDraftForTest(t, f, request)
	f.store.byMID[mid] = messageKey{id: "native-collision", associated: false}
	before := messageProof(f.store)
	if _, err := d.save(context.Background(), f.session, 0); !errors.Is(err, core.ErrConflict) {
		t.Fatal("reserved MID stole another object")
	}
	if !reflect.DeepEqual(before, messageProof(f.store)) || len(f.store.fai.records) != 0 || d.saved {
		t.Fatal("identity collision partially committed")
	}
}

func TestContentsNoForeignIdentifiersProjectsInternalSourceKey(t *testing.T) {
	for _, noForeign := range []bool{false, true} {
		f, _ := messageFixture(t)
		request := syntheticFAIImport()
		d := newFAIDraftForTest(t, f, request)
		row := saveFAIDraftForTest(t, f, d, 0)
		config := testContentsStateConfig()
		if noForeign {
			config.frame.flags |= 0x100
		}
		_, prepared := preparedContentsForTest(t, f, config, contentsSyncState{})
		transfer, err := f.store.contentsFastTransfer(context.Background(), config.frame, contentsSelectionForTest(t, config.frame, f.store), prepared.delta)
		if err != nil {
			t.Fatal(err)
		}
		defer transfer.close()
		want := request.source
		if noForeign {
			want = f.store.hierarchyXID(row.mid)
		}
		found := false
		for _, token := range decodeTestContents(t, transfer.stream.data) {
			if token.tag == 0x65e00102 && bytes.Equal(token.value.([]byte), want[:]) {
				found = true
			}
		}
		if !found || f.store.fai.records[d.id].sync.source != request.source {
			t.Fatal("SourceKey projection or stored lineage changed")
		}
	}
}

func TestCreatedGivenSubtractionIsBoundedAndDoesNotAlias(t *testing.T) {
	replica := [16]byte{1}
	set := singleReplicaSet(replica, []idRange{{1, maxGlobalCount}})
	before := cloneIDSet(set)
	got, changed := withoutCreatedGiven(set, replica, []uint64{2, maxGlobalCount - 1, 2})
	want := singleReplicaSet(replica, []idRange{{1, 1}, {3, maxGlobalCount - 2}, {maxGlobalCount, maxGlobalCount}})
	if !changed || !reflect.DeepEqual(got, want) || !reflect.DeepEqual(set, before) {
		t.Fatal("bounded subtraction or immutability failed")
	}
	if _, changed := withoutCreatedGiven(set, [16]byte{2}, []uint64{2}); changed {
		t.Fatal("foreign Given promoted")
	}
	if _, changed := withoutCreatedGiven(set, replica, make([]uint64, maxFAICollectorChanges+1)); changed {
		t.Fatal("oversized grant promoted")
	}
	var ranges []idRange
	for i := range maxIDSetRanges {
		n := uint64(i)*4 + 1
		ranges = append(ranges, idRange{n, n + 2})
	}
	if _, changed := withoutCreatedGiven(singleReplicaSet(replica, ranges), replica, []uint64{2}); changed {
		t.Fatal("fragmentation exceeded IDSET range quota")
	}
}

func TestFAIInternalGIDAliasPreservesLineageAndDeleteAcknowledgement(t *testing.T) {
	f, _ := messageFixture(t)
	request := syntheticFAIImport()
	d := newFAIDraftForTest(t, f, request)
	row := saveFAIDraftForTest(t, f, d, 0)
	alias := f.store.hierarchyXID(row.mid)
	updated := request
	updated.source = alias
	updated.change = testIncrementChangeXID(request.change)
	updated.predecessors = faiPCL{updated.change.namespace(): updated.change.local()}
	next := newFAIDraftForTest(t, f, updated)
	second := saveFAIDraftForTest(t, f, next, 0)
	if second.mid != row.mid || second.sourceKey != request.source || len(f.store.fai.records) != 1 {
		t.Fatal("internal alias rewrote lineage or created a second object")
	}
	changes, err := f.store.deleteFAIChanges(context.Background(), f.session, d.root, faiDeleteRequest{sources: [][22]byte{alias}})
	if err != nil || len(changes) != 1 || changes[0] == 0 || !f.store.fai.records[d.id].deleted {
		t.Fatal("alias delete lost its acknowledgement")
	}
	if _, _, err := f.store.findFAISource(f.binding.fids[SentItems], alias); !errors.Is(err, core.ErrAccessDenied) {
		t.Fatal("alias crossed folder")
	}
	native := messageObserve(t, f, Inbox).messages[0]
	if _, _, err := f.store.findFAISource(d.root, native.sourceKey); !errors.Is(err, core.ErrAccessDenied) {
		t.Fatal("alias promoted native mail into local FAI namespace")
	}
}

func TestFAINewGivenGrantsComposeAcrossCollectors(t *testing.T) {
	f, _ := messageFixture(t)
	config := testContentsStateConfig()
	s, p := preparedContentsForTest(t, f, config, contentsSyncState{})
	state := finishPreparedForTest(t, s, p)
	for range 8 {
		c := collectorForTest(t, f, state)
		request, mid := reservedFAIImportForTest(t, f)
		d := newFAIDraftForTest(t, f, request)
		if _, err := c.save(context.Background(), f.session, d, 0); err != nil {
			t.Fatal(err)
		}
		next, permit := collectorFinalForTest(t, f, c, state.given)
		n := folderGlobalCount(mid)
		next.given = singleReplicaSet(f.store.replica, append(contentsRanges(state.given), idRange{n, n}))
		f.store.finishFAIStatePermit(permit, true)
		if err := f.store.validateContentsCheckpointRoot(t.Context(), s.scope, next, s.root); err != nil {
			t.Fatal("published collector chain rejected", err)
		}
		state = next
		c.close()
	}
}
