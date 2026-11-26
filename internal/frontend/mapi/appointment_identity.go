// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/binary"
	"encoding/hex"
	"strings"

	"ewsgo/internal/core"
)

const globalObjectIDHeader = "040000008200E00074C5B7101A82E008"
const globalObjectIDPrefix = "\x04\x00\x00\x00\x82\x00\xe0\x00\x74\xc5\xb7\x10\x1a\x82\xe0\x08"

// Imported meetings share their organizer's ID. A native iCalendar UID is
// wrapped as UTF-8 per MS-OXCICAL 2.1.3.1.1.20.26, preserving its exact value.
func appointmentGlobalIdentity(uid string) core.Binary {
	const headerSize = 40
	const thirdPartyMarker = "vCal-Uid\x01\x00\x00\x00"
	if uid == "" || len(uid) > 0xffff-headerSize-len(thirdPartyMarker) || !validMailText(uid) {
		return nil
	}
	if len(uid) >= 2*(headerSize+1) && strings.EqualFold(uid[:32], globalObjectIDHeader) {
		if decoded, err := hex.DecodeString(uid); err == nil && uint32(len(decoded)-headerSize) == binary.LittleEndian.Uint32(decoded[36:]) {
			clear(decoded[16:20]) // This is the single appointment or the series master.
			return core.Binary(decoded)
		}
	}
	data := append([]byte(globalObjectIDPrefix), make([]byte, 20)...)
	data = binary.LittleEndian.AppendUint32(data, uint32(len(thirdPartyMarker)+len(uid)))
	data = append(data, thirdPartyMarker...)
	return core.Binary(append(data, uid...))
}
