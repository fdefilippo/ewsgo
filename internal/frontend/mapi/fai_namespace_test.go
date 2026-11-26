// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestNamedPropertyZeroLIDAndEmptyNameRemainDistinct(t *testing.T) {
	set := [16]byte{1}
	numeric := propertyName{Set: set, LID: 0}
	named := propertyName{Set: set, Kind: 1}
	keys := make([]core.Key, 0, 2)
	for _, name := range []propertyName{numeric, named} {
		key := faiCoreProperty(contentsProperty{name: name})
		roundTrip, err := faiNamedProperty(key)
		if err != nil || roundTrip != name {
			t.Fatalf("property round trip: %+v, %v", roundTrip, err)
		}
		keys = append(keys, key)
	}
	first, _, err := messageFingerprint(core.Object{Properties: core.Properties{keys[0]: core.Text("value")}})
	if err != nil {
		t.Fatal(err)
	}
	second, _, err := messageFingerprint(core.Object{Properties: core.Properties{keys[1]: core.Text("value")}})
	if err != nil || first == second {
		t.Fatalf("fingerprints must distinguish numeric zero from empty name: %v", err)
	}
}

// Only fixtures populate the namespace in this slice. No application/ROP
// writer can invoke this helper; the collector task owns staging and admission.
func seedFAI(t *testing.T, s *ramStore, records ...faiRecord) {
	t.Helper()
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	mapRecords := map[core.ID]faiRecord{}
	total := 0
	for _, r := range records {
		var err error
		r.bytes, err = s.validateFAIRecord(r)
		if err != nil {
			t.Fatal("invalid synthetic FAI", err)
		}
		r.object, err = cloneMessageObject(r.object)
		if err != nil {
			t.Fatal("clone FAI")
		}
		mapRecords[r.object.ID] = r
		total += r.bytes
	}
	lease, err := s.reserveContentsMemory(total)
	if err != nil {
		t.Fatal("fixture storage budget")
	}
	old := s.fai.lease
	s.fai = faiNamespace{records: mapRecords, bytes: total, lease: lease}
	old.release()
}
func faiTestRecord(s *ramStore, fid uint64, n int) faiRecord {
	return faiRecord{folder: fid, object: core.Object{ID: core.ID(fmt.Sprintf("fai:%032x", n)), Parent: s.byFID[fid].native, Kind: core.Message, Revision: 1, Properties: core.Properties{
		core.Key{ID: 0x1a}:   core.Text("IPM.Microsoft.FolderDesign.NamedView"),
		core.Key{ID: 0x67aa}: core.Bool(true), core.IsRead: core.Bool(false),
		core.Key{ID: 0x3007}: core.Timestamp(hierarchyTestTime), core.LastModifiedAt: core.Timestamp(hierarchyTestTime),
		core.Key{ID: 0x7007}: core.Integer(8), core.Key{ID: 0x7006}: core.Text("Synthetic view"),
		core.Key{ID: 0x7001}: core.Binary{1, 2, 3},
	}}}
}
func TestFAINamespaceInitiallyEmptyIsolatedAndReconnect(t *testing.T) {
	f, box := messageFixture(t)
	for _, role := range []SpecialFolderRole{Inbox, Outbox, SentItems} {
		o, err := f.store.observeFAIMessages(context.Background(), f.session, f.binding.fids[role])
		if err != nil || o == nil || len(o.snapshot.messages) != 0 || !o.snapshot.associated || o.lease != nil {
			t.Fatal("new local namespace not empty")
		}
		o.close()
	}
	if box.sourceCalls != 0 {
		t.Fatal("FAI read invoked native mail reader")
	}
	inbox, sent := f.binding.fids[Inbox], f.binding.fids[SentItems]
	seedFAI(t, f.store, faiTestRecord(f.store, inbox, 1), faiTestRecord(f.store, sent, 2))
	first, err := f.store.observeFAIMessages(context.Background(), f.session, inbox)
	if err != nil || len(first.snapshot.messages) != 1 || first.snapshot.messages[0].object.ID != "fai:00000000000000000000000000000001" {
		t.Fatal("folder namespace isolation")
	}
	defer first.close()
	if first.lease.size() == 0 || f.stores.contents.used != f.store.fai.bytes+first.lease.size() {
		t.Fatal("result buffers not separately charged")
	}
	catalog, _ := privateFolders(context.Background(), f.session)
	rebound, err := f.stores.bind(context.Background(), catalog)
	if err != nil || rebound.store != f.store {
		t.Fatal("reconnect binding")
	}
	second, err := rebound.store.observeFAIMessages(context.Background(), f.session, inbox)
	if err != nil || !reflect.DeepEqual(first.snapshot, second.snapshot) {
		t.Fatal("reconnect changed FAI view")
	}
	second.close()
	second.close()
	if f.stores.contents.used != f.store.fai.bytes+first.lease.size() {
		t.Fatal("lease not released exactly once")
	}
	first.snapshot.messages[0].object.Properties[core.Key{ID: 0x7001}].(core.Binary)[0] = 99
	third, err := f.store.observeFAIMessages(context.Background(), f.session, inbox)
	if err != nil || third.snapshot.messages[0].object.Properties[core.Key{ID: 0x7001}].(core.Binary)[0] != 1 {
		t.Fatal("caller mutated retained FAI")
	}
	third.close()
	other := f.session.Identity()
	other.Scope.Account = "another-account"
	foreign := scopedSession{Session: f.session, identity: other}
	if o, err := f.store.observeFAIMessages(context.Background(), foreign, inbox); !errors.Is(err, core.ErrAccessDenied) || o != nil {
		t.Fatal("foreign scope accessed namespace")
	}
	catalog.scope = other.Scope
	newAccount, err := f.stores.bind(context.Background(), catalog)
	if err != nil || len(newAccount.store.fai.records) != 0 {
		t.Fatal("account namespace reused")
	}
	catalog.scope = f.session.Identity().Scope
	restarted, err := newRAMStores().bind(context.Background(), catalog)
	if err != nil || len(restarted.store.fai.records) != 0 || restarted.store.replica == f.store.replica {
		t.Fatal("restart retained FAI")
	}
}

func TestFAINamespaceCalendarLocalOnlyAndNamedProperties(t *testing.T) {
	f, _ := newCalendarFixture(t)
	calendarEntry(t, f)
	object, code := f.state.mailbox.object(f.handles[0], 4)
	if code != 0 {
		t.Fatal("binding")
	}
	s := object.binding.store
	fid, err := s.specialFolderFID(context.Background(), f.session, calendarMetadataRole)
	if err != nil {
		t.Fatal("Calendar metadata")
	}
	r := faiTestRecord(s, fid, 1)
	r.object.Properties[core.Key{ID: 0x1a}] = core.Text("IPM.Configuration.WorkHours")
	key := core.Key{Set: "00112233-4455-6677-8899-aabbccddeeff", Name: "PRIVATE-synthetic"}
	name, err := faiNamedProperty(key)
	if err != nil || name.Set != ([16]byte{0x33, 0x22, 0x11, 0, 0x55, 0x44, 0x77, 0x66, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff}) {
		t.Fatal("named GUID byte order")
	}
	s.named.ids(context.Background(), []propertyName{name}, true)
	r.object.Properties[key] = core.Binary{4, 5, 6}
	seedFAI(t, s, r)
	o, err := s.observeFAIMessages(context.Background(), f.session, fid)
	if err != nil || len(o.snapshot.messages) != 1 || !reflect.DeepEqual(o.snapshot.messages[0].object.Properties[key], core.Binary{4, 5, 6}) {
		t.Fatal("Calendar FAI/named data")
	}
	defer o.close()
	if _, err := s.observeMailMessages(context.Background(), f.session, fid); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal("Calendar event reads enabled")
	}
	for _, v := range []any{s.fai, r, o, o.lease} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			out := fmt.Sprintf(format, v)
			if strings.Contains(out, "PRIVATE") || strings.Contains(out, "fai:") || !strings.Contains(out, "private") {
				t.Fatal("FAI diagnostics exposed contents")
			}
		}
	}
}

func TestFAINamespaceFailuresPreserveRegistryAndBudget(t *testing.T) {
	for _, mode := range []string{"revoked", "cancelled", "unknown_folder", "wrong_root", "invalid_class", "unknown_property", "bad_type", "bad_date", "normal", "mime", "unregistered_name", "ambiguous_named_integer", "over_object_bytes", "over_count", "over_store_bytes", "bad_accounting", "unreserved_storage", "copy_budget"} {
		t.Run(mode, func(t *testing.T) {
			f := newHierarchyFixture(t)
			fid := f.binding.fids[Inbox]
			r := faiTestRecord(f.store, fid, 1)
			seedFAI(t, f.store, r)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "revoked":
				f.box.state.Owner = false
			case "cancelled":
				cancel()
			case "unknown_folder":
				fid = folderID(9999)
			case "wrong_root":
				identity := f.session.Identity()
				identity.Scope.Backend = "foreign"
				f.session = scopedSession{Session: f.session, identity: identity}
			case "invalid_class":
				r.object.Properties[core.Key{ID: 0x1a}] = core.Text("IPM.Invalid.")
			case "unknown_property":
				r.object.Properties[core.Key{ID: 0x1234}] = core.Text("unsupported")
			case "bad_type":
				r.object.Properties[core.Key{ID: 0x7007}] = core.Text("8")
			case "bad_date":
				r.object.Properties[core.LastModifiedAt] = core.Timestamp(time.Time{})
			case "normal":
				r.object.Properties[core.Key{ID: 0x67aa}] = core.Bool(false)
			case "mime":
				r.object.MIME = []byte("not a FAI stream")
			case "unregistered_name":
				r.object.Properties[core.Key{Set: "00112233-4455-6677-8899-aabbccddeeff", Name: "unknown"}] = core.Binary{1}
			case "ambiguous_named_integer":
				key := core.Key{Set: "00112233-4455-6677-8899-aabbccddeeff", ID: 1}
				name, _ := faiNamedProperty(key)
				f.store.named.ids(context.Background(), []propertyName{name}, true)
				r.object.Properties[key] = core.Integer(1)
			case "over_object_bytes":
				r.object.Properties[core.Key{ID: 0x7001}] = core.Binary(make([]byte, 128<<10))
			case "over_count":
				for n := 2; n <= 65; n++ {
					r := faiTestRecord(f.store, fid, n)
					f.store.fai.records[r.object.ID] = r
				}
			case "over_store_bytes":
				f.store.fai.records = map[core.ID]faiRecord{}
				for n := 1; n <= 40; n++ {
					r := faiTestRecord(f.store, fid, n)
					r.object.Properties[core.Key{ID: 0x7001}] = core.Binary(make([]byte, 120<<10))
					r.bytes, _ = f.store.validateFAIRecord(r)
					f.store.fai.records[r.object.ID] = r
				}
			case "bad_accounting":
				f.store.fai.bytes++
			case "unreserved_storage":
				f.store.fai.lease.release()
			case "copy_budget":
				lease, err := f.store.reserveContentsMemory(maxStoreContentsMemory - f.store.fai.bytes)
				if err != nil {
					t.Fatal("fixture reservation")
				}
				defer lease.release()
			}
			if mode == "invalid_class" || mode == "unknown_property" || mode == "bad_type" || mode == "bad_date" || mode == "normal" || mode == "mime" || mode == "unregistered_name" || mode == "ambiguous_named_integer" || mode == "over_object_bytes" {
				r.bytes = f.store.fai.records[r.object.ID].bytes
				f.store.fai.records[r.object.ID] = r
			}
			proof := messageProof(f.store)
			used := f.stores.contents.used
			if o, err := f.store.observeFAIMessages(ctx, f.session, fid); err == nil || o != nil {
				t.Fatal("invalid FAI read accepted")
			}
			if !reflect.DeepEqual(proof, messageProof(f.store)) || used != f.stores.contents.used {
				t.Fatal("failed FAI read changed registry or leaked reservation")
			}
		})
	}
}

func TestContentsMemorySharedLimitsAndConcurrentRelease(t *testing.T) {
	stores := newRAMStores()
	a := &ramStore{allocator: stores}
	b := &ramStore{allocator: stores}
	c := &ramStore{allocator: stores}
	x, err := a.reserveContentsMemory(maxStoreContentsMemory)
	if err != nil {
		t.Fatal("first store reservation")
	}
	if _, err := a.reserveContentsMemory(1); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal("per-store quota")
	}
	y, err := b.reserveContentsMemory(maxStoreContentsMemory)
	if err != nil {
		t.Fatal("second store reservation")
	}
	if _, err := c.reserveContentsMemory(1); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal("global quota bypassed")
	}
	x.release()
	y.release()
	var wg sync.WaitGroup
	for i := 0; i < 64; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := a.reserveContentsMemory(1024)
			if err != nil {
				t.Error("concurrent reserve")
				return
			}
			lease.release()
			lease.release()
		}()
	}
	wg.Wait()
	if stores.contents.used != 0 || len(stores.contents.stores) != 0 {
		t.Fatal("budget leaked")
	}
	if _, err := a.reserveContentsMemory(-1); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("negative reserve")
	}
	if lease, err := a.reserveContentsMemory(0); err != nil || lease != nil {
		t.Fatal("empty reservation allocated")
	}
}

func TestFAINamespacePostCaptureRevocationReleasesCopies(t *testing.T) {
	for _, cancelInstead := range []bool{false, true} {
		f := newHierarchyFixture(t)
		fid := f.binding.fids[Inbox]
		seedFAI(t, f.store, faiTestRecord(f.store, fid, 1))
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		triggered := false
		box := &hierarchyHookMailbox{stateMailbox: f.box, ownerHook: func() {
			// A positive copy reservation proves this occurs after the source
			// callback, during the final authorization pass.
			if f.stores.contents.used > f.store.fai.bytes {
				triggered = true
				if cancelInstead {
					cancel()
				} else {
					f.box.state.Owner = false
				}
			}
		}}
		f.session = &testSession{box}
		before := messageProof(f.store)
		if o, err := f.store.observeFAIMessages(ctx, f.session, fid); err == nil || o != nil || !triggered {
			t.Fatal("post-capture authorization bypassed")
		}
		if !reflect.DeepEqual(before, messageProof(f.store)) || f.stores.contents.used != f.store.fai.bytes {
			t.Fatal("failed capture committed registry or leaked buffers")
		}
	}
}
