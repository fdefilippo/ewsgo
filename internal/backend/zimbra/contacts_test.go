// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"errors"
	"testing"

	"ewsgo/internal/core"
)

func TestReadContactPreservesNativeRevisionsAndAddressSlots(t *testing.T) {
	contactXML, err := parseXML([]byte(`<cn xmlns="urn:zimbraMail" id="95" l="7" ms="18" rev="12" md="1795600000" fileAsStr="Example, Alice">
		<a n="firstName">Alice</a><a n="lastName">Example</a><a n="email2">second@example.test</a>
		<a n="workCity">Roma</a><a n="mobilePhone">+39 012345</a><a n="notes">Test &lt;&amp;&gt;</a>
	</cn>`))
	if err != nil {
		t.Fatal(err)
	}
	contact, err := readContact(contactXML, "f:7")
	if err != nil {
		t.Fatal(err)
	}
	id := contact.Identity
	if id.ID != "contact:95" || id.Parent != "f:7" || id.Kind != core.ContactItem ||
		id.Revision != 18 || id.Properties[core.ContentRevision] != core.Integer(12) {
		t.Fatal("contact identity or revisions changed")
	}
	if contact.DisplayName != "Alice Example" || contact.FileAs != "Example, Alice" ||
		contact.Emails != [3]string{"", "second@example.test", ""} || contact.BusinessAddress.City != "Roma" ||
		contact.MobilePhone != "+39 012345" || contact.Notes != "Test <&>" {
		t.Fatal("contact field or indexed slot changed")
	}
	contactXML.Children = append(contactXML.Children, attr(element(mailNS, "a", "group"), "n", "type"))
	if _, err = readContact(contactXML, "f:7"); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal("contact group was treated as a person")
	}
}
