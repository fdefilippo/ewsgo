// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"maps"
	"testing"
	"time"

	"ewsgo/internal/core"
)

type contactImportMailbox struct {
	*personalContactsMailbox
	attempts int
}

func qualifyNativeContactImport(t *testing.T, ctx context.Context, native core.Session, inbox uint64, okROP func([]byte) []byte, run func([]byte, uint32) []byte, softDelete bool, moveTo uint64) {
	t.Helper()
	box := native.Mailbox()
	folder, err := box.(core.SpecialFolderReader).SpecialFolder(ctx, core.ContactsFolder)
	if err != nil {
		t.Fatal("native Contacts unavailable")
	}
	reader, writer := box.(core.ContactReader), box.(core.ContactWriter)
	name := "EWSGo synthetic cache card " + time.Now().UTC().Format("20060102T150405.000000000")
	find := func(ctx context.Context) (matches []core.Contact) {
		t.Helper()
		cards, err := reader.ListContacts(ctx, folder.ID)
		if err != nil {
			t.Error("could not read native fixture")
		}
		for _, card := range cards {
			if card.DisplayName == name {
				matches = append(matches, card)
			}
		}
		return matches
	}
	defer func() {
		cleanup, done := context.WithTimeout(context.Background(), 10*time.Second)
		defer done()
		for _, card := range find(cleanup) {
			if err := writer.TrashContact(cleanup, card); err != nil {
				t.Error("could not trash synthetic contact")
			}
		}
	}()
	okROP(folderOperation(inbox, 4, 0, 1))
	wire := okROP(folderPropertiesOperation(4, 1, 1, 0x36d10102))
	if len(wire) != 55 {
		t.Fatal("Contacts EntryID unavailable")
	}
	sourceFolder := [22]byte(wire[31:53])
	converted := okROP(conversionOperation(0x44, wire[31:]))
	fid := binary.LittleEndian.Uint64(converted[6:])
	okROP(folderOperation(fid, 4, 0, 1))
	reserved := okROP([]byte{0x7f, 4, 0, 1, 0, 0, 0})
	request := syntheticFAIImport()
	request.flags = 0
	copy(request.source[:], reserved[6:])
	wantMID := folderID(ownMoveCounter([16]byte(request.source[:16]), request.source))
	okROP([]byte{0x7e, 4, 1, 2, 1})
	if reply := okROP(faiImportOperation(request)); len(reply) != 14 || binary.LittleEndian.Uint64(reply[6:]) != 0 {
		t.Fatal("ImportMessageChange prematurely returned a MID")
	}
	okROP([]byte{ropFastTransferDestinationConfigure, 4, 3, 4, 1, 0})
	const note, email = "Scheda di prova via FastTransfer", "contact-fixture@example.invalid"
	payload := contactImportPayload(t, name, note, email)
	okROP(faiUploadBuffer(4, payload[:3]))
	okROP(faiUploadBuffer(4, payload[3:]))
	okROP([]byte{1, 4, 4})
	if len(find(ctx)) != 0 {
		t.Fatal("FastTransfer published the native contact before SaveChanges")
	}
	save := []byte{0x0c, 4, 4, 3, 0}
	got := okROP(save)
	if len(got) != 15 || binary.LittleEndian.Uint64(got[7:]) != wantMID || !bytes.Equal(okROP(save), got) {
		t.Fatal("SaveChanges did not return the same reserved MID twice")
	}
	cards := find(ctx)
	if len(cards) != 1 || cards[0].Notes != note || cards[0].Emails[0] != email || cards[0].ImportReference == ([16]byte{}) {
		t.Fatal("native contact differs from staged FastTransfer")
	}
	confirmed, err := box.(core.ContactImportWriter).FindContactByReference(ctx, folder.ID, cards[0].ImportReference)
	if err != nil || confirmed.Identity.ID != cards[0].Identity.ID {
		t.Fatal("the native contact reference could not resolve its exact identity")
	}
	okROP([]byte{1, 4, 3})
	if reply := run(faiImportOperation(request), 65535); !bytes.Equal(reply, ropError(0x72, 3, faiError(errFAIIgnore, false))) {
		t.Fatal("repeated ICS upload was not acknowledged as already applied")
	}
	okROP(openMessageOperation(4, 1, 3, fid, wantMID, 0x0fff, 0))
	if wire := okROP(folderPropertiesOperation(4, 3, 1, 0x1000001f, 0x65e00102)); !bytes.Equal(wire[7:], append(contentsUnicode(note), append([]byte{22, 0}, request.source[:]...)...)) {
		t.Fatal("opened native contact differs from the originally reserved source key")
	}
	okROP([]byte{1, 4, 3})
	edit := nextContactImport(request)
	const revisedNote = "Scheda aggiornata via cache"
	stage := func(r faiImportRequest, note string) {
		t.Helper()
		props := faiSetOperation(3, faiSetRequest{
			{0x001a001f, core.Text(contactClass)}, {0x3001001f, core.Text(name)}, {0x1000001f, core.Text(note)},
		})
		wire := okROP(append(faiImportOperation(r), props...))
		if len(wire) != 22 || !bytes.Equal(wire[14:], propertyProblemsResponse(0x0a, 3, nil)) {
			t.Fatal("batched contact edit did not stage all contact properties")
		}
	}
	stage(edit, revisedNote)
	if current := find(ctx); len(current) != 1 || current[0].Notes != note {
		t.Fatal("staged edit reached Zimbra before save")
	}
	if reply := okROP(save); len(reply) != 15 || binary.LittleEndian.Uint64(reply[7:]) != wantMID {
		t.Fatal("cached edit did not preserve the existing MID")
	}
	edited := find(ctx)
	if len(edited) != 1 || edited[0].Notes != revisedNote || edited[0].Emails[0] != "" ||
		edited[0].Identity.ID != confirmed.Identity.ID || edited[0].ImportReference != confirmed.ImportReference {
		t.Fatal("native edit did not replace mapped fields and preserve contact metadata")
	}
	okROP([]byte{1, 4, 3})
	if reply := run(faiImportOperation(edit), 65535); !bytes.Equal(reply, ropError(0x72, 3, faiError(errFAIIgnore, false))) {
		t.Fatal("replay of a confirmed contact edit was not ignored")
	}
	okROP(openMessageOperation(4, 1, 3, fid, wantMID, 0x0fff, 0))
	if wire := okROP(folderPropertiesOperation(4, 3, 1, 0x65e20102)); !bytes.Equal(wire, faiChangeKeyResponse(3, edit.change, false)) {
		t.Fatal("reopened contact did not retain the imported ChangeKey")
	}
	okROP([]byte{1, 4, 3})
	if moveTo != 0 {
		okROP([]byte{1, 4, 2})
		qualifyNativeContactTrashMove(t, ctx, native, moveTo, sourceFolder, edit, edited[0], okROP)
		return
	}
	stale := nextContactImport(edit)
	stage(stale, "Stale offline notes")
	nativeEdited, err := writer.UpdateContact(ctx, edited[0], core.ContactPatch{"JobTitle": "Concurrent native edit"})
	if err != nil || !bytes.Equal(run(save, 65535), ropError(0x0c, 4, faiError(core.ErrConflict, true))) {
		t.Fatal("nearby SOAP change did not block the older cache upload")
	}
	current := find(ctx)
	if len(current) != 1 || current[0].Identity.Revision != nativeEdited.Identity.Revision ||
		current[0].JobTitle != "Concurrent native edit" || current[0].Notes != revisedNote {
		t.Fatal("stale cache upload changed the native contact")
	}
	if softDelete {
		okROP([]byte{1, 4, 3, 1, 4, 2})
		qualifyNativeContactSoftDelete(t, ctx, reader, current[0], request, okROP, run)
	}
}

func qualifyNativeContactTrashMove(t *testing.T, ctx context.Context, native core.Session, fid uint64, sourceFolder [22]byte, prior faiImportRequest, before core.Contact, okROP func([]byte) []byte) {
	t.Helper()
	catalog, err := privateFolders(ctx, native)
	if err != nil {
		t.Fatal("native Trash unavailable", err)
	}
	trash := catalog.folders[DeletedItems].NativeID
	box := native.Mailbox()
	okROP(folderOperation(fid, 4, 0, 1))
	reserved := okROP([]byte{0x7f, 4, 0, 1, 0, 0, 0})
	destination := [22]byte(reserved[6:28])
	change := nextContactImport(prior).change
	okROP([]byte{0x7e, 4, 1, 2, 1})
	move := importMoveWire(sourceFolder, prior.source, destination, prior.predecessors.marshal(), change)
	response := okROP(move)
	if len(response) != 14 || binary.LittleEndian.Uint64(response[6:]) != 0 || !bytes.Equal(okROP(move), response) {
		t.Fatal("contact move response or replay differs")
	}
	if _, err := box.(core.ContactReader).GetContact(ctx, before.Identity.Parent, before.Identity.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("source contact remains visible after move")
	}
	after, err := box.(core.TrashedContactReader).GetTrashedContact(ctx, trash, before.Identity.ID)
	if err != nil || after.Identity.Parent != trash || after.Identity.Revision <= before.Identity.Revision ||
		after.DisplayName != before.DisplayName || after.Notes != before.Notes {
		t.Fatal("native Trash card did not preserve the same contact", err)
	}
	mid := folderID(ownMoveCounter([16]byte(destination[:16]), destination))
	okROP(openMessageOperation(4, 1, 3, fid, mid, 0x0fff, 0))
	want := append(contentsUnicode(contactClass), 22, 0)
	want = append(want, destination[:]...)
	if wire := okROP(folderPropertiesOperation(4, 3, 1, 0x001a001f, 0x65e00102)); !bytes.Equal(wire[7:], want) {
		t.Fatal("destination contact did not open with the requested SourceKey")
	}
	okROP([]byte{1, 4, 3, 1, 4, 2})
}

func qualifyNativeContactSoftDelete(t *testing.T, ctx context.Context, reader core.ContactReader, contact core.Contact, request faiImportRequest, okROP func([]byte) []byte, run func([]byte, uint32) []byte) {
	t.Helper()
	execute := func(ops []byte) ([]byte, storeResults) { return okROP(ops), storeResults{} }
	config := fullContentsConfig()
	config.flags = 0x21
	okROP(contentsConfigureOperation(config, nil))
	initial := contentsFourTestSets(t, drainFX(t, execute, 2, 16384))
	replica, counter := [16]byte(request.source[:16]), ownMoveCounter([16]byte(request.source[:16]), request.source)
	if !initial.given.contains(replica, counter) {
		t.Fatal("pre-deletion checkpoint omitted the synthetic contact")
	}
	okROP([]byte{1, 4, 2})
	okROP([]byte{0x7e, 4, 1, 2, 1})
	deletion := faiDeleteOperation(request.source)
	if reply := run(deletion, 65535); !bytes.Equal(reply, ropError(0x74, 2, NotSupported)) {
		t.Fatal("hard deletion was accepted for a contact")
	}
	deletion[3] = 0
	okROP(deletion)
	okROP(deletion) // Same SourceKey after the contact entered the dumpster.
	if _, err := reader.GetContact(ctx, contact.Identity.Parent, contact.Identity.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("recoverable contact remains visible in its active folder")
	}
	okROP([]byte{1, 4, 2})
	okROP(contentsConfigureOperation(config, nil))
	uploadContentsWireTestState(t, execute, initial)
	after := contentsFourTestSets(t, drainFX(t, execute, 2, 16384))
	if after.given.contains(replica, counter) {
		t.Fatal("deleted SourceKey remains in the new contact checkpoint")
	}
	okROP([]byte{1, 4, 2})
}

func nextContactImport(previous faiImportRequest) faiImportRequest {
	change := []byte(previous.change)
	change[len(change)-1]++
	out := previous
	out.change = changeXID(change)
	out.predecessors = maps.Clone(previous.predecessors)
	out.predecessors[out.change.namespace()] = out.change.local()
	out.flags = 0x40 // FailOnConflict.
	return out
}

func (b *contactImportMailbox) CreateContactWithReference(_ context.Context, folder core.ID, patch core.ContactPatch, reference [16]byte) (core.Contact, error) {
	b.attempts++
	b.item = core.Contact{
		Identity: core.Object{ID: "contact:867", Parent: folder, Kind: core.ContactItem, Revision: 4,
			Properties: core.Properties{core.MetadataRevision: core.Integer(4), core.ContentRevision: core.Integer(4),
				core.LastModifiedAt: core.Timestamp(hierarchyTestTime)}},
		ImportReference: reference, DisplayName: patch["DisplayName"], Notes: patch["Body"],
		Emails: [3]string{patch["EmailAddress1"]},
	}
	return core.Contact{}, core.ErrOutcomeUnknown
}

func (b *contactImportMailbox) FindContactByReference(_ context.Context, folder core.ID, reference [16]byte) (core.Contact, error) {
	if b.item.Identity.Parent != folder || b.item.ImportReference != reference {
		return core.Contact{}, core.ErrNotFound
	}
	return b.item, nil
}

func contactImportPayload(t *testing.T, name, note, email string) []byte {
	t.Helper()
	fx := &fastTransfer{limit: maxContactEditBytes}
	fx.scalar(0x001a001f, core.Text(contactClass))
	fx.scalar(0x3001001f, core.Text(name))
	fx.scalar(0x1000001f, core.Text(note))
	property, err := corePropertyName(core.Key{Set: contactAddressSet, ID: 0x8083})
	if err != nil {
		t.Fatal(err)
	}
	fx.word(0x8001001f)
	fx.append(property.Set[:], false)
	fx.append([]byte{0}, false)
	fx.word(property.LID)
	fx.scalarValue(0x8001001f, core.Text(email))
	if fx.err != nil {
		t.Fatal(fx.err)
	}
	return fx.data
}

func TestSQLiteContactImportRecoversLostIdentity(t *testing.T) {
	f, base := newContactsFixture(t)
	backend := &contactImportMailbox{personalContactsMailbox: &personalContactsMailbox{contactsMailbox: base}}
	f.session = &testSession{backend}
	f.transport.EnableFAICollectorSpike()
	reopen := sqliteExecuteFixture(t, f)
	db := reopen()
	open := func() uint64 {
		t.Helper()
		f.handles = append(f.handles, 0xffffffff)
		converted := faiRunOK(t, f, conversionOperation(0x44, contactsEntry(t, f)[22:]))
		fid := binary.LittleEndian.Uint64(converted[6:])
		faiRunOK(t, f, folderOperation(fid, 4, 0, 1))
		faiRunOK(t, f, []byte{0x7e, 4, 1, 2, 1})
		return fid
	}
	fid := open()
	store := replicaStore(t, f)
	reserved := faiRunOK(t, f, []byte{0x7f, 4, 0, 1, 0, 0, 0})
	r := syntheticFAIImport()
	r.flags = 0
	copy(r.source[:], reserved[6:])
	mid := folderID(ownMoveCounter(store.replica, r.source))
	upload := func(data []byte) {
		t.Helper()
		faiRunOK(t, f, faiImportOperation(r))
		faiRunOK(t, f, []byte{ropFastTransferDestinationConfigure, 4, 3, 4, 1, 0})
		faiRunOK(t, f, faiUploadBuffer(4, data[:3]))
		faiRunOK(t, f, faiUploadBuffer(4, data[3:]))
		faiRunOK(t, f, []byte{1, 4, 4})
	}
	const name, note, email = "Synthetic cache contact", "Test notes", "fixture@example.invalid"
	save := []byte{0x0c, 4, 4, 3, 0}
	requireError := func(op []byte, slot byte, want error) {
		t.Helper()
		got, _ := f.run(op)
		if !bytes.Equal(got, ropError(op[0], slot, faiError(want, op[0] == 0x0c))) {
			t.Fatalf("unexpected response %x for ROP 0x%02x", got, op[0])
		}
	}
	payload := contactImportPayload(t, name, note, email)
	upload(payload)
	if backend.attempts != 0 {
		t.Fatal("upload dispatched creation before Save")
	}
	requireError(save, 4, core.ErrOutcomeUnknown)
	pending := store.pendingMessageImports[mid]
	if backend.attempts != 1 || pending.NativeID != "" || pending.ContactReference == ([16]byte{}) ||
		pending.ContactReference != backend.item.ImportReference || backend.item.DisplayName != name || backend.item.Notes != note ||
		backend.item.Emails[0] != email {
		t.Fatal("native contact or durable claim differs from upload")
	}
	if _, err := store.observeContacts(t.Context(), f.session, fid); !errors.Is(err, core.ErrOutcomeUnknown) {
		t.Fatal("observation assigned another MID to a pending contact")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopen()
	store = replicaStore(t, f)
	if reopenedFID := open(); reopenedFID != fid {
		t.Fatal("Contacts FID changed after reopening SQLite")
	}
	upload(contactImportPayload(t, name, "Different payload", email))
	requireError(save, 4, core.ErrConflict)
	faiRunOK(t, f, []byte{1, 4, 3})
	upload(payload)
	item := backend.item
	backend.item = core.Contact{} // A missing match cannot authorize another CreateContact.
	got, _ := f.run(save)
	if binary.LittleEndian.Uint32(got[2:]) == 0 || backend.attempts != 1 {
		t.Fatal("missing native match retried CreateContact or acknowledged it")
	}
	backend.item = item
	saved := faiRunOK(t, f, save)
	if len(saved) != 15 || binary.LittleEndian.Uint64(saved[7:]) != mid || backend.attempts != 1 {
		t.Fatal("recovery did not publish the reserved contact MID exactly once")
	}
	view, err := store.observeContacts(t.Context(), f.session, fid)
	if err != nil || len(view.messages) != 1 || view.messages[0].mid != mid || store.messages[store.byMID[mid]].change != r.change {
		t.Fatal("published contact does not preserve the imported identity and ChangeKey", err)
	}
	requireError(faiImportOperation(r), 3, errFAIIgnore)
}
