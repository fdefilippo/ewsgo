// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"

	"ewsgo/internal/core"
)

type syncMoveMailbox struct {
	*readStateMailbox
	core.MailMessagePlacementWriter
	calls     int
	loseReply bool
}

func (b *syncMoveMailbox) ReadMailContents(_ context.Context, folder core.ID) (core.MailContents, error) {
	result := core.MailContents{Folder: folder}
	for _, object := range b.objects {
		if object.Parent == folder {
			result.Messages = append(result.Messages, object)
		}
	}
	return result, nil
}

func (b *syncMoveMailbox) MoveMailMessage(_ context.Context, id core.ID, revision core.Revision, to core.ID) (core.Object, error) {
	b.calls++
	for i := range b.objects {
		object := &b.objects[i]
		if object.ID == id && object.Revision == revision {
			object.Parent = to
			object.Revision++
			object.Properties[core.MetadataRevision] = core.Integer(object.Revision)
			if b.loseReply {
				return core.Object{}, core.ErrOutcomeUnknown
			}
			return *object, nil
		}
	}
	return core.Object{}, core.ErrConflict
}

func importMoveWire(folder, source, destination [22]byte, history []byte, change changeXID) []byte {
	wire := appendBlob([]byte{0x78, 4, 2}, folder[:])
	wire = appendBlob(wire, source[:])
	wire = appendBlob(wire, history)
	wire = appendBlob(wire, destination[:])
	return appendBlob(wire, []byte(change))
}

func TestSQLiteImportMessageMoveReconcilesLostReplyThroughExecute(t *testing.T) {
	t.Run("MatchingHistory", func(t *testing.T) { testSQLiteImportMessageMove(t, false) })
	t.Run("NewerClient", func(t *testing.T) { testSQLiteImportMessageMove(t, true) })
}

func testSQLiteImportMessageMove(t *testing.T, newer bool) {
	f, base := newContentsDownloadFixture(t)
	box := &syncMoveMailbox{readStateMailbox: &readStateMailbox{messageFixtureMailbox: base}}
	box.objects[0].Properties[core.IsDraft] = core.Bool(false)
	f.session = &testSession{box}
	f.transport.EnableFAICollectorSpike()
	reopen := sqliteExecuteFixture(t, f)
	db := reopen()
	store := replicaStore(t, f)
	inbox, sent := f.fids[Inbox], f.fids[SentItems]
	snapshot, err := store.observeMailMessages(t.Context(), f.session, inbox)
	if err != nil || len(snapshot.messages) != 1 {
		t.Fatal("source fixture unavailable", err)
	}
	message := snapshot.messages[0]
	gc, code := store.reserveLocalIDs(t.Context(), 1)
	if code != 0 {
		t.Fatal("could not reserve destination ID")
	}
	var target [22]byte
	copy(target[:16], store.replica[:])
	copy(target[16:], gc[:])
	destination := folderID(ownMoveCounter(store.replica, target))
	change := gidChangeXID(hierarchyXID([16]byte{0x2b}, 5))
	folder := store.hierarchyXID(inbox)
	history, expectedChange, result := message.predecessors, change, uint32(0)
	if newer {
		prior := gidChangeXID(hierarchyXID([16]byte{0x2b}, 4))
		history = append(append(bytes.Clone(history), 22), prior...)
		expectedChange, result = message.changeKey, syncClientChangeNewer
	}
	wire := importMoveWire(folder, message.sourceKey, target, history, change)

	openCollector := func() {
		t.Helper()
		faiRunOK(t, f, folderOperation(sent, 4, 0, 1))
		faiRunOK(t, f, []byte{0x7e, 4, 1, 2, 1})
	}
	requireCode := func(request []byte, code uint32) {
		t.Helper()
		reply, _ := f.run(request)
		want := ropError(0x78, 2, code)
		if code == 0 {
			want = binary.LittleEndian.AppendUint64(want, 0)
		}
		if !bytes.Equal(reply, want) {
			t.Fatalf("unexpected import move reply %x, want %x", reply, want)
		}
	}
	openCollector()
	requireCode(importMoveWire(folder, message.sourceKey, target, append([]byte{22}, change...), change),
		faiError(core.ErrConflict, false))
	requireCode(importMoveWire(folder, message.sourceKey, hierarchyXID([16]byte{42}, 1), message.predecessors, change),
		0x80070057)
	f.execute(wire, uint32(10+4*len(f.handles)+13))
	if box.calls != 0 {
		t.Fatal("conflicting or undersized request moved native data")
	}

	box.loseReply = true
	requireCode(wire, faiError(core.ErrOutcomeUnknown, false))
	if box.calls != 1 || box.objects[0].Parent != store.byFID[sent].native || store.byMID[destination] != (messageKey{}) {
		t.Fatal("uncertain move published a destination before reconciliation")
	}
	if _, err := store.observeMailMessages(t.Context(), f.session, sent); !errors.Is(err, core.ErrOutcomeUnknown) {
		t.Fatal("observation exposed the old MID under the new folder", err)
	}
	event := NotificationEvent{ObjectKind: core.Message, Change: core.Change{
		ID: message.object.ID, Kind: core.Moved,
		PreviousParent: store.byFID[inbox].native, Parent: store.byFID[sent].native,
	}}
	visible := map[core.ID]hierarchyNode{event.PreviousParent: {}, event.Parent: {}}
	if _, err := store.projectObjectNotification(t.Context(), event, visible, true); !errors.Is(err, core.ErrOutcomeUnknown) {
		t.Fatal("notification exposed a move before reconciliation", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = reopen()
	store = replicaStore(t, f)
	openCollector()
	requireCode(wire, result)
	requireCode(wire, result)
	if box.calls != 1 {
		t.Fatal("retransmission issued a duplicate native move")
	}
	destinationView, err := store.observeMailMessages(t.Context(), f.session, sent)
	if err != nil || len(destinationView.messages) != 1 {
		t.Fatal("destination observation failed", err)
	}
	row := destinationView.messages[0]
	if row.mid != destination || row.sourceKey != target || row.changeKey != expectedChange || row.object.ID != message.object.ID ||
		!bytes.Equal(row.object.MIME, message.object.MIME) {
		t.Fatal("destination did not preserve content and use the requested MID")
	}
	pcl, err := parseFAIPCL(row.predecessors)
	if err != nil || pcl.includesXID(change) == newer || !pcl.includesXID(message.changeKey) {
		t.Fatal("destination lost change history", err)
	}
	notification, err := store.projectObjectNotification(t.Context(), event, visible, true)
	if err != nil || notification.mid != destination || notification.oldMID != message.mid {
		t.Fatal("move notification did not project both message identifiers", err)
	}
	payload := notification.appendTo(nil, objectNotificationTarget{})
	if binary.LittleEndian.Uint64(payload[16:24]) != destination || binary.LittleEndian.Uint64(payload[32:40]) != message.mid {
		t.Fatal("move notification encoded the wrong message identifiers")
	}
	sourceView, err := store.observeMailMessages(t.Context(), f.session, inbox)
	if err != nil || len(sourceView.messages) != 0 {
		t.Fatal("old folder still contains the message", err)
	}
	if tombstone := store.messages[store.byMID[message.mid]]; tombstone.present || tombstone.parent != inbox {
		t.Fatal("old MID was not retained as a source tombstone")
	}
	if _, _, err = store.openedMessageScope(t.Context(), f.session, inbox, openedMessage{mid: message.mid, includeSoftDeleted: true}); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("retired MID exposes a native object", err)
	}
	faiRunOK(t, f, []byte{0x82, 4, 2, 3})
	checkpoint := collectorThreeTestSets(t, drainFX(t, f.run, 3, 300), idSet{})
	if !checkpoint.seen.contains(store.replica, row.cn) {
		t.Fatal("collector omitted the move change from Seen")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopen()
	store = replicaStore(t, f)
	openCollector()
	requireCode(wire, result)
	if box.calls != 1 || store.messageChangeKey(store.messages[store.byMID[destination]]) != expectedChange {
		t.Fatal("reopening lost the result or the destination history")
	}
}

type contactMoveMailbox struct {
	*personalContactsMailbox
	core.ContactWriter
	trash core.ID
	calls int
}

func (b *contactMoveMailbox) ListContacts(ctx context.Context, folder core.ID) ([]core.Contact, error) {
	if b.item.Identity.Parent == b.trash && folder == b.contacts.ID {
		return nil, nil
	}
	return b.personalContactsMailbox.ListContacts(ctx, folder)
}

func (b *contactMoveMailbox) GetContact(ctx context.Context, folder, id core.ID) (core.Contact, error) {
	if b.item.Identity.Parent == b.trash {
		return core.Contact{}, core.ErrNotFound
	}
	return b.personalContactsMailbox.GetContact(ctx, folder, id)
}

func (b *contactMoveMailbox) TrashContact(_ context.Context, before core.Contact) error {
	b.calls++
	if before.Identity.ID != b.item.Identity.ID || before.Identity.Parent != b.contacts.ID ||
		before.Identity.Revision != b.item.Identity.Revision || b.item.Identity.Parent != b.contacts.ID {
		return core.ErrConflict
	}
	b.item.Identity.Parent = b.trash
	b.item.Identity.Revision++
	b.item.Identity.Properties[core.MetadataRevision] = core.Integer(b.item.Identity.Revision)
	return core.ErrOutcomeUnknown
}

func (b *contactMoveMailbox) ListTrashedContacts(_ context.Context, folder core.ID) ([]core.Contact, error) {
	if folder != b.trash {
		return nil, core.ErrNotFound
	}
	if b.item.Identity.Parent != folder {
		return nil, nil
	}
	return []core.Contact{b.item}, nil
}

func (b *contactMoveMailbox) GetTrashedContact(ctx context.Context, folder, id core.ID) (core.Contact, error) {
	items, err := b.ListTrashedContacts(ctx, folder)
	if err != nil {
		return core.Contact{}, err
	}
	if len(items) != 1 || items[0].Identity.ID != id {
		return core.Contact{}, core.ErrNotFound
	}
	return items[0], nil
}

func (b *contactMoveMailbox) ReadMailContents(_ context.Context, folder core.ID) (core.MailContents, error) {
	view := core.MailContents{Folder: folder}
	if folder == b.trash {
		view.Messages = []core.Object{messageTestObject("one-mail-item", folder)}
	}
	return view, nil
}

func TestSQLiteImportContactMoveToTrashThroughExecute(t *testing.T) {
	f, base := newContactsFixture(t)
	box := &contactMoveMailbox{personalContactsMailbox: &personalContactsMailbox{
		contactsMailbox: base,
		item: core.Contact{DisplayName: "Synthetic card", Emails: [3]string{"card@example.invalid"}, Identity: core.Object{
			ID: "contact:42", Parent: base.contacts.ID, Kind: core.ContactItem, Revision: 7,
			Properties: core.Properties{core.MetadataRevision: core.Integer(7), core.ContentRevision: core.Integer(5),
				core.LastModifiedAt: core.Timestamp(hierarchyTestTime)},
		}},
	}}
	f.session = &testSession{box}
	f.transport.EnableFAICollectorSpike()
	f.transport.EnableHierarchyDownloadSpike()
	f.transport.EnableContentsDownloadSpike()
	f.transport.EnableContentsTableSpike()
	reopen := sqliteExecuteFixture(t, f)
	db := reopen()
	store := replicaStore(t, f)
	trash := f.fids[DeletedItems]
	box.trash = store.trash
	binding := f.state.mailbox.objects[f.handles[0]].binding
	fid, err := parseFolderEntryID(contactsEntry(t, f), binding.mailboxGUID, store.replica)
	if err != nil {
		t.Fatal("could not map Contacts", err)
	}
	view, err := store.observeContacts(t.Context(), f.session, fid)
	if err != nil || len(view.messages) != 1 {
		t.Fatal("source fixture unavailable", err)
	}
	source := view.messages[0]
	gc, code := store.reserveLocalIDs(t.Context(), 1)
	if code != 0 {
		t.Fatal("could not reserve destination ID")
	}
	var target [22]byte
	copy(target[:16], store.replica[:])
	copy(target[16:], gc[:])
	destination := folderID(ownMoveCounter(store.replica, target))
	change := gidChangeXID(hierarchyXID([16]byte{0x2b}, 5))
	wire := importMoveWire(store.hierarchyXID(fid), source.sourceKey, target, source.predecessors, change)
	openCollector := func(to uint64) {
		t.Helper()
		faiRunOK(t, f, folderOperation(to, 4, 0, 1))
		faiRunOK(t, f, []byte{0x7e, 4, 1, 2, 1})
	}
	check := func(expected uint32) {
		t.Helper()
		out, _ := f.run(wire)
		want := ropError(0x78, 2, expected)
		if expected == 0 {
			want = binary.LittleEndian.AppendUint64(want, 0)
		}
		if !bytes.Equal(out, want) {
			t.Fatalf("contact move returned %x, want %x", out, want)
		}
	}
	openCollector(f.fids[Inbox])
	check(NotSupported)
	if box.calls != 0 {
		t.Fatal("non-Trash destination dispatched a contact move")
	}
	openCollector(trash)
	check(faiError(core.ErrOutcomeUnknown, false))
	if _, err := store.observeMailMessages(t.Context(), f.session, trash); !errors.Is(err, core.ErrOutcomeUnknown) {
		t.Fatal("unconfirmed move was published to Trash", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = reopen()
	store = replicaStore(t, f)
	openCollector(trash)
	check(0)
	check(0)
	if box.calls != 1 {
		t.Fatal("reconciliation dispatched the native operation again")
	}
	view, err = store.observeMailMessages(t.Context(), f.session, trash)
	if err != nil || len(view.messages) != 2 {
		t.Fatal("Trash observation did not publish the contact", err)
	}
	mail, row := view.messages[0], view.messages[1]
	if mail.special != 0 || mail.object.ID != "one-mail-item" {
		t.Fatal("Trash observation lost the mail item")
	}
	if row.mid != destination || row.special != core.ContactsFolder || row.object.ID != source.object.ID || row.changeKey != change {
		t.Fatal("Trash did not publish the requested MID and contact namespace")
	}
	opened, err := store.readOpenedMessage(t.Context(), f.session, trash, openedMessage{mid: destination, codePage: 1200})
	if err != nil || opened.object.Properties[core.Subject] != core.Text("Synthetic card") {
		t.Fatal("Trash contact read failed", err)
	}
	if _, err := store.readOpenedMessage(t.Context(), f.session, fid, openedMessage{mid: source.mid}); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("source MID still opens after move", err)
	}
	faiRunOK(t, f, []byte{1, 4, 2})
	config := fullContentsConfig()
	config.flags = 0x21
	faiRunOK(t, f, contentsConfigureOperation(config, nil))
	raw := drainFX(t, f.run, 2, 16384)
	checkpoint := contentsFourTestSets(t, raw)
	if !checkpoint.given.contains(store.replica, folderGlobalCount(mail.mid)) ||
		!checkpoint.given.contains(store.replica, folderGlobalCount(destination)) ||
		!containsContactFX(t, raw, 0x001a001f, contactClass) {
		t.Fatal("Trash ICS did not deliver mail and the typed contact")
	}
	addressName, err := corePropertyName(core.Key{Set: contactAddressSet, ID: 0x8083})
	if err != nil {
		t.Fatal(err)
	}
	assertContactEmailFX(t, raw, addressName.Set, []int32{0}, 1, 0x8083, "card@example.invalid")
	faiRunOK(t, f, []byte{1, 4, 2})
	view, err = store.observeContacts(t.Context(), f.session, fid)
	if err != nil || len(view.messages) != 0 {
		t.Fatal("source card was not removed", err)
	}
	for _, known := range view.known {
		if known.mid == source.mid && known.parent == fid && !known.present {
			return
		}
	}
	t.Fatal("source Contacts ICS snapshot lost the tombstone")
}

func TestImportMessageMoveDistinguishesNativeMoveFromNewRevision(t *testing.T) {
	for _, moved := range []bool{false, true} {
		name := "NewRevision"
		if moved {
			name = "MovedByAnotherClient"
		}
		t.Run(name, func(t *testing.T) {
			f, base := newContentsDownloadFixture(t)
			box := &syncMoveMailbox{readStateMailbox: &readStateMailbox{messageFixtureMailbox: base}}
			box.objects[0].Properties[core.IsDraft] = core.Bool(false)
			f.session = &testSession{box}
			f.transport.EnableFAICollectorSpike()
			sqliteExecuteFixture(t, f)()
			store := replicaStore(t, f)
			inbox, sent, trash := f.fids[Inbox], f.fids[SentItems], f.fids[DeletedItems]
			snapshot, err := store.observeMailMessages(t.Context(), f.session, inbox)
			if err != nil || len(snapshot.messages) != 1 {
				t.Fatal("source fixture unavailable", err)
			}
			row := snapshot.messages[0]
			gc, code := store.reserveLocalIDs(t.Context(), 1)
			if code != 0 {
				t.Fatal("could not reserve destination ID")
			}
			var target [22]byte
			copy(target[:16], store.replica[:])
			copy(target[16:], gc[:])
			wire := importMoveWire(store.hierarchyXID(inbox), row.sourceKey, target, row.predecessors,
				gidChangeXID(hierarchyXID([16]byte{0x2b}, 5)))

			obj := &box.objects[0]
			obj.Revision++
			obj.Properties[core.MetadataRevision] = core.Integer(obj.Revision)
			obj.Properties[core.ContentRevision] = core.Integer(obj.Revision)
			want := faiError(core.ErrConflict, false)
			if moved {
				obj.Parent = store.byFID[trash].native
				want = syncObjectDeleted
			}

			faiRunOK(t, f, folderOperation(sent, 4, 0, 1))
			faiRunOK(t, f, []byte{0x7e, 4, 1, 2, 1})
			reply, _ := f.run(wire)
			if !bytes.Equal(reply, ropError(0x78, 2, want)) || box.calls != 0 || len(store.messageMoves) != 0 {
				t.Fatalf("unexpected result %x, native calls %d", reply, box.calls)
			}
			source, err := store.observeMailMessages(t.Context(), f.session, inbox)
			if err != nil {
				t.Fatal(err)
			}
			if moved {
				target, err := store.observeMailMessages(t.Context(), f.session, trash)
				if err != nil || len(source.messages) != 0 || len(target.messages) != 1 ||
					target.messages[0].object.ID != row.object.ID {
					t.Fatalf("external move not observed in both folder views: %v", err)
				}
			} else if len(source.messages) != 1 || source.messages[0].cn == row.cn {
				t.Fatal("source did not publish the new revision")
			}
		})
	}
}
