// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/binary"

	"ewsgo/internal/core"
)

type hierarchyImportProperty struct {
	tag  uint32
	wire []byte
}

type hierarchyImport struct {
	parent, source [22]byte
	modified       core.Timestamp
	change         changeXID
	predecessors   []byte
	name           string
	properties     []hierarchyImportProperty
}

func (hierarchyImport) String() string   { return "hierarchyImport{private}" }
func (hierarchyImport) GoString() string { return "hierarchyImport{private}" }

// Decode the two tagged property arrays (MS-OXCFXICS 2.2.3.2.4.3.1).
// Additional values retain their original encoding so execution can apply
// the Logon code page and validate them for the chosen folder class.
func parseHierarchyImport(r *reader) (hierarchyImport, error) {
	var out hierarchyImport
	fixed := [...]uint32{0x65e10102, 0x65e00102, 0x30080040, 0x65e20102, 0x65e30102, 0x3001001f}
	count := r.take(2)
	if r.err != nil || int(binary.LittleEndian.Uint16(count)) != len(fixed) {
		return hierarchyImport{}, ErrMalformed
	}
	for _, expected := range fixed {
		tag := r.u32()
		if tag != expected {
			return hierarchyImport{}, ErrMalformed
		}
		value, err := readContentsValue(r, uint16(tag))
		if err != nil {
			return hierarchyImport{}, err
		}
		switch tag {
		case 0x65e10102, 0x65e00102:
			raw := value.(core.Binary)
			parentIsAnchor := tag == 0x65e10102 && len(raw) == 0
			if !parentIsAnchor && (len(raw) != 22 || !validFAIXID([22]byte(raw))) {
				return hierarchyImport{}, ErrMalformed
			}
			if tag == 0x65e10102 {
				copy(out.parent[:], raw)
			} else {
				copy(out.source[:], raw)
			}
		case 0x30080040:
			out.modified = value.(core.Timestamp)
		case 0x65e20102:
			out.change = changeXID(value.(core.Binary))
			if !out.change.valid() {
				return hierarchyImport{}, ErrMalformed
			}
		case 0x65e30102:
			out.predecessors = value.(core.Binary)
		case 0x3001001f:
			out.name = string(value.(core.Text))
			if out.name == "" {
				return hierarchyImport{}, ErrMalformed
			}
		}
	}
	count = r.take(2)
	if r.err != nil {
		return hierarchyImport{}, ErrMalformed
	}
	properties := int(binary.LittleEndian.Uint16(count))
	if properties == 0 || properties > (len(r.data)-r.pos)/4 {
		return hierarchyImport{}, ErrMalformed
	}
	for range properties {
		tag := r.u32()
		if r.err != nil {
			return hierarchyImport{}, ErrMalformed
		}
		start := r.pos
		if err := skipHierarchyImportProperty(r, uint16(tag)); err != nil {
			return hierarchyImport{}, err
		}
		out.properties = append(out.properties, hierarchyImportProperty{tag: tag, wire: r.data[start:r.pos]})
	}
	return out, nil
}

func skipHierarchyImportProperty(r *reader, kind uint16) error {
	switch kind {
	case 0x001e: // String8, NUL-terminated.
		for r.err == nil {
			value := r.take(1)
			if r.err == nil && value[0] == 0 {
				return nil
			}
		}
		return ErrMalformed
	case 0x0048: // PtypGuid
		r.take(16)
		return r.err
	case 0x1102: // PtypMultipleBinary
		return skipAdditionalEntryIDs(r)
	default:
		_, err := readContentsValue(r, kind)
		return err
	}
}
