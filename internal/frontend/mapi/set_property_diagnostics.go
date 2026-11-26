// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/binary"
	"fmt"
	"strconv"
)

const (
	setPropertyNone byte = iota
	setPropertyMalformed
	setPropertyLimit
	setPropertyHeaderOnly
)

// SetPropertyDiagnostic cannot contain values, handles, lengths or strings.
// HeaderOnly is not validation of the values, the object or the complete batch.
type SetPropertyDiagnostic struct {
	state byte
	count uint16
	first uint32
}

// Fields exposes only the owner-approved bounded count and first low-ID tag.
// Private fields prevent other packages from injecting request text here.
func (d SetPropertyDiagnostic) Fields() (inspection, count, firstTag string) {
	switch d.state {
	case setPropertyNone:
		return "none", "none", "none"
	case setPropertyLimit:
		return "limit", "limit", "none"
	case setPropertyHeaderOnly:
		tag := "none"
		if d.count > 0 {
			switch id := uint16(d.first >> 16); {
			case id == 0 || id == 0xffff:
				tag = "reserved_id"
			case id >= 0x8000:
				tag = "high_id"
			default:
				tag = fmt.Sprintf("0x%08x", d.first)
			}
		}
		return "header_only", strconv.Itoa(int(d.count)), tag
	default:
		return "malformed", "none", "none"
	}
}

// InspectSetProperty examines only a first SetProperties header in the existing
// plain ROP framing. It never scans, copies, decodes or retains property values.
func InspectSetProperty(data []byte) SetPropertyDiagnostic {
	ops, handles, err := plainROPFields(data)
	if err == ErrUnsupported {
		return SetPropertyDiagnostic{}
	}
	bad := SetPropertyDiagnostic{state: setPropertyMalformed}
	if err != nil {
		return bad
	}
	if len(ops) == 0 || !isSetPropertiesROP(ops[0]) {
		return SetPropertyDiagnostic{}
	}
	if len(ops) < 7 || int(ops[2]) >= len(handles)/4 {
		return bad
	}
	size := int(binary.LittleEndian.Uint16(ops[3:5]))
	if size < 2 || size > len(ops)-5 {
		return bad
	}
	count := binary.LittleEndian.Uint16(ops[5:7])
	if count > 256 {
		return SetPropertyDiagnostic{state: setPropertyLimit}
	}
	if int(count) > (size-2)/4 || count == 0 && size != 2 {
		return bad
	}
	d := SetPropertyDiagnostic{state: setPropertyHeaderOnly, count: count}
	if count > 0 {
		d.first = binary.LittleEndian.Uint32(ops[7:11])
	}
	return d
}

// Configure before serving, only in the authenticated TLS laboratory RAM store.
// The service validates the prerequisites; no SetProperties execution is added.
func (t *SessionTransport) EnableSetPropertyTrace() { t.setPropertyTrace = true }
