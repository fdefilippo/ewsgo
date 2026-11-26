// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"

	"ewsgo/internal/core"
)

// Search criteria can be read by a different session than the one that set
// them. Store decoded text and preserve the original wire for every other field.
type searchRestrictionText struct {
	codePage uint32
	fields   []searchTextField
}

type searchTextField struct {
	start, end int
	value      core.Text
	hasValue   bool
}

func (t *searchRestrictionText) normalizeTag(tag uint32) uint32 {
	if t != nil && uint16(tag) == 0x1e {
		return tag&0xffff0000 | 0x1f
	}
	return tag
}

func (t *searchRestrictionText) remember(start, end int, value core.Text, hasValue bool) {
	if t != nil {
		t.fields = append(t.fields, searchTextField{start: start, end: end, value: value, hasValue: hasValue})
	}
}

func restrictionCharset(codePage uint32) string {
	switch codePage {
	case 1252:
		return "windows-1252"
	case 65001:
		return "utf-8"
	default:
		return ""
	}
}

func (t *searchRestrictionText) readString8(r *reader) (core.Text, error) {
	charset := restrictionCharset(t.codePage)
	if charset == "" {
		return "", ErrUnsupported
	}
	end := bytes.IndexByte(r.data[r.pos:], 0)
	if end < 0 {
		return "", ErrMalformed
	}
	wire := r.take(end + 1)
	value, err := decodeMailText(charset, wire[:end])
	if err != nil {
		return "", ErrMalformed
	}
	return core.Text(value), nil
}

func searchRestrictionWire(raw []byte, fields []searchTextField, unicode bool, codePage uint32) ([]byte, bool) {
	if len(fields) == 0 {
		return raw, true
	}
	kind := uint16(0x1f)
	if !unicode {
		kind = 0x1e
	}
	out := make([]byte, 0, len(raw))
	previous := 0
	for _, field := range fields {
		out = append(out, raw[previous:field.start]...)
		start := len(out)
		out = append(out, raw[field.start:field.start+4]...)
		out[start] = byte(kind)
		if field.hasValue {
			wire, ok := propertyString(string(field.value), kind, codePage)
			if !ok {
				return nil, false
			}
			if !unicode {
				decoded, err := decodeMailText(restrictionCharset(codePage), wire[:len(wire)-1])
				if err != nil || decoded != string(field.value) {
					return nil, false
				}
			}
			out = append(out, raw[field.start+4:field.start+8]...)
			out[start+4] = byte(kind)
			out = append(out, wire...)
		}
		previous = field.end
	}
	return append(out, raw[previous:]...), true
}
