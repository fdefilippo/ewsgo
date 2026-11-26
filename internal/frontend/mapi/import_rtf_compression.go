// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/binary"
	"hash/crc32"

	"ewsgo/internal/core"
)

func uncompressedRTFEnvelope(rtf []byte) []byte {
	out := make([]byte, 16, 16+len(rtf))
	binary.LittleEndian.PutUint32(out, uint32(12+len(rtf)))
	binary.LittleEndian.PutUint32(out[4:], uint32(len(rtf)))
	copy(out[8:], "MELA")
	return append(out, rtf...)
}

// MS-OXRTFCP §2.1.2.1 places this prefix at offset zero in the ring.
const rtfDictionaryPrefix = `{\rtf1\ansi\mac\deff0\deftab720{\fonttbl;}{\f0\fnil \froman \fswiss \fmodern \fscript \fdecor MS Sans SerifSymbolArialTimes New RomanCourier{\colortbl\red0\green0\blue0` +
	"\r\n" + `\par \pard\plain\f0\fs20\b\i\u\tab\tx`

// uncompressImportRTF decodes the two MS-OXRTFCP envelopes, checking the
// advertised output size before allocating and requiring the LZFu end marker.
func uncompressImportRTF(raw []byte) ([]byte, error) {
	const headerSize = 16
	if len(raw) < headerSize || uint64(binary.LittleEndian.Uint32(raw)) != uint64(len(raw)-4) {
		return nil, core.ErrInvalid
	}
	size := binary.LittleEndian.Uint32(raw[4:])
	if size > maxMessageImportBytes {
		return nil, core.ErrLimit
	}
	kind := string(raw[8:12])
	checksum := binary.LittleEndian.Uint32(raw[12:])
	data := raw[headerSize:]
	switch kind {
	case "MELA":
		if len(data) != int(size) {
			return nil, core.ErrInvalid
		}
		return data, nil
	case "LZFu":
		// This CRC starts with zero and omits the final complement. Go
		// complements both its CRC input and output.
		if ^crc32.Update(^uint32(0), crc32.IEEETable, data) != checksum {
			return nil, core.ErrInvalid
		}
		return expandImportRTF(data, int(size))
	default:
		return nil, core.ErrInvalid
	}
}

func expandImportRTF(data []byte, size int) ([]byte, error) {
	const ringMask = 4095
	var dictionary [ringMask + 1]byte
	write := copy(dictionary[:], rtfDictionaryPrefix)
	initialized := write
	cursor := 0
	result := make([]byte, 0, size)
	r := reader{data: data}
	for r.pos < len(data) {
		control := r.data[r.pos]
		r.pos++
		for bit := byte(1); bit != 0; bit <<= 1 {
			length := 1
			literal := byte(0)
			if control&bit == 0 {
				field := r.take(1)
				if r.err != nil {
					return nil, core.ErrInvalid
				}
				literal = field[0]
			} else {
				reference := r.take(2)
				if r.err != nil {
					return nil, core.ErrInvalid
				}
				token := binary.BigEndian.Uint16(reference)
				cursor = int(token >> 4)
				if cursor == write {
					if len(result) != size {
						return nil, core.ErrInvalid
					}
					return result, nil // Remaining bytes are stream padding.
				}
				length = int(token&15) + 2
			}
			if r.err != nil || length > size-len(result) {
				return nil, core.ErrInvalid
			}
			for range length {
				value := literal
				if control&bit != 0 {
					if cursor >= initialized {
						return nil, core.ErrInvalid
					}
					value = dictionary[cursor]
					cursor = (cursor + 1) & ringMask
				}
				result = append(result, value)
				dictionary[write] = value
				write = (write + 1) & ringMask
				if initialized < len(dictionary) {
					initialized++
				}
			}
		}
	}
	return nil, core.ErrInvalid
}
