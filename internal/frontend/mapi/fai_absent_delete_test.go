// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"ewsgo/internal/core"
)

func TestFAIAbsentDeleteDoesNotInventMessageOrChange(t *testing.T) {
	f, box := messageFixture(t)
	source := syntheticFAIImport().source
	root := f.binding.fids[Inbox]
	before, mappings := f.store.next, len(f.store.byMessage)
	changes, err := f.store.deleteFAIChanges(context.Background(), f.session, root, faiDeleteRequest{sources: [][22]byte{source}})
	if err != nil {
		t.Fatal("absent delete should record the deletion instead of returning NotFound")
	}
	if len(changes) != 0 || f.store.next != before || len(f.store.byMessage) != mappings || len(f.store.fai.records) != 0 || box.sourceCalls != 0 {
		t.Fatal("absent delete invented message/change or read native messages")
	}
	if _, err := f.store.newFAIDraft(context.Background(), f.session, root, syntheticFAIImport()); !errors.Is(err, core.ErrConflict) {
		t.Fatal("previously absent deleted identity could be resurrected")
	}
	used := f.stores.contents.used
	if f.store.fai.bytes != 128 || used != 128 || f.store.fai.absent[source] != root {
		t.Fatal("absent tombstone is not charged to the namespace budget")
	}
	if err := f.store.deleteFAI(context.Background(), f.session, root, faiDeleteRequest{sources: [][22]byte{source}}); err != nil || f.store.next != before || f.stores.contents.used != used {
		t.Fatal("repeat absent deletion changed counters or retained memory")
	}
	view, err := f.store.observeFAIMessages(context.Background(), f.session, root)
	if err != nil || len(view.snapshot.messages) != 0 {
		t.Fatal("absent deletion became an exportable message")
	}
	view.close()
	if f.stores.contents.used != used {
		t.Fatal("absent view leaked memory")
	}
	other := syntheticFAIImport()
	other.source = faiTestXID(56, 10)
	draft := newFAIDraftForTest(t, f, other)
	saveFAIDraftForTest(t, f, draft, 0)
	draft.close()
	if f.store.fai.absent[source] != root {
		t.Fatal("unrelated FAI save discarded absent deletion")
	}
	if _, err := f.store.newFAIDraft(context.Background(), f.session, f.binding.fids[SentItems], syntheticFAIImport()); !errors.Is(err, core.ErrAccessDenied) {
		t.Fatal("absent identity was rebound to another folder")
	}
}

func TestFAIAbsentDeleteAtomicBatchAndGuards(t *testing.T) {
	for _, mode := range []string{"native", "owner", "scope", "cancel", "memory", "unissued", "invalid", "other_folder", "success"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := messageFixture(t)
			request := syntheticFAIImport()
			draft := newFAIDraftForTest(t, f, request)
			saveFAIDraftForTest(t, f, draft, 0)
			draft.close()
			native := messageObserve(t, f, Inbox).messages[0]
			unknown := faiTestXID(55, 10)
			sources := [][22]byte{request.source, unknown}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			session := f.session
			var blocker *contentsLease
			switch mode {
			case "native":
				sources = append(sources, native.sourceKey)
			case "owner":
				f.box.state.Owner = false
			case "scope":
				identity := session.Identity()
				identity.Scope.Account = "foreign"
				session = scopedSession{Session: session, identity: identity}
			case "cancel":
				cancel()
			case "memory":
				blocker, _ = f.store.reserveContentsMemory(maxStoreContentsMemory - f.stores.contents.used)
			case "unissued":
				sources = append(sources, hierarchyXID(f.store.replica, f.store.next+100))
			case "invalid":
				sources = append(sources, [22]byte{})
			case "other_folder":
				if err := f.store.deleteFAI(ctx, session, f.binding.fids[SentItems], faiDeleteRequest{sources: [][22]byte{unknown}}); err != nil {
					t.Fatal("other folder fixture")
				}
			}
			before := messageProof(f.store)
			used := f.stores.contents.used
			changes, err := f.store.deleteFAIChanges(ctx, session, draft.root, faiDeleteRequest{sources: sources})
			if mode == "success" {
				if err != nil || len(changes) != 1 || !f.store.fai.records[draft.id].deleted || f.store.fai.absent[unknown] != draft.root {
					t.Fatal("mixed existing/absent deletion failed")
				}
				return
			}
			if err == nil || !reflect.DeepEqual(before, messageProof(f.store)) || f.stores.contents.used != used || f.store.fai.records[draft.id].deleted {
				t.Fatal("failed mixed deletion changed namespace, registry or budget")
			}
			if mode != "other_folder" && len(f.store.fai.absent) != 0 {
				t.Fatal("failed batch retained an absent tombstone")
			}
			blocker.release()
		})
	}
}

func TestFAIAbsentDeletePreventsStagedSaveAndSharesQuota(t *testing.T) {
	f, _ := messageFixture(t)
	request := syntheticFAIImport()
	draft := newFAIDraftForTest(t, f, request)
	if err := f.store.deleteFAI(context.Background(), f.session, draft.root, faiDeleteRequest{sources: [][22]byte{request.source}}); err != nil {
		t.Fatal("delete staged identity")
	}
	if _, err := draft.save(context.Background(), f.session, 4); !errors.Is(err, core.ErrConflict) {
		t.Fatal("ForceSave resurrected an absent deleted identity")
	}
	draft.close()
	var sources [][22]byte
	for i := 1; i < maxFAIObjects; i++ {
		sources = append(sources, faiTestXID(55, byte(i)))
	}
	if err := f.store.deleteFAI(context.Background(), f.session, draft.root, faiDeleteRequest{sources: sources}); err != nil {
		t.Fatal("fill tombstone quota")
	}
	used, next := f.stores.contents.used, f.store.next
	if err := f.store.deleteFAI(context.Background(), f.session, draft.root, faiDeleteRequest{sources: [][22]byte{faiTestXID(56, 1)}}); !errors.Is(err, core.ErrUnsupported) || len(f.store.fai.absent) != maxFAIObjects || f.stores.contents.used != used || f.store.next != next {
		t.Fatal("absent tombstone quota not atomic")
	}
	other := request
	other.source = faiTestXID(57, 1)
	if d, err := f.store.newFAIDraft(context.Background(), f.session, draft.root, other); !errors.Is(err, core.ErrUnsupported) {
		if d != nil {
			d.close()
		}
		t.Fatal("live FAI did not share absent tombstone quota")
	}
}

func TestFAIAbsentDeleteCollectorStateAndReconnect(t *testing.T) {
	f, _, store := newFAIExecuteFixture(t)
	before, mappings := store.next, len(store.byMessage)
	source := faiTestXID(55, 10)
	faiRunOK(t, f, faiDeleteOperation(source))
	faiRunOK(t, f, []byte{0x82, 4, 2, 4})
	state := collectorThreeTestSets(t, drainFX(t, f.run, 4, 4096), idSet{})
	if len(state.seen.replicas)+len(state.seenFAI.replicas)+len(state.read.replicas) != 0 || store.next != before || len(store.byMessage) != mappings {
		t.Fatal("absent deletion invented checkpoint counters")
	}
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	f.open(int(Inbox))
	faiRunOK(t, f, []byte{0x7e, 4, 1, 2, 1})
	uploadContentsWireTest(t, f, state)
	faiRunOK(t, f, faiDeleteOperation(source))
	request := syntheticFAIImport()
	request.source = source
	out, c := f.run(faiImportOperation(request))
	if c.errors != 1 || !reflect.DeepEqual(out, ropError(0x72, 3, faiError(core.ErrConflict, false))) || store.next != before || len(store.fai.absent) != 1 {
		t.Fatal("reconnect lost absent deletion or allocated a draft")
	}
}

func TestFAIAbsentDeleteReservedOwnSourceAndNamespaceWidth(t *testing.T) {
	f, _ := messageFixture(t)
	first, code := f.store.reserveLocalIDs(context.Background(), 1)
	if code != 0 {
		t.Fatal("reserve client source")
	}
	source := hierarchyXID(f.store.replica, 1)
	copy(source[16:], first[:])
	root := f.binding.fids[Inbox]
	before := f.store.next
	if err := f.store.deleteFAI(context.Background(), f.session, root, faiDeleteRequest{sources: [][22]byte{source}}); err != nil || f.store.next != before {
		t.Fatal("unused issued GID deletion allocated a new counter")
	}
	foreign := variableFAIImport(8)
	draft := newFAIDraftForTest(t, f, foreign)
	saveFAIDraftForTest(t, f, draft, 0)
	draft.close()
	wrongWidth := faiTestXID(99, 10)
	ns := foreign.change.namespace()
	copy(wrongWidth[:16], ns[:])
	used, next := f.stores.contents.used, f.store.next
	if err := f.store.deleteFAI(context.Background(), f.session, root, faiDeleteRequest{sources: [][22]byte{wrongWidth}}); !errors.Is(err, core.ErrInvalid) || len(f.store.fai.absent) != 1 || f.stores.contents.used != used || f.store.next != next {
		t.Fatal("absent SourceKey changed an established LocalId width")
	}
}
