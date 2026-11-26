// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"errors"
	"testing"

	"ewsgo/internal/core"
)

func TestContactUpdatesUseTheExactIndexedField(t *testing.T) {
	updates := func(key string) node {
		return el(types, "Updates", "",
			el(types, "SetItemField", "",
				at(at(el(types, "IndexedFieldURI", ""), "FieldURI", "contacts:EmailAddress"), "FieldIndex", "EmailAddress2"),
				el(types, "Contact", "", el(types, "EmailAddresses", "",
					at(el(types, "Entry", "second@example.test"), "Key", key)))),
			el(types, "DeleteItemField", "",
				at(at(el(types, "IndexedFieldURI", ""), "FieldURI", "contacts:PhysicalAddress:City"), "FieldIndex", "Home")))
	}
	patch, err := contactUpdates(updates("EmailAddress2"))
	clearValue, clearPresent := patch["HomeCity"]
	if err != nil || len(patch) != 2 || patch["EmailAddress2"] != "second@example.test" || !clearPresent || clearValue != "" {
		t.Fatal("indexed contact update lost its key or deletion")
	}
	if _, err := contactUpdates(updates("EmailAddress1")); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("update accepted a value for a different index")
	}
}
