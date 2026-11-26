// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
)

// ROPInspection contains only closed diagnostic labels. It describes the
// framing and, for a single plain buffer, the first operation's public opcode.
// Release/OpenFolder/Logon receive their existing layout checks; Table flags
// are classified only with a complete fixed header and valid slots. Other
// named opcodes are not claims that a request is well-formed or implemented.
// It does not validate an entire ROP batch, create handles or execute anything.
type ROPInspection struct {
	Layout string
	First  string
	Flags  []string
}

func InspectROP(data []byte) ROPInspection {
	bad := ROPInspection{Layout: "malformed", First: "none"}
	if len(data) < 10 || len(data) > MaxBody {
		return bad
	}
	at, buffers := 0, 0
	compressed, obfuscated, last := false, false, false
	for at < len(data) {
		if buffers >= 256 {
			return ROPInspection{Layout: "limit", First: "none"}
		}
		if len(data)-at < 8 {
			return bad
		}
		h := data[at:]
		version, flags := binary.LittleEndian.Uint16(h), binary.LittleEndian.Uint16(h[2:])
		size, actual := int(binary.LittleEndian.Uint16(h[4:])), int(binary.LittleEndian.Uint16(h[6:]))
		if version != 0 || flags & ^uint16(7) != 0 || size < 2 || size > len(h)-8 {
			return bad
		}
		if flags&1 == 0 && size != actual || flags&1 != 0 && size >= actual {
			return bad
		}
		compressed = compressed || flags&1 != 0
		obfuscated = obfuscated || flags&2 != 0
		at += 8 + size
		buffers++
		last = flags&4 != 0
		if last {
			break
		}
	}
	if !last || at != len(data) {
		return bad
	}
	result := ROPInspection{Layout: "plain", First: "none"}
	if buffers > 1 {
		result.Layout = "packed"
	}
	if compressed {
		if buffers > 1 {
			result.Layout = "packed_compressed"
		} else {
			result.Layout = "compressed"
		}
	}
	if obfuscated {
		if result.Layout == "plain" {
			result.Layout = "xor"
		} else {
			result.Layout += "_xor"
		}
	}
	if result.Layout != "plain" {
		return result
	}
	buffer, err := ParseROP(data)
	if err != nil {
		return bad
	}
	if len(buffer.Operations) == 0 {
		result.First = "empty"
		return result
	}
	if buffer.Operations[0] != 1 && buffer.Operations[0] != 2 && buffer.Operations[0] != 0xfe {
		result.First = "other"
		if name := publicROPLabels[buffer.Operations[0]]; name != "" {
			result.First = name
		}
		result.Flags = inspectTableFlags(buffer.Operations, len(buffer.Handles))
		return result
	}
	if len(buffer.Operations) < 3 {
		result.First = "malformed"
		return result
	}
	op := buffer.Operations
	result.First = "malformed"
	if int(op[2]) >= len(buffer.Handles) {
		return result
	}
	switch op[0] {
	case 0x01:
		result.First = "release"
	case 0x02:
		if len(op) < 13 || int(op[3]) >= len(buffer.Handles) {
			return result
		}
		result.First = "open_folder"
	case 0xfe:
		if len(op) < 14 {
			return result
		}
		size := int(binary.LittleEndian.Uint16(op[12:]))
		if size > 4096 || size > len(op)-14 {
			return result
		}
		if size > 0 {
			name := op[14 : 14+size]
			if name[size-1] != 0 || bytes.IndexByte(name[:size-1], 0) >= 0 {
				return result
			}
			for _, b := range name[:size-1] {
				if b < 32 || b > 127 {
					return result
				}
			}
		}
		result.First = "logon"
		// OutputHandleIndex was checked, not an input handle's value. LogonId,
		// handle values and Essdn never leave this routine.
		for _, flag := range []struct {
			bit  byte
			name string
		}{{1, "private"}, {2, "undercover"}, {4, "ghosted"}, {8, "spooler_process"}} {
			if op[3]&flag.bit != 0 {
				result.Flags = append(result.Flags, flag.name)
			}
		}
		if op[3]&0xf0 != 0 {
			result.Flags = append(result.Flags, "other_logon_bits")
		}
		open := binary.LittleEndian.Uint32(op[4:])
		known := uint32(0)
		for _, flag := range []struct {
			bit  uint32
			name string
		}{
			{1, "admin_privilege"}, {2, "public"}, {4, "home_logon"}, {8, "take_ownership"},
			{0x100, "alternate_server"}, {0x200, "ignore_home_mdb"}, {0x400, "no_mail"},
			{0x01000000, "per_mdb_replid_mapping"}, {0x20000000, "support_progress"},
		} {
			known |= flag.bit
			if open&flag.bit != 0 {
				result.Flags = append(result.Flags, flag.name)
			}
		}
		if open & ^known != 0 {
			result.Flags = append(result.Flags, "other_open_bits")
		}
	default:
		result.First = "other"
	}
	return result
}

// The flag vocabulary is public protocol metadata, independent of support in
// parseTableOperation. Never scan for a later opcode in unvalidated bytes.
func inspectTableFlags(op []byte, handles int) []string {
	if len(op) < 5 || int(op[2]) >= handles || int(op[3]) >= handles {
		return nil
	}
	type bitLabel struct {
		bit   byte
		label string
	}
	var bits []bitLabel
	switch op[0] {
	case 0x04:
		bits = []bitLabel{{4, "table_depth"}, {8, "table_deferred_errors"}, {0x10, "table_no_notifications"}, {0x20, "table_soft_deletes"}, {0x40, "table_unicode"}, {0x80, "table_suppress_notifications"}}
	case 0x05:
		bits = []bitLabel{{2, "table_associated"}, {8, "table_deferred_errors"}, {0x10, "table_no_notifications"}, {0x20, "table_soft_deletes"}, {0x40, "table_unicode"}}
	case 0x21:
		bits = []bitLabel{{0x40, "table_unicode"}}
	case 0x3e:
		bits = []bitLabel{{2, "table_include_free_busy"}}
	default:
		return nil
	}
	if op[4] == 0 {
		return []string{"table_flags_zero"}
	}
	var flags []string
	known := byte(0)
	for _, b := range bits {
		known |= b.bit
		if op[4]&b.bit != 0 {
			flags = append(flags, b.label)
		}
	}
	if op[4] & ^known != 0 {
		flags = append(flags, "table_other_bits")
	}
	return flags
}
