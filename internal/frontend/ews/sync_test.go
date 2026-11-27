// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"encoding/base64"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/backend/memory"
	"ewsgo/internal/core"
)

func TestHierarchyAcceptsMailCraftRootSelector(t *testing.T) {
	scope := core.Scope{Backend: "synthetic", Account: "one"}
	box, err := memory.New().Open(context.Background(), scope)
	if err != nil {
		t.Fatal(err)
	}
	h := New()
	session := &testSession{box: box}
	for _, tc := range []struct {
		ref  node
		code string
	}{
		{at(el(types, "DistinguishedFolderId", ""), "Id", "msgfolderroot"), "NoError"},
		{at(el(types, "FolderId", ""), "Id", h.encode(scope, box.Root())), "NoError"},
		{at(el(types, "DistinguishedFolderId", ""), "Id", "inbox"), "ErrorInvalidPropertyForOperation"},
		{at(el(types, "FolderId", ""), "Id", h.encode(scope, "foreign-root")), "ErrorInvalidPropertyForOperation"},
	} {
		op := el(messages, "SyncFolderHierarchy", "", el(messages, "FolderShape", "", el(types, "BaseShape", "IdOnly")), el(messages, "SyncFolderId", "", tc.ref))
		r := httptest.NewRequest("POST", "https://example.invalid/EWS/Exchange.asmx", strings.NewReader(string(envelope(op))))
		w := httptest.NewRecorder()
		h.Serve(w, r, session)
		reply, err := parse(w.Body.Bytes())
		if err != nil || reply.find(messages, "ResponseCode") == nil || reply.find(messages, "ResponseCode").Text != tc.code {
			t.Fatalf("root selector: expected %s", tc.code)
		}
	}
}

type fixtureMailbox struct {
	core.Mailbox
	objects     map[core.ID]core.Object
	pages       []core.SyncPage
	calls, sets int
	syncErr     error
}

func (f *fixtureMailbox) Get(_ context.Context, id core.ID) (core.Object, error) {
	o, ok := f.objects[id]
	if !ok {
		return core.Object{}, core.ErrNotFound
	}
	return o, nil
}
func (f *fixtureMailbox) SyncFolder(context.Context, core.ID, core.Cursor, int) (core.SyncPage, error) {
	if f.syncErr != nil {
		return core.SyncPage{}, f.syncErr
	}
	if f.calls >= len(f.pages) {
		return core.SyncPage{}, core.ErrResync
	}
	page := f.pages[f.calls]
	f.calls++
	return page, nil
}
func (f *fixtureMailbox) SetRead(_ context.Context, id core.ID, read bool) (core.Object, error) {
	o, err := f.Get(context.Background(), id)
	if err != nil {
		return o, err
	}
	o.Properties, _ = core.CloneProperties(o.Properties)
	o.Properties[core.IsRead] = core.Bool(read)
	o.Revision++
	f.objects[id] = o
	f.sets++
	return o, nil
}
func (*fixtureMailbox) ReadMIME(context.Context, core.ID) ([]byte, error) {
	return []byte("MIME synthetic\r\n"), nil
}
func TestPagedDeltaWritesAndMovedIdentity(t *testing.T) {
	scope := core.Scope{Backend: "synthetic", Account: "one"}
	folder := core.ID("f:2")
	var objects []core.Object
	box := &fixtureMailbox{objects: map[core.ID]core.Object{}}
	for i := range 3 {
		id := core.ID("m:" + strconv.Itoa(i+1))
		o := core.Object{ID: id, Parent: folder, Kind: core.Message, Revision: 1, Properties: core.Properties{core.Subject: core.Text("Synthetic"), core.IsRead: core.Bool(false)}}
		objects = append(objects, o)
		box.objects[id] = o
	}
	updated := objects[1]
	updated.Properties, _ = core.CloneProperties(updated.Properties)
	updated.Properties[core.IsRead] = core.Bool(true)
	updated.Revision = 2
	box.pages = []core.SyncPage{{Objects: objects, Next: "native-1"}, {Objects: []core.Object{updated}, Next: "native-2"}, {Deleted: []core.ID{"m:1"}, Next: "native-3"}}
	h := NewWithPageSize(1)
	session := &testSession{box: box}
	call := func(op node) node {
		t.Helper()
		r := httptest.NewRequest("POST", "https://example.invalid/EWS/Exchange.asmx", strings.NewReader(string(envelope(op))))
		w := httptest.NewRecorder()
		h.Serve(w, r, session)
		if w.Code != 200 {
			t.Fatal(w.Code)
		}
		root, err := parse(w.Body.Bytes())
		if err != nil {
			t.Fatal(err)
		}
		return root
	}
	syncOp := func(state string) node {
		op := el(messages, "SyncFolderItems", "", el(messages, "SyncFolderId", "", at(el(types, "FolderId", ""), "Id", h.encode(scope, folder))))
		if state != "" {
			op.Children = append(op.Children, el(messages, "SyncState", state))
		}
		op.Children = append(op.Children, el(messages, "MaxChangesReturned", "1"))
		return op
	}
	state, itemID := "", ""
	for i := range 3 {
		reply := call(syncOp(state))
		cursor := reply.find(messages, "SyncState")
		if cursor == nil {
			t.Fatal("cursor missing")
		}
		state = cursor.Text
		last := reply.find(messages, "IncludesLastItemInRange")
		if last == nil || (last.Text == "true") != (i == 2) {
			t.Fatal("wrong page boundary")
		}
		if i == 0 {
			itemID = reply.find(types, "ItemId").attr("Id")
		}
	}
	if box.calls != 1 {
		t.Fatal("pending snapshot was skipped or refetched")
	}
	get := el(messages, "GetItem", "", el(messages, "ItemShape", "", el(types, "IncludeMimeContent", "true")), el(messages, "ItemIds", "", at(el(types, "ItemId", ""), "Id", itemID)))
	reply := call(get)
	mime := reply.find(types, "MimeContent")
	if mime == nil {
		t.Fatal("MIME absent")
	}
	decoded, _ := base64.StdEncoding.DecodeString(mime.Text)
	if string(decoded) != "MIME synthetic\r\n" {
		t.Fatal("MIME changed")
	}
	update := at(at(el(messages, "UpdateItem", "", el(messages, "ItemChanges", "", el(types, "ItemChange", "", at(el(types, "ItemId", ""), "Id", itemID), el(types, "Updates", "", el(types, "SetItemField", "", at(el(types, "FieldURI", ""), "FieldURI", "message:IsRead"), el(types, "Message", "", el(types, "IsRead", "true"))))))), "ConflictResolution", "NeverOverwrite"), "MessageDisposition", "SaveOnly")
	reply = call(update)
	if reply.find(messages, "ResponseCode").Text != "ErrorInvalidPropertyForOperation" || box.sets != 0 {
		t.Fatal("unsupported CAS was faked")
	}
	update.Attrs[0].Value = "AlwaysOverwrite"
	reply = call(update)
	if reply.find(messages, "ResponseCode").Text != "NoError" || box.sets != 1 {
		t.Fatal("read state not written")
	}
	// Unsupported additional fields must not produce a partial success.
	update.Children[0].Children[0].Children[1].Children[0].Children[1].Children = append(update.Children[0].Children[0].Children[1].Children[0].Children[1].Children, el(types, "Subject", "Ignored?"))
	reply = call(update)
	if reply.find(messages, "ResponseCode").Text != "ErrorInvalidPropertyForOperation" || box.sets != 1 {
		t.Fatal("unknown field silently ignored")
	}
	reply = call(syncOp(state))
	if reply.find(types, "Update") == nil || box.calls != 2 {
		t.Fatal("external delta absent")
	}
	state = reply.find(messages, "SyncState").Text
	moved := box.objects["m:1"]
	moved.Parent = "f:3"
	box.objects[moved.ID] = moved
	if h.itemID(scope, moved) == itemID {
		t.Fatal("wire item ID survived move")
	}
	reply = call(get)
	if reply.find(messages, "ResponseCode").Text != "ErrorItemNotFound" {
		t.Fatal("stale moved ID accepted")
	}
	reply = call(syncOp(state))
	if deleted := reply.find(types, "Delete"); deleted == nil || deleted.find(types, "ItemId").attr("Id") != itemID {
		t.Fatal("source deletion ID does not match former item")
	}
	state = reply.find(messages, "SyncState").Text
	box.syncErr = core.ErrResync
	reply = call(syncOp(state))
	if reply.find(messages, "ResponseCode").Text != "ErrorInvalidSyncStateData" {
		t.Fatal("native resync not mapped")
	}
}
func TestCursorScopeExpiryAndRetentionBounds(t *testing.T) {
	h := New()
	scope := core.Scope{Backend: "synthetic", Account: "one"}
	state := syncState{scope: scope, folder: "f:2", known: map[core.ID]string{"m:1": "one"}}
	token := h.saveState(state)
	loaded, err := h.loadState(token, scope, "f:2", false)
	if err != nil {
		t.Fatal(err)
	}
	loaded.known["m:1"] = "changed"
	again, _ := h.loadState(token, scope, "f:2", false)
	if again.known["m:1"] != "one" {
		t.Fatal("cursor snapshot mutated")
	}
	for _, request := range []struct {
		scope     core.Scope
		folder    core.ID
		hierarchy bool
	}{{core.Scope{Backend: "synthetic", Account: "two"}, "f:2", false}, {scope, "f:3", false}, {scope, "f:2", true}} {
		if _, err = h.loadState(token, request.scope, request.folder, request.hierarchy); err != core.ErrResync {
			t.Fatal("cursor reused outside scope")
		}
	}
	old := h.states[token]
	old.expires = time.Now().Add(-time.Second)
	h.states[token] = old
	if _, err = h.loadState(token, scope, "f:2", false); err != core.ErrResync {
		t.Fatal("expired state accepted")
	}
	for range 140 {
		h.saveState(state)
	}
	if len(h.states) > 128 {
		t.Fatal("cursor count unbounded")
	}
	state.pending = make([]syncChange, 2000)
	for range 10 {
		h.saveState(state)
	}
	pending := 0
	for _, s := range h.states {
		pending += len(s.pending)
	}
	if pending > 4096 {
		t.Fatal("pending data unbounded")
	}
}

func TestEphemeralSyncBoundsAndMailboxIsolation(t *testing.T) {
	const folder core.ID = "f:2"
	h := NewWithPageSize(512)
	folderObject := core.Object{ID: folder, Kind: core.Folder}
	mailbox := func(account string, pages []core.SyncPage) (pullFixtureSession, *fixtureMailbox) {
		box := &fixtureMailbox{objects: map[core.ID]core.Object{folder: folderObject}, pages: pages}
		return pullFixtureSession{
			Session:  &testSession{box: box},
			identity: core.Identity{Scope: core.Scope{Backend: "synthetic", Account: account}},
		}, box
	}
	objects := make([]core.Object, 2001)
	for i := range objects {
		objects[i] = core.Object{ID: core.ID("m:" + strconv.Itoa(i+1)), Parent: folder, Kind: core.Message, Revision: 1}
	}
	alice, aliceBox := mailbox("alice", []core.SyncPage{
		{Objects: objects[:2000], Next: "alice-first", More: true}, {Objects: objects[2000:], Next: "alice-second"},
	})
	bob, bobBox := mailbox("bob", []core.SyncPage{{Objects: objects[:1], Next: "bob-first"}})
	syncOp := func(s core.Session, cursor string) node {
		op := el(messages, "SyncFolderItems", "",
			el(messages, "SyncFolderId", "", at(el(types, "FolderId", ""), "Id", h.encode(s.Identity().Scope, folder))),
			el(messages, "MaxChangesReturned", "512"))
		if cursor != "" {
			op.Children = append(op.Children, el(messages, "SyncState", cursor))
		}
		return op
	}
	cursor := ""
	for i := range 4 {
		result := serveWriteTest(t, h, alice, syncOp(alice, cursor), "NoError")
		changes := result.find(messages, "Changes")
		if len(changes.Children) != min(512, 2000-512*i) || aliceBox.calls != 1 {
			t.Fatal("wrong page boundary or native view was refetched")
		}
		cursor = result.find(messages, "SyncState").Text
		if i == 0 {
			serveWriteTest(t, h, bob, syncOp(bob, cursor), "ErrorInvalidSyncStateData")
			if bobBox.calls != 0 {
				t.Fatal("foreign cursor reached backend")
			}
			serveWriteTest(t, h, bob, syncOp(bob, ""), "NoError")
			restarted := NewWithPageSize(512)
			restarted.key = h.key
			serveWriteTest(t, restarted, alice, syncOp(alice, cursor), "ErrorInvalidSyncStateData")
		}
	}
	result := serveWriteTest(t, h, alice, syncOp(alice, cursor), "ErrorDataSizeLimitExceeded")
	if result.find(messages, "SyncState") != nil || aliceBox.calls != 2 || bobBox.calls != 1 {
		t.Fatal("oversized sync returned usable state or affected another mailbox")
	}
	h.mu.Lock()
	state := h.states[cursor]
	state.expires = time.Now().Add(-time.Second)
	h.states[cursor] = state
	h.mu.Unlock()
	serveWriteTest(t, h, alice, syncOp(alice, cursor), "ErrorInvalidSyncStateData")
}
