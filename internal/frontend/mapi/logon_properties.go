// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"

	"ewsgo/internal/core"
)

const (
	maxLogonNameBytes = 1024 // Encoded bytes including the terminator.
	logonUserEntryID  = 0x6619
	logonOwnerEntryID = 0x661b
)

func logonAddressEntryTag(id, kind uint16) bool {
	return (id == logonUserEntryID || id == logonOwnerEntryID) && (kind == 0 || kind == 0x0102)
}

// A Logon row is not a Folder row. Only the visible native owner's name
// and address-book EntryID are projected here. Search capabilities, icons and
// unknown properties remain explicit column errors. No name is fabricated.
func (t *SessionTransport) getLogonProperties(ctx context.Context, session core.Session, state *contextState, object serverObject, op Operation, diagnostic *StorePropertyDiagnostic) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	if _, err := object.binding.store.folder(ctx, session, object.binding.fids[MailboxRoot]); err != nil {
		return fail(storeError(err))
	}
	if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
		return fail(storeError(err))
	}
	request := *op.Properties
	name, nameError := "", uint32(PropertyNotFound)
	var ownerEntryID []byte
	entryError := uint32(PropertyNotFound)
	for _, tag := range request.tags {
		kind, id := uint16(tag), uint16(tag>>16)
		if !(id == 0x661c && (kind == 0 || kind == 0x001e || kind == 0x001f)) && !logonAddressEntryTag(id, kind) {
			continue
		}
		visible, code := t.visibleDirectory(ctx, session)
		if code != 0 {
			nameError = code
			entryError = code
		} else if entry, found := visible[session.Identity().Scope]; found {
			if entry.DisplayName != "" {
				name, nameError = entry.DisplayName, 0
			}
			var err error
			ownerEntryID, err = t.namespace.ownAddressBookEntryID(entry.Identity)
			if err == nil {
				entryError = 0
			}
		}
		break // One current directory read even with duplicate columns.
	}
	if ctx.Err() != nil {
		return fail(0x80004005)
	}
	values := make([]folderPropertyValue, len(request.tags))
	for i, tag := range request.tags {
		value := folderPropertyValue{kind: 0x000a, code: NotSupported}
		kind := uint16(tag)
		if uint16(tag>>16) == 0x661c && (kind == 0 || kind == 0x001e || kind == 0x001f) {
			value = logonOwnerName(name, nameError, kind, request.wantUnicode, state.codePage)
		}
		if logonAddressEntryTag(uint16(tag>>16), kind) {
			// A private owner logon binds the caller and the mailbox owner to
			// the same scope. Both tags refer to the same visible GAL object.
			value = folderPropertyValue{kind: 0x000a, code: entryError}
			if entryError == 0 {
				data := binary.LittleEndian.AppendUint16(nil, uint16(len(ownerEntryID)))
				value = folderPropertyValue{kind: 0x0102, data: append(data, ownerEntryID...)}
			}
		}
		switch tag {
		case 0x0ea00048, 0x0e5c000b, 0x30090003, 0x66310102:
			// This logon has no sharing, indexing, status row or public store.
			value.code = PropertyNotFound
		}
		values[i] = value
		if value.code != 0 {
			diagnostic.Errors++
		} else {
			diagnostic.Values++
		}
	}
	return propertyReadResponse(op, values)
}

func logonOwnerName(name string, code uint32, kind uint16, unicode bool, cp uint32) folderPropertyValue {
	fail := func(code uint32) folderPropertyValue { return folderPropertyValue{kind: 0x000a, code: code} }
	if code != 0 {
		return fail(code)
	}
	if kind == 0 {
		kind = 0x001e
		if unicode {
			kind = 0x001f
		}
	}
	if kind == 0x001e && cp != 1252 && cp != 65001 {
		return fail(0x8004011e)
	}
	// Bound the UTF-8 input before encoding. A name representable in a
	// single-byte codepage can occupy more bytes in the backend's UTF-8.
	if len(name) > 4*maxLogonNameBytes {
		return fail(0x8007000e)
	}
	data, ok := propertyString(name, kind, cp)
	if !ok {
		return fail(0x80070057)
	}
	if len(data) > maxLogonNameBytes {
		return fail(0x8007000e)
	}
	return folderPropertyValue{kind: kind, data: data}
}
