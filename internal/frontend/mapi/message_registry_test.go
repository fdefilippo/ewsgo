// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ewsgo/internal/core"
)

type messageFixtureMailbox struct {
	*stateMailbox
	objects     []core.Object
	readHook    func()
	sourceCalls int
	sourceErr   error
}

func (b *messageFixtureMailbox) ReadMailContents(_ context.Context, folder core.ID) (core.MailContents, error) {
	b.sourceCalls++
	if b.readHook != nil {
		b.readHook()
	}
	if b.sourceErr != nil {
		return core.MailContents{}, b.sourceErr
	}
	return core.MailContents{Folder: folder, Messages: b.objects}, nil
}
func messageFixture(t *testing.T) (*hierarchyFixture, *messageFixtureMailbox) {
	f := newHierarchyFixture(t)
	box := &messageFixtureMailbox{stateMailbox: f.box}
	f.session = &testSession{box}
	box.objects = []core.Object{messageTestObject("opaque-message", f.store.byFID[f.binding.fids[Inbox]].native)}
	return f, box
}
func messageTestObject(id, parent core.ID) core.Object {
	return core.Object{ID: id, Parent: parent, Kind: core.Message, Revision: 7, MIME: []byte("From: synthetic@example.invalid\r\n\r\nComplete synthetic body"), Properties: core.Properties{core.MetadataRevision: core.Integer(7), core.ContentRevision: core.Integer(3), core.LastModifiedAt: core.Timestamp(hierarchyTestTime), core.IsRead: core.Bool(false), core.Subject: core.Text("Synthetic subject"), core.ReceivedAt: core.Timestamp(hierarchyTestTime.Add(-time.Hour))}}
}
func messageObserve(t *testing.T, f *hierarchyFixture, role SpecialFolderRole) messageSnapshot {
	t.Helper()
	s, e := f.store.observeMailMessages(context.Background(), f.session, f.binding.fids[role])
	if e != nil {
		t.Fatal("message observation failed", e)
	}
	return s
}
func TestMessageRegistryIdentityReadStateAndContentVersions(t *testing.T) {
	f, box := messageFixture(t)
	first := messageObserve(t, f, Inbox)
	if len(first.messages) != 1 || len(first.known) != 1 {
		t.Fatal("missing current/known message")
	}
	row := first.messages[0]
	if row.mid == 0 || row.cn == 0 || row.readCN == 0 || row.cn == row.readCN || row.associated || row.read || row.parent != f.binding.fids[Inbox] {
		t.Fatal("message counters/classification")
	}
	if _, exists := f.store.byFID[row.mid]; exists {
		t.Fatal("MID became OpenFolder target")
	}
	if folderGlobalCount(row.mid) == row.cn || folderGlobalCount(row.mid) == row.readCN {
		t.Fatal("MID/CN counters collide")
	}
	if !bytes.Equal(row.sourceKey[:16], first.replica[:]) || !bytes.Equal(row.sourceKey[16:], binary.LittleEndian.AppendUint64(nil, row.mid)[2:]) || row.changeKey != gidChangeXID(hierarchyXID(first.replica, row.cn)) || row.predecessors[0] != 22 || !bytes.Equal(row.predecessors[1:], []byte(row.changeKey)) {
		t.Fatal("message XID/PCL format")
	}
	catalog, _ := privateFolders(context.Background(), f.session)
	bound, err := f.stores.bind(context.Background(), catalog)
	if err != nil || bound.store != f.store {
		t.Fatal("reconnect lost store")
	}
	if next := messageObserve(t, f, Inbox); !reflect.DeepEqual(first, next) {
		t.Fatal("identical read/reconnect changed version")
	}
	box.objects[0].Properties[core.IsRead] = core.Bool(true)
	box.objects[0].Revision = 8
	box.objects[0].Properties[core.MetadataRevision] = core.Integer(8)
	box.objects[0].Properties[core.LastModifiedAt] = core.Timestamp(hierarchyTestTime.Add(time.Minute))
	read := messageObserve(t, f, Inbox).messages[0]
	if read.mid != row.mid || read.cn != row.cn || read.changeKey != row.changeKey || read.readCN <= row.readCN || !read.read || !read.modified.Equal(row.modified) || read.object.Properties[core.LastModifiedAt] != core.Timestamp(row.modified) {
		t.Fatal("read-only transition changed body version/native content date")
	}
	box.objects[0].MIME = []byte("From: synthetic@example.invalid\r\n\r\nChanged body")
	box.objects[0].Properties[core.ContentRevision] = core.Integer(4)
	changed := messageObserve(t, f, Inbox).messages[0]
	if changed.mid != row.mid || changed.cn <= row.cn || changed.readCN != read.readCN || changed.modified != hierarchyTestTime.Add(time.Minute) {
		t.Fatal("real content change not tracked separately")
	}
	first.messages[0].object.MIME[0] = 'X'
	first.messages[0].object.Properties[core.Subject] = core.Text("caller edit")
	again := messageObserve(t, f, Inbox).messages[0]
	if again.object.Properties[core.Subject] != core.Text("Synthetic subject") || again.object.MIME[0] == 'X' {
		t.Fatal("detached output aliased native input/registry")
	}
}

func TestMessageRegistryRecipientRoleChangesContentAndClones(t *testing.T) {
	f, box := messageFixture(t)
	box.objects[0].Recipients = []core.Recipient{{Kind: core.RecipientTo, Address: "synthetic@example.invalid", DisplayName: "Synthetic"}}
	first := messageObserve(t, f, Inbox).messages[0]
	box.objects[0].Recipients[0].Kind = core.RecipientCc
	second := messageObserve(t, f, Inbox).messages[0]
	if second.mid != first.mid || second.cn <= first.cn || second.readCN != first.readCN || first.object.Recipients[0].Kind != core.RecipientTo || second.object.Recipients[0].Kind != core.RecipientCc {
		t.Fatal("role-only change lost, or recipient result aliased source")
	}
	box.objects[0].Recipients[0].Kind = 255
	if _, err := f.store.observeMailMessages(context.Background(), f.session, f.binding.fids[Inbox]); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("invalid semantic recipient role accepted")
	}
}
func TestMessageRegistryMoveDeletionReturnAndFAINamespace(t *testing.T) {
	f, box := messageFixture(t)
	first := messageObserve(t, f, Inbox).messages[0]
	box.objects[0].Parent = f.store.byFID[f.binding.fids[SentItems]].native
	moved := messageObserve(t, f, SentItems).messages[0]
	if moved.mid != first.mid || moved.cn <= first.cn || moved.sourceKey != first.sourceKey || moved.parent == first.parent {
		t.Fatal("move changed native identity")
	}
	box.objects = nil
	empty := messageObserve(t, f, Inbox)
	if len(empty.messages) != 0 || !empty.known[0].present || empty.known[0].parent != moved.parent {
		t.Fatal("old scope deletion killed moved identity")
	}
	removed := messageObserve(t, f, SentItems)
	if removed.known[0].present || removed.known[0].mid != first.mid || removed.known[0].cn <= moved.cn {
		t.Fatal("tombstone not retained")
	}
	stable := messageObserve(t, f, SentItems)
	if !reflect.DeepEqual(stable, removed) {
		t.Fatal("repeated absence changes version")
	}
	box.objects = []core.Object{messageTestObject("opaque-message", f.store.byFID[f.binding.fids[Inbox]].native)}
	returned := messageObserve(t, f, Inbox).messages[0]
	if returned.mid != first.mid || returned.cn <= removed.known[0].cn {
		t.Fatal("return lost reserved identity")
	}
	fai, err := f.store.observeMessageSet(context.Background(), f.session, f.binding.fids[Inbox], true, func(_ context.Context, _ core.Mailbox, key folderKey) ([]core.Object, error) {
		o := messageTestObject("opaque-message", key.native)
		o.MIME = nil
		return []core.Object{o}, nil
	})
	if err != nil || len(fai.messages) != 1 || !fai.messages[0].associated || fai.messages[0].mid == first.mid || len(fai.known) != 2 {
		t.Fatal("FAI/native namespaces collided", err)
	}
	after := messageObserve(t, f, Inbox)
	if len(after.known) != 2 || after.messages[0].cn != returned.cn {
		t.Fatal("FAI observation changed normal version")
	}
}

type registryProof struct {
	next      uint64
	folders   map[folderKey]uint64
	byMessage map[messageKey]uint64
	byMID     map[uint64]messageKey
	versions  map[messageKey]messageVersion
}

func messageProof(s *ramStore) registryProof {
	s.mu.Lock()
	defer s.mu.Unlock()
	return registryProof{s.next, maps.Clone(s.byKey), maps.Clone(s.byMessage), maps.Clone(s.byMID), maps.Clone(s.messages)}
}
func TestMessageRegistryFailuresAreAtomicAndAuthorized(t *testing.T) {
	for _, mode := range []string{"source_error", "revoked_after_read", "cancel_after_read", "wrong_scope", "unknown_anchor", "bad_parent", "missing_date", "missing_ms", "invalid_revision", "duplicate", "large_body", "many", "unknown_value", "invalid_id", "invalid_date", "mapping_quota", "registry_quota", "counter_overflow"} {
		t.Run(mode, func(t *testing.T) {
			f, box := messageFixture(t)
			messageObserve(t, f, Inbox)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			anchor := f.binding.fids[Inbox]
			switch mode {
			case "source_error":
				box.sourceErr = core.ErrResync
			case "revoked_after_read":
				box.readHook = func() { box.state.Owner = false }
			case "cancel_after_read":
				box.readHook = cancel
			case "wrong_scope":
				identity := f.session.Identity()
				identity.Scope.Account = "foreign"
				f.session = scopedSession{Session: f.session, identity: identity}
			case "unknown_anchor":
				anchor = folderID(123456)
			case "bad_parent":
				box.objects[0].Parent = "foreign-folder"
			case "missing_date":
				delete(box.objects[0].Properties, core.LastModifiedAt)
			case "missing_ms":
				delete(box.objects[0].Properties, core.MetadataRevision)
			case "invalid_revision":
				box.objects[0].Properties[core.MetadataRevision] = core.Integer(99)
			case "duplicate":
				box.objects = append(box.objects, box.objects[0])
			case "large_body":
				box.objects[0].MIME = make([]byte, core.MaxMailContentMIMEBytes+1)
			case "many":
				box.objects = make([]core.Object, core.MaxMailContentMessages+1)
			case "unknown_value":
				box.objects[0].Properties[core.Subject] = nil
			case "invalid_id":
				box.objects[0].ID = "opaque\x00id"
			case "invalid_date":
				box.objects[0].Properties[core.LastModifiedAt] = core.Timestamp(time.Time{})
			case "mapping_quota":
				for len(f.store.byKey)+len(f.store.byMessage) < maxRAMMappings {
					key := messageKey{id: core.ID(fmt.Sprint("retained", len(f.store.byMessage)))}
					f.store.next++
					mid := folderID(f.store.next)
					f.store.byMessage[key] = mid
					f.store.byMID[mid] = key
					f.store.messages[key] = messageVersion{}
				}
				box.objects[0].ID = "new-after-quota"
			case "registry_quota":
				for i := 0; i < 260; i++ {
					key := messageKey{id: core.ID(strings.Repeat("a", 4096) + fmt.Sprint(i))}
					f.store.next++
					mid := folderID(f.store.next)
					f.store.byMessage[key] = mid
					f.store.byMID[mid] = key
					f.store.messages[key] = messageVersion{}
				}
			case "counter_overflow":
				f.store.next = maxGlobalCount
				box.objects[0].MIME = []byte("changed body")
			}
			before := messageProof(f.store)
			got, err := f.store.observeMailMessages(ctx, f.session, anchor)
			if err == nil || !reflect.DeepEqual(got, messageSnapshot{}) || !reflect.DeepEqual(messageProof(f.store), before) {
				t.Fatal("failure returned data or changed registry")
			}
			if (mode == "wrong_scope" || mode == "unknown_anchor") && box.sourceCalls != 1 {
				t.Fatal("unauthorized callback executed")
			}
		})
	}
}
func TestMessageRegistryFingerprintTypedCanonicalAndBounded(t *testing.T) {
	a := messageTestObject("one", "folder")
	a.Properties[core.Key{ID: 100}] = core.Strings{"a", "b"}
	a.Recipients = []core.Recipient{{Address: "a@example.invalid", DisplayName: "A"}}
	a.Attachments = []core.Attachment{{ID: "part", MediaType: "text/plain", Data: []byte("full attachment")}}
	first, _, err := messageFingerprint(a)
	if err != nil {
		t.Fatal(err)
	}
	clone, _ := cloneMessageObject(a)
	clone.Properties = core.Properties{}
	for key, value := range a.Properties {
		clone.Properties[key] = value
	}
	same, _, err := messageFingerprint(clone)
	if err != nil || same != first {
		t.Fatal("map order changed fingerprint")
	}
	for _, alter := range []func(*core.Object){func(o *core.Object) { o.MIME[0] ^= 1 }, func(o *core.Object) { o.Properties[core.Subject] = core.Text("changed") }, func(o *core.Object) { o.Properties[core.Key{ID: 100}] = core.Strings{"ab"} }, func(o *core.Object) { o.Recipients[0].Address = "b@example.invalid" }, func(o *core.Object) { o.Attachments[0].Data[0] ^= 1 }} {
		other, _ := cloneMessageObject(a)
		alter(&other)
		fingerprint, _, e := messageFingerprint(other)
		if e != nil || fingerprint == first {
			t.Fatal("projected content change omitted")
		}
	}
	if !bytes.Equal(a.Attachments[0].Data, []byte("full attachment")) {
		t.Fatal("clone shares attachment storage")
	}
	for _, text := range []string{fmt.Sprintf("%+v", messageKey{id: "private-id"}), fmt.Sprintf("%#v", messageNode{object: a}), fmt.Sprintf("%+v", messageSnapshot{messages: []messageRow{{object: a}}})} {
		if strings.Contains(text, "private-id") || strings.Contains(text, "Synthetic subject") || strings.Contains(text, "attachment") {
			t.Fatal("private value in formatting")
		}
	}
}
func TestMessageRegistryFolderQuotaSharedAndAllocatorConcurrency(t *testing.T) {
	f, box := messageFixture(t)
	messageObserve(t, f, Inbox)
	var wg sync.WaitGroup
	for i := 0; i < 24; i++ {
		wg.Add(3)
		go func() {
			defer wg.Done()
			if _, e := f.store.observeMailMessages(context.Background(), f.session, f.binding.fids[Inbox]); e != nil {
				t.Error("concurrent observe")
			}
		}()
		go func(n int) {
			defer wg.Done()
			if _, e := f.store.assign(t.Context(), []folderKey{{native: core.ID(fmt.Sprint("future", n))}}); e != nil {
				t.Error("concurrent folder allocation")
			}
		}(i)
		go func() {
			defer wg.Done()
			if _, c := f.store.reserveLocalIDs(context.Background(), 1); c != 0 {
				t.Error("reservation")
			}
		}()
	}
	wg.Wait()
	_ = box
	proof := messageProof(f.store)
	seen := map[uint64]bool{}
	for _, fid := range proof.folders {
		seen[fid] = true
	}
	for mid := range proof.byMID {
		if seen[mid] {
			t.Fatal("folder/message collision")
		}
	}
	for len(f.store.byKey)+len(f.store.byMessage) < maxRAMMappings {
		key := messageKey{id: core.ID(fmt.Sprint("retained", len(f.store.byMessage)))}
		f.store.next++
		mid := folderID(f.store.next)
		f.store.byMessage[key] = mid
		f.store.byMID[mid] = key
		f.store.messages[key] = messageVersion{}
	}
	before := messageProof(f.store)
	if _, e := f.store.assign(t.Context(), []folderKey{{native: "not-assigned"}}); e == nil || !reflect.DeepEqual(before, messageProof(f.store)) {
		t.Fatal("folder allocator bypassed combined quota")
	}
	if _, e := f.store.observeHierarchy(context.Background(), f.session, f.binding.fids[MailboxRoot], hierarchyTestTime); e == nil || !reflect.DeepEqual(before, messageProof(f.store)) {
		t.Fatal("hierarchy bypassed combined quota")
	}
}
func TestMessageRegistryVirtualEmptyAndRestart(t *testing.T) {
	f, box := messageFixture(t)
	first := messageObserve(t, f, Inbox)
	calls := box.sourceCalls
	if empty := messageObserve(t, f, Outbox); len(empty.messages) != 0 || box.sourceCalls != calls {
		t.Fatal("virtual namespace called native normal source")
	}
	catalog, _ := privateFolders(context.Background(), f.session)
	binding, err := newRAMStores().bind(context.Background(), catalog)
	if err != nil || binding.store.replica == first.replica || len(binding.store.byMessage) != 0 {
		t.Fatal("restart retained message epoch")
	}
	if _, e := f.store.observeMailMessages(context.Background(), nil, f.binding.fids[Inbox]); !errors.Is(e, core.ErrAccessDenied) {
		t.Fatal("nil session authorized")
	}
}

func TestMessageRegistryCalendarAllowsOnlyLocalAssociatedNamespace(t *testing.T) {
	f, _ := newCalendarFixture(t)
	calendarEntry(t, f)
	object, code := f.state.mailbox.object(f.handles[0], 4)
	if code != 0 {
		t.Fatal("fixture binding")
	}
	store := object.binding.store
	fid, err := store.specialFolderFID(context.Background(), f.session, calendarMetadataRole)
	if err != nil {
		t.Fatal("fixture Calendar")
	}
	calls := 0
	source := func(context.Context, core.Mailbox, folderKey) ([]core.Object, error) {
		calls++
		return nil, nil // Complete, initially empty local FAI namespace.
	}
	before := messageProof(store)
	if _, err := store.observeMessageSet(context.Background(), f.session, fid, false, source); !errors.Is(err, core.ErrUnsupported) || calls != 0 || !reflect.DeepEqual(before, messageProof(store)) {
		t.Fatal("Calendar Normal was accepted, invoked source or changed registry")
	}
	got, err := store.observeMessageSet(context.Background(), f.session, fid, true, source)
	if err != nil || calls != 1 || !got.associated || len(got.messages) != 0 || !reflect.DeepEqual(before, messageProof(store)) {
		t.Fatal("local Calendar FAI namespace unavailable or changed identities")
	}
}

func TestMessageRegistryViewAndAssociatedQuotas(t *testing.T) {
	for _, mode := range []string{"view_bytes", "fai_bytes", "fai_count"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := messageFixture(t)
			associated := mode != "view_bytes"
			parent := f.store.byFID[f.binding.fids[Inbox]].native
			objects := []core.Object{messageTestObject("synthetic", parent)}
			switch mode {
			case "view_bytes":
				objects = nil
				for i := 0; i < 4; i++ {
					o := messageTestObject(core.ID(fmt.Sprint(i)), parent)
					o.MIME = make([]byte, core.MaxMailContentMIMEBytes)
					objects = append(objects, o)
				}
			case "fai_bytes":
				objects[0].Properties[core.Subject] = core.Text(strings.Repeat("a", 128<<10))
			case "fai_count":
				objects = make([]core.Object, 65)
			}
			before := messageProof(f.store)
			got, err := f.store.observeMessageSet(context.Background(), f.session, f.binding.fids[Inbox], associated, func(context.Context, core.Mailbox, folderKey) ([]core.Object, error) { return objects, nil })
			if !errors.Is(err, core.ErrUnsupported) || !reflect.DeepEqual(got, messageSnapshot{}) || !reflect.DeepEqual(before, messageProof(f.store)) {
				t.Fatal("quota returned data or changed registry")
			}
		})
	}
}
