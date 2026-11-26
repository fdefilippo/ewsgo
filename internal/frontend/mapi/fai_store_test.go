// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func newFAIDraftForTest(t *testing.T, f *hierarchyFixture, request faiImportRequest) *faiDraft {
	t.Helper()
	d, err := f.store.newFAIDraft(context.Background(), f.session, f.binding.fids[Inbox], request)
	if err != nil {
		t.Fatal("new draft", err)
	}
	t.Cleanup(d.close)
	values := faiSetRequest{{0x001a001f, core.Text("IPM.Microsoft.FolderDesign.NamedView")}, {0x30070040, core.Timestamp(hierarchyTestTime.Add(-time.Hour))}, {0x7006001f, core.Text("Synthetic view")}, {0x70010102, core.Binary{1, 2, 3}}}
	if _, err := d.set(context.Background(), f.session, values); err != nil {
		t.Fatal("set draft", err)
	}
	return d
}
func saveFAIDraftForTest(t *testing.T, f *hierarchyFixture, d *faiDraft, flags byte) messageRow {
	t.Helper()
	row, err := d.save(context.Background(), f.session, flags)
	if err != nil {
		t.Fatal("save draft", err)
	}
	return row
}
func TestFAIStoreCreateStagingSaveRereadAndLocalEdits(t *testing.T) {
	f, box := messageFixture(t)
	request := syntheticFAIImport()
	d := newFAIDraftForTest(t, f, request)
	before := f.store.next
	empty, err := f.store.observeFAIMessages(context.Background(), f.session, d.root)
	if err != nil || len(empty.snapshot.messages) != 0 || len(f.store.fai.records) != 0 || f.store.next != before {
		t.Fatal("draft escaped before save")
	}
	empty.close()
	first := saveFAIDraftForTest(t, f, d, 2)
	if first.sourceKey != request.source || first.changeKey != request.change || !bytes.Equal(first.predecessors, request.predecessors.marshal()) || !first.associated || first.modified != request.modified {
		t.Fatal("imported lineage/date not preserved")
	}
	observed, err := f.store.observeFAIMessages(context.Background(), f.session, d.root)
	if err != nil || len(observed.snapshot.messages) != 1 || !reflect.DeepEqual(first, observed.snapshot.messages[0]) {
		t.Fatal("saved FAI reread changed")
	}
	observed.close()
	if box.sourceCalls != 0 {
		t.Fatal("FAI accessed native message source")
	}
	if row := saveFAIDraftForTest(t, f, d, 2); row.cn != first.cn || row.readCN != first.readCN {
		t.Fatal("idempotent save advanced")
	}
	if _, err := d.set(context.Background(), f.session, faiSetRequest{{0x7006001f, core.Text("Local changed view")}}); err != nil {
		t.Fatal(err)
	}
	second := saveFAIDraftForTest(t, f, d, 1)
	pcl, err := parseFAIPCL(second.predecessors)
	if err != nil || second.mid != first.mid || second.cn <= first.cn || second.changeKey == first.changeKey || !bytes.Equal([]byte(second.changeKey[:16]), f.store.replica[:]) || !pcl.includes(request.predecessors) || !pcl.includesXID(second.changeKey) {
		t.Fatal("local save lineage not advanced/merged")
	}
	if _, err := d.set(context.Background(), f.session, faiSetRequest{{0x7006001f, core.Text("denied")}}); !errors.Is(err, core.ErrAccessDenied) {
		t.Fatal("KeepOpenReadOnly still writable")
	}
	again, err := f.store.observeFAIMessages(context.Background(), f.session, d.root)
	if err != nil || len(again.snapshot.messages) != 1 || again.snapshot.messages[0].cn != second.cn || again.snapshot.messages[0].changeKey != second.changeKey {
		t.Fatal("reread created spurious version after local save")
	}
	again.close()
	d.close()
	if f.stores.contents.used != f.store.fai.bytes {
		t.Fatal("draft lease leaked")
	}
}
func TestFAIStoreNewerOldConflictLWWAndFailFlag(t *testing.T) {
	for _, incomingWins := range []bool{false, true} {
		f, _ := messageFixture(t)
		old := syntheticFAIImport()
		d := newFAIDraftForTest(t, f, old)
		first := saveFAIDraftForTest(t, f, d, 0)
		if _, err := f.store.newFAIDraft(context.Background(), f.session, d.root, old); !errors.Is(err, errFAIIgnore) {
			t.Fatal("equal version not ignored")
		}
		next := old
		next.change = gidChangeXID(faiTestXID(1, 4))
		next.modified = old.modified.Add(time.Minute)
		next.predecessors = faiPCL{[16]byte{1}: testXIDLocal([6]byte{0, 0, 0, 0, 0, 4})}
		newer := newFAIDraftForTest(t, f, next)
		second := saveFAIDraftForTest(t, f, newer, 0)
		if second.mid != first.mid || second.cn <= first.cn || second.changeKey != next.change {
			t.Fatal("newer import not applied")
		}
		conflict := next
		conflict.change = gidChangeXID(faiTestXID(2, 1))
		conflict.predecessors = faiPCL{[16]byte{2}: testXIDLocal([6]byte{0, 0, 0, 0, 0, 1})}
		conflict.flags = 0x50
		if _, err := f.store.newFAIDraft(context.Background(), f.session, d.root, conflict); !errors.Is(err, core.ErrConflict) {
			t.Fatal("FailOnConflict accepted")
		}
		conflict.flags = 0x10
		if incomingWins {
			conflict.modified = next.modified.Add(time.Minute)
		} else {
			conflict.modified = next.modified.Add(-time.Minute)
		}
		cd := newFAIDraftForTest(t, f, conflict)
		if _, err := cd.set(context.Background(), f.session, faiSetRequest{{0x7006001f, core.Text("Conflicting view")}}); err != nil {
			t.Fatal(err)
		}
		winner := saveFAIDraftForTest(t, f, cd, 0)
		expected := next.change
		if incomingWins {
			expected = conflict.change
		}
		pcl, err := parseFAIPCL(winner.predecessors)
		if err != nil || winner.changeKey != expected || !pcl.includes(next.predecessors) || !pcl.includes(conflict.predecessors) || winner.cn <= second.cn {
			t.Fatal("LWW/merged history wrong")
		}
		row, err := f.store.observeFAIMessages(context.Background(), f.session, d.root)
		if err != nil || row.snapshot.messages[0].cn != winner.cn {
			t.Fatal("conflict resolution not stable")
		}
		row.close()
	}
}
func TestFAIStoreConcurrentDraftConflictAndForceSave(t *testing.T) {
	f, _ := messageFixture(t)
	request := syntheticFAIImport()
	a := newFAIDraftForTest(t, f, request)
	b := newFAIDraftForTest(t, f, request)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, d := range []*faiDraft{a, b} {
		wg.Add(1)
		go func() { defer wg.Done(); _, err := d.save(context.Background(), f.session, 2); results <- err }()
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		if err == nil {
			success++
		} else if errors.Is(err, core.ErrConflict) {
			conflict++
		} else {
			t.Fatal("unexpected concurrent save error")
		}
	}
	if success != 1 || conflict != 1 || len(f.store.fai.records) != 1 {
		t.Fatal("concurrent saves both committed/lost")
	}
	stale := b
	if b.saved {
		stale = a
	}
	if _, err := stale.set(context.Background(), f.session, faiSetRequest{{0x7006001f, core.Text("Forced view")}}); err != nil {
		t.Fatal(err)
	}
	forced := saveFAIDraftForTest(t, f, stale, 4)
	if forced.object.Properties[core.Key{ID: 0x7006}] != core.Text("Forced view") {
		t.Fatal("ForceSave failed to overwrite stale draft")
	}
}
func TestFAIStoreAtomicDeleteIsolationTombstoneAndReturnDenied(t *testing.T) {
	f, box := messageFixture(t)
	request := syntheticFAIImport()
	d := newFAIDraftForTest(t, f, request)
	row := saveFAIDraftForTest(t, f, d, 0)
	native := messageObserve(t, f, Inbox).messages[0]
	beforeBytes, beforeCount := f.store.fai.bytes, f.store.next
	if err := f.store.deleteFAI(context.Background(), f.session, d.root, faiDeleteRequest{sources: [][22]byte{request.source, native.sourceKey}}); !errors.Is(err, core.ErrAccessDenied) || f.store.fai.bytes != beforeBytes || f.store.next != beforeCount || f.store.fai.records[d.id].deleted {
		t.Fatal("mixed deletion had partial effects")
	}
	wrongFolder := f.binding.fids[SentItems]
	if err := f.store.deleteFAI(context.Background(), f.session, wrongFolder, faiDeleteRequest{sources: [][22]byte{request.source}}); !errors.Is(err, core.ErrAccessDenied) {
		t.Fatal("cross-folder delete")
	}
	if err := f.store.deleteFAI(context.Background(), f.session, d.root, faiDeleteRequest{flags: 2, sources: [][22]byte{request.source}}); err != nil {
		t.Fatal("delete", err)
	}
	after, err := f.store.observeFAIMessages(context.Background(), f.session, d.root)
	if err != nil || len(after.snapshot.messages) != 0 || !f.store.fai.records[d.id].deleted {
		t.Fatal("deleted FAI visible")
	}
	after.close()
	next := f.store.next
	if err := f.store.deleteFAI(context.Background(), f.session, d.root, faiDeleteRequest{sources: [][22]byte{request.source}}); err != nil || f.store.next != next {
		t.Fatal("repeat delete not idempotent")
	}
	request.change = testIncrementChangeXID(request.change)
	request.predecessors = faiPCL{[16]byte{1}: testXIDLocal([6]byte{0, 0, 0, 0, 0, 4})}
	if _, err := f.store.newFAIDraft(context.Background(), f.session, d.root, request); !errors.Is(err, core.ErrConflict) {
		t.Fatal("deleted identity resurrected")
	}
	key := f.store.byMID[row.mid]
	if f.store.messages[key].present || f.store.byMessage[key] != row.mid || len(f.store.fai.records[d.id].object.Properties) != 0 || len(box.objects) != 1 {
		t.Fatal("tombstone/native boundary broken")
	}
}
func TestFAIStoreSaveRollbackQuotaRevocationCancellationAndInvalid(t *testing.T) {
	for _, mode := range []string{"quota", "revocation", "cancel", "invalid", "registry"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := messageFixture(t)
			d := newFAIDraftForTest(t, f, syntheticFAIImport())
			before := f.store.next
			used := f.stores.contents.used
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var blocker *contentsLease
			switch mode {
			case "quota":
				blocker, _ = f.store.reserveContentsMemory(maxStoreContentsMemory - used - 1)
			case "revocation":
				f.box.state.Owner = false
			case "cancel":
				cancel()
			case "invalid":
				delete(d.properties, core.Key{ID: 0x3007})
			case "registry":
				f.store.next = maxGlobalCount
				before = f.store.next
			}
			if _, err := d.save(ctx, f.session, 2); err == nil || len(f.store.fai.records) != 0 || f.store.next != before || len(f.store.byMID) != 0 {
				t.Fatal("failed save mutated namespace/registry")
			}
			blocker.release()
			if f.stores.contents.used != used {
				t.Fatal("save rollback leaked budget")
			}
		})
	}
}
func TestFAIStoreNamedIsolationAndDiscard(t *testing.T) {
	f, _ := messageFixture(t)
	d := newFAIDraftForTest(t, f, syntheticFAIImport())
	name := propertyName{Set: [16]byte{1, 2, 3, 4}, Kind: 1, Name: "Synthetic configuration"}
	ids, code := f.store.named.ids(context.Background(), []propertyName{name}, true)
	if code != 0 {
		t.Fatal("name registry")
	}
	binary := core.Binary{1, 2, 3}
	if _, err := d.set(context.Background(), f.session, faiSetRequest{{uint32(ids[0])<<16 | 0x102, binary}}); err != nil {
		t.Fatal("named scalar", err)
	}
	binary[0] = 99
	row := saveFAIDraftForTest(t, f, d, 2)
	p := contentsProperty{kind: 0x102, name: name}
	key := faiCoreProperty(p)
	if row.object.Properties[key].(core.Binary)[0] != 1 {
		t.Fatal("caller mutation reached saved object")
	}
	row.object.Properties[key].(core.Binary)[0] = 98
	again, err := f.store.observeFAIMessages(context.Background(), f.session, d.root)
	if err != nil || again.snapshot.messages[0].object.Properties[key].(core.Binary)[0] != 1 {
		t.Fatal("returned row aliases archive")
	}
	again.close()
	other := f.session.Identity()
	other.Scope.Account = "other"
	if _, err := d.set(context.Background(), scopedSession{Session: f.session, identity: other}, faiSetRequest{{0x7006001f, core.Text("denied")}}); !errors.Is(err, core.ErrAccessDenied) {
		t.Fatal("foreign draft access")
	}
	d.close()
	d.close()
	if _, err := d.save(context.Background(), f.session, 2); !errors.Is(err, core.ErrClosed) {
		t.Fatal("discarded draft saved")
	}
}

func TestFAIStoreOwnReplicaSourceRequiresRealClientReservation(t *testing.T) {
	f, _ := messageFixture(t)
	first, code := f.store.reserveLocalIDs(context.Background(), 2)
	if code != 0 {
		t.Fatal("client reservation")
	}
	request := syntheticFAIImport()
	copy(request.source[:16], f.store.replica[:])
	copy(request.source[16:], first[:])
	request.change = gidChangeXID(request.source)
	request.change = testIncrementChangeXID(request.change)
	request.predecessors = faiPCL{f.store.replica: ""}
	var local [6]byte
	copy(local[:], string(request.change[16:]))
	request.predecessors[f.store.replica] = testXIDLocal(local)
	d := newFAIDraftForTest(t, f, request)
	row := saveFAIDraftForTest(t, f, d, 2)
	if row.sourceKey != request.source || row.mid == folderID(folderGlobalCount(f.binding.fids[Inbox])) {
		t.Fatal("client SourceKey not retained")
	}
	future := request
	future.predecessors = faiPCL{f.store.replica: xidLocal("\xff\xff\xff\xff\xff\xff")}
	future.change = gidChangeXID(hierarchyXID(f.store.replica, maxGlobalCount))
	if _, err := f.store.newFAIDraft(context.Background(), f.session, d.root, future); !errors.Is(err, core.ErrResync) {
		t.Fatal("future own counter accepted")
	}
}

func TestFAIStoreDownloadDeltaUsesImportedKeysAndLocalVersions(t *testing.T) {
	f, _ := messageFixture(t)
	c := testContentsStateConfig()
	c.frame.flags = 0x19
	request := syntheticFAIImport()
	draft := newFAIDraftForTest(t, f, request)
	created := saveFAIDraftForTest(t, f, draft, 0)
	state, prepared := preparedContentsForTest(t, f, c, contentsSyncState{})
	if len(prepared.delta.changes) != 1 || prepared.delta.changes[0].row.sourceKey != request.source || prepared.delta.changes[0].row.changeKey != request.change {
		t.Fatal("full FAI lineage")
	}
	initial := finishPreparedForTest(t, state, prepared)
	request.change = gidChangeXID(faiTestXID(1, 4))
	request.predecessors = faiPCL{[16]byte{1}: xidLocal("\x00\x00\x00\x00\x00\x04")}
	request.modified = request.modified.Add(time.Minute)
	newer := newFAIDraftForTest(t, f, request)
	changed := saveFAIDraftForTest(t, f, newer, 0)
	state, prepared = preparedContentsForTest(t, f, c, initial)
	if len(prepared.delta.changes) != 1 || prepared.delta.changes[0].row.mid != created.mid || prepared.delta.changes[0].row.cn != changed.cn || prepared.delta.changes[0].row.changeKey != request.change {
		t.Fatal("changed FAI delta")
	}
	initial = finishPreparedForTest(t, state, prepared)
	if err := f.store.deleteFAI(context.Background(), f.session, draft.root, faiDeleteRequest{sources: [][22]byte{request.source}}); err != nil {
		t.Fatal("delete")
	}
	state, prepared = preparedContentsForTest(t, f, c, initial)
	if len(prepared.delta.changes) != 0 || len(prepared.delta.deleted) != 1 || prepared.delta.deleted[0] != created.mid || prepared.delta.final.given.contains(f.store.replica, folderGlobalCount(created.mid)) {
		t.Fatal("FAI deletion delta")
	}
	finishPreparedForTest(t, state, prepared)
}
