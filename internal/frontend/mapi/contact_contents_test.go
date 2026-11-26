// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"reflect"
	"testing"

	"ewsgo/internal/core"
)

type personalContactsMailbox struct {
	*contactsMailbox
	item core.Contact
}

func (b *personalContactsMailbox) ListContacts(_ context.Context, folder core.ID) ([]core.Contact, error) {
	if folder != b.contacts.ID {
		return nil, core.ErrNotFound
	}
	if b.item.Identity.ID == "" {
		return nil, nil
	}
	return []core.Contact{b.item}, nil
}

func (b *personalContactsMailbox) GetContact(_ context.Context, folder, id core.ID) (core.Contact, error) {
	if folder != b.contacts.ID || id != b.item.Identity.ID {
		return core.Contact{}, core.ErrNotFound
	}
	return b.item, nil
}

func TestPersonalContactsContentsAndOpenMessage(t *testing.T) {
	f, base := newContactsFixture(t)
	box := &personalContactsMailbox{contactsMailbox: base, item: core.Contact{
		Identity: core.Object{ID: "contact:123", Parent: base.contacts.ID, Kind: core.ContactItem, Revision: 7,
			Properties: core.Properties{core.MetadataRevision: core.Integer(7), core.ContentRevision: core.Integer(5), core.LastModifiedAt: core.Timestamp(hierarchyTestTime)}},
		DisplayName: "Ada Lovelace", GivenName: "Ada", FileAs: "Lovelace, Ada", Notes: "Synthetic contact note",
	}}
	f.session = &testSession{box: box}
	f.transport.EnableHierarchyDownloadSpike()
	f.transport.EnableContentsDownloadSpike()
	f.transport.EnableContentsTableSpike()
	f.handles = append(f.handles, 0xffffffff, 0xffffffff)
	entry := contactsEntry(t, f)
	binding := f.state.mailbox.objects[f.handles[0]].binding
	s := binding.store
	fid, err := parseFolderEntryID(entry, binding.mailboxGUID, s.replica)
	if err != nil {
		t.Fatal("invalid contacts folder identity", err)
	}
	f.run(folderOperation(fid, 4, 0, 1))
	fileAs, err := corePropertyName(core.Key{Set: contactAddressSet, ID: 0x8005})
	if err != nil {
		t.Fatal(err)
	}
	ids, code := s.named.ids(t.Context(), []propertyName{fileAs}, true)
	if code != 0 || len(ids) != 1 {
		t.Fatal("could not bind PidLidFileUnder")
	}
	namedTag := uint32(ids[0])<<16 | 0x001f
	table, _ := openTableTest(t, f, 0x10) // NoNotifications.
	if table.special != core.ContactsFolder {
		t.Fatal("contacts table classified as mail")
	}
	tags := []uint32{0x674a0014, 0x001a001f, 0x3001001f, 0x3a06001f, namedTag}
	f.run(tableColumnsWire(2, tags...))
	out, _ := f.run(tableQueryWire(2, 0, 1, 10))
	rows := decodeTableRows(t, out, tags)
	if len(rows) != 1 || !reflect.DeepEqual(rows[0][1:], []any{"IPM.Contact", "Ada Lovelace", "Ada", "Lovelace, Ada"}) {
		t.Fatal("unexpected contact table properties", rows)
	}
	mid := rows[0][0].(uint64)
	out, _ = f.run(openMessageOperation(4, 1, 3, fid, mid, 0x0fff, 0))
	if len(out) <= 6 || !bytes.Equal(out[:6], ropError(3, 3, 0)) {
		t.Fatal("could not open contact", out)
	}
	properties := folderPropertiesOperation(4, 3, 1, 0x0fff0102, 0x1000001f, namedTag, 0x65e20102)
	out, result := f.run(properties)
	if result.properties.Values != 4 || !bytes.Contains(out, contentsUnicode(box.item.Notes)) ||
		!bytes.Contains(out, marshalMessageEntryID(binding.mailboxGUID, s.replica, fid, mid)) {
		t.Fatal("opened contact is missing its identity or notes", out, result.properties)
	}
	box.item.Identity.Properties[core.MetadataRevision] = core.Integer(8)
	box.item.Identity.Revision = 8
	box.item.Notes = "Updated contact note"
	view, err := s.observeContacts(t.Context(), f.session, fid)
	if err != nil || len(view.messages) != 1 || view.messages[0].mid != mid {
		t.Fatal("native update changed the MAPI identity", err)
	}
	out, result = f.run(properties)
	if result.properties.Values != 4 || !bytes.Contains(out, contentsUnicode(box.item.Notes)) {
		t.Fatal("opened contact did not reread native properties")
	}
	out, _ = f.run(openMessageOperation(4, 0, 3, f.fids[Inbox], mid, 0x0fff, 0))
	if !bytes.Equal(out, ropError(3, 3, PropertyNotFound)) {
		t.Fatal("contact opened through the mail namespace")
	}
	if _, err := s.observeMailMessages(t.Context(), f.session, fid); err == nil {
		t.Fatal("mail synchronization accepted the contacts folder")
	}
	box.state.Owner = false
	out, _ = f.run(tableQueryWire(2, 0, 1, 10))
	if !bytes.Equal(out, ropError(0x15, 2, accessDenied)) {
		t.Fatal("fixed contact table available after owner permission was revoked")
	}
	box.state.Owner = true
	f.run([]byte{1, 4, 2})
	config := fullContentsConfig()
	config.flags = 0x21 // Normal, Unicode, full changes.
	configureContentsTest(t, f, config)
	initial := drainFX(t, f.run, 2, 73)
	if !containsContactFX(t, initial, 0x1000001f, box.item.Notes) || !containsContactFX(t, initial, 0x001a001f, contactClass) {
		t.Fatal("cache stream is missing its contact payload")
	}
	state := contentsFourTestSets(t, initial)
	if len(state.given.replicas) == 0 || len(state.seen.replicas) == 0 {
		t.Fatal("cache stream did not track the native contact")
	}
	f.run([]byte{1, 4, 2})
	box.item.Identity.ID = ""
	configureContentsTest(t, f, config)
	uploadContentsWireTest(t, f, state)
	deleted := drainFX(t, f.run, 2, 73)
	if len(contentsFourTestSets(t, deleted).given.replicas) != 0 {
		t.Fatal("cache checkpoint kept a removed contact")
	}
	deletion := false
	counter := globalCount(folderGlobalCount(mid))
	wantDeletion := append(append([]byte{1, 0, 6}, counter[:]...), 0)
	for _, token := range decodeTestContents(t, deleted) {
		if token.tag == 0x67e50102 {
			deletion = bytes.Equal(token.value.([]byte), wantDeletion)
		}
	}
	if !deletion {
		t.Fatal("native removal did not produce a contact deletion delta")
	}
}

func containsContactFX(t *testing.T, raw []byte, tag uint32, value string) bool {
	t.Helper()
	for _, token := range decodeTestContents(t, raw) {
		if token.tag == tag && token.value == value {
			return true
		}
	}
	return false
}
