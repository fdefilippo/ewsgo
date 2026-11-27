// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/storage/sqlite"
)

func TestRAMStoreBindsOtherMailboxWhilePersistentLoadIsPending(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	db, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "stores.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, _, session := ramFixture(t)
	catalog, err := privateFolders(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	slow, fast := catalog, catalog
	slow.scope.Account = "slow"
	fast.scope.Account = "fast"
	entered, unblock := make(chan struct{}), make(chan struct{})
	var loads atomic.Int32
	var resume sync.Once
	stopWaiting := func() { resume.Do(func() { close(unblock) }) }
	defer stopWaiting()
	stores := newRAMStores()
	stores.replicaStorage = func(ctx context.Context, scope core.Scope) (boundReplica, error) {
		if scope == slow.scope {
			if loads.Add(1) == 1 {
				close(entered)
			}
			select {
			case <-ctx.Done():
				return boundReplica{}, ctx.Err()
			case <-unblock:
			}
		}
		account, err := db.BindMailbox(ctx, scope, "test")
		return boundReplica{store: db, mailbox: account.MailboxID}, err
	}
	type result struct {
		binding storeBinding
		err     error
	}
	bind := func(ctx context.Context, catalog privateFolderCatalog) <-chan result {
		resultCh := make(chan result, 1)
		go func() {
			binding, err := stores.bind(ctx, catalog)
			binding.releasePin()
			resultCh <- result{binding, err}
		}()
		return resultCh
	}
	await := func(outcome <-chan result) result {
		t.Helper()
		select {
		case got := <-outcome:
			return got
		case <-ctx.Done():
			t.Fatal("logon did not finish")
			return result{}
		}
	}
	first := bind(ctx, slow)
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("durable load did not start")
	}
	second := bind(ctx, slow)
	waitCtx, cancelWait := context.WithTimeout(ctx, 20*time.Millisecond)
	defer cancelWait()
	if got := await(bind(waitCtx, slow)); !errors.Is(got.err, context.DeadlineExceeded) {
		t.Fatalf("waiting logon returned %v", got.err)
	}
	independent := await(bind(ctx, fast))
	if independent.err != nil {
		t.Fatalf("independent mailbox could not log on: %v", independent.err)
	}
	stopWaiting()
	a, b := await(first), await(second)
	if a.err != nil || b.err != nil || loads.Load() != 1 || a.binding.store != b.binding.store ||
		a.binding.fids != b.binding.fids || a.binding.store.replica == independent.binding.store.replica {
		t.Fatalf("logons did not share exactly one load per mailbox: %d, %v, %v", loads.Load(), a.err, b.err)
	}
}

func TestPersistentRAMStoresEvictOnlyAfterLastUser(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "stores.sqlite")
	open := func() (*ramStores, *sqlite.Store) {
		t.Helper()
		db, err := sqlite.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		stores := newRAMStores()
		stores.replicaStorage = func(ctx context.Context, scope core.Scope) (boundReplica, error) {
			box, err := db.BindMailbox(ctx, scope, "test")
			return boundReplica{store: db, mailbox: box.MailboxID}, err
		}
		return stores, db
	}
	_, _, session := ramFixture(t)
	catalog, err := privateFolders(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	bind := func(stores *ramStores, account int) storeBinding {
		t.Helper()
		catalog.scope.Account = fmt.Sprintf("account-%d", account)
		binding, err := stores.bind(ctx, catalog)
		if err != nil {
			t.Fatalf("bind %d: %v", account, err)
		}
		return binding
	}
	stores, db := open()
	active := bind(stores, 0)
	objects := newMailboxObjects(stores)
	handle, err := objects.addLogon(0, active)
	if err != nil {
		t.Fatal(err)
	}
	scanned := bind(stores, 1)
	unpinScan := scanned.store.pinForScan()
	scanned.releasePin()
	old := bind(stores, 2)
	member := core.Scope{Backend: catalog.scope.Backend, Account: "recipient"}
	memberIDs, err := old.store.permissionMemberIDs(ctx, []core.Scope{member})
	if err != nil || memberIDs[member] <= maxPermissionMembers {
		t.Fatalf("persist ACL member outside the legacy RAM range: %v", err)
	}
	key := folderKey{native: "assigned-before-eviction"}
	ids, err := old.store.assign(ctx, []folderKey{key})
	if err != nil {
		t.Fatal(err)
	}
	old.store.permissionMemberLease, err = old.store.reserveContentsMemory(4096)
	if err != nil {
		t.Fatal(err)
	}
	old.releasePin()
	for i := 3; i < maxRAMStores+7; i++ {
		bind(stores, i).releasePin()
	}
	if len(stores.entries) != maxRAMStores || stores.entries[active.store.scope] != active.store ||
		stores.entries[scanned.store.scope] != scanned.store || stores.entries[old.store.scope] != nil {
		t.Fatal("eviction removed a live user or retained the oldest idle mailbox")
	}
	if _, code := objects.logonObject(handle, 0); code != 0 || stores.contents.used != 0 {
		t.Fatal("eviction broke the current logon or leaked the removed store's memory")
	}
	check := func(stores *ramStores) {
		t.Helper()
		reopened := bind(stores, 2)
		defer reopened.releasePin()
		if reopened.store == old.store || reopened.fids != old.fids ||
			reopened.store.replica != old.store.replica || reopened.store.byKey[key] != ids[0] {
			t.Fatal("reopening changed durable identifiers")
		}
		target, err := reopened.store.permissionMemberScope(ctx, memberIDs[member])
		if err != nil || target != member {
			t.Fatalf("ACL member changed after reopening: %v", err)
		}
		other := core.Scope{Backend: member.Backend, Account: "different-recipient"}
		ids, err := reopened.store.permissionMemberIDs(ctx, []core.Scope{other, member})
		if err != nil || ids[member] != memberIDs[member] || ids[other] == ids[member] {
			t.Fatalf("ACL member reallocated: %v", err)
		}
	}
	check(stores)
	objects.close()
	unpinScan()
	if active.store.pins != 0 || scanned.store.pins != 0 {
		t.Fatal("disconnect or notification scan leaked a store pin")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	restarted, _ := open()
	check(restarted)
}

func ramFixture(t *testing.T) (*ramStores, storeBinding, *testSession) {
	t.Helper()
	session := &testSession{catalogFixture()}
	catalog, err := privateFolders(context.Background(), session)
	if err != nil {
		t.Fatal(err)
	}
	stores := newRAMStores()
	binding, err := stores.bind(context.Background(), catalog)
	if err != nil {
		t.Fatal(err)
	}
	return stores, binding, session
}

func TestRAMStoreReconnectIdentityScopeAndRestart(t *testing.T) {
	stores, first, session := ramFixture(t)
	catalog, _ := privateFolders(context.Background(), session)
	second, err := stores.bind(context.Background(), catalog)
	if err != nil || first.store != second.store || first.fids != second.fids || first.store.replica == [16]byte{} {
		t.Fatal("reconnect lost RAM identity", err)
	}
	seen := map[uint64]bool{}
	for _, fid := range first.fids {
		if fid == 0 || fid&0xffff != 1 || seen[fid] {
			t.Fatal("invalid or duplicate FID")
		}
		seen[fid] = true
	}
	for _, scope := range []core.Scope{{Backend: "synthetic", Account: "another"}, {Backend: "other", Account: "one"}} {
		catalog.scope = scope
		other, err := stores.bind(context.Background(), catalog)
		if err != nil || other.store == first.store || other.store.replica == first.store.replica {
			t.Fatal("account/backend mapping shared", err)
		}
	}
	catalog.scope = session.Identity().Scope
	restarted, err := newRAMStores().bind(context.Background(), catalog)
	if err != nil || restarted.store.replica == first.store.replica {
		t.Fatal("restart retained a RAM replica", err)
	}
	// Numeric FIDs may repeat after restart, but the replica changes. Durable
	// identities and client cache continuity are deliberately not qualified.
	if restarted.fids != first.fids {
		t.Fatal("unexpected independent allocator order")
	}
	catalog.folders[IPMSubtree].NativeID = "different-root"
	if _, err := stores.bind(context.Background(), catalog); err != core.ErrConflict {
		t.Fatal("native root silently replaced within replica")
	}
}

func TestRAMStoreFoldersCurrentAccessAndVirtualParents(t *testing.T) {
	_, binding, session := ramFixture(t)
	ctx := context.Background()
	for role, fid := range binding.fids {
		folder, err := binding.store.folder(ctx, session, fid)
		if err != nil || folder.fid != fid || folder.properties[core.DisplayName] == nil && SpecialFolderRole(role) != IPMSubtree {
			t.Fatal("folder is not an accessible object", role, err)
		}
		parent := binding.fids[MailboxRoot]
		if role == int(MailboxRoot) {
			parent = 0
		} else if role == int(Outbox) || role == int(Inbox) || role == int(SentItems) || role == int(DeletedItems) {
			parent = binding.fids[IPMSubtree]
		}
		if folder.parent != parent {
			t.Fatal("virtual/native hierarchy", role)
		}
	}
	box := session.box.(*catalogMailbox)
	box.getErr = core.ErrAccessDenied
	for _, role := range []SpecialFolderRole{MailboxRoot, Inbox, Outbox} {
		if _, err := binding.store.folder(ctx, session, binding.fids[role]); !errors.Is(err, core.ErrAccessDenied) {
			t.Fatal("cached mapping bypassed current access", role)
		}
	}
	box.getErr = nil
	foreign := scopedSession{Session: session, identity: core.Identity{Scope: core.Scope{Backend: "synthetic", Account: "foreign"}}}
	if _, err := binding.store.folder(ctx, foreign, binding.fids[Inbox]); err != core.ErrAccessDenied {
		t.Fatal("foreign session accepted")
	}
	if _, err := binding.store.folder(ctx, session, 0); err != core.ErrNotFound {
		t.Fatal("unknown FID accepted")
	}
	box.page.Objects[1].Properties[core.DisplayName] = core.Text("renamed")
	folder, err := binding.store.folder(ctx, session, binding.fids[Inbox])
	if err != nil || folder.properties[core.DisplayName] != core.Text("renamed") {
		t.Fatal("rename not read natively")
	}
	folder.properties[core.DisplayName] = core.Text("changed local clone")
	if box.page.Objects[1].Properties[core.DisplayName] != core.Text("renamed") {
		t.Fatal("returned mutable backend properties")
	}
	box.page.Objects = append(box.page.Objects[:1], box.page.Objects[2:]...)
	if _, err := binding.store.folder(ctx, session, binding.fids[Inbox]); err != core.ErrNotFound {
		t.Fatal("deleted native folder remained accessible")
	}
}

func TestRAMStoreQuotaAtomicityNoReuseAndConcurrentReconnect(t *testing.T) {
	stores, binding, session := ramFixture(t)
	var wg sync.WaitGroup
	catalog, _ := privateFolders(context.Background(), session)
	for range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			got, err := stores.bind(context.Background(), catalog)
			if err != nil || got.fids != binding.fids || got.store != binding.store {
				t.Error("concurrent reconnect changed identity")
			}
		}()
	}
	wg.Wait()
	remaining := maxRAMMappings - len(binding.store.byKey)
	keys := make([]folderKey, remaining)
	for i := range keys {
		keys[i] = folderKey{native: core.ID(fmt.Sprintf("opaque-%d", i))}
	}
	ids, err := binding.store.assign(t.Context(), keys)
	if err != nil || len(ids) != remaining || ids[0] == binding.fids[Inbox] {
		t.Fatal("mapping allocation", err)
	}
	before := binding.store.next
	if _, err := binding.store.assign(t.Context(), []folderKey{keys[0], {native: "excess"}}); err != core.ErrUnsupported || binding.store.next != before || len(binding.store.byKey) != maxRAMMappings {
		t.Fatal("quota partially allocated or reused IDs")
	}
	if old, err := binding.store.assign(t.Context(), []folderKey{keys[0]}); err != nil || old[0] != ids[0] {
		t.Fatal("existing mapping lost at quota")
	}
	for len(stores.entries) < maxRAMStores {
		catalog.scope.Account = fmt.Sprint(len(stores.entries))
		if _, err := stores.bind(context.Background(), catalog); err != nil {
			t.Fatal(err)
		}
	}
	catalog.scope.Account = "overflow"
	if _, err := stores.bind(context.Background(), catalog); err != core.ErrUnsupported || len(stores.entries) != maxRAMStores {
		t.Fatal("store quota bypassed")
	}
	stores.nextHandle.Store(0xfffffffe)
	if _, err := stores.handle(); err != core.ErrUnsupported {
		t.Fatal("reserved handle sentinel issued")
	}
}
