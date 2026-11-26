// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"net/http"
	"strings"

	"ewsgo/internal/core"
)

// visibleDirectory validates the entire current catalog before exposing any
// field. A saved MID is never sufficient authorization to return an entry.
func (t *SessionTransport) visibleDirectory(ctx context.Context, session core.Session) (map[core.Scope]core.DirectoryEntry, uint32) {
	source, ok := session.(core.DirectorySession)
	if !ok {
		return nil, NotSupported
	}
	directory := source.Directory()
	if directory == nil {
		return nil, NotSupported
	}
	entries, err := directory.Entries(ctx)
	if err != nil {
		if errors.Is(err, core.ErrAccessDenied) {
			return nil, 0x80070005
		}
		if errors.Is(err, core.ErrUnsupported) {
			return nil, NotSupported
		}
		return nil, 0x80004005
	}
	if len(entries) > core.MaxDirectoryEntries {
		return nil, 0x80004005
	}
	visible := make(map[core.Scope]core.DirectoryEntry, len(entries))
	dns := make(map[string]bool, len(entries))
	for _, entry := range entries {
		if entry.Identity.Scope.Backend != session.Identity().Scope.Backend {
			return nil, 0x80004005
		}
		mapped, err := t.namespace.Mailbox(entry.Identity)
		if err != nil {
			return nil, 0x80004005
		}
		dn := strings.ToLower(mapped.LegacyDN)
		if dns[dn] {
			return nil, 0x80004005
		}
		dns[dn] = true
		visible[entry.Identity.Scope] = entry
	}
	return visible, 0
}

func (t *SessionTransport) directoryProperties(ctx context.Context, session core.Session, state *contextState, request GetProps) ([]byte, int, int) {
	cp := state.codePage
	if request.State == nil {
		return getPropsResponse(0x80070057, cp, nil), 0, 0
	}
	cp = request.State.CodePage
	if unsupportedString8Codepage(cp, request.Tags) {
		return getPropsResponse(0x8004011e, cp, nil), 0, 0
	}
	if request.State.ContainerID != 0 {
		return getPropsResponse(0x80040405, cp, nil), 0, 0
	}
	if state.directory == nil {
		return getPropsResponse(NotSupported, cp, nil), 0, 0
	}
	visible, code := t.visibleDirectory(ctx, session)
	if code != 0 {
		return getPropsResponse(code, cp, nil), 0, 0
	}
	var entry core.DirectoryEntry
	found := false
	for scope, id := range state.directory.ids {
		if id == request.State.CurrentRec {
			entry, found = visible[scope]
			break
		}
	}
	project := func(tag uint32, codepage uint32) addressBookValue {
		if uint16(tag>>16) == 0x0fff {
			kind := uint16(tag)
			if entry.Identity.Scope == session.Identity().Scope && (kind == 0 || kind == 0x0102) {
				if request.Flags&0x00000002 != 0 { // fEphID, MS-OXNSPI 3.1.4.1.7.
					raw := append([]byte{0x87, 0, 0, 0}, state.directory.guid[:]...)
					raw = append32(append32(append32(raw, 1), 0), request.State.CurrentRec)
					return addressBookValue{tag: tag&0xffff0000 | 0x0102, data: appendBlob([]byte{0xff}, raw)}
				}
				if raw, err := t.namespace.ownAddressBookEntryID(entry.Identity); err == nil {
					return addressBookValue{tag: tag&0xffff0000 | 0x0102, data: appendBlob([]byte{0xff}, raw)}
				}
			}
			return missingProperty(tag)
		}
		return t.directoryProperty(entry, tag, codepage)
	}
	tags := request.Tags
	if tags == nil {
		// NspiGetPropList (3.1.4.1.6) lists only properties with values,
		// with strings typed String8. No GetPropList endpoint is implied.
		tags = make([]uint32, 0, 8)
		if found {
			for _, tag := range []uint32{0x3001001e, 0x3002001e, 0x3003001e, 0x39fe001e, 0x300b0102, 0x8c6d0102, 0x0fff0102, 0x800f101e} {
				if project(tag, 1252).tag&0xffff != 0x000a {
					tags = append(tags, tag)
				}
			}
		}
	}
	if unsupportedString8Codepage(cp, tags) {
		return getPropsResponse(0x8004011e, cp, nil), 0, 0
	}
	values := make([]addressBookValue, 0, len(tags))
	missing := 0
	for _, tag := range tags {
		value := missingProperty(tag)
		if found {
			value = project(tag, cp)
		}
		if value.tag&0xffff == 0x000a {
			missing++
		}
		values = append(values, value)
	}
	if missing != 0 {
		code = ErrorsReturned
	}
	body := getPropsResponse(code, cp, values)
	if len(body) > MaxBody {
		return getPropsResponse(TableTooBig, cp, nil), 0, 0
	}
	return body, len(values), missing
}

// STAT.CodePage affects String8, not binary or explicitly Unicode values.
// A null tag list is resolved from native presence before this final check.
func unsupportedString8Codepage(cp uint32, tags []uint32) bool {
	if cp == 1252 || cp == 65001 {
		return false
	}
	for _, tag := range tags {
		if uint16(tag) == 0x001e || uint16(tag) == 0x101e {
			return true
		}
	}
	return false
}

// Only native strings and identities owned by EWSGo are mapped. In particular,
// native GAL presence alone does not establish a recipient's Exchange object
// type, EntryID layout, home database or permission to open that mailbox.
func (t *SessionTransport) directoryProperty(entry core.DirectoryEntry, tag, cp uint32) addressBookValue {
	kind := uint16(tag)
	id := uint16(tag >> 16)
	mapped, err := t.namespace.Mailbox(entry.Identity)
	if err != nil {
		return missingProperty(tag)
	}
	if id == 0x8c6d && (kind == 0 || kind == 0x0102) {
		guid, err := t.namespace.AddressBookGUID(entry.Identity.Scope)
		if err != nil {
			return missingProperty(tag)
		}
		return addressBookValue{tag: tag&0xffff0000 | 0x0102, data: appendBlob([]byte{0xff}, guid[:])}
	}
	if id == 0x300b && (kind == 0 || kind == 0x0102) {
		key := append([]byte("EX:"+strings.ToUpper(mapped.LegacyDN)), 0)
		return addressBookValue{tag: tag&0xffff0000 | 0x0102, data: appendBlob([]byte{0xff}, key)}
	}
	if id == 0x800f {
		if kind == 0 {
			kind = 0x101f
		}
		if kind != 0x101e && kind != 0x101f {
			return missingProperty(tag)
		}
		// The core exposes the native primary SMTP only. No aliases, X500
		// addresses or recipient type are inferred from GAL presence.
		value, ok := addressBookString("SMTP:"+entry.Identity.Name, kind&0x0fff, cp)
		if !ok {
			return missingProperty(tag)
		}
		// AddressBookPropertyValue: array HasValue, DWORD element count,
		// then each string's own HasValue and NUL-terminated encoding.
		data := append(append32([]byte{0xff}, 1), value...)
		return addressBookValue{tag: tag&0xffff0000 | uint32(kind), data: data}
	}
	var value string
	switch id {
	case 0x3001:
		value = entry.DisplayName
	case 0x3002:
		value = "EX"
	case 0x3003:
		value = mapped.LegacyDN
	case 0x39fe:
		value = entry.Identity.Name
	default:
		return missingProperty(tag)
	}
	if value == "" {
		return missingProperty(tag)
	}
	if kind == 0 {
		kind = 0x001f
	}
	data, ok := addressBookString(value, kind, cp)
	if !ok {
		return missingProperty(tag)
	}
	return addressBookValue{tag: tag&0xffff0000 | uint32(kind), data: data}
}

func recordPropertyRequest(w http.ResponseWriter, request GetProps, tooMany bool) {
	if recorder, ok := w.(interface{ RecordNSPICodepage(uint32, bool) }); ok {
		if request.State == nil {
			recorder.RecordNSPICodepage(0, false)
		} else {
			recorder.RecordNSPICodepage(request.State.CodePage, true)
		}
	}
	if recorder, ok := w.(interface{ RecordNSPIProperties([]uint32, bool) }); ok {
		if tooMany {
			recorder.RecordNSPIProperties(make([]uint32, MaxPropertyTags+1), false)
		} else {
			recorder.RecordNSPIProperties(request.Tags, request.Tags == nil)
		}
	}
}

func recordPropertyResult(w http.ResponseWriter, values, missing int) {
	if recorder, ok := w.(interface{ RecordNSPIPropertyResult(int, int) }); ok {
		recorder.RecordNSPIPropertyResult(values, missing)
	}
}
