// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestPersonalContactsFolderAdvertisesCreateContents(t *testing.T) {
	f, metadata := newContactsFixture(t)
	entry := contactsEntry(t, f)
	converted, _ := f.run(conversionOperation(0x44, entry[22:]))
	fid := binary.LittleEndian.Uint64(converted[6:])
	f.run(folderOperation(fid, 4, 0, 1))
	props := folderPropertiesOperation(4, 1, 1, folderAccessTag)
	readAccess := func() uint32 {
		t.Helper()
		row, c := f.run(props)
		if len(row) != 11 || c.properties.Errors != 0 {
			t.Fatalf("invalid access property row: %x", row)
		}
		return binary.LittleEndian.Uint32(row[7:])
	}
	const createContents = 0x10
	if got := readAccess(); got&createContents != 0 {
		t.Fatalf("metadata-only address book advertises create: %x", got)
	}
	f.session = &testSession{box: &uncertainContactMailbox{contactsMailbox: metadata}}
	if got := readAccess(); got&createContents == 0 {
		t.Fatalf("writable address book missing create: %x", got)
	}
	f.open(int(Inbox))
	if got := readAccess(); got&createContents != 0 {
		t.Fatalf("contact capability advertised for Inbox: %x", got)
	}
}

type uncertainContactMailbox struct {
	*contactsMailbox
	creates int
	created core.Contact
}

func (b *uncertainContactMailbox) CreateContact(context.Context, core.ID, core.ContactPatch) (core.Contact, error) {
	b.creates++
	return core.Contact{Identity: b.created.Identity}, core.ErrOutcomeUnknown
}

func (b *uncertainContactMailbox) GetContact(_ context.Context, parent, id core.ID) (core.Contact, error) {
	if b.created.Identity.Parent == parent && b.created.Identity.ID == id && id != "" {
		return b.created, nil
	}
	return core.Contact{}, core.ErrNotFound
}

func (b *uncertainContactMailbox) ListContacts(_ context.Context, parent core.ID) ([]core.Contact, error) {
	if b.created.Identity.Parent == parent && b.created.Identity.ID != "" {
		return []core.Contact{b.created}, nil
	}
	return nil, nil
}

func (*uncertainContactMailbox) UpdateContact(context.Context, core.Contact, core.ContactPatch) (core.Contact, error) {
	return core.Contact{}, core.ErrUnsupported
}

func (*uncertainContactMailbox) TrashContact(context.Context, core.Contact) error {
	return core.ErrUnsupported
}

func TestContactCreateDoesNotRetryUnknownNativeOutcome(t *testing.T) {
	for _, tc := range []struct {
		name         string
		acknowledged bool
	}{{"no native acknowledgement", false}, {"native ID acknowledged", true}} {
		t.Run(tc.name, func(t *testing.T) { testUncertainContactCreate(t, tc.acknowledged) })
	}
}

func testUncertainContactCreate(t *testing.T, acknowledged bool) {
	t.Helper()
	f, contacts := newContactsFixture(t)
	box := &uncertainContactMailbox{contactsMailbox: contacts}
	if acknowledged {
		box.created = core.Contact{DisplayName: "MAPI contact", Identity: core.Object{
			Kind: core.ContactItem, ID: "contact:42", Parent: contacts.contacts.ID, Revision: 4,
			Properties: core.Properties{
				core.MetadataRevision: core.Integer(4), core.ContentRevision: core.Integer(4),
				core.LastModifiedAt: core.Timestamp(time.Now().UTC()),
			},
		}}
	}
	f.session = &testSession{box: box}
	object, code := f.state.mailbox.object(f.handles[0], 4)
	if code != 0 {
		t.Fatal("private store not open")
	}
	ctx := context.Background()
	store := object.binding.store
	fid, err := store.specialFolderFID(ctx, f.session, contactsMetadataRole)
	if err != nil {
		t.Fatal(err)
	}
	draft, err := store.stageNewContact(ctx, f.session, fid)
	if err != nil {
		t.Fatal(err)
	}
	defer draft.close()
	object.fid = fid
	object.message = &openedMessage{contactDraft: draft}
	stage := faiSetRequest{
		{tag: 0x001a001f, value: core.Text(contactClass)},
		{tag: 0x3001001f, value: core.Text("MAPI contact")},
	}
	if problems, err := draft.stage(ctx, f.session, object, stage, nil); err != nil || len(problems) != 0 {
		t.Fatal("could not stage contact", err)
	}
	if err := draft.save(ctx, f.session, object, 2); !errors.Is(err, core.ErrOutcomeUnknown) || object.message.mid != 0 {
		t.Fatal("expected an uncertain result with no MID", err)
	}
	err = draft.save(ctx, f.session, object, 2)
	if box.creates != 1 {
		t.Fatal("uncertain creation was repeated")
	}
	if acknowledged {
		if err != nil || object.message.mid == 0 {
			t.Fatal("acknowledged creation was not resolved by readback", err)
		}
	} else if !errors.Is(err, core.ErrOutcomeUnknown) || object.message.mid != 0 {
		t.Fatal("MID assigned to an unacknowledged creation", err)
	}
}
