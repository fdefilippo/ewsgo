// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"slices"
	"testing"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
	"ewsgo/internal/storage/sqlite"
)

func TestSQLiteHierarchyReopenAndRetry(t *testing.T) {
	f := newHierarchyFixture(t)
	ctx := t.Context()
	reopen := sqliteReplicaFixture(t, f)
	db := reopen()
	first := f.snapshot(t, MailboxRoot, hierarchyTestTime)
	firstFIDs := f.binding.fids
	child := f.store.byKey[folderKey{native: "child:opaque"}]
	parent := f.store.byKey[folderKey{native: "parent/opaque"}]
	state := hierarchyState{store: f.store, root: first.root}
	seed, err := state.planHierarchy(ctx, f.session, first)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = reopen()
	unchanged := f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(time.Minute))
	if f.binding.fids != firstFIDs || !reflect.DeepEqual(first, unchanged) {
		t.Fatal("reopen changed the hierarchy, keys, date or revision")
	}
	mailbox := f.store.durable.mailbox
	version := mapireplica.Version{Epoch: first.replica, HighWater: first.highWater}
	if err := db.CommitMAPIReplica(ctx, mailbox, version, mapireplica.Mutation{HighWater: first.highWater + 1}); err != nil {
		t.Fatal(err)
	}

	objects := f.box.page.Objects
	objects[len(objects)-1].Parent = f.store.inbox
	f.box.page.Objects = slices.Delete(objects, len(objects)-2, len(objects)-1)
	if _, err := f.store.observeHierarchy(ctx, f.session, first.root, hierarchyTestTime.Add(time.Hour)); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("stale process published a hierarchy: %v", err)
	}
	if f.store.next != first.highWater || !f.store.hierarchy[folderKey{native: "parent/opaque"}].present {
		t.Fatal("failed commit published partial RAM state")
	}

	reopen()
	second := f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(time.Hour))
	resumed := hierarchyState{store: f.store, root: second.root, given: seed.finalGiven, seen: seed.finalSeen}
	delta, err := resumed.planHierarchy(ctx, f.session, second)
	if err != nil || !slices.Equal(delta.removed, []uint64{parent}) || len(delta.changes) != 1 {
		t.Fatalf("wrong resumed delta: changed=%d removed=%d err=%v", len(delta.changes), len(delta.removed), err)
	}
	move := delta.changes[0]
	if move.fid != child || move.parent != f.binding.fids[Inbox] || move.cn <= version.HighWater+1 {
		t.Fatal("move lost stable FID, new parent or change number")
	}
	reopen()
	if got := f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(2*time.Hour)); !reflect.DeepEqual(second, got) {
		t.Fatal("reopen after commit changed the removal or the move")
	}
}

func sqliteReplicaFixture(t *testing.T, f *hierarchyFixture) func() *sqlite.Store {
	t.Helper()
	ctx := t.Context()
	catalog, err := privateFolders(ctx, f.session)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "replica.sqlite")
	return func() *sqlite.Store {
		t.Helper()
		db, err := sqlite.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		stores := newRAMStores()
		stores.privateConfig = f.stores.privateConfig
		stores.replicaStorage = func(ctx context.Context, scope core.Scope) (boundReplica, error) {
			box, err := db.BindMailbox(ctx, scope, "synthetic")
			return boundReplica{store: db, mailbox: box.MailboxID}, err
		}
		binding, err := stores.bind(ctx, catalog)
		if err != nil {
			t.Fatal(err)
		}
		f.store, f.stores, f.binding = binding.store, stores, binding
		return db
	}
}

func TestSQLiteLocalFolderValuesReopenAndDelete(t *testing.T) {
	f, box := messageFixture(t)
	ctx := t.Context()
	reopen := sqliteReplicaFixture(t, f)
	reopen()
	root, inbox := f.binding.fids[MailboxRoot], f.binding.fids[Inbox]
	first := &box.objects[0]
	first.Properties[core.IsRead] = core.Bool(true)
	first.MIME = []byte("From: synthetic@example.invalid\r\nSubject: Original\r\n\r\nBody")

	fid, code := f.store.createFolder(ctx, f.session, root, createFolderRequest{kind: searchFolderType, name: "Static search"})
	if code != 0 {
		t.Fatalf("create search: %#x", code)
	}
	filter := append32(append32([]byte{6, 1}, 0x0e070003), 1)
	if code := f.store.setSearchCriteria(ctx, f.session, f.binding, fid, searchCriteriaOperation{
		flags: searchRestart | searchStatic, raw: filter, folders: []uint64{inbox},
	}, 1252, &storeResults{}); code != 0 {
		t.Fatalf("set criteria: %#x", code)
	}
	before, code := f.store.readSearchCriteria(ctx, f.session, fid)
	if code != 0 || before == nil || before.static == nil || len(before.static.links[inbox]) != 1 {
		t.Fatalf("first search: %#x", code)
	}
	if _, code := f.store.updateMetadata(ctx, f.session, fid, folderMetadata{folderHiddenTag >> 16: {1}}, false); code != 0 {
		t.Fatalf("hide folder: %#x", code)
	}
	if code := f.store.setOSTValue(ctx, fid, nil); code != 0 {
		t.Fatalf("store empty OST value: %#x", code)
	}
	first.Properties[core.IsRead] = core.Bool(false)
	reopen()
	criteria, code := f.store.readSearchCriteria(ctx, f.session, fid)
	if code != 0 || criteria == nil || criteria.static == nil || !reflect.DeepEqual(criteria.static.links, before.static.links) || !slices.Equal(criteria.raw, filter) || !slices.Equal(criteria.folders, []uint64{inbox}) {
		t.Fatalf("search scope or frozen membership changed: %#x", code)
	}
	if got, present := f.store.ostValue(fid); !present || len(got) != 0 {
		t.Fatal("empty OST value lost")
	}
	if got := f.store.metadata[fid][folderHiddenTag>>16]; !slices.Equal(got, []byte{1}) {
		t.Fatal("folder metadata lost")
	}
	if partial, code := f.store.deleteRAMFolder(ctx, f.session, root, fid, deleteFolderHard); partial || code != 0 {
		t.Fatalf("delete search: partial=%v code=%#x", partial, code)
	}
	reopen()
	if _, exists := f.store.dynamic[folderGlobalCount(fid)]; exists || f.store.metadata[fid] != nil {
		t.Fatal("deleted folder or metadata reappeared")
	}
	if _, present := f.store.ostValue(fid); present {
		t.Fatal("deleted OST value reappeared")
	}
	if _, exists := f.store.byFID[fid]; !exists {
		t.Fatal("deleted FID lost its claim on the counter")
	}
}
