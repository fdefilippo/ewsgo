// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"ewsgo/internal/core"
)

type notificationJournalFixture struct {
	events []NotificationEvent
}

func (j *notificationJournalFixture) Head(context.Context, core.Session) (NotificationPosition, error) {
	head := NotificationPosition{Generation: "test"}
	if len(j.events) > 0 {
		head.Revision = j.events[len(j.events)-1].Revision
	}
	return head, nil
}

func (*notificationJournalFixture) Refresh(context.Context, core.Session) error { return nil }

func (j *notificationJournalFixture) Page(_ context.Context, _ core.Session, after NotificationPosition, limit int) ([]NotificationEvent, NotificationPosition, error) {
	var page []NotificationEvent
	for _, event := range j.events {
		if event.Revision > after.Revision {
			page = append(page, event)
		}
		if len(page) == limit {
			break
		}
	}
	if len(page) != 0 {
		after.Revision = page[len(page)-1].Revision
	}
	return page, after, nil
}

func TestRegisterNotificationWireAndBounds(t *testing.T) {
	wire := []byte{0x29, 4, 0, 2, 4, 4, 0, 0}
	wire = binary.LittleEndian.AppendUint64(wire, 123)
	wire = binary.LittleEndian.AppendUint64(wire, 456)
	for n := 1; n < len(wire); n++ {
		ops, err := (ROPBuffer{Operations: wire[:n], Handles: make([]uint32, 3)}).DecodeOperations()
		if ops != nil || err == nil {
			t.Fatalf("truncated register accepted at %d", n)
		}
	}
	ops, err := (ROPBuffer{Operations: append(wire, 1, 4, 2), Handles: make([]uint32, 3)}).DecodeOperations()
	if err != nil || len(ops) != 2 || ops[0].Output != 2 || *ops[0].Notification != (notificationRequest{mask: 0x0404, fid: 123, mid: 456}) {
		t.Fatal("extended, scoped RegisterNotification or adjacent Release parsed incorrectly")
	}
	wire[6] = 1
	if _, err := (ROPBuffer{Operations: wire, Handles: make([]uint32, 3)}).DecodeOperations(); err != ErrMalformed {
		t.Fatal("nonzero Reserved accepted", err)
	}
}

func TestFolderCreationNotifiesParentSubscribers(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	store := object.binding.store
	const parent core.ID = "native-parent"
	const child core.ID = "native-child"
	f.box.page.Objects = append(f.box.page.Objects, core.Object{
		ID: parent, Parent: store.root, Kind: core.Folder,
		Properties: core.Properties{core.DisplayName: core.Text("Parent"), core.FolderItemCount: core.Integer(3)},
	})
	fids, err := store.assign(t.Context(), []folderKey{{native: parent}})
	if err != nil {
		t.Fatal(err)
	}
	journal := &notificationJournalFixture{}
	f.transport.EnableObjectNotifications(journal)
	direct := binary.LittleEndian.AppendUint64([]byte{0x29, 4, 0, 2, notifyModified, 0, 0}, fids[0])
	faiRunOK(t, f, binary.LittleEndian.AppendUint64(direct, 0))
	faiRunOK(t, f, []byte{0x29, 4, 0, 3, notifyModified, 0, 1})
	journal.events = []NotificationEvent{
		{core.Change{Kind: core.Created, ID: "invisible-child", Parent: parent, Revision: 1}, core.Folder},
		{core.Change{Kind: core.Created, ID: child, Parent: parent, Revision: 2}, core.Folder},
	}
	f.box.page.Objects = append(f.box.page.Objects, core.Object{
		ID: child, Parent: parent, Kind: core.Folder,
		Properties: core.Properties{core.DisplayName: core.Text("Visible child")},
	})
	var want []byte
	for _, slot := range []int{2, 3} {
		want = append32(append(want, 0x2a), f.handles[slot])
		want = append(want, 4)
		want = binary.LittleEndian.AppendUint16(want, notifyModified|notifyHasTotal)
		want = binary.LittleEndian.AppendUint64(want, fids[0])
		want = binary.LittleEndian.AppendUint16(want, 0)
		want = append32(want, 3)
	}
	if response, _ := f.run(nil); !bytes.Equal(response, want) {
		t.Fatalf("parent notification for direct and whole-store subscribers: %x", response)
	}
	if response, _ := f.run(nil); len(response) != 0 {
		t.Fatalf("replayed parent notifications: %x", response)
	}
}

func TestMessageCreationNotifiesSubscribedFolderWithNativeCounts(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	store := object.binding.store
	const folder core.ID = "native-notification-folder"
	f.box.page.Objects = append(f.box.page.Objects, core.Object{
		ID: folder, Parent: store.root, Kind: core.Folder, Properties: core.Properties{
			core.DisplayName: core.Text("Folder"), core.FolderItemCount: core.Integer(7), core.FolderUnreadCount: core.Integer(2),
		},
	})
	fids, err := store.assign(t.Context(), []folderKey{{native: folder}})
	if err != nil {
		t.Fatal(err)
	}
	fid := fids[0]
	journal := &notificationJournalFixture{}
	f.transport.EnableObjectNotifications(journal)
	registration := binary.LittleEndian.AppendUint64([]byte{0x29, 4, 0, 2, notifyCreated | notifyModified, 0, 0}, fid)
	registration = binary.LittleEndian.AppendUint64(registration, 0)
	response, _ := f.run(registration)
	if !bytes.Equal(response, []byte{0x29, 2, 0, 0, 0, 0}) {
		t.Fatalf("registration: %x", response)
	}
	journal.events = []NotificationEvent{
		{core.Change{Kind: core.Created, ID: "new-unrelated-mail", Parent: "opaque-sent", Revision: 1}, core.Message},
		{core.Change{Kind: core.Created, ID: "new-mail", Parent: folder, Revision: 2}, core.Message},
	}
	sub := f.state.mailbox.objects[f.handles[2]].notification
	body, _ := f.execute(nil, uint32(10+len(f.handles)*4+51))
	if pending := storeResponse(t, body).Operations; !bytes.Equal(pending, []byte{0x6e, 0, 0}) ||
		sub.position.Revision != 1 || len(store.byMessage) != 0 {
		t.Fatal("budget split the two notifications or allocated an MID before delivery")
	}
	wire, _ := f.run(nil)
	if len(wire) != 52 || binary.LittleEndian.Uint16(wire[6:8]) != notifyCreated|notifyMessage {
		t.Fatalf("missing message-created and folder-modified responses: %x", wire)
	}
	var folderWire []byte
	folderWire = append32(append(folderWire, 0x2a), f.handles[2])
	folderWire = append(folderWire, 4)
	folderWire = binary.LittleEndian.AppendUint16(folderWire, notifyModified|notifyHasTotal|notifyHasUnread)
	folderWire = binary.LittleEndian.AppendUint64(folderWire, fid)
	folderWire = binary.LittleEndian.AppendUint16(folderWire, 0)
	folderWire = append32(append32(folderWire, 7), 2)
	if !bytes.Equal(wire[26:], folderWire) || sub.position.Revision != 2 {
		t.Fatalf("folder notification or journal position: %x", wire[26:])
	}
	if wire, _ = f.run(nil); len(wire) != 0 {
		t.Fatalf("replayed delivered notifications: %x", wire)
	}
}

func TestObjectNotificationPreservesNonInboxMIDAndBoundedDelivery(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	store := object.binding.store
	const nativeFolder core.ID = "native-ordinary-folder"
	f.box.page.Objects = append(f.box.page.Objects, core.Object{
		ID: nativeFolder, Parent: store.root, Kind: core.Folder,
		Properties: core.Properties{core.DisplayName: core.Text("Ordinary")},
	})
	fids, err := store.assign(t.Context(), []folderKey{{native: nativeFolder}})
	if err != nil {
		t.Fatal(err)
	}
	const nativeMessage core.ID = "native-ordinary-message"
	journal := &notificationJournalFixture{events: []NotificationEvent{
		{core.Change{Kind: core.Created, ID: nativeMessage, Parent: nativeFolder, Revision: 1}, core.Message},
	}}
	f.transport.EnableObjectNotifications(journal)
	f.transport.synchronizationOptionsTrace = true
	scoped := binary.LittleEndian.AppendUint64([]byte{0x29, 4, 0, 2, notifyCreated | notifyMoved | notifyDeleted | 2, 0, 0}, fids[0])
	scoped = binary.LittleEndian.AppendUint64(scoped, 0)
	rop, counts := f.run(scoped)
	if !bytes.Equal(rop, []byte{0x29, 2, 0, 0, 0, 0}) {
		t.Fatal("registration did not return exactly its own response")
	}
	if events, scopes := counts.execution.NotificationRegistration(); len(events) != 4 || len(scopes) != 1 || scopes[0] != "folder" {
		t.Fatal("registration diagnostics omitted the request mask or scope")
	}
	sub := f.state.mailbox.objects[f.handles[2]].notification
	if sub.position.Revision != 1 {
		t.Fatal("registration replayed existing mailbox changes")
	}
	journal.events = append(journal.events,
		NotificationEvent{core.Change{Kind: core.Created, ID: "unrelated", Parent: "opaque-sent", Revision: 2}, core.Message},
		NotificationEvent{core.Change{Kind: core.Created, ID: nativeMessage, Parent: nativeFolder, Revision: 3}, core.Message},
	)
	body, _ := f.execute(nil, uint32(10+len(f.handles)*4+25))
	if pending := storeResponse(t, body).Operations; !bytes.Equal(pending, []byte{0x6e, 0, 0}) || sub.position.Revision != 2 || len(store.byMessage) != 0 {
		t.Fatal("small output consumed a notification or allocated its MID")
	}
	rop, counts = f.run(nil)
	if len(rop) != 26 || rop[0] != 0x2a || binary.LittleEndian.Uint32(rop[1:]) != f.handles[2] || rop[5] != 4 ||
		binary.LittleEndian.Uint16(rop[6:]) != notifyCreated|notifyMessage || binary.LittleEndian.Uint64(rop[8:]) != fids[0] {
		t.Fatal("created message notification shape or addressing")
	}
	if events := counts.execution.NotificationDelivery(); len(events) != 1 || events[0] != "message_created" {
		t.Fatal("delivery diagnostics omitted the event class")
	}
	mid := binary.LittleEndian.Uint64(rop[16:])
	box.objects = []core.Object{messageTestObject(nativeMessage, nativeFolder)}
	view, err := store.observeMailMessages(t.Context(), f.session, fids[0])
	if err != nil || len(view.messages) != 1 || view.messages[0].mid != mid || view.messages[0].cn == 0 {
		t.Fatal("notification and normal contents disagree on MID or revision", err)
	}
	journal.events = append(journal.events, NotificationEvent{core.Change{Kind: core.Moved, ID: nativeMessage, Parent: "opaque-sent", PreviousParent: nativeFolder, Revision: 4}, core.Message})
	rop, _ = f.run(nil)
	if len(rop) != 40 || binary.LittleEndian.Uint16(rop[6:]) != notifyMoved|notifyMessage ||
		binary.LittleEndian.Uint64(rop[8:]) != f.fids[SentItems] || binary.LittleEndian.Uint64(rop[24:]) != fids[0] ||
		binary.LittleEndian.Uint64(rop[32:]) != mid {
		t.Fatal("move notification did not retain both locations and MID")
	}
	journal.events = append(journal.events, NotificationEvent{core.Change{Kind: core.Deleted, ID: nativeMessage, PreviousParent: nativeFolder, Revision: 5}, core.Message})
	rop, _ = f.run(nil)
	if len(rop) != 24 || binary.LittleEndian.Uint16(rop[6:]) != notifyDeleted|notifyMessage ||
		binary.LittleEndian.Uint64(rop[8:]) != fids[0] || binary.LittleEndian.Uint64(rop[16:]) != mid {
		t.Fatal("deleted notification omitted the previous folder or MID")
	}
	if rop, _ = f.run(nil); len(rop) != 0 {
		t.Fatal("consumed journal event replayed")
	}
	f.run([]byte{1, 4, 2})
	if targets := f.state.mailbox.notificationTargets(); len(targets) != 0 {
		t.Fatal("Release retained a subscription")
	}
}

func TestObjectNotificationsUseContextHandleBudget(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	f.transport.synchronizationOptionsTrace = true
	journal := &notificationJournalFixture{}
	f.transport.EnableObjectNotifications(journal)
	folder := f.fids[Inbox]
	f.open(int(Inbox))
	const lastSlot = 33
	for len(f.handles) <= lastSlot {
		f.handles = append(f.handles, 0xffffffff)
	}
	register := func(slot, mask byte) []byte {
		wire := binary.LittleEndian.AppendUint64([]byte{0x29, 4, 1, slot, mask, 0, 0}, folder)
		return binary.LittleEndian.AppendUint64(wire, 0)
	}
	for slot := byte(2); slot < lastSlot; slot++ {
		faiRunOK(t, f, register(slot, notifyDeleted))
	}
	faiRunOK(t, f, register(lastSlot, notifyCreated))
	objects := f.state.mailbox
	if len(objects.notificationTargets()) != 32 {
		t.Fatal("the context lost a notification subscription")
	}
	store := objects.objects[f.handles[0]].binding.store
	journal.events = append(journal.events, NotificationEvent{
		core.Change{Kind: core.Created, ID: "tail-fixture", Parent: store.byFID[folder].native, Revision: 1}, core.Message,
	})
	wire, _ := f.run(nil)
	if len(wire) != 26 || wire[0] != 0x2a || binary.LittleEndian.Uint32(wire[1:5]) != f.handles[lastSlot] ||
		binary.LittleEndian.Uint16(wire[6:8]) != notifyCreated|notifyMessage {
		t.Fatal("the last subscription did not receive the event")
	}
	released := f.handles[lastSlot]
	f.run([]byte{1, 4, lastSlot})
	faiRunOK(t, f, register(lastSlot, notifyCreated))
	if f.handles[lastSlot] == released || len(objects.notificationTargets()) != 32 {
		t.Fatal("Release did not replace exactly one subscription")
	}
	objects.allocated = maxContextHandleAllocations
	wire, counts := f.run(register(lastSlot, notifyCreated))
	if !bytes.Equal(wire, ropError(0x29, lastSlot, 0x8007000e)) || len(objects.notificationTargets()) != 32 {
		t.Fatal("registration bypassed the context allocation limit")
	}
	_, _, firstError, _, _ := counts.execution.Fields()
	if firstError != "register_notification:out_of_memory" {
		t.Fatalf("unclassified context admission error: %s", firstError)
	}
}
