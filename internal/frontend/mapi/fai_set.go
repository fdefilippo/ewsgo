// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"time"

	"ewsgo/internal/core"
)

type faiPropertyProblem struct {
	index     uint16
	tag, code uint32
}

// faiSetDisposition separates wire recognition from storage policy. A rejected
// property is reported through MS-OXCDATA PropertyProblem without preventing
// the other supported properties from being staged. Fatal validation, quota
// and authorization errors still leave the entire draft unchanged.
func faiSetDisposition(v faiWireProperty, current core.Properties) (retain bool, code uint32) {
	switch v.tag {
	case 0x300b0102, 0x3ffa001f: // SearchKey, LastModifierName: server-owned.
		return false, accessDenied
	case 0x30080040: // The import header owns LastModificationTime.
		if previous, ok := current[core.LastModifiedAt].(core.Timestamp); ok && time.Time(previous).Equal(time.Time(v.value.(core.Timestamp))) {
			return false, 0
		}
		return false, accessDenied
	case 0x0e070003:
		// FAI has no read/unread state. An identical derived mfFAI value,
		// optionally carrying the ignored mfRead bit, needs no stored value.
		// Other flags/transport state are outside this laboratory profile.
		if flags := v.value.(core.Integer); flags == 0x40 || flags == 0x41 {
			return false, 0
		}
		return false, NotSupported
	case 0x685d0003:
		// Observed ID+type only; no documented meaning or storage contract.
		return false, NotSupported
	case 0x36e20003:
		// PidTagOrdinalMost belongs to a Folder's Task ordering contract.
		// Recognize the scalar without storing it as FAI configuration.
		return false, NotSupported
	case 0x00170003:
		if importance := v.value.(core.Integer); importance < 0 || importance > 2 {
			return false, 0x80070057
		}
	case 0x00360003:
		if sensitivity := v.value.(core.Integer); sensitivity < 0 || sensitivity > 3 {
			return false, 0x80070057
		}
	}
	return true, 0
}
