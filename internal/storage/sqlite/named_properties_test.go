// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"errors"
	"fmt"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"

	"ewsgo/internal/core"
)

const testPropertySet = "00062008-0000-0000-c000-000000000046"

func TestNamedPropertyIDsSurviveReopenAndIsolateScopes(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "names.sqlite")
	store, mailbox := openJournalFixture(t, path)
	upper := core.Key{Set: internetHeadersSet, Name: "X-EWSGo-Test"}
	lower := core.Key{Set: internetHeadersSet, Name: "x-ewsgo-test"}
	zero := core.Key{Set: testPropertySet}
	empty := core.Key{Set: testPropertySet, EmptyName: true}
	standard := core.Key{Set: standardMAPISet, ID: 0x3001}
	names := []core.Key{upper, lower, zero, empty, standard}
	ids, err := store.NamedPropertyIDs(ctx, mailbox, names, false)
	if err != nil || !slices.Equal(ids, []uint16{0, 0, 0, 0, 0x3001}) {
		t.Fatalf("lookup before allocation: %x, %v", ids, err)
	}
	assigned := []uint16{0x8001, 0x8001, 0x8002, 0x8003, 0x3001}
	ids, err = store.NamedPropertyIDs(ctx, mailbox, names, true)
	if err != nil || !slices.Equal(ids, assigned) {
		t.Fatalf("allocation: %x, %v", ids, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, reopened := openJournalFixture(t, path)
	ids, err = store.NamedPropertyIDs(ctx, reopened, names, false)
	if err != nil || mailbox != reopened || !slices.Equal(ids, assigned) {
		t.Fatalf("lookup after reopening: %x, %v", ids, err)
	}
	reverse, err := store.NamedPropertyKeys(ctx, mailbox, append(ids, 0, 0xfffe))
	want := map[uint16]core.Key{0x8001: lower, 0x8002: zero, 0x8003: empty, 0x3001: standard}
	if err != nil || !reflect.DeepEqual(reverse, want) {
		t.Fatalf("inverse: %+v, %v", reverse, err)
	}
	ids, err = store.NamedPropertyIDs(ctx, mailbox, nil, true)
	if err != nil || !slices.Equal(ids, []uint16{0x8001, 0x8002, 0x8003}) {
		t.Fatalf("enumeration: %x, %v", ids, err)
	}
	for _, scope := range []struct{ account, instance string }{
		{"another-account", "one"}, {"journal-test", "another-instance"},
	} {
		other, err := store.BindMailbox(ctx, core.Scope{Backend: "fixture", Account: scope.account}, scope.instance)
		if err != nil {
			t.Fatal(err)
		}
		got, err := store.NamedPropertyKeys(ctx, other.MailboxID, []uint16{0x8001})
		if err != nil || len(got) != 0 {
			t.Fatalf("named property escaped mailbox scope: %+v, %v", got, err)
		}
		ids, err := store.NamedPropertyIDs(ctx, other.MailboxID, []core.Key{empty}, true)
		if err != nil || !slices.Equal(ids, []uint16{0x8001}) {
			t.Fatalf("independent allocation: %x, %v", ids, err)
		}
	}
	if _, err := store.NamedPropertyIDs(ctx, "unknown", []core.Key{upper}, true); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("allocation for unbound mailbox: %v", err)
	}
}

func TestConcurrentNamedPropertyAllocations(t *testing.T) {
	path := filepath.Join(t.TempDir(), "concurrent.sqlite")
	first, mailbox := openJournalFixture(t, path)
	second, _ := openJournalFixture(t, path)
	stores := []*Store{first, second}
	const workers = 12
	type outcome struct {
		ids []uint16
		err error
	}
	var results [workers]outcome
	var wg sync.WaitGroup
	shared := core.Key{Set: testPropertySet, Name: "shared"}
	for i := range results {
		wg.Go(func() {
			unique := core.Key{Set: testPropertySet, Name: fmt.Sprintf("unique-%d", i)}
			results[i].ids, results[i].err = stores[i%len(stores)].NamedPropertyIDs(t.Context(), mailbox, []core.Key{shared, unique}, true)
		})
	}
	wg.Wait()
	assigned := map[uint16]bool{0x8001: true}
	for _, result := range results {
		if result.err != nil || len(result.ids) != 2 || result.ids[0] != 0x8001 || assigned[result.ids[1]] {
			t.Fatalf("concurrent allocation: %x, %v", result.ids, result.err)
		}
		assigned[result.ids[1]] = true
	}
}

func TestNamedPropertyQuotaRollsBackWholeBatch(t *testing.T) {
	ctx := t.Context()
	store, mailbox := openJournalFixture(t, filepath.Join(t.TempDir(), "quota.sqlite"))
	if _, err := store.db.ExecContext(ctx, `
		INSERT INTO named_property_ids VALUES (?, ?, 0, 'fixture', 0, 65533)
	`, mailbox, testPropertySet); err != nil {
		t.Fatal(err)
	}
	names := []core.Key{{Set: testPropertySet, Name: "one"}, {Set: testPropertySet, Name: "two"}}
	if _, err := store.RegisterNamedProperties(ctx, mailbox, names[:1], 0x9000); !errors.Is(err, ErrNamedPropertyQuota) {
		t.Fatalf("frontend ID quota: %v", err)
	}
	if _, err := store.NamedPropertyIDs(ctx, mailbox, names, true); !errors.Is(err, ErrNamedPropertyQuota) {
		t.Fatalf("full ID space: %v", err)
	}
	ids, err := store.NamedPropertyIDs(ctx, mailbox, names, false)
	if err != nil || !slices.Equal(ids, []uint16{0, 0}) {
		t.Fatalf("assignment survived failed batch: %x, %v", ids, err)
	}
	ids, err = store.NamedPropertyIDs(ctx, mailbox, names[:1], true)
	if err != nil || !slices.Equal(ids, []uint16{0xfffe}) {
		t.Fatalf("last assignment: %x, %v", ids, err)
	}
	if _, err := store.LoadNamedPropertyBindings(ctx, mailbox, 1); !errors.Is(err, ErrNamedPropertyQuota) {
		t.Fatalf("bounded load: %v", err)
	}
	loaded, err := store.LoadNamedPropertyBindings(ctx, mailbox, 2)
	if err != nil || len(loaded) != 2 || loaded[0xfffe] != names[0] {
		t.Fatalf("reload assigned names: %+v, %v", loaded, err)
	}
}
