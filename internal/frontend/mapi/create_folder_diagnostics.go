// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/binary"
	"fmt"
)

// CreateFolderDiagnostic retains header categories and the ROP return code.
// It never keeps any string, request slice, handle, parent identity or raw option.
type CreateFolderDiagnostic struct {
	state, kind                 byte
	unicode, existing, reserved bool
	result                      uint32
}

func (CreateFolderDiagnostic) String() string   { return "CreateFolderDiagnostic{redacted}" }
func (CreateFolderDiagnostic) GoString() string { return "CreateFolderDiagnostic{redacted}" }

func (d CreateFolderDiagnostic) Fields() (inspection, kind, encoding, existing, reserved string) {
	if d.state == 0 {
		return "none", "none", "none", "none", "none"
	}
	if d.state == 1 {
		return "malformed", "none", "none", "none", "none"
	}
	inspection, kind = "header_only", "other"
	if d.state == 3 {
		inspection = "executed"
	}
	if d.kind == 1 {
		kind = "generic"
	} else if d.kind == 2 {
		kind = "search"
	}
	encoding = "string8"
	if d.unicode {
		encoding = "unicode"
	}
	existing = "false"
	if d.existing {
		existing = "true"
	}
	reserved = "zero"
	if d.reserved {
		reserved = "nonzero"
	}
	return
}

func (d CreateFolderDiagnostic) Result() string {
	if d.state != 3 {
		return ""
	}
	return fmt.Sprintf("0x%08x", d.result)
}

func (d *CreateFolderDiagnostic) record(request createFolderRequest, response []byte) {
	result := binary.LittleEndian.Uint32(response[2:])
	// Keep the first execution unless a later operation reports a failure.
	if d.state == 3 && (d.result != 0 || result == 0) {
		return
	}
	*d = CreateFolderDiagnostic{state: 3, unicode: true, existing: request.existing, reserved: request.reserved != 0, result: result}
	if request.kind == 1 || request.kind == 2 {
		d.kind = request.kind
	}
}

// Called exactly at the first unsupported ROP after a decoded prefix. Only
// the five fixed bytes after the common header are read. HeaderOnly does not
// validate DisplayName/Comment termination, the parent object or batch suffix.
func inspectCreateFolderHeader(rest []byte, handles int) CreateFolderDiagnostic {
	if handles < 1 || handles > MaxHandles || len(rest) < 5 || int(rest[0]) >= handles {
		return CreateFolderDiagnostic{state: 1}
	}
	d := CreateFolderDiagnostic{state: 2, unicode: rest[2] != 0, existing: rest[3] != 0, reserved: rest[4] != 0}
	if rest[1] == 1 || rest[1] == 2 {
		d.kind = rest[1]
	}
	return d
}
func (t *SessionTransport) EnableCreateFolderTrace() { t.createFolderTrace = true }
