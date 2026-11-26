// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"strconv"
)

const maxSetListDiagnosticProperties = 16
const maxSetListDiagnosticBytes = 16 << 10

// No values, lengths, slices, high/reserved tags or object IDs survive.
// A list-only result establishes cursor boundaries, not valid property values,
// a valid target, supported semantics or a fully valid ROP batch.
type SetListDiagnostic struct {
	state                        byte
	tags                         [maxSetListDiagnosticProperties]uint32
	low, high, reserved, visited uint8
}

func (SetListDiagnostic) String() string   { return "SetListDiagnostic{redacted}" }
func (SetListDiagnostic) GoString() string { return "SetListDiagnostic{redacted}" }

func (d SetListDiagnostic) Fields() (inspection string, tags []string, visited, high, reserved string) {
	inspection = "none"
	switch d.state {
	case 1:
		inspection = "malformed"
	case 2:
		inspection = "limit"
	case 3:
		inspection = "stopped_type"
	case 4:
		inspection = "list_only"
	}
	if d.state != 3 && d.state != 4 {
		return inspection, []string{"none"}, "none", "none", "none"
	}
	tags = make([]string, 0, d.low)
	for _, tag := range d.tags[:d.low] {
		tags = append(tags, fmt.Sprintf("0x%08x", tag))
	}
	return inspection, tags, strconv.Itoa(int(d.visited)), strconv.Itoa(int(d.high)), strconv.Itoa(int(d.reserved))
}

// Only the first SetProperties ROP in plain framing is inspected. Cursor
// movement is bounded to the declared list, never to the following ROPs.
func InspectSetList(data []byte) SetListDiagnostic {
	ops, handles, err := plainROPFields(data)
	if err == ErrUnsupported {
		return SetListDiagnostic{}
	}
	bad := SetListDiagnostic{state: 1}
	if err != nil {
		return bad
	}
	return inspectSetListOperation(ops, len(handles)/4)
}

// The store decoder may also supply the exact boundary of an unsupported
// SetProperties following an already decoded prefix. Never search for opcodes.
func inspectSetListOperation(ops []byte, handles int) SetListDiagnostic {
	bad := SetListDiagnostic{state: 1}
	if len(ops) == 0 || !isSetPropertiesROP(ops[0]) {
		return SetListDiagnostic{}
	}
	if len(ops) < 7 || int(ops[2]) >= handles {
		return bad
	}
	size, count := int(binary.LittleEndian.Uint16(ops[3:5])), int(binary.LittleEndian.Uint16(ops[5:7]))
	if size < 2 || size > len(ops)-5 || count > (size-2)/4 {
		return bad
	}
	if size > maxSetListDiagnosticBytes || count > maxSetListDiagnosticProperties {
		return SetListDiagnostic{state: 2}
	}
	r := reader{data: ops[7 : 5+size]}
	d := SetListDiagnostic{state: 4}
	for range count {
		tag := r.u32()
		if r.err != nil {
			return bad
		}
		d.visited++
		switch id := uint16(tag >> 16); {
		case id == 0 || id == 0xffff:
			d.reserved++
		case id >= 0x8000:
			d.high++
		default:
			d.tags[d.low] = tag
			d.low++
		}
		if tag == 0x36d81102 {
			// The one closed MV_BINARY property supported by the RAM bag.
			// Traverse only count/length fields; retain no values or lengths.
			if err := skipAdditionalEntryIDs(&r); err != nil {
				if err == ErrMalformed {
					return bad
				}
				return SetListDiagnostic{state: 2}
			}
		} else if !skipScalarPropertyValue(&r, uint16(tag)) {
			d.state = 3
			return d
		}
		if r.err != nil {
			return bad
		}
	}
	if r.done() != nil {
		return bad
	}
	return d
}

// Skip only documented scalar layouts. Strings are scanned for terminators,
// never decoded or copied; binary payload is not inspected. Multi-valued,
// object, restriction and action types stop at their tag without guessing.
func skipScalarPropertyValue(r *reader, kind uint16) bool {
	switch kind {
	case 0x0002:
		r.take(2)
	case 0x0003, 0x0004, 0x000a:
		r.take(4)
	case 0x0005, 0x0006, 0x0007, 0x0014, 0x0040:
		r.take(8)
	case 0x000b:
		r.take(1)
	case 0x0048:
		r.take(16)
	case 0x001e:
		end := bytes.IndexByte(r.data[r.pos:], 0)
		if end < 0 {
			r.err = ErrMalformed
		} else {
			r.take(end + 1)
		}
	case 0x001f:
		for r.err == nil {
			pair := r.take(2)
			if r.err != nil {
				break
			}
			if pair[0] == 0 && pair[1] == 0 {
				break
			}
		}
	case 0x0102:
		head := r.take(2)
		if r.err == nil {
			r.take(int(binary.LittleEndian.Uint16(head)))
		}
	default:
		return false
	}
	return true
}

func (t *SessionTransport) EnableSetListTrace() { t.setListTrace = true }
