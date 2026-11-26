// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import "regexp"

const messageIDAtom = `[A-Za-z0-9!#$%&'*+/=?^_\x60{|}~-]+`
const messageIDDotAtom = messageIDAtom + `(\.` + messageIDAtom + `)*`

var internetMessageID = regexp.MustCompile("^" + messageIDDotAtom + "@" + messageIDDotAtom + "$")

// ValidInternetMessageID accepts the dot-atom form of a Message-ID,
// excluding the angle brackets of the MIME header field.
func ValidInternetMessageID(value string) bool {
	return len(value) <= 984 && internetMessageID.MatchString(value)
}
