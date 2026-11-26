// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"fmt"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/storage/sqlite"
)

type pullFixtureSession struct {
	core.Session
	identity core.Identity
}

func (s pullFixtureSession) Identity() core.Identity { return s.identity }

func TestPullNotificationsJournalWatermarksAndRecovery(t *testing.T) {
	ctx := t.Context()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "mail.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	box := &persistentFixtureMailbox{fixtureMailbox{objects: map[core.ID]core.Object{
		"folder-a": {ID: "folder-a", Kind: core.Folder, Parent: "f:1"},
		"folder-b": {ID: "folder-b", Kind: core.Folder, Parent: "f:1"},
	}}}
	session := &testSession{box: box}
	scope := session.Identity().Scope
	binding, err := store.BindMailbox(ctx, scope, "ews-pull-test")
	if err != nil {
		t.Fatal(err)
	}
	var checkpoint core.NativeCheckpoint
	apply := func(next core.Cursor, changes ...sqlite.DeltaEvent) {
		t.Helper()
		checkpoint, err = store.ApplyDelta(ctx, binding.MailboxID, checkpoint, next, changes)
		if err != nil {
			t.Fatal(err)
		}
	}
	h, err := NewPersistent(ctx, store, "ews-pull-test", func(context.Context, core.Session) error { return nil }, 10)
	if err != nil {
		t.Fatal(err)
	}
	call := func(s core.Session, op node, want string) node {
		t.Helper()
		w := httptest.NewRecorder()
		r := httptest.NewRequest("POST", "https://example.invalid/EWS/Exchange.asmx", strings.NewReader(string(envelope(op)))).WithContext(ctx)
		h.Serve(w, r, s)
		got, err := parse(w.Body.Bytes())
		if err != nil || w.Code != 200 || nodeText(got.find(messages, "ResponseCode")) != want {
			t.Fatalf("%s: response code %q, HTTP %d, error %v", op.XMLName.Local, nodeText(got.find(messages, "ResponseCode")), w.Code, err)
		}
		return got
	}
	subscribe := func(events ...string) node {
		var flags []node
		for _, event := range events {
			flags = append(flags, el(types, "EventType", event))
		}
		return el(messages, "Subscribe", "", el(messages, "PullSubscriptionRequest", "",
			el(types, "FolderIds", "", at(el(types, "FolderId", ""), "Id", h.encode(scope, "folder-a"))),
			el(types, "EventTypes", "", flags...), el(types, "Timeout", "1")))
	}
	apply("baseline", sqlite.DeltaEvent{ObjectKind: core.Message, Change: core.Change{Kind: core.Created, ID: "historical", Parent: "folder-a"}})
	call(session, subscribe("NewMailEvent"), "ErrorInvalidEvent")
	sub := call(session, subscribe("CreatedEvent", "MovedEvent", "DeletedEvent"), "NoError")
	id, token := nodeText(sub.find(messages, "SubscriptionId")), nodeText(sub.find(messages, "Watermark"))
	poll := func(s core.Session, mark, want string) node {
		return call(s, el(messages, "GetEvents", "", el(messages, "SubscriptionId", id), el(messages, "Watermark", mark)), want)
	}
	foreign := session.Identity()
	foreign.Scope.Account = "other-mailbox"
	poll(pullFixtureSession{session, foreign}, token, "ErrorSubscriptionAccessDenied")
	poll(session, token+"invalid", "ErrorInvalidWatermark")
	result := poll(session, token, "NoError")
	if result.find(types, "StatusEvent") == nil || result.find(types, "CreatedEvent") != nil {
		t.Fatal("bootstrap emitted historical notifications")
	}
	var fresh []sqlite.DeltaEvent
	for i := range maxPullEvents + 1 {
		fresh = append(fresh, sqlite.DeltaEvent{ObjectKind: core.Message, Change: core.Change{Kind: core.Created, ID: core.ID(fmt.Sprintf("new-%d", i)), Parent: "folder-a"}})
	}
	fresh = append(fresh,
		sqlite.DeltaEvent{ObjectKind: core.Message, Change: core.Change{Kind: core.Created, ID: "hidden", Parent: "folder-b"}},
		sqlite.DeltaEvent{ObjectKind: core.Message, Change: core.Change{Kind: core.Moved, ID: "new-0", Parent: "folder-b", PreviousParent: "folder-a"}},
		sqlite.DeltaEvent{ObjectKind: core.Message, Change: core.Change{Kind: core.Deleted, ID: "new-1", Parent: "folder-a"}},
	)
	apply("observed", fresh...)
	events := poll(session, token, "NoError").find(messages, "Notification")
	if events == nil || len(events.Children) != 3+maxPullEvents || nodeText(events.child(types, "MoreEvents")) != "true" {
		t.Fatal("notification page was not bounded")
	}
	replay := poll(session, token, "NoError").find(messages, "Notification")
	if !sameNotificationWire(events, replay) {
		t.Fatal("replayed watermark changed the event sequence, timestamp or identifiers")
	}
	last := nodeText(events.Children[len(events.Children)-1].child(types, "Watermark"))
	events = poll(session, last, "NoError").find(messages, "Notification")
	move := events.child(types, "MovedEvent")
	if len(events.Children) != 6 || move == nil || events.child(types, "DeletedEvent") == nil || nodeText(events.child(types, "MoreEvents")) != "false" {
		t.Fatal("move out, deletion or folder filtering failed")
	}
	if move.child(types, "ItemId").attr("Id") == move.child(types, "OldItemId").attr("Id") ||
		move.child(types, "OldParentFolderId").attr("Id") != h.encode(scope, "folder-a") {
		t.Fatal("move did not preserve both placements")
	}
	if _, err := time.Parse(time.RFC3339Nano, nodeText(move.child(types, "TimeStamp"))); err != nil {
		t.Fatal("missing observation timestamp", err)
	}
	head, err := store.JournalHead(ctx, binding.MailboxID)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.CompactJournal(ctx, binding.MailboxID, head); err != nil {
		t.Fatal(err)
	}
	poll(session, token, "ErrorMissedNotificationEvents")
	h.mu.Lock()
	expired := h.pulls[id]
	expired.expires = time.Now().Add(-time.Second)
	h.pulls[id] = expired
	h.mu.Unlock()
	poll(session, last, "ErrorSubscriptionNotFound")
	active := call(session, subscribe("CreatedEvent"), "NoError")
	unsubscribe := el(messages, "Unsubscribe", "", el(messages, "SubscriptionId", nodeText(active.find(messages, "SubscriptionId"))))
	call(session, unsubscribe, "NoError")
	call(session, unsubscribe, "ErrorSubscriptionNotFound")
}

func sameNotificationWire(a, b *node) bool {
	return a != nil && b != nil && string(envelope(*a)) == string(envelope(*b))
}
