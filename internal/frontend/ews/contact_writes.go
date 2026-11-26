// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"errors"
	"strings"

	"ewsgo/internal/core"
)

func contactWriteField(uri node) (core.ContactField, string, string, error) {
	name := uri.attr("FieldURI")
	if isElement(uri, types, "FieldURI") && len(uri.Children) == 0 {
		switch field := contactFields[name]; field {
		case "", "FileAs", "LastModifiedTime":
		default:
			return core.ContactField(field), field, "", nil
		}
	}
	if isElement(uri, types, "IndexedFieldURI") && len(uri.Children) == 0 {
		key := uri.attr("FieldIndex")
		switch name {
		case "contacts:EmailAddress":
			if validContactIndex("EmailAddresses", key) {
				return core.ContactField(key), "EmailAddresses", key, nil
			}
		case "contacts:PhoneNumber":
			if validContactIndex("PhoneNumbers", key) {
				return core.ContactField(key), "PhoneNumbers", key, nil
			}
		default:
			property, ok := strings.CutPrefix(name, "contacts:PhysicalAddress:")
			if ok && validContactIndex("PhysicalAddresses", key) && validContactAddressField(property) {
				return core.ContactField(key + property), "PhysicalAddresses", key, nil
			}
		}
	}
	return "", "", "", core.ErrUnsupported
}

func validContactIndex(field, key string) bool {
	switch field {
	case "EmailAddresses":
		return key == "EmailAddress1" || key == "EmailAddress2" || key == "EmailAddress3"
	case "PhoneNumbers":
		switch key {
		case "BusinessPhone", "BusinessPhone2", "HomePhone", "HomePhone2", "MobilePhone", "BusinessFax", "HomeFax", "OtherTelephone":
			return true
		}
	case "PhysicalAddresses":
		return key == "Business" || key == "Home" || key == "Other"
	}
	return false
}

func validContactAddressField(name string) bool {
	switch name {
	case "Street", "City", "State", "CountryOrRegion", "PostalCode":
		return true
	}
	return false
}

func parseContactWrite(item node) (core.ContactPatch, error) {
	if !isElement(item, types, "Contact") {
		return nil, core.ErrUnsupported
	}
	patch := make(core.ContactPatch)
	set := func(field core.ContactField, value node) error {
		if _, duplicate := patch[field]; duplicate || len(value.Children) != 0 {
			return core.ErrInvalid
		}
		if field == "Body" && value.attr("BodyType") != "Text" {
			return core.ErrUnsupported
		}
		patch[field] = value.Text
		return nil
	}
	seen := make(map[string]bool, len(item.Children))
	for _, field := range item.Children {
		name := field.XMLName.Local
		if field.XMLName.Space != types || seen[name] {
			return nil, core.ErrInvalid
		}
		seen[name] = true
		switch name {
		case "EmailAddresses", "PhoneNumbers", "PhysicalAddresses":
			entries := make(map[string]bool)
			for _, entry := range field.Children {
				key := entry.attr("Key")
				if !isElement(entry, types, "Entry") || entries[key] || !validContactIndex(name, key) {
					return nil, core.ErrUnsupported
				}
				entries[key] = true
				if name != "PhysicalAddresses" {
					if err := set(core.ContactField(key), entry); err != nil {
						return nil, err
					}
					continue
				}
				for _, part := range entry.Children {
					if part.XMLName.Space != types || !validContactAddressField(part.XMLName.Local) {
						return nil, core.ErrUnsupported
					}
					if err := set(core.ContactField(key+part.XMLName.Local), part); err != nil {
						return nil, err
					}
				}
			}
		default:
			if name == "ItemClass" && field.Text == "IPM.Contact" && len(field.Children) == 0 {
				continue
			}
			uri := "contacts:" + name
			if name == "Body" {
				uri = "item:Body"
			}
			key, _, _, err := contactWriteField(at(el(types, "FieldURI", ""), "FieldURI", uri))
			if err != nil {
				return nil, err
			}
			if err := set(key, field); err != nil {
				return nil, err
			}
		}
	}
	if len(patch) == 0 {
		return nil, core.ErrInvalid
	}
	return patch, nil
}

func containsItemType(list *node, kind string) bool {
	if list != nil {
		for _, item := range list.Children {
			if isElement(item, types, kind) {
				return true
			}
		}
	}
	return false
}

func (h *Handler) createContacts(ctx context.Context, op node, s core.Session) []node {
	const operation = "CreateItem"
	disposition := op.attr("MessageDisposition")
	if disposition != "" && disposition != "SaveOnly" {
		return syncError(operation, core.ErrUnsupported)
	}
	writer, ok := s.Mailbox().(core.ContactWriter)
	if !ok {
		return syncError(operation, core.ErrUnsupported)
	}
	list := op.child(messages, "Items")
	if !validBatch(list) {
		return syncError(operation, core.ErrInvalid)
	}
	folderRef := at(el(types, "DistinguishedFolderId", ""), "Id", "contacts")
	if saved := op.child(messages, "SavedItemFolderId"); saved != nil {
		if len(saved.Children) != 1 || len(op.Children) != 2 {
			return syncError(operation, core.ErrInvalid)
		}
		folderRef = saved.Children[0]
	} else if len(op.Children) != 1 {
		return syncError(operation, core.ErrInvalid)
	}
	folder, matched, err := h.contactFolder(ctx, s, folderRef)
	if err != nil {
		return []node{writeFailure(operation, err, "ErrorFolderNotFound")}
	}
	if !matched {
		return syncError(operation, core.ErrUnsupported)
	}
	out := make([]node, 0, len(list.Children))
	for _, item := range list.Children {
		patch, err := parseContactWrite(item)
		if err != nil {
			out = append(out, writeFailure(operation, err, "ErrorItemNotFound"))
			continue
		}
		created, err := writer.CreateContact(ctx, folder.ID, patch)
		out = append(out, h.contactWriteReply(operation, s.Identity().Scope, created, err))
	}
	return out
}

func (h *Handler) contactWriteReply(op string, scope core.Scope, contact core.Contact, err error) node {
	if err != nil {
		return writeFailure(op, err, "ErrorItemNotFound")
	}
	return success(op, el(messages, "Items", "", h.contact(scope, contact, true)))
}

func contactUpdates(updates node) (core.ContactPatch, error) {
	patch := make(core.ContactPatch)
	for _, update := range updates.Children {
		if update.XMLName.Space != types || len(update.Children) == 0 {
			return nil, core.ErrInvalid
		}
		uri := update.Children[0]
		key, container, index, err := contactWriteField(uri)
		if err != nil {
			return nil, err
		}
		if _, duplicate := patch[key]; duplicate {
			return nil, core.ErrInvalid
		}
		switch update.XMLName.Local {
		case "SetItemField":
			if len(update.Children) != 2 {
				return nil, core.ErrInvalid
			}
			card := update.Children[1]
			if !isElement(card, types, "Contact") || len(card.Children) != 1 ||
				!isElement(card.Children[0], types, container) {
				return nil, core.ErrInvalid
			}
			if index != "" {
				entries := card.Children[0].Children
				if len(entries) != 1 || entries[0].attr("Key") != index {
					return nil, core.ErrInvalid
				}
			}
			values, err := parseContactWrite(card)
			if err != nil {
				return nil, err
			}
			value, ok := values[key]
			if !ok || len(values) != 1 {
				return nil, core.ErrInvalid
			}
			patch[key] = value
		case "DeleteItemField":
			if len(update.Children) != 1 {
				return nil, core.ErrInvalid
			}
			patch[key] = ""
		default:
			return nil, core.ErrUnsupported
		}
	}
	return patch, nil
}

func (h *Handler) readContactForWrite(ctx context.Context, s core.Session, ref reference, key string, overwrite bool) (core.Contact, error) {
	reader, ok := s.Mailbox().(core.ContactReader)
	if !ok {
		return core.Contact{}, core.ErrUnsupported
	}
	card, err := reader.GetContact(ctx, ref.Parent, ref.ID)
	if err != nil {
		return core.Contact{}, err
	}
	if card.Identity.ID != ref.ID || card.Identity.Parent != ref.Parent || card.Identity.Kind != core.ContactItem {
		return core.Contact{}, core.ErrNotFound
	}
	if !overwrite && key != "" && key != h.changeKey(s.Identity().Scope, card.Identity) {
		return core.Contact{}, core.ErrConflict
	}
	return card, nil
}

func (h *Handler) updateContact(ctx context.Context, s core.Session, ref reference, key string, updates node, overwrite bool) node {
	const operation = "UpdateItem"
	writer, ok := s.Mailbox().(core.ContactWriter)
	if !ok {
		return writeFailure(operation, core.ErrUnsupported, "ErrorItemNotFound")
	}
	patch, err := contactUpdates(updates)
	if err != nil {
		return writeFailure(operation, err, "ErrorItemNotFound")
	}
	scope := s.Identity().Scope
	for attempt := range 2 {
		card, err := h.readContactForWrite(ctx, s, ref, key, overwrite)
		if err != nil {
			return writeFailure(operation, err, "ErrorItemNotFound")
		}
		saved, err := writer.UpdateContact(ctx, card, patch)
		if !overwrite || attempt == 1 || !errors.Is(err, core.ErrConflict) {
			return h.contactWriteReply(operation, scope, saved, err)
		}
	}
	return writeFailure(operation, core.ErrConflict, "ErrorItemNotFound")
}

func (h *Handler) trashContact(ctx context.Context, s core.Session, ref reference, key string) node {
	const operation = "DeleteItem"
	writer, ok := s.Mailbox().(core.ContactWriter)
	if !ok {
		return writeFailure(operation, core.ErrUnsupported, "ErrorItemNotFound")
	}
	card, err := h.readContactForWrite(ctx, s, ref, key, false)
	if err == nil {
		err = writer.TrashContact(ctx, card)
	}
	if err != nil {
		return writeFailure(operation, err, "ErrorItemNotFound")
	}
	return success(operation)
}
