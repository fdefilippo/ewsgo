// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"unicode/utf16"
	"unicode/utf8"
)

const MaxPropertyTags = 256
const maxWirePropertyTags = 100000
const ErrorsReturned uint32 = 0x00040380
const TableTooBig uint32 = 0x80040403
const PropertyNotFound uint32 = 0x8004010f

var ErrTooManyProperties = errors.New("NSPI property list exceeds local limit")

// GetProps is MS-OXCMAPIHTTP 2.2.5.7.1, not the ROP GetProperties request.
// A nil tag array means defaults; a present empty array requests no values.
type GetProps struct {
	Flags     uint32
	State     *STAT
	Tags      []uint32
	Auxiliary []byte
}

func readSTAT(r *reader) *STAT {
	return &STAT{SortType: r.u32(), ContainerID: r.u32(), CurrentRec: r.u32(), Delta: int32(r.u32()), NumPos: r.u32(), TotalRecs: r.u32(), CodePage: r.u32(), TemplateLocale: r.u32(), SortLocale: r.u32()}
}

func appendSTAT(out []byte, s *STAT) []byte {
	for _, n := range []uint32{s.SortType, s.ContainerID, s.CurrentRec, uint32(s.Delta), s.NumPos, s.TotalRecs, s.CodePage, s.TemplateLocale, s.SortLocale} {
		out = append32(out, n)
	}
	return out
}

func ParseGetProps(data []byte) (GetProps, error) {
	if len(data) > MaxBody {
		return GetProps{}, ErrMalformed
	}
	r := reader{data: data}
	v := GetProps{Flags: r.u32()}
	hasState := r.take(1)
	if len(hasState) != 1 {
		return GetProps{}, ErrMalformed
	}
	if hasState[0] != 0 {
		v.State = readSTAT(&r)
	}
	hasTags := r.take(1)
	if len(hasTags) != 1 {
		return GetProps{}, ErrMalformed
	}
	tooMany := false
	if hasTags[0] != 0 {
		count := r.u32()
		if count > maxWirePropertyTags {
			return GetProps{}, ErrMalformed
		}
		wire := r.take(int(count) * 4)
		if r.err != nil {
			return GetProps{}, ErrMalformed
		}
		tooMany = count > MaxPropertyTags
		if !tooMany {
			v.Tags = make([]uint32, int(count))
			for i := range v.Tags {
				v.Tags[i] = binary.LittleEndian.Uint32(wire[i*4:])
			}
		}
	}
	v.Auxiliary = bytes.Clone(r.blob(MaxAuxiliary))
	if err := r.done(); err != nil {
		return GetProps{}, err
	}
	if tooMany {
		return v, ErrTooManyProperties
	}
	return v, nil
}

func (v GetProps) Marshal() ([]byte, error) {
	if len(v.Tags) > MaxPropertyTags || len(v.Auxiliary) > MaxAuxiliary {
		return nil, ErrMalformed
	}
	out := append32(nil, v.Flags)
	if v.State == nil {
		out = append(out, 0)
	} else {
		out = appendSTAT(append(out, 1), v.State)
	}
	if v.Tags == nil {
		out = append(out, 0)
	} else {
		out = append32(append(out, 1), uint32(len(v.Tags)))
		for _, tag := range v.Tags {
			out = append32(out, tag)
		}
	}
	return appendBlob(out, v.Auxiliary), nil
}

// addressBookValue carries one already-encoded AddressBookPropertyValue. Unlike
// ROP PropertyRow, every item has a tag; strings/binary have a HasValue byte.
type addressBookValue struct {
	tag  uint32
	data []byte
}

func missingProperty(tag uint32) addressBookValue {
	return addressBookValue{tag: tag&0xffff0000 | 0x000a, data: append32(nil, PropertyNotFound)}
}

func getPropsResponse(code, codePage uint32, values []addressBookValue) []byte {
	out := append32(append32(append32(nil, 0), code), codePage)
	if code != 0 && code != ErrorsReturned || values == nil {
		out = append(out, 0)
	} else {
		out = append32(append(out, 1), uint32(len(values)))
		for _, v := range values {
			out = append(append32(out, v.tag), v.data...)
		}
	}
	return append32(out, 0)
}

// Windows-1252's upper control range is not Latin-1. Unrepresentable Unicode
// uses '?' (the substitution permitted by MS-OXNSPI 3.1.4.3.3).
var cp1252 = [...]rune{'€', 0x81, '‚', 'ƒ', '„', '…', '†', '‡', 'ˆ', '‰', 'Š', '‹', 'Œ', 0x8d, 'Ž', 0x8f, 0x90, '‘', '’', '“', '”', '•', '–', '—', '˜', '™', 'š', '›', 'œ', 0x9d, 'ž', 'Ÿ'}

func addressBookString(value string, kind uint16, codePage uint32) ([]byte, bool) {
	data, ok := propertyString(value, kind, codePage)
	if !ok {
		return nil, false
	}
	return append([]byte{0xff}, data...), true
}

// Shared scalar string encoding only. NSPI adds its HasValue byte separately;
// ROP PropertyRow uses its own flags and must not reuse the NSPI wire layout.
func propertyString(value string, kind uint16, codePage uint32) ([]byte, bool) {
	if len(value) > 8192 || !utf8.ValidString(value) || bytes.IndexByte([]byte(value), 0) >= 0 {
		return nil, false
	}
	out := []byte{}
	switch kind {
	case 0x001f:
		for _, r := range utf16.Encode([]rune(value)) {
			out = binary.LittleEndian.AppendUint16(out, r)
		}
		return binary.LittleEndian.AppendUint16(out, 0), true
	case 0x001e:
		return propertyString8(value, codePage)
	}
	return nil, false
}

// Input is already validated by the caller. FastTransfer can carry strings
// longer than the scalar property-row bound.
func propertyString8(value string, codePage uint32) ([]byte, bool) {
	switch codePage {
	case 65001:
		return append([]byte(value), 0), true
	case 1252:
		out := make([]byte, 0, len(value)+1)
		for _, r := range value {
			b := byte('?')
			if r < 0x80 || r >= 0xa0 && r <= 0xff {
				b = byte(r)
			} else {
				for i, c := range cp1252 {
					if r == c {
						b = byte(i + 0x80)
						break
					}
				}
			}
			out = append(out, b)
		}
		return append(out, 0), true
	}
	return nil, false
}
