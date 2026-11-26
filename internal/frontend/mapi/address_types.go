// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import "encoding/binary"

// Approved, fixed laboratory configuration. EX names the gateway directory;
// SMTP names native mail addresses. This is not permission/capability to send.
// Any future list change must revise GwartTime consistently with Logon.
const addressTypeConfigRevision uint64 = 1
const laboratoryAddressTypes = "EX\x00SMTP\x00"
const addressTypesResponseSize = 10 + len(laboratoryAddressTypes)

// MS-OXCROPS 2.2.7.3.2 and MS-OXOMSG 2.2.4.3: the referenced server object is
// ignored, while the input handle-table index is validated by the decoder.
// Authentication, route, scope and context remain checked by SessionTransport.
func addressTypesResponse(input byte) []byte {
	out := ropError(0x49, input, 0)
	out = binary.LittleEndian.AppendUint16(out, 2)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(laboratoryAddressTypes)))
	return append(out, laboratoryAddressTypes...)
}
