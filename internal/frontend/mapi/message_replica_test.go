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

func TestSQLiteMessageProjectionSurvivesReadMoveDelete(t *testing.T) {
	f, box := messageFixture(t)
	reopen := sqliteReplicaFixture(t, f)
	db := reopen()
	f.snapshot(t, MailboxRoot, hierarchyTestTime)
	first := messageObserve(t, f, Inbox)
	if len(first.messages) != 1 {
		t.Fatal("message not projected")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopen()
	if got := messageObserve(t, f, Inbox); !reflect.DeepEqual(first, got) {
		t.Fatal("reopen changed a message's identifiers or revision")
	}

	o := &box.objects[0]
	o.Revision++
	o.Properties[core.MetadataRevision] = core.Integer(o.Revision)
	o.Properties[core.IsRead] = core.Bool(true)
	read := messageObserve(t, f, Inbox)
	row := read.messages[0]
	initial := first.messages[0]
	if row.cn != initial.cn || row.readCN <= initial.readCN || !row.read || row.mid != initial.mid {
		t.Fatal("mark read must advance only the read version")
	}
	reopen()
	if got := messageObserve(t, f, Inbox); !reflect.DeepEqual(read, got) {
		t.Fatal("read state was not retained")
	}

	o.Parent = f.store.sent
	moved := messageObserve(t, f, SentItems).messages[0]
	if moved.mid != row.mid || moved.cn <= row.cn || moved.readCN != row.readCN || moved.parent != f.binding.fids[SentItems] {
		t.Fatal("move lost the identity or did not revise membership")
	}
	reopen()
	box.objects = nil
	oldFolder := messageObserve(t, f, Inbox)
	if len(oldFolder.known) != 1 || !oldFolder.known[0].present || oldFolder.known[0].parent != moved.parent {
		t.Fatal("old folder treated the moved message as deleted")
	}
	removed := messageObserve(t, f, SentItems)
	if len(removed.known) != 1 || removed.known[0].present || removed.known[0].mid != moved.mid || removed.known[0].cn <= moved.cn {
		t.Fatal("deletion did not publish the version")
	}
	reopen()
	if got := messageObserve(t, f, SentItems); !reflect.DeepEqual(removed, got) {
		t.Fatal("tombstone disappeared or advanced after reopening")
	}
}

func TestSQLiteReadStateRetriesAfterNativeWrite(t *testing.T) {
	f, native := messageFixture(t)
	box := &readStateMailbox{messageFixtureMailbox: native}
	f.session = &testSession{box}
	reopen := sqliteReplicaFixture(t, f)
	reopen()
	row := messageObserve(t, f, Inbox).messages[0]
	change := readStateChange{source: row.sourceKey, read: !row.read}
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	box.setHook = cancel
	inbox := f.binding.fids[Inbox]
	if _, err := f.store.importReadStates(ctx, f.session, inbox, []readStateChange{change}); !errors.Is(err, context.Canceled) || box.sets != 1 {
		t.Fatalf("cancel after the native write: %v, writes %d", err, box.sets)
	}
	reopen()
	counters, err := f.store.importReadStates(t.Context(), f.session, inbox, []readStateChange{change})
	if err != nil || box.sets != 1 || len(counters) != 1 || counters[0] <= row.readCN {
		t.Fatalf("read-state retry: %v, writes %d", err, box.sets)
	}
	reopen()
	got := messageObserve(t, f, Inbox).messages[0]
	if got.mid != row.mid || got.cn != row.cn || got.readCN != counters[0] || got.read != change.read {
		t.Fatal("restart changed the acknowledged read state or message identity")
	}
}

func TestSQLiteMessageIDsIssuedByNotificationAndRecoveryTable(t *testing.T) {
	f, box := messageFixture(t)
	reopen := sqliteReplicaFixture(t, f)
	reopen()
	message := box.objects[0]
	event := NotificationEvent{Kind: core.Created, ObjectKind: core.Message, ID: message.ID, Parent: message.Parent}
	projection := map[core.ID]hierarchyNode{message.Parent: {key: folderKey{native: message.Parent}}}
	wire, err := f.store.projectObjectNotification(t.Context(), event, projection, true)
	if err != nil || wire.mid == 0 {
		t.Fatal("notification allocation:", err)
	}
	ref := recoverableReference{id: "deleted-item", origin: message.Parent, inTrashView: true}
	recoverables, err := f.store.assignRecoverableMIDs(t.Context(), []recoverableReference{ref})
	if err != nil || len(recoverables) != 1 || recoverables[0] == wire.mid {
		t.Fatal("recoverable allocation:", err)
	}
	reopen()
	if got, active := f.store.recoverableMessage(recoverables[0]); active {
		t.Fatal("restart trusted recovery view before native re-observation:", got.inTrashView)
	}
	restored, err := f.store.assignRecoverableMIDs(t.Context(), []recoverableReference{ref})
	if err != nil || !reflect.DeepEqual(restored, recoverables) {
		t.Fatal("same authorized recovery row acquired a new MID:", err)
	}
	if got := messageObserve(t, f, Inbox).messages[0]; got.mid != wire.mid {
		t.Fatal("contents observation discarded the previously published MID")
	}
}
