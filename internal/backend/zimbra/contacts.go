// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"strings"
	"time"

	"ewsgo/internal/core"
)

const maxContactList = 1000
const contactImportAttribute = "xEwsgoOpaque"
const contactTrashID = "3"

func (m *mailbox) ListContacts(ctx context.Context, folder core.ID) ([]core.Contact, error) {
	target, err := m.groupwareFolder(ctx, folder, core.ContactsFolder)
	if err != nil {
		return nil, err
	}
	return m.contacts(ctx, folder, target, "")
}

func (m *mailbox) GetContact(ctx context.Context, folder, id core.ID) (core.Contact, error) {
	target, err := m.groupwareFolder(ctx, folder, core.ContactsFolder)
	if err != nil {
		return core.Contact{}, err
	}
	return m.contact(ctx, folder, target, id)
}

func (m *mailbox) ListTrashedContacts(ctx context.Context, folder core.ID) ([]core.Contact, error) {
	if folder != core.ID("f:"+contactTrashID) {
		return nil, core.ErrNotFound
	}
	return m.contacts(ctx, folder, groupwareTarget{nativeFolder: contactTrashID}, "")
}

func (m *mailbox) GetTrashedContact(ctx context.Context, folder, id core.ID) (core.Contact, error) {
	if folder != core.ID("f:"+contactTrashID) {
		return core.Contact{}, core.ErrNotFound
	}
	return m.contact(ctx, folder, groupwareTarget{nativeFolder: contactTrashID}, id)
}

func (m *mailbox) contact(ctx context.Context, folder core.ID, target groupwareTarget, id core.ID) (core.Contact, error) {
	native, ok := strings.CutPrefix(string(id), core.ContactIDPrefix)
	if !ok || !target.validItem(native) {
		return core.Contact{}, core.ErrNotFound
	}
	items, err := m.contacts(ctx, folder, target, native)
	switch {
	case err != nil:
		return core.Contact{}, err
	case len(items) == 0:
		return core.Contact{}, core.ErrNotFound
	default:
		return items[0], nil
	}
}

func (m *mailbox) contacts(ctx context.Context, folder core.ID, target groupwareTarget, id string) ([]core.Contact, error) {
	req := attr(attr(attr(element(mailNS, "GetContactsRequest", ""), "l", target.nativeFolder), "sync", "1"), "maxMembers", "1")
	if id != "" {
		req.Children = []node{attr(element(mailNS, "cn", ""), "id", id)}
	}
	response, err := m.native.request(ctx, mailNS, "GetContacts", "", req)
	if errors.Is(err, ErrLimit) {
		return nil, core.ErrLimit
	}
	if err != nil {
		return nil, semanticError(err)
	}
	if len(response.Children) > maxContactList {
		return nil, core.ErrLimit
	}
	if id != "" && len(response.Children) > 1 {
		return nil, ErrProtocol
	}
	items := make([]core.Contact, 0, len(response.Children))
	seen := make(map[string]bool, len(response.Children))
	for _, child := range response.Children {
		native := child.get("id")
		if child.XMLName != (xml.Name{Space: mailNS, Local: "cn"}) || !target.validItem(native) ||
			seen[native] || (id != "" && id != native) || child.get("l") != target.nativeFolder {
			return nil, ErrProtocol
		}
		seen[native] = true
		item, err := readContact(child, folder)
		if errors.Is(err, core.ErrUnsupported) && id == "" {
			continue
		}
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, ctx.Err()
}

func readContact(n node, folder core.ID) (core.Contact, error) {
	values := make(map[string]string, len(n.Children))
	for _, child := range n.Children {
		if child.XMLName != (xml.Name{Space: mailNS, Local: "a"}) {
			continue
		}
		key := child.get("n")
		if key == "" {
			return core.Contact{}, ErrProtocol
		}
		if _, repeated := values[key]; repeated {
			return core.Contact{}, ErrProtocol
		}
		values[key] = child.Text
	}
	if values["type"] == "group" {
		return core.Contact{}, core.ErrUnsupported
	}
	modified, err := optionalFolderNumber(n, "md")
	if err != nil || modified == nil || *modified > 253402300799 {
		return core.Contact{}, ErrProtocol
	}
	revision, err := optionalFolderNumber(n, "ms")
	if err != nil || revision == nil || *revision == 0 {
		return core.Contact{}, ErrProtocol
	}
	content, err := optionalFolderNumber(n, "rev")
	if err != nil || content == nil || *content == 0 {
		return core.Contact{}, ErrProtocol
	}
	address := func(prefix string) core.ContactAddress {
		return core.ContactAddress{
			Street: values[prefix+"Street"], City: values[prefix+"City"],
			State: values[prefix+"State"], Country: values[prefix+"Country"],
			PostalCode: values[prefix+"PostalCode"],
		}
	}
	displayName := values["fullName"]
	if displayName == "" {
		var name []string
		for _, key := range []string{"firstName", "middleName", "lastName"} {
			if part := strings.TrimSpace(values[key]); part != "" {
				name = append(name, part)
			}
		}
		displayName = strings.Join(name, " ")
	}
	if displayName == "" {
		displayName = n.get("fileAsStr")
	}
	var reference [16]byte
	if raw := values[contactImportAttribute]; len(raw) == 32 {
		if _, err := hex.Decode(reference[:], []byte(raw)); err != nil {
			reference = [16]byte{}
		}
	}
	return core.Contact{
		ImportReference: reference,
		Identity: core.Object{
			ID: core.ID(core.ContactIDPrefix + n.get("id")), Parent: folder, Kind: core.ContactItem,
			Revision: core.Revision(*revision),
			Properties: core.Properties{
				core.MetadataRevision: core.Integer(*revision), core.ContentRevision: core.Integer(*content),
				core.LastModifiedAt: core.Timestamp(time.Unix(int64(*modified), 0).UTC()),
			},
		},
		FileAs: n.get("fileAsStr"), DisplayName: displayName, GivenName: values["firstName"],
		MiddleName: values["middleName"], Surname: values["lastName"], Nickname: values["nickname"],
		CompanyName: values["company"], Department: values["department"], JobTitle: values["jobTitle"],
		WebPage: values["workURL"], Notes: values["notes"],
		Emails:          [3]string{values["email"], values["email2"], values["email3"]},
		BusinessAddress: address("work"), HomeAddress: address("home"), OtherAddress: address("other"),
		BusinessPhone: values["workPhone"], BusinessPhone2: values["workPhone2"],
		HomePhone: values["homePhone"], HomePhone2: values["homePhone2"], MobilePhone: values["mobilePhone"],
		BusinessFax: values["workFax"], HomeFax: values["homeFax"], OtherPhone: values["otherPhone"],
	}, nil
}
