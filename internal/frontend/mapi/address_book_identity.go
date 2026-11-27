// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import "ewsgo/internal/core"

// The current namespace has a fixed ASCII LegacyDN: two UUIDs and fixed
// separators. MS-OXNSPI 2.2.9.3 adds 28 header bytes and a NUL terminator.
const maxOwnAddressBookEntryID = 131

const mailUserEntryIDHeader = "\x00\x00\x00\x00\xdc\xa7\x40\xc8\xc0\x42\x10\x1a\xb4\xb9\x08\x00\x2b\x2f\xe1\x82\x01\x00\x00\x00\x00\x00\x00\x00"

// ownAddressBookEntryID encodes the already approved gateway LegacyDN; it
// introduces no ID allocator or persistence. Callers must prove current GAL
// visibility and that this is the authenticated mailbox.
func (n Namespace) ownAddressBookEntryID(identity core.Identity) ([]byte, error) {
	return n.mailUserEntryID(identity)
}

// mailUserEntryID requires caller proof of a mailbox user: authenticated own
// identity, a typed native directory object or a native user grant matched to
// the currently visible directory.
// It must never classify an arbitrary GAL object as DT_MAILUSER.
func (n Namespace) mailUserEntryID(identity core.Identity) ([]byte, error) {
	mapped, err := n.Mailbox(identity)
	if err != nil {
		return nil, err
	}
	// GUID_NSPI is a FlatUID_r byte constant, not a textual GUID to reorder.
	out := append(append([]byte(mailUserEntryIDHeader), mapped.LegacyDN...), 0) // R4=1; DT_MAILUSER=0.
	if len(out) > maxOwnAddressBookEntryID {
		return nil, core.ErrInvalid
	}
	return out, nil
}
