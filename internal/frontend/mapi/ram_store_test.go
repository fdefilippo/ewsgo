// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"

	"ewsgo/internal/core"
)

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
