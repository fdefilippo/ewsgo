// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

// SynchronizationDiagnostic contains only closed enum values. No flags,
// restrictions, property tags, handles, FIDs or request text are retained.
type SynchronizationDiagnostic struct {
	state byte
	kind  byte
}

func (d SynchronizationDiagnostic) Fields() (inspection, kind string) {
	switch d.state {
	case 0:
		return "none", "none"
	case 1:
		return "malformed", "none"
	default:
		switch d.kind {
		case 1:
			return "header_only", "contents"
		case 2:
			return "header_only", "hierarchy"
		default:
			return "header_only", "other"
		}
	}
}

// Called only at the first unsupported ROP boundary after a decoded prefix.
// HeaderOnly does not validate restrictions, tags, the object or the batch.
// The complete batch remains unsupported; no prefix is executed.
func inspectSynchronizationHeader(rest []byte, handles int) SynchronizationDiagnostic {
	if len(rest) < 7 || int(rest[0]) >= handles {
		return SynchronizationDiagnostic{state: 1}
	}
	d := SynchronizationDiagnostic{state: 2}
	if rest[1] == 1 || rest[1] == 2 {
		d.kind = rest[1]
	}
	return d
}

// Configure before serving; the service requires the authenticated TLS RAM lab.
func (t *SessionTransport) EnableSynchronizationTrace() { t.synchronizationTrace = true }
