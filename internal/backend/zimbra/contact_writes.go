// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"slices"
	"strings"
	"unicode/utf8"

	"ewsgo/internal/core"
)

var contactAttributes = map[core.ContactField]string{
	"DisplayName": "fullName", "GivenName": "firstName", "Surname": "lastName",
	"MiddleName": "middleName", "Nickname": "nickname", "CompanyName": "company",
	"Department": "department", "JobTitle": "jobTitle", "BusinessHomePage": "workURL", "Body": "notes",
	"EmailAddress1": "email", "EmailAddress2": "email2", "EmailAddress3": "email3",
	"BusinessPhone": "workPhone", "BusinessPhone2": "workPhone2", "HomePhone": "homePhone",
	"HomePhone2": "homePhone2", "MobilePhone": "mobilePhone", "BusinessFax": "workFax",
	"HomeFax": "homeFax", "OtherTelephone": "otherPhone",
	"BusinessStreet": "workStreet", "BusinessCity": "workCity", "BusinessState": "workState",
	"BusinessCountryOrRegion": "workCountry", "BusinessPostalCode": "workPostalCode",
	"HomeStreet": "homeStreet", "HomeCity": "homeCity", "HomeState": "homeState",
	"HomeCountryOrRegion": "homeCountry", "HomePostalCode": "homePostalCode",
	"OtherStreet": "otherStreet", "OtherCity": "otherCity", "OtherState": "otherState",
	"OtherCountryOrRegion": "otherCountry", "OtherPostalCode": "otherPostalCode",
}

func contactWriteFields(patch core.ContactPatch) ([]node, error) {
	if len(patch) == 0 {
		return nil, core.ErrInvalid
	}
	fields := make([]core.ContactField, 0, len(patch))
	for field := range patch {
		fields = append(fields, field)
	}
	slices.Sort(fields)
	var result []node
	for _, field := range fields {
		key, ok := contactAttributes[field]
		if !ok {
			return nil, core.ErrUnsupported
		}
		value := patch[field]
		if !utf8.ValidString(value) || strings.ContainsRune(value, 0) {
			return nil, core.ErrInvalid
		}
		result = append(result, attr(element(mailNS, "a", value), "n", key))
	}
	return result, nil
}

func (m *mailbox) CreateContact(ctx context.Context, folder core.ID, patch core.ContactPatch) (core.Contact, error) {
	return m.createContact(ctx, folder, patch, [16]byte{})
}

func (m *mailbox) CreateContactWithReference(ctx context.Context, folder core.ID, patch core.ContactPatch, reference [16]byte) (core.Contact, error) {
	if reference == ([16]byte{}) {
		return core.Contact{}, core.ErrInvalid
	}
	return m.createContact(ctx, folder, patch, reference)
}

func (m *mailbox) FindContactByReference(ctx context.Context, folder core.ID, reference [16]byte) (core.Contact, error) {
	if reference == ([16]byte{}) {
		return core.Contact{}, core.ErrInvalid
	}
	items, err := m.ListContacts(ctx, folder)
	if err != nil {
		return core.Contact{}, err
	}
	var found core.Contact
	for _, item := range items {
		if item.ImportReference != reference {
			continue
		}
		if found.Identity.ID != "" {
			return core.Contact{}, core.ErrConflict
		}
		found = item
	}
	if found.Identity.ID == "" {
		return core.Contact{}, core.ErrNotFound
	}
	return found, nil
}

func (m *mailbox) createContact(ctx context.Context, folder core.ID, patch core.ContactPatch, reference [16]byte) (core.Contact, error) {
	target, err := m.groupwareFolder(ctx, folder, core.ContactsFolder)
	if err != nil {
		return core.Contact{}, err
	}
	fields, err := contactWriteFields(patch)
	if err != nil {
		return core.Contact{}, err
	}
	if reference != ([16]byte{}) {
		fields = append(fields, attr(element(mailNS, "a", hex.EncodeToString(reference[:])), "n", contactImportAttribute))
	}
	card := attr(element(mailNS, "cn", "", fields...), "l", target.nativeFolder)
	req := element(mailNS, "CreateContactRequest", "", card)
	response, err := m.native.request(ctx, mailNS, "CreateContact", "", req)
	if err != nil {
		return core.Contact{}, messageWriteError(err)
	}
	return m.verifyContactWrite(ctx, folder, target, "", response)
}

func contactWriteTarget(contact core.Contact) (string, error) {
	o := contact.Identity
	id, ok := strings.CutPrefix(string(o.ID), core.ContactIDPrefix)
	spec := specialFolderSpec(core.ContactsFolder)
	if !ok || !nativeID.MatchString(id) || o.Kind != core.ContactItem || o.Revision == 0 ||
		o.Parent != core.ID("f:"+spec.id) {
		return "", core.ErrNotFound
	}
	return id, nil
}

func (m *mailbox) UpdateContact(ctx context.Context, before core.Contact, patch core.ContactPatch) (core.Contact, error) {
	folder := before.Identity.Parent
	target, err := m.groupwareFolder(ctx, folder, core.ContactsFolder)
	if err != nil {
		return core.Contact{}, err
	}
	id, valid := strings.CutPrefix(string(before.Identity.ID), core.ContactIDPrefix)
	if !valid || !target.validItem(id) || before.Identity.Kind != core.ContactItem || before.Identity.Revision == 0 {
		return core.Contact{}, core.ErrNotFound
	}
	fields, err := contactWriteFields(patch)
	if err != nil {
		return core.Contact{}, err
	}
	card := attr(element(mailNS, "cn", "", fields...), "id", id)
	req := attr(element(mailNS, "ModifyContactRequest", "", card), "replace", "0")
	response, err := m.native.request(ctx, mailNS, "ModifyContact", "", req, modifiedConstraint(before.Identity.Revision))
	if err != nil {
		return core.Contact{}, messageWriteError(err)
	}
	return m.verifyContactWrite(ctx, folder, target, id, response)
}

func (m *mailbox) verifyContactWrite(ctx context.Context, folder core.ID, target groupwareTarget, expected string, response node) (core.Contact, error) {
	card := response.child(mailNS, "cn")
	if len(response.Children) != 1 || card == nil {
		return core.Contact{}, core.ErrOutcomeUnknown
	}
	id := card.get("id")
	if !target.validItem(id) || expected != "" && id != expected {
		return core.Contact{}, core.ErrOutcomeUnknown
	}
	identity := core.ID(core.ContactIDPrefix + id)
	item, err := m.GetContact(ctx, folder, identity)
	if err != nil {
		if expected == "" {
			return core.Contact{Identity: core.Object{ID: identity, Parent: folder, Kind: core.ContactItem}}, core.ErrOutcomeUnknown
		}
		return core.Contact{}, core.ErrOutcomeUnknown
	}
	return item, nil
}

func (m *mailbox) TrashContact(ctx context.Context, before core.Contact) error {
	id, err := contactWriteTarget(before)
	if err != nil {
		return err
	}
	return m.contactAction(ctx, id, before.Identity.Revision, "trash")
}

func (m *mailbox) SoftDeleteContact(ctx context.Context, before core.Contact) error {
	id, err := contactWriteTarget(before)
	if err != nil {
		return err
	}
	if err := m.native.requireVisibleDumpster(ctx); err != nil {
		return err
	}
	if err := m.contactAction(ctx, id, before.Identity.Revision, "delete"); err != nil {
		return err
	}
	if _, err := m.GetContact(ctx, before.Identity.Parent, before.Identity.ID); !errors.Is(err, core.ErrNotFound) {
		return core.ErrOutcomeUnknown
	}
	query := element(mailNS, "query", "item:"+id)
	req := attr(attr(attr(element(mailNS, "SearchRequest", "", query), "inDumpster", "1"), "types", "contact"), "limit", "2")
	match, err := m.native.request(ctx, mailNS, "Search", "", req)
	if err != nil || len(match.Children) != 1 || match.get("more") != "0" && match.get("more") != "false" {
		return core.ErrOutcomeUnknown
	}
	item := match.Children[0]
	if item.XMLName != (xml.Name{Space: mailNS, Local: "cn"}) || item.get("id") != id ||
		core.ID("f:"+item.get("l")) != before.Identity.Parent {
		return core.ErrOutcomeUnknown
	}
	return nil
}

func (m *mailbox) contactAction(ctx context.Context, id string, revision core.Revision, op string) error {
	action := attr(attr(element(mailNS, "action", ""), "id", id), "op", op)
	req := element(mailNS, "ContactActionRequest", "", action)
	response, err := m.native.request(ctx, mailNS, "ContactAction", "", req, modifiedConstraint(revision))
	if err != nil {
		return messageWriteError(err)
	}
	result := response.child(mailNS, "action")
	if len(response.Children) != 1 || result == nil || result.get("id") != id || result.get("op") != op {
		return core.ErrOutcomeUnknown
	}
	return nil
}
