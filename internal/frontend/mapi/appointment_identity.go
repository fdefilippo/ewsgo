// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"strings"

	"ewsgo/internal/core"
)

const globalObjectIDHeader = "040000008200E00074C5B7101A82E008"
const thirdPartyMarker = "vCal-Uid\x01\x00\x00\x00"
const globalObjectIDPrefix = "\x04\x00\x00\x00\x82\x00\xe0\x00\x74\xc5\xb7\x10\x1a\x82\xe0\x08"

// Imported meetings share their organizer's ID. A native iCalendar UID is
// wrapped as UTF-8 per MS-OXCICAL 2.1.3.1.1.20.26, preserving its exact value.
func appointmentGlobalIdentity(uid string) core.Binary {
	const headerSize = 40
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

// The date in an occurrence GOID is part of its identity. Meeting dispatch
// currently accepts the single appointment/master form only.
func appointmentUID(raw core.Binary) (string, error) {
	if len(raw) <= 40 || len(raw) > 4096 || !bytes.HasPrefix(raw, []byte(globalObjectIDPrefix)) ||
		!bytes.Equal(raw[16:20], []byte{0, 0, 0, 0}) || binary.LittleEndian.Uint32(raw[36:]) != uint32(len(raw)-40) {
		return "", core.ErrInvalid
	}
	id := strings.ToUpper(hex.EncodeToString(raw))
	if native, ok := strings.CutPrefix(string(raw[40:]), thirdPartyMarker); ok {
		id = native
	}
	if id == "" || len(id) > 255 || !validMailText(id) || strings.TrimSpace(id) != id || strings.ContainsAny(id, "\r\n") {
		return "", core.ErrInvalid
	}
	return id, nil
}
