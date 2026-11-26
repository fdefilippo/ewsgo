// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"ewsgo/internal/core"
	"ewsgo/internal/storage/sqlite"
)

type persistentFixtureMailbox struct{ fixtureMailbox }

func (*persistentFixtureMailbox) Root() core.ID { return "f:1" }

func TestPersistentEWSInitialPagesAndMovesSurviveRestart(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "mail.db")
	open := func() *sqlite.Store {
		t.Helper()
		store, err := sqlite.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		return store
	}
	store := open()
	box := &persistentFixtureMailbox{fixtureMailbox{objects: map[core.ID]core.Object{}}}
	session := &testSession{box: box}
	scope := session.Identity().Scope
	projection, err := store.OpenNativeProjection(ctx, scope, "backend-one")
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint core.NativeCheckpoint
	apply := func(cursor string, observed []core.NativeObject, removed []core.ID) {
		t.Helper()
		if err := projection.Apply(ctx, checkpoint, core.Cursor(cursor), observed, removed); err != nil {
			t.Fatal(err)
		}
		checkpoint, err = projection.Checkpoint(ctx)
		if err != nil {
			t.Fatal(err)
		}
		for _, o := range observed {
			box.objects[o.ID] = core.Object{ID: o.ID, Kind: o.Kind, Parent: o.Parent, Revision: core.Revision(o.Revision)}
		}
		for _, id := range removed {
			delete(box.objects, id)
		}
	}
	apply("native-a", []core.NativeObject{
		{ID: "f:1", Kind: core.Folder, Revision: 1},
		{ID: "f:2", Parent: "f:1", Kind: core.Folder, Revision: 1},
		{ID: "f:3", Parent: "f:1", Kind: core.Folder, Revision: 1},
		{ID: "m:1", Parent: "f:2", Kind: core.Message, Revision: 1},
		{ID: "m:2", Parent: "f:2", Kind: core.Message, Revision: 1},
	}, nil)
	newHandler := func(store *sqlite.Store) *Handler {
		t.Helper()
		h, err := NewPersistent(ctx, store, "backend-one", func(context.Context, core.Session) error { return nil }, 1)
		if err != nil {
			t.Fatal(err)
		}
		return h
	}
	h := newHandler(store)
	call := func(op node, code string) node {
		t.Helper()
		r := httptest.NewRequest("POST", "https://example.invalid/EWS/Exchange.asmx", strings.NewReader(string(envelope(op))))
		w := httptest.NewRecorder()
		h.Serve(w, r, session)
		parsed, err := parse(w.Body.Bytes())
		result := parsed.find(messages, "ResponseCode")
		if err != nil || w.Code != 200 || result == nil || result.Text != code {
			t.Fatalf("%s: HTTP %d, parsed %v, want %s, got %v", op.XMLName.Local, w.Code, err, code, result)
		}
		return parsed
	}
	folderA, folderB := h.encode(scope, "f:2"), h.encode(scope, "f:3")
	syncItems := func(folder, state string) node {
		op := el(messages, "SyncFolderItems", "", el(messages, "SyncFolderId", "", at(el(types, "FolderId", ""), "Id", folder)))
		if state != "" {
			op.Children = append(op.Children, el(messages, "SyncState", state))
		}
		op.Children = append(op.Children, el(messages, "MaxChangesReturned", "1"))
		return op
	}
	first := call(syncItems(folderA, ""), "NoError")
	state1 := first.find(messages, "SyncState").Text
	itemA := first.find(types, "ItemId").attr("Id")
	if first.find(messages, "IncludesLastItemInRange").Text != "false" || first.find(types, "Create") == nil {
		t.Fatal("initial first page missing")
	}
	empty := call(syncItems(folderB, ""), "NoError")
	stateB := empty.find(messages, "SyncState").Text
	apply("native-b", []core.NativeObject{{ID: "m:1", Parent: "f:3", Kind: core.Message, Revision: 2}}, nil)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store = open()
	h = newHandler(store)
	projection, err = store.OpenNativeProjection(ctx, scope, "backend-one")
	if err != nil {
		t.Fatal(err)
	}
	second := call(syncItems(folderA, state1), "NoError")
	state2 := second.find(messages, "SyncState").Text
	if second.find(messages, "IncludesLastItemInRange").Text != "false" || second.find(types, "Create") == nil || second.find(types, "ItemId").attr("Id") == itemA {
		t.Fatal("snapshot did not resume, or hid the delta following it")
	}
	deleted := call(syncItems(folderA, state2), "NoError")
	if deleted.find(types, "Delete") == nil || deleted.find(types, "ItemId").attr("Id") != itemA {
		t.Fatal("source move did not delete the published ID")
	}
	added := call(syncItems(folderB, stateB), "NoError")
	if added.find(types, "Create") == nil || added.find(types, "ItemId").attr("Id") == itemA {
		t.Fatal("destination move did not create its own ID")
	}
	stateB = added.find(messages, "SyncState").Text
	call(syncItems(folderB, state1), "ErrorInvalidSyncStateData")
	call(syncItems(folderA, itemA), "ErrorInvalidSyncStateData")

	// A cursor belongs to the client. Concurrent reads may safely replay it.
	var group sync.WaitGroup
	for range 4 {
		group.Go(func() {
			request := httptest.NewRequest("POST", "https://example.invalid/EWS/Exchange.asmx", strings.NewReader(string(envelope(syncItems(folderA, state2)))))
			response := httptest.NewRecorder()
			h.Serve(response, request, session)
			got, err := parse(response.Body.Bytes())
			removed := got.find(types, "Delete")
			if err != nil || removed == nil || removed.find(types, "ItemId").attr("Id") != itemA {
				t.Error("concurrent replay lost the source deletion")
			}
		})
	}
	group.Wait()

	apply("native-c", nil, []core.ID{"m:1"})
	if got := call(syncItems(folderB, stateB), "NoError"); got.find(types, "Delete") == nil {
		t.Fatal("deleted item dropped from delta")
	}
	mailbox, err := store.BindMailbox(ctx, scope, "backend-one")
	if err != nil {
		t.Fatal(err)
	}
	head, err := store.JournalHead(ctx, mailbox.MailboxID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompactJournal(ctx, mailbox.MailboxID, head); err != nil {
		t.Fatal(err)
	}
	call(syncItems(folderA, state1), "ErrorInvalidSyncStateData")
	if got := call(syncItems(folderA, ""), "NoError"); got.find(types, "Create") == nil || got.find(messages, "IncludesLastItemInRange").Text != "true" {
		t.Fatal("bootstrap after compaction missed the remaining object")
	}
}
