// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"slices"
	"unicode/utf16"

	"ewsgo/internal/core"
)

const (
	namedPropertyNoStrings = 0x01
	namedPropertyNoIDs     = 0x02
)

// MS-OXCDATA 2.6.1. A miss is a Kind 0xFF entry with an empty GUID.
func appendPropertyName(out []byte, name propertyName, limit int) ([]byte, uint32) {
	size := 17 // Kind and FlatUID.
	var units []uint16
	switch name.Kind {
	case 0:
		size += 4
	case 1:
		units = utf16.Encode([]rune(name.Name))
		if len(units) > 126 {
			return nil, NotSupported // NameSize includes the UTF-16 terminator.
		}
		size += 1 + 2*(len(units)+1)
	}
	if size > limit-len(out) {
		return nil, tableBufferTooSmall
	}
	out = append(out, name.Kind)
	out = append(out, name.Set[:]...)
	switch name.Kind {
	case 0:
		out = append32(out, name.LID)
	case 1:
		out = append(out, byte(2*(len(units)+1)))
		for _, unit := range units {
			out = binary.LittleEndian.AppendUint16(out, unit)
		}
		out = append(out, 0, 0)
	}
	return out, 0
}

func readNamedProperties(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if object.table != nil || object.stream != nil || object.notification != nil ||
		object.download != nil && object.download.draft == nil {
		return fail(NotSupported)
	}
	store := object.binding.store
	fid := object.fid
	if object.isLogon {
		fid = object.binding.fids[MailboxRoot]
	}
	ids := op.NamedPropertyIDs
	if op.ID == 0x5f {
		ids, code = objectNamedPropertyIDs(ctx, session, object, fid, op)
		if code != 0 {
			return fail(code)
		}
	} else if _, err := store.folder(ctx, session, fid); err != nil {
		return fail(storeError(err))
	}
	if err := ctx.Err(); err != nil {
		return fail(storeError(err))
	}
	out := binary.LittleEndian.AppendUint16(fail(0), uint16(len(ids)))
	if op.ID == 0x5f {
		if len(out)+2*len(ids) > op.PropertyListLimit {
			return fail(tableBufferTooSmall)
		}
		for _, id := range ids {
			out = binary.LittleEndian.AppendUint16(out, id)
		}
	}
	store.named.mu.Lock()
	defer store.named.mu.Unlock()
	for _, id := range ids {
		name := propertyName{Kind: 0xff}
		if id < 0x8000 {
			name = propertyName{Set: psMAPI, LID: uint32(id)}
		} else if registered, ok := store.named.registeredName(id); ok {
			name = registered
		}
		out, code = appendPropertyName(out, name, op.PropertyListLimit)
		if code != 0 {
			return fail(code)
		}
	}
	return out
}

func objectNamedPropertyIDs(ctx context.Context, session core.Session, object serverObject, fid uint64, op Operation) ([]uint16, uint32) {
	store := object.binding.store
	switch {
	case object.download != nil:
		draft := object.download.draft
		draft.mu.Lock()
		defer draft.mu.Unlock()
		if draft.closed {
			return nil, faiError(core.ErrClosed, true)
		}
		if _, err := store.messageAnchor(ctx, session, draft.root, true); err != nil {
			return nil, faiError(err, true)
		}
		return store.named.queryIDs(draft.properties, op), 0
	case object.message != nil:
		lease, err := store.reserveContentsPeak(ctx, openMessageWorkspace)
		if err != nil {
			return nil, storeError(err)
		}
		defer lease.release()
		row, err := store.readOpenedMessage(ctx, session, fid, *object.message)
		if err != nil {
			return nil, storeError(err)
		}
		if row.associated || row.special != 0 {
			return store.named.queryIDs(row.object.Properties, op), 0
		}
		if nativeMeetingKind(row.object.Properties) != 0 {
			if err := registerMeetingNames(ctx, &store.named); err != nil {
				return nil, storeError(err)
			}
			values := contentsValues{}
			projectMeetingProperties(values, row.object.Properties)
			properties := make(core.Properties)
			for _, fields := range [][]calendarNamedProperty{appointmentNamedProperties, meetingNamedProperties} {
				for _, field := range fields {
					if value, ok := values[field.property]; ok {
						properties[field.key] = value
					}
				}
			}
			return store.named.queryIDs(properties, op), 0
		}
	case object.attachment != nil:
		lease, err := store.reserveContentsPeak(ctx, openMessageWorkspace)
		if err != nil {
			return nil, storeError(err)
		}
		defer lease.release()
		if err := store.authorizeAttachmentContent(ctx, session, fid, object.attachment.message); err != nil {
			return nil, attachmentError(err)
		}
	default:
		if _, err := store.folder(ctx, session, fid); err != nil {
			return nil, storeError(err)
		}
	}
	// Backend metadata is private.
	return nil, 0
}

func (s *namedPropertyRegistry) queryIDs(props core.Properties, op Operation) []uint16 {
	s.mu.Lock()
	defer s.mu.Unlock()
	var ids []uint16
	for key := range props {
		name, err := faiNamedProperty(key)
		if err != nil || op.NamedPropertySet != nil && name.Set != *op.NamedPropertySet {
			continue
		}
		if name.Kind == 0 && op.Flags&namedPropertyNoIDs != 0 || name.Kind == 1 && op.Flags&namedPropertyNoStrings != 0 {
			continue
		}
		if id := s.byKey[name]; id != 0 {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	return slices.Compact(ids)
}
