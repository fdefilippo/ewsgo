// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
)

var contactFields = map[string]string{
	"item:Body": "Body", "item:LastModifiedTime": "LastModifiedTime",
	"contacts:FileAs": "FileAs", "contacts:DisplayName": "DisplayName",
	"contacts:GivenName": "GivenName", "contacts:MiddleName": "MiddleName",
	"contacts:Surname": "Surname", "contacts:Nickname": "Nickname",
	"contacts:CompanyName": "CompanyName", "contacts:Department": "Department",
	"contacts:JobTitle": "JobTitle", "contacts:BusinessHomePage": "BusinessHomePage",
}

func (s responseShape) contactOnly() bool {
	for _, name := range contactFields {
		if s.extra[name] {
			return true
		}
	}
	return false
}

func isContactFolderReference(ref node) bool {
	return isElement(ref, types, "DistinguishedFolderId") && ref.attr("Id") == "contacts"
}

// contactFolder resolves the native default address book with the current
// authenticated session. It leaves mail folder routing to the mail handlers.
func (h *Handler) contactFolder(ctx context.Context, s core.Session, ref node) (core.Object, bool, error) {
	distinguished := isContactFolderReference(ref)
	var id core.ID
	if !distinguished {
		if !isElement(ref, types, "FolderId") {
			return core.Object{}, false, nil
		}
		var err error
		id, err = h.decode(s.Identity().Scope, ref.attr("Id"))
		if err != nil {
			return core.Object{}, false, nil
		}
	}
	box := s.Mailbox()
	if strings.HasPrefix(string(id), core.SharedGroupwareFolderIDPrefix) {
		if shared, ok := box.(core.SharedGroupwareFolders); ok {
			folder, err := shared.ReadSharedGroupwareFolder(ctx, id, core.ContactsFolder)
			return folder, true, err
		}
		return core.Object{}, true, core.ErrNotFound
	}
	folders, ok := box.(core.SpecialFolderReader)
	if _, contacts := box.(core.ContactReader); !ok || !contacts {
		return core.Object{}, distinguished, core.ErrUnsupported
	}
	folder, err := folders.SpecialFolder(ctx, core.ContactsFolder)
	if err != nil || distinguished {
		return folder, distinguished, err
	}
	return folder, id == folder.ID, nil
}

func (h *Handler) findItems(ctx context.Context, op node, s core.Session, shape responseShape) []node {
	const operation = "FindItem"
	if op.child(messages, "CalendarView") != nil {
		return h.findAppointments(ctx, op, s, shape)
	}
	if shape.appointmentOnly() {
		return syncError(operation, core.ErrUnsupported)
	}
	parents := op.child(messages, "ParentFolderIds")
	if parents != nil && len(parents.Children) == 1 {
		parent := parents.Children[0]
		if isContactFolderReference(parent) || op.child(messages, "Restriction") == nil {
			folder, matched, err := h.contactFolder(ctx, s, parent)
			if matched {
				if err != nil {
					return []node{writeFailure(operation, err, "ErrorFolderNotFound")}
				}
				return h.findContacts(ctx, op, s, shape, folder.ID)
			}
		}
	}
	if shape.contactOnly() {
		return syncError(operation, core.ErrUnsupported)
	}
	return h.findDraftItems(ctx, op, s, shape)
}

func (h *Handler) findContacts(ctx context.Context, op node, s core.Session, shape responseShape, folder core.ID) []node {
	const operation = "FindItem"
	if op.attr("Traversal") != "Shallow" || len(shape.extended) != 0 || shape.extra["Body"] {
		return syncError(operation, core.ErrUnsupported)
	}
	offset, limit, descending := 0, 100, false
	seen := make(map[string]bool, len(op.Children))
	for _, part := range op.Children {
		name := part.XMLName.Local
		if part.XMLName.Space != messages || seen[name] {
			return syncError(operation, core.ErrInvalid)
		}
		seen[name] = true
		switch name {
		case "ItemShape", "ParentFolderIds":
		case "IndexedPageItemView":
			var offsetErr, limitErr error
			offset, offsetErr = strconv.Atoi(part.attr("Offset"))
			limit, limitErr = strconv.Atoi(part.attr("MaxEntriesReturned"))
			if offsetErr != nil || limitErr != nil || offset < 0 || limit < 1 || limit > 100 ||
				part.attr("BasePoint") != "Beginning" || len(part.Children) != 0 {
				return syncError(operation, core.ErrInvalid)
			}
		case "SortOrder":
			if len(part.Children) != 1 {
				return syncError(operation, core.ErrUnsupported)
			}
			order := part.Children[0]
			field := order.child(types, "FieldURI")
			direction := order.attr("Order")
			if !isElement(order, types, "FieldOrder") || len(order.Children) != 1 || field == nil ||
				field.attr("FieldURI") != "contacts:DisplayName" || direction != "Ascending" && direction != "Descending" {
				return syncError(operation, core.ErrUnsupported)
			}
			descending = direction == "Descending"
		default:
			return syncError(operation, core.ErrUnsupported)
		}
	}
	reader, ok := s.Mailbox().(core.ContactReader)
	if !ok {
		return syncError(operation, core.ErrUnsupported)
	}
	contacts, err := reader.ListContacts(ctx, folder)
	if err != nil {
		return []node{writeFailure(operation, err, "ErrorFolderNotFound")}
	}
	slices.SortFunc(contacts, func(a, b core.Contact) int {
		cmp := strings.Compare(strings.ToLower(a.DisplayName), strings.ToLower(b.DisplayName))
		if cmp == 0 {
			cmp = strings.Compare(string(a.Identity.ID), string(b.Identity.ID))
		}
		if descending {
			return -cmp
		}
		return cmp
	})
	start := min(offset, len(contacts))
	end := min(start+limit, len(contacts))
	items := make([]node, 0, end-start)
	for _, contact := range contacts[start:end] {
		items = append(items, shape.apply(h.contact(s.Identity().Scope, contact, false)))
	}
	root := el(messages, "RootFolder", "", el(types, "Items", "", items...))
	root = at(at(at(root, "IndexedPagingOffset", strconv.Itoa(end)), "TotalItemsInView", strconv.Itoa(len(contacts))),
		"IncludesLastItemInRange", strconv.FormatBool(end == len(contacts)))
	return []node{success(operation, root)}
}

func (h *Handler) getContact(ctx context.Context, s core.Session, ref reference, shape responseShape) node {
	reader, ok := s.Mailbox().(core.ContactReader)
	if !ok {
		return itemError(core.ErrNotFound)
	}
	if shape.mime || len(shape.extended) != 0 || shape.extra["Attachments"] {
		return itemError(core.ErrUnsupported)
	}
	contact, err := reader.GetContact(ctx, ref.Parent, ref.ID)
	if errors.Is(err, core.ErrLimit) {
		return failure("GetItem", "ErrorDataSizeLimitExceeded")
	}
	if err != nil {
		return itemError(err)
	}
	if contact.Identity.ID != ref.ID || contact.Identity.Parent != ref.Parent || contact.Identity.Kind != core.ContactItem {
		return itemError(core.ErrNotFound)
	}
	return success("GetItem", el(messages, "Items", "", shape.apply(h.contact(s.Identity().Scope, contact, true))))
}

func (h *Handler) contact(scope core.Scope, c core.Contact, includeBody bool) node {
	o := c.Identity
	children := []node{
		at(at(el(types, "ItemId", ""), "Id", h.itemID(scope, o)), "ChangeKey", h.changeKey(scope, o)),
		at(el(types, "ParentFolderId", ""), "Id", h.encode(scope, o.Parent)),
		el(types, "ItemClass", "IPM.Contact"),
	}
	if includeBody && c.Notes != "" {
		children = append(children, at(el(types, "Body", c.Notes), "BodyType", "Text"))
	}
	if modified, ok := o.Properties[core.LastModifiedAt].(core.Timestamp); ok {
		children = append(children, el(types, "LastModifiedTime", time.Time(modified).UTC().Format(time.RFC3339)))
	}
	add := func(name, value string) {
		if value != "" {
			children = append(children, el(types, name, value))
		}
	}
	add("FileAs", c.FileAs)
	add("DisplayName", c.DisplayName)
	add("GivenName", c.GivenName)
	add("MiddleName", c.MiddleName)
	add("Nickname", c.Nickname)
	add("CompanyName", c.CompanyName)
	var emails []node
	for i, address := range c.Emails {
		if address != "" {
			emails = append(emails, at(el(types, "Entry", address), "Key", "EmailAddress"+strconv.Itoa(i+1)))
		}
	}
	if len(emails) != 0 {
		children = append(children, el(types, "EmailAddresses", "", emails...))
	}
	var addresses []node
	for _, a := range []struct {
		key string
		val core.ContactAddress
	}{{"Business", c.BusinessAddress}, {"Home", c.HomeAddress}, {"Other", c.OtherAddress}} {
		var fields []node
		for _, field := range []struct{ name, value string }{
			{"Street", a.val.Street}, {"City", a.val.City}, {"State", a.val.State},
			{"CountryOrRegion", a.val.Country}, {"PostalCode", a.val.PostalCode},
		} {
			if field.value != "" {
				fields = append(fields, el(types, field.name, field.value))
			}
		}
		if len(fields) != 0 {
			addresses = append(addresses, at(el(types, "Entry", "", fields...), "Key", a.key))
		}
	}
	if len(addresses) != 0 {
		children = append(children, el(types, "PhysicalAddresses", "", addresses...))
	}
	var phones []node
	for _, p := range []struct{ key, value string }{
		{"BusinessPhone", c.BusinessPhone}, {"BusinessPhone2", c.BusinessPhone2},
		{"HomePhone", c.HomePhone}, {"HomePhone2", c.HomePhone2}, {"MobilePhone", c.MobilePhone},
		{"BusinessFax", c.BusinessFax}, {"HomeFax", c.HomeFax}, {"OtherTelephone", c.OtherPhone},
	} {
		if p.value != "" {
			phones = append(phones, at(el(types, "Entry", p.value), "Key", p.key))
		}
	}
	if len(phones) != 0 {
		children = append(children, el(types, "PhoneNumbers", "", phones...))
	}
	add("BusinessHomePage", c.WebPage)
	add("Department", c.Department)
	add("JobTitle", c.JobTitle)
	add("Surname", c.Surname)
	return el(types, "Contact", "", children...)
}
