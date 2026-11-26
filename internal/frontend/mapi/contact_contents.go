// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"net/mail"
	"slices"
	"strings"
	"time"

	"ewsgo/internal/core"
)

const contactClass = "IPM.Contact"
const contactAddressSet = "00062004-0000-0000-c000-000000000046"

type contactTextProperty struct {
	key   core.Key
	field core.ContactField
	value *string
}

// MS-OXOCNTC contact name, telephone and postal address properties.
func contactTextFields(c *core.Contact) []contactTextProperty {
	return []contactTextProperty{
		{core.Key{ID: 0x1000}, "Body", &c.Notes},
		{core.Key{ID: 0x3001}, "DisplayName", &c.DisplayName},
		{core.Key{ID: 0x3a06}, "GivenName", &c.GivenName},
		{core.Key{ID: 0x3a11}, "Surname", &c.Surname},
		{core.Key{ID: 0x3a16}, "CompanyName", &c.CompanyName},
		{core.Key{ID: 0x3a17}, "JobTitle", &c.JobTitle},
		{core.Key{ID: 0x3a18}, "Department", &c.Department},
		{core.Key{ID: 0x3a44}, "MiddleName", &c.MiddleName},
		{core.Key{ID: 0x3a4f}, "Nickname", &c.Nickname},
		{core.Key{ID: 0x3a51}, "BusinessHomePage", &c.WebPage},
		{core.Key{ID: 0x3a08}, "BusinessPhone", &c.BusinessPhone},
		{core.Key{ID: 0x3a09}, "HomePhone", &c.HomePhone},
		{core.Key{ID: 0x3a1b}, "BusinessPhone2", &c.BusinessPhone2},
		{core.Key{ID: 0x3a1c}, "MobilePhone", &c.MobilePhone},
		{core.Key{ID: 0x3a1f}, "OtherTelephone", &c.OtherPhone},
		{core.Key{ID: 0x3a24}, "BusinessFax", &c.BusinessFax},
		{core.Key{ID: 0x3a25}, "HomeFax", &c.HomeFax},
		{core.Key{ID: 0x3a2f}, "HomePhone2", &c.HomePhone2},
		{core.Key{ID: 0x3a59}, "HomeCity", &c.HomeAddress.City},
		{core.Key{ID: 0x3a5a}, "HomeCountryOrRegion", &c.HomeAddress.Country},
		{core.Key{ID: 0x3a5b}, "HomePostalCode", &c.HomeAddress.PostalCode},
		{core.Key{ID: 0x3a5c}, "HomeState", &c.HomeAddress.State},
		{core.Key{ID: 0x3a5d}, "HomeStreet", &c.HomeAddress.Street},
		{core.Key{ID: 0x3a5f}, "OtherCity", &c.OtherAddress.City},
		{core.Key{ID: 0x3a60}, "OtherCountryOrRegion", &c.OtherAddress.Country},
		{core.Key{ID: 0x3a61}, "OtherPostalCode", &c.OtherAddress.PostalCode},
		{core.Key{ID: 0x3a62}, "OtherState", &c.OtherAddress.State},
		{core.Key{ID: 0x3a63}, "OtherStreet", &c.OtherAddress.Street},
		{core.Key{Set: contactAddressSet, ID: 0x8005}, "", &c.FileAs},
		{core.Key{Set: contactAddressSet, ID: 0x8045}, "BusinessStreet", &c.BusinessAddress.Street},
		{core.Key{Set: contactAddressSet, ID: 0x8046}, "BusinessCity", &c.BusinessAddress.City},
		{core.Key{Set: contactAddressSet, ID: 0x8047}, "BusinessState", &c.BusinessAddress.State},
		{core.Key{Set: contactAddressSet, ID: 0x8048}, "BusinessPostalCode", &c.BusinessAddress.PostalCode},
		{core.Key{Set: contactAddressSet, ID: 0x8049}, "BusinessCountryOrRegion", &c.BusinessAddress.Country},
	}
}
func contactMessage(c core.Contact, parent core.ID) (core.Object, error) {
	item := c.Identity
	if id, ok := strings.CutPrefix(string(item.ID), core.ContactIDPrefix); !ok || id == "" || item.Kind != core.ContactItem || item.Parent != parent {
		return core.Object{}, core.ErrInvalid
	}
	metadataRevision, hasMetadataRevision := item.Properties[core.MetadataRevision].(core.Integer)
	contentRevision, hasContentRevision := item.Properties[core.ContentRevision].(core.Integer)
	modified, hasModified := item.Properties[core.LastModifiedAt].(core.Timestamp)
	if !hasMetadataRevision || metadataRevision <= 0 || uint64(metadataRevision) != uint64(item.Revision) ||
		!hasContentRevision || contentRevision <= 0 || !hasModified || time.Time(modified).Year() < 1601 || time.Time(modified).Year() > 9999 {
		return core.Object{}, core.ErrInvalid
	}
	props := core.Properties{
		core.MetadataRevision: metadataRevision, core.ContentRevision: contentRevision,
		core.LastModifiedAt: modified, core.IsRead: core.Bool(true),
	}
	if err := projectContactFields(props, c); err != nil {
		return core.Object{}, err
	}
	return core.Object{ID: item.ID, Parent: parent, Kind: core.Message, Revision: item.Revision, Properties: props}, nil
}

func projectContactFields(props core.Properties, c core.Contact) error {
	props[core.Subject] = core.Text(c.DisplayName)
	for _, text := range contactTextFields(&c) {
		value := *text.value
		if !validMailText(value) {
			return core.ErrInvalid
		}
		if value != "" {
			props[text.key] = core.Text(value)
		}
	}
	putContactEmails(props, c)
	return nil
}

func (s *ramStore) observeContacts(ctx context.Context, session core.Session, fid uint64) (messageSnapshot, error) {
	return s.observeScopedMessageSet(ctx, session, fid, false, core.ContactsFolder, func(ctx context.Context, box core.Mailbox, key folderKey) ([]core.Object, error) {
		reader, ok := box.(core.ContactReader)
		if !ok {
			return nil, core.ErrUnsupported
		}
		contacts, err := reader.ListContacts(ctx, key.native)
		if err != nil {
			return nil, err
		}
		if len(contacts) > core.MaxMailContentIndexItems {
			return nil, core.ErrLimit
		}
		items := make([]core.Object, 0, len(contacts))
		for _, contact := range contacts {
			if s.contactAwaitingImport(fid, contact) {
				return nil, core.ErrOutcomeUnknown
			}
			item, err := contactMessage(contact, key.native)
			if err != nil {
				return nil, err
			}
			items = append(items, item)
		}
		return items, nil
	})
}

func trashedContactNodes(ctx context.Context, reader core.TrashedContactReader, folder core.ID) ([]messageNode, error) {
	contacts, err := reader.ListTrashedContacts(ctx, folder)
	if err != nil {
		return nil, err
	}
	if len(contacts) > core.MaxMailContentIndexItems {
		return nil, core.ErrLimit
	}
	items := make([]core.Object, 0, len(contacts))
	for _, contact := range contacts {
		item, err := contactMessage(contact, folder)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return scopedMessageNodes(items, folder, false, core.ContactsFolder)
}

func (s *ramStore) readOpenedContact(ctx context.Context, session core.Session, fid, mid uint64, key messageKey, folder folderKey) (messageRow, error) {
	box := session.Mailbox()
	var contact core.Contact
	var err error
	special := core.ContactsFolder
	if folder.native == s.trash {
		reader, ok := box.(core.TrashedContactReader)
		if !ok {
			return messageRow{}, core.ErrUnsupported
		}
		contact, err = reader.GetTrashedContact(ctx, folder.native, key.id)
		special = 0
	} else {
		reader, ok := box.(core.ContactReader)
		if !ok {
			return messageRow{}, core.ErrUnsupported
		}
		contact, err = reader.GetContact(ctx, folder.native, key.id)
	}
	if err != nil {
		return messageRow{}, err
	}
	if contact.Identity.ID != key.id || contact.Identity.Parent != folder.native {
		return messageRow{}, core.ErrNotFound
	}
	item, err := contactMessage(contact, folder.native)
	if err != nil {
		return messageRow{}, err
	}
	if _, err = s.scopedMessageAnchor(ctx, session, fid, false, special); err != nil {
		return messageRow{}, err
	}
	s.mu.Lock()
	version := s.messages[key]
	s.mu.Unlock()
	if !version.present || version.parent != fid {
		return messageRow{}, core.ErrNotFound
	}
	row := messageRow{parent: fid, mid: mid, special: core.ContactsFolder, read: true, object: item, sourceKey: s.hierarchyXID(mid)}
	if version.nativeRevision == item.Revision {
		row.changeKey = s.messageChangeKey(version)
	}
	return row, nil
}

func (s *ramStore) moveContactsToTrash(ctx context.Context, session core.Session, source, destination uint64, mids []uint64) (uint32, bool) {
	from, err := s.scopedMessageAnchor(ctx, session, source, false, core.ContactsFolder)
	if errors.Is(err, core.ErrNotFound) {
		return NotSupported, false
	}
	if err != nil {
		return storeError(err), false
	}
	to, err := s.messageAnchor(ctx, session, destination, false)
	if err != nil {
		return storeError(err), false
	}
	if to.native == "" || to.native == from.native {
		return NotSupported, false
	}
	box := session.Mailbox()
	reader, canRead := box.(core.ContactReader)
	writer, canWrite := box.(core.ContactWriter)
	if !canRead || !canWrite {
		return NotSupported, false
	}
	return executeMessageBatch(ctx, mids, func(mid uint64) error {
		key, folder, err := s.openedMessageScope(ctx, session, source, openedMessage{mid: mid})
		if err != nil {
			return err
		}
		if key.special != core.ContactsFolder || folder != from {
			return core.ErrNotFound
		}
		contact, err := reader.GetContact(ctx, from.native, key.id)
		if err != nil {
			return err
		}
		if contact.Identity.Kind != core.ContactItem || contact.Identity.ID != key.id || contact.Identity.Parent != from.native {
			return core.ErrNotFound
		}
		return writer.TrashContact(ctx, contact)
	})
}

// Caller holds contentsMu. Validate the whole batch before making native writes.
func (s *ramStore) softDeleteContacts(ctx context.Context, session core.Session, fid uint64, ids []core.ID, writer core.ContactSoftDeleter) error {
	folder, err := s.scopedMessageAnchor(ctx, session, fid, false, core.ContactsFolder)
	if err != nil {
		return err
	}
	reader, ok := session.Mailbox().(core.ContactReader)
	if !ok {
		return core.ErrUnsupported
	}
	targets := make([]core.Contact, 0, len(ids))
	for _, id := range ids {
		item, err := reader.GetContact(ctx, folder.native, id)
		if errors.Is(err, core.ErrNotFound) {
			continue // Retransmission after the contact left this folder.
		}
		if err != nil {
			return err
		}
		if item.Identity.ID != id || item.Identity.Kind != core.ContactItem || item.Identity.Parent != folder.native || item.Identity.Revision == 0 {
			return core.ErrConflict
		}
		targets = append(targets, core.Contact{Identity: item.Identity})
	}
	for _, item := range targets {
		if err := writer.SoftDeleteContact(ctx, item); err != nil {
			return err
		}
	}
	return nil
}

var contactFields = func() map[core.Key]core.ContactField {
	fields := make(map[core.Key]core.ContactField)
	for _, property := range contactTextFields(&core.Contact{}) {
		fields[property.key] = property.field
	}
	return fields
}()

var contactEmailTypes = func() map[uint32]uint16 {
	types := map[uint32]uint16{0x8028: 0x1003, 0x8029: 0x0003}
	for base := uint32(0x8080); base <= 0x80a0; base += 0x10 {
		for _, offset := range []uint32{0, 2, 3, 4} {
			types[base+offset] = 0x001f
		}
		types[base+5] = 0x0102
	}
	return types
}()

func putContactEmails(props core.Properties, contact core.Contact) {
	var slots core.Int32s
	var mask core.Integer
	for slot, raw := range contact.Emails {
		// Native contact fields can contain legacy text or an RFC 5322 mailbox.
		// Only project an electronic identity for a single valid SMTP address.
		parsed, err := mail.ParseAddress(raw)
		if err != nil || !strings.ContainsRune(parsed.Address, '@') || !validMailText(parsed.Address) || !validMailText(parsed.Name) {
			continue
		}
		address := parsed.Address
		display := parsed.Name
		if display == "" {
			display = contact.DisplayName
		}
		if display == "" {
			display = address
		}
		entry := append([]byte("\x00\x00\x00\x00"+oneOffProvider), 0, 0, 1, 0x80)
		entry = append(entry, contentsUnicode(display)...)
		entry = append(entry, contentsUnicode("SMTP")...)
		entry = append(entry, contentsUnicode(address)...)
		base := uint32(0x8080 + 0x10*slot)
		props[core.Key{Set: contactAddressSet, ID: base}] = core.Text(display)
		props[core.Key{Set: contactAddressSet, ID: base + 2}] = core.Text("SMTP")
		props[core.Key{Set: contactAddressSet, ID: base + 3}] = core.Text(address)
		props[core.Key{Set: contactAddressSet, ID: base + 4}] = core.Text(address)
		props[core.Key{Set: contactAddressSet, ID: base + 5}] = core.Binary(entry)
		slots = append(slots, int32(slot))
		mask |= 1 << slot
	}
	if len(slots) != 0 {
		props[core.Key{Set: contactAddressSet, ID: 0x8028}] = slots
		props[core.Key{Set: contactAddressSet, ID: 0x8029}] = mask
	}
}

func contactProperty(key core.Key) (contentsProperty, bool) {
	var kind uint16
	if _, isText := contactFields[key]; isText {
		kind = 0x001f
	} else if key.Set == contactAddressSet && !key.IsName() {
		kind = contactEmailTypes[key.ID]
	}
	if kind == 0 {
		return contentsProperty{}, false
	}
	if key.Set == "" {
		return fixedContentsProperty(key.ID<<16 | uint32(kind)), true
	}
	name, err := corePropertyName(key)
	return contentsProperty{name: name, kind: kind}, err == nil
}

func projectContact(row messageRow) projectedMessage {
	values := contentsValues{}
	putContentsValue(values, 0x001a001f, core.Text(contactClass))
	putContentsValue(values, 0x0037001f, row.object.Properties[core.Subject])
	putContentsValue(values, 0x0e070003, core.Integer(1))
	bytes := 512
	for key, value := range row.object.Properties {
		if property, ok := contactProperty(key); ok {
			values[property] = value
			bytes += 64
			switch v := value.(type) {
			case core.Text:
				bytes += 2 * len(v)
			case core.Binary:
				bytes += len(v)
			case core.Int32s:
				bytes += 4 * len(v)
			}
		}
	}
	return projectedMessage{values: values, bytes: bytes}
}

func contactSchema(p contentsProperty) bool {
	if p.name == (propertyName{}) {
		_, ok := contactProperty(core.Key{ID: uint32(p.id)})
		return p.kind == 0x001f && ok
	}
	property, ok := contactProperty(core.Key{Set: contactAddressSet, ID: p.name.LID})
	return ok && property == p
}

func contactContentsSchema(associated bool) func(contentsProperty) bool {
	fai := contentsProjectionSchema(false, associated)
	return func(p contentsProperty) bool {
		if contactSchema(p) || fai(p) {
			return true
		}
		return p == fixedContentsProperty(0x001a001f) || p == fixedContentsProperty(0x0037001f) || p == fixedContentsProperty(0x0e070003)
	}
}

func registerContactNames(ctx context.Context, names *namedPropertyRegistry) error {
	var properties []propertyName
	for key := range contactFields {
		if key.Set != "" {
			prop, _ := contactProperty(key)
			properties = append(properties, prop.name)
		}
	}
	for id := range contactEmailTypes {
		prop, _ := contactProperty(core.Key{Set: contactAddressSet, ID: id})
		properties = append(properties, prop.name)
	}
	slices.SortFunc(properties, func(a, b propertyName) int { return int(a.LID) - int(b.LID) })
	if _, code := names.ids(ctx, properties, true); code != 0 {
		return core.ErrUnsupported
	}
	return nil
}

func contactVariableProperty(tag uint32) bool {
	id, kind := tag>>16, uint16(tag)
	if id >= 0x8001 && id <= 0xfffe && (kind == 0x0102 || kind == 0x1003) {
		return true
	}
	if kind != 0 && kind != 0x001e && kind != 0x001f {
		return false
	}
	_, fixed := contactFields[core.Key{ID: id}]
	return (fixed && id != 0x3001) || (id >= 0x8001 && id <= 0xfffe)
}

func openedContactField(row messageRow, tag uint32, names *namedPropertyRegistry, unicode bool, cp uint32, budget int) (folderPropertyValue, bool) {
	property, err := resolveContentsIdentity(tag, names)
	if err != nil {
		return folderPropertyValue{}, false
	}
	key := core.Key{ID: uint32(property.id)}
	if property.name != (propertyName{}) {
		key = core.Key{Set: contactAddressSet, ID: property.name.LID}
	}
	canonical, ok := contactProperty(key)
	if !ok || canonical.id != property.id || canonical.name != property.name {
		return folderPropertyValue{}, false
	}
	value := row.object.Properties[key]
	if value == nil {
		return folderPropertyValue{kind: 0x000a, code: PropertyNotFound}, true
	}
	kind := property.kind
	if kind == 0 {
		kind = canonical.kind
		if kind == 0x001f && !unicode {
			kind = 0x001e
		}
	}
	limit := maxFolderStringBytes
	if contactVariableProperty(tag) {
		limit = min(limit, budget+4)
	}
	var data []byte
	switch v := value.(type) {
	case core.Text:
		if len(v) <= limit {
			data, ok = propertyString(string(v), kind, cp)
		} else {
			return folderPropertyValue{kind: 0x000a, code: 0x8007000e}, true
		}
	case core.Integer:
		ok = kind == 3
		data = binary.LittleEndian.AppendUint32(nil, uint32(v))
	case core.Int32s:
		ok = kind == 0x1003
		data = encodeInt32s(v)
	case core.Binary:
		ok = kind == 0x0102
		if len(v)+2 > limit {
			return folderPropertyValue{kind: 0x000a, code: 0x8007000e}, true
		}
		data = append(binary.LittleEndian.AppendUint16(nil, uint16(len(v))), v...)
	default:
		ok = false
	}
	if !ok {
		return folderPropertyValue{kind: 0x000a, code: NotSupported}, true
	}
	if len(data) > limit {
		return folderPropertyValue{kind: 0x000a, code: 0x8007000e}, true
	}
	return folderPropertyValue{kind: kind, data: data}, true
}

func encodeInt32s(values core.Int32s) []byte {
	data := make([]byte, 4, 4+4*len(values))
	binary.LittleEndian.PutUint32(data, uint32(len(values)))
	for _, value := range values {
		data = binary.LittleEndian.AppendUint32(data, uint32(value))
	}
	return data
}

func contactPropertyTags(props core.Properties, names *namedPropertyRegistry) []uint32 {
	names.mu.Lock()
	defer names.mu.Unlock()
	var tags []uint32
	for key := range props {
		p, ok := contactProperty(key)
		if !ok {
			continue
		}
		id := p.id
		if p.name != (propertyName{}) {
			id = names.byKey[p.name]
		}
		if id != 0 {
			tags = append(tags, uint32(id)<<16|uint32(p.kind))
		}
	}
	return tags
}
