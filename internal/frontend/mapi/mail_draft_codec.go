// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"

	"ewsgo/internal/core"
)

const sentMailServerIDTag = 0x674000fb

func (s *ramStore) isDefaultSentServerID(raw []byte) bool {
	if len(raw) != 21 || raw[0] != 1 || binary.LittleEndian.Uint64(raw[9:]) != 0 || binary.LittleEndian.Uint32(raw[17:]) != 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	fid := binary.LittleEndian.Uint64(raw[1:])
	return fid != 0 && fid == s.byKey[folderKey{native: s.sent}]
}

// A normal message's SetProperties list can mix editable MIME fields and
// client-specific metadata. Consume documented wire types so that an unknown
// property can get its own error without rejecting the rest of the batch.
func parseMailDraftSet(r *reader) (faiSetRequest, error) {
	return parseMessageSetValues(r, readMailDraftSetValue)
}

func parseMessageSetValues(r *reader, readValue func(*reader, uint32) (core.Value, error)) (faiSetRequest, error) {
	h := r.take(4)
	if r.err != nil {
		return nil, ErrMalformed
	}
	size, count := int(binary.LittleEndian.Uint16(h)), int(binary.LittleEndian.Uint16(h[2:]))
	if size < 2 || count > (size-2)/4 {
		return nil, ErrMalformed
	}
	list := reader{data: r.take(size - 2)}
	if r.err != nil {
		return nil, ErrMalformed
	}
	out := make(faiSetRequest, 0, count)
	for range count {
		tag := list.u32()
		if list.err != nil {
			return nil, ErrMalformed
		}
		value, err := readValue(&list, tag)
		if err != nil {
			return nil, err
		}
		out = append(out, faiWireProperty{tag, value})
	}
	return out, list.done()
}

func parseMailImportSet(r *reader) (faiSetRequest, error) {
	return parseMessageSetValues(r, func(r *reader, tag uint32) (core.Value, error) {
		id := tag >> 16
		kind := uint16(tag)
		named := id >= 0x8000 && id <= 0xfffe
		appointment := !named && appointmentImportProperty(fixedContentsProperty(tag))
		if kind == 0x001f && (importedMailPropertyTag(tag) || appointment ||
			contactFields[core.Key{ID: id}] != "" || named) {
			value, err := createFolderString(r, len(r.data)-r.pos)
			return core.Text(value), err
		}
		// The wire IDs of named properties depend on the store. Decode scalar
		// values here and let the authorized import handle select its schema.
		if importedMailPropertyTag(tag) || appointment ||
			named && contentsScalarType(kind) {
			return readContentsValue(r, kind)
		}
		return readMailDraftSetValue(r, tag)
	})
}

func readMailDraftSetValue(r *reader, tag uint32) (core.Value, error) {
	kind := uint16(tag)
	if kind == 0x001f && (mailDraftTextTag(tag) || draftAttachmentTag(tag)) {
		value, err := createFolderString(r, len(r.data)-r.pos)
		return core.Text(value), err
	}
	if tag == 0x0e070003 || mailDraftSetTag(tag) || draftAttachmentTag(tag) {
		return readContentsValue(r, kind)
	}
	if kind == 0x00fb {
		// PtypServerId includes its Ours byte in the counted payload.
		size := readRecipientUint16(r)
		value := r.take(int(size))
		if r.err != nil || len(value) == 0 || value[0] > 1 || value[0] == 1 && len(value) != 21 {
			return nil, ErrMalformed
		}
		if tag == sentMailServerIDTag {
			return core.Binary(bytes.Clone(value)), nil
		}
		return nil, nil
	}
	if kind&0x1000 == 0 {
		if skipScalarPropertyValue(r, kind) {
			return nil, r.err
		}
		return nil, ErrUnsupported
	}
	element := kind &^ 0x1000
	switch element {
	case 2, 3, 4, 5, 6, 7, 0x14, 0x1e, 0x1f, 0x40, 0x48, 0x102:
	default:
		return nil, ErrUnsupported
	}
	count := r.u32()
	if r.err != nil || count > uint32(len(r.data)-r.pos) {
		return nil, ErrMalformed
	}
	for range count {
		skipScalarPropertyValue(r, element)
		if r.err != nil {
			return nil, r.err
		}
	}
	return nil, nil
}
