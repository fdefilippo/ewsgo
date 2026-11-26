// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"errors"
	"net/mail"
	"strings"

	"ewsgo/internal/core"
)

const maxContactEditBytes = 64 << 10

// An open contact holds a native revision and the changed fields only.
// Pending edits live with the MAPI handle and reach Zimbra on SaveChanges.
type contactDraft struct {
	base                  core.Contact
	patch                 core.ContactPatch
	lease                 *contentsLease
	baseBytes, patchBytes int
	readOnly, uncertain   bool
	refreshPending        bool
	class                 string
	createdID             core.ID
}

func (s *ramStore) isContactMID(mid uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byMID[mid].special == core.ContactsFolder
}

func canWritePersonalContacts(box core.Mailbox) bool {
	_, canRead := box.(core.ContactReader)
	_, canWrite := box.(core.ContactWriter)
	return canRead && canWrite
}

func (s *ramStore) readEditableContact(ctx context.Context, session core.Session, row messageRow) (*contactDraft, error) {
	box := session.Mailbox()
	reader, canRead := box.(core.ContactReader)
	_, canWrite := box.(core.ContactWriter)
	if !canRead || !canWrite {
		return nil, core.ErrUnsupported
	}
	base, err := reader.GetContact(ctx, row.object.Parent, row.object.ID)
	if err != nil {
		return nil, err
	}
	if base.Identity.Kind != core.ContactItem || base.Identity.ID != row.object.ID || base.Identity.Parent != row.object.Parent {
		return nil, core.ErrNotFound
	}
	if base.Identity.Revision != row.object.Revision {
		return nil, core.ErrConflict
	}
	size := contactEditSize(base)
	if size > maxContactEditBytes {
		return nil, core.ErrLimit
	}
	lease, err := s.reserveContentsMemory(2 * maxContactEditBytes)
	if err != nil {
		return nil, err
	}
	return &contactDraft{base: base, patch: core.ContactPatch{}, lease: lease, baseBytes: size}, nil
}

func (s *ramStore) stageNewContact(ctx context.Context, session core.Session, fid uint64) (*contactDraft, error) {
	folder, err := s.scopedMessageAnchor(ctx, session, fid, false, core.ContactsFolder)
	if err != nil {
		return nil, err
	}
	if !canWritePersonalContacts(session.Mailbox()) {
		return nil, core.ErrUnsupported
	}
	lease, err := s.reserveContentsMemory(2 * maxContactEditBytes)
	if err != nil {
		return nil, err
	}
	base := core.Contact{Identity: core.Object{Kind: core.ContactItem, Parent: folder.native}}
	return &contactDraft{base: base, patch: core.ContactPatch{}, lease: lease, baseBytes: contactEditSize(base), class: "IPM.Note"}, nil
}

func contactEditSize(contact core.Contact) int {
	size := 4096 // Identity, string headers and the staged patch map.
	for _, field := range contactTextFields(&contact) {
		size += len(*field.value)
	}
	for _, address := range contact.Emails {
		size += len(address)
	}
	return size
}

func (d *contactDraft) close() {
	if d != nil {
		d.lease.release()
	}
}

var contactEmailFields = [3]core.ContactField{"EmailAddress1", "EmailAddress2", "EmailAddress3"}

func editableContactField(tag uint32, names *namedPropertyRegistry) core.ContactField {
	property, err := resolveContentsIdentity(tag, names)
	if err != nil {
		return ""
	}
	return contactFieldForProperty(property)
}

func contactFieldForProperty(property contentsProperty) core.ContactField {
	if property.name == (propertyName{}) {
		return contactFields[core.Key{ID: uint32(property.id)}]
	}
	key := core.Key{Set: contactAddressSet, ID: property.name.LID}
	meta, ok := contactProperty(key)
	if !ok || meta.name != property.name {
		return ""
	}
	for slot, field := range contactEmailFields {
		if key.ID == uint32(0x8083+0x10*slot) {
			return field
		}
	}
	return contactFields[key]
}

func readContactSetValue(r *reader, tag uint32) (core.Value, error) {
	switch uint16(tag) {
	case 0x001f:
		value, err := createFolderString(r, len(r.data)-r.pos)
		return core.Text(value), err
	case 0x001e:
		end := bytes.IndexByte(r.data[r.pos:], 0)
		if end < 0 {
			return nil, ErrMalformed
		}
		value := r.take(end + 1)
		return core.Binary(value[:end]), r.err // Decode with the opened object's code page.
	default:
		return readMailDraftSetValue(r, tag)
	}
}

func (d *contactDraft) stage(ctx context.Context, session core.Session, object serverObject, values faiSetRequest, removes []uint32) ([]faiPropertyProblem, error) {
	if err := d.checkAccess(ctx, session, object); err != nil {
		return nil, err
	}
	if d.uncertain {
		return nil, core.ErrOutcomeUnknown
	}
	if d.readOnly {
		return nil, core.ErrAccessDenied
	}
	if err := d.refresh(ctx, session, object); err != nil {
		return nil, err
	}
	var problems []faiPropertyProblem
	set := func(index int, tag uint32, value core.Value, clear bool) {
		field := editableContactField(tag, &object.binding.store.named)
		class := tag>>16 == 0x001a && !clear
		if (!class && field == "") || (!clear && uint16(tag) != 0x001e && uint16(tag) != 0x001f) {
			problems = append(problems, faiPropertyProblem{uint16(index), tag, NotSupported})
			return
		}
		var text string
		if !clear {
			switch v := value.(type) {
			case core.Text:
				text = string(v)
			case core.Binary:
				var err error
				text, err = decodeMailText(restrictionCharset(object.message.codePage), v)
				if err != nil {
					problems = append(problems, faiPropertyProblem{uint16(index), tag, 0x8004011e})
					return
				}
			default:
				problems = append(problems, faiPropertyProblem{uint16(index), tag, NotSupported})
				return
			}
		}
		if class {
			if !strings.EqualFold(text, contactClass) {
				problems = append(problems, faiPropertyProblem{uint16(index), tag, NotSupported})
				return
			}
			d.class = contactClass
			return
		}
		if !validContactText(field, text) {
			problems = append(problems, faiPropertyProblem{uint16(index), tag, 0x80070057})
			return
		}
		newSize := d.patchBytes - len(d.patch[field]) + len(text)
		if newSize > maxContactEditBytes || d.baseBytes+newSize > 2*maxContactEditBytes {
			problems = append(problems, faiPropertyProblem{uint16(index), tag, 0x8007000e})
			return
		}
		d.patchBytes = newSize
		d.patch[field] = text
	}
	for i, value := range values {
		set(i, value.tag, value.value, false)
	}
	for i, tag := range removes {
		kind := uint16(tag)
		if kind != 0 && kind != 0x001e && kind != 0x001f {
			problems = append(problems, faiPropertyProblem{uint16(i), tag, NotSupported})
			continue
		}
		set(i, tag, nil, true)
	}
	return problems, nil
}

func validContactText(field core.ContactField, text string) bool {
	if !validMailText(text) {
		return false
	}
	if strings.HasPrefix(string(field), "EmailAddress") && text != "" {
		address, err := mail.ParseAddress(text)
		return err == nil && strings.ContainsRune(address.Address, '@')
	}
	return true
}

func (d *contactDraft) checkAccess(ctx context.Context, session core.Session, object serverObject) error {
	store := object.binding.store
	if object.message.mid == 0 {
		key, err := store.scopedMessageAnchor(ctx, session, object.fid, false, core.ContactsFolder)
		if err != nil {
			return err
		}
		if key.native != d.base.Identity.Parent {
			return core.ErrNotFound
		}
	} else {
		key, folder, err := store.openedMessageScope(ctx, session, object.fid, *object.message)
		if err != nil {
			return err
		}
		if key.special != core.ContactsFolder || key.id != d.base.Identity.ID || folder.native != d.base.Identity.Parent {
			return core.ErrNotFound
		}
	}
	return nil
}

func (d *contactDraft) preview(row messageRow) (messageRow, error) {
	if len(d.patch) == 0 || d.uncertain {
		return row, nil
	}
	if row.object.Revision != d.base.Identity.Revision {
		return messageRow{}, core.ErrConflict
	}
	preview := d.applyPatch()
	projected, err := contactMessage(preview, preview.Identity.Parent)
	row.object = projected
	return row, err
}

func (d *contactDraft) applyPatch() core.Contact {
	result := d.base
	for _, field := range contactTextFields(&result) {
		if value, found := d.patch[field.field]; found {
			*field.value = value
		}
	}
	for slot, field := range contactEmailFields {
		if value, found := d.patch[field]; found {
			result.Emails[slot] = value
		}
	}
	return result
}

func (d *contactDraft) unsavedRow(ctx context.Context, session core.Session, object serverObject) (messageRow, error) {
	if err := d.checkAccess(ctx, session, object); err != nil {
		return messageRow{}, err
	}
	if d.uncertain {
		return messageRow{}, core.ErrOutcomeUnknown
	}
	props := core.Properties{{ID: 0x001a}: core.Text(d.class)}
	if err := projectContactFields(props, d.applyPatch()); err != nil {
		return messageRow{}, err
	}
	return messageRow{parent: object.fid, special: core.ContactsFolder, object: core.Object{Properties: props}}, nil
}

func (d *contactDraft) save(ctx context.Context, session core.Session, object serverObject, flags byte) error {
	mode, ok := faiSaveMode(flags)
	if !ok {
		return core.ErrInvalid
	}
	if err := d.checkAccess(ctx, session, object); err != nil {
		return err
	}
	if d.uncertain {
		return d.recoverCreation(ctx, session, object, mode == 1)
	}
	creating := d.base.Identity.ID == ""
	if creating && (d.class != contactClass || len(d.patch) == 0) {
		return core.ErrInvalid
	}
	if len(d.patch) == 0 {
		d.readOnly = d.readOnly || mode == 1
		return d.refresh(ctx, session, object)
	}
	if d.readOnly {
		return core.ErrAccessDenied
	}
	writer, ok := session.Mailbox().(core.ContactWriter)
	if !ok {
		return core.ErrUnsupported
	}
	var updated core.Contact
	var err error
	if creating {
		updated, err = writer.CreateContact(ctx, d.base.Identity.Parent, d.patch)
	} else {
		updated, err = writer.UpdateContact(ctx, d.base, d.patch)
	}
	if err == nil && (updated.Identity.ID == "" || !creating && updated.Identity.ID != d.base.Identity.ID ||
		updated.Identity.Parent != d.base.Identity.Parent || updated.Identity.Kind != d.base.Identity.Kind ||
		updated.Identity.Revision == 0 || updated.Identity.Revision < d.base.Identity.Revision) {
		err = core.ErrOutcomeUnknown
	}
	if err != nil {
		d.uncertain = errors.Is(err, core.ErrOutcomeUnknown)
		if creating && d.uncertain && updated.Identity.Parent == d.base.Identity.Parent && updated.Identity.Kind == core.ContactItem {
			d.createdID = updated.Identity.ID
		}
		return err
	}
	return d.completeSave(ctx, session, object, updated, mode == 1)
}

func (d *contactDraft) recoverCreation(ctx context.Context, session core.Session, object serverObject, readOnly bool) error {
	reader, ok := session.Mailbox().(core.ContactReader)
	if !ok || d.createdID == "" {
		return core.ErrOutcomeUnknown
	}
	item, err := reader.GetContact(ctx, d.base.Identity.Parent, d.createdID)
	if err != nil || item.Identity.ID != d.createdID || item.Identity.Parent != d.base.Identity.Parent ||
		item.Identity.Kind != core.ContactItem || item.Identity.Revision == 0 {
		return core.ErrOutcomeUnknown
	}
	d.createdID, d.uncertain = "", false
	return d.completeSave(ctx, session, object, item, readOnly)
}

func (d *contactDraft) completeSave(ctx context.Context, session core.Session, object serverObject, updated core.Contact, readOnly bool) error {
	// The base already occupied at most one allowance and the saved edit at
	// most another. A subsequent edit must fit the same total reservation.
	d.baseBytes, d.readOnly, d.patchBytes = contactEditSize(updated), readOnly, 0
	if d.baseBytes > 2*maxContactEditBytes {
		d.readOnly = true
		updated = core.Contact{Identity: updated.Identity}
	}
	d.base = updated
	clear(d.patch)
	d.refreshPending = true
	return d.refresh(ctx, session, object)
}

func (d *contactDraft) refresh(ctx context.Context, session core.Session, object serverObject) error {
	if !d.refreshPending {
		return nil
	}
	snapshot, err := object.binding.store.observeContacts(ctx, session, object.fid)
	if err != nil {
		return core.ErrOutcomeUnknown
	}
	for _, row := range snapshot.messages {
		if (object.message.mid == 0 || row.mid == object.message.mid) && row.object.ID == d.base.Identity.ID && row.object.Revision >= d.base.Identity.Revision {
			object.message.mid = row.mid
			d.refreshPending = false
			return nil
		}
	}
	return core.ErrOutcomeUnknown
}
