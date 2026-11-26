// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

// MS-OXCMSG 2.2.1.3 bounds the opaque class label. A class does not grant
// write access or select server behavior; the FAI handle owns that authority.
func validMessageClass(class string) bool {
	if len(class) == 0 || len(class) > 255 || class[len(class)-1] == '.' {
		return false
	}
	for i := range len(class) {
		if class[i] < 0x20 || class[i] > 0x7f {
			return false
		}
	}
	return true
}
