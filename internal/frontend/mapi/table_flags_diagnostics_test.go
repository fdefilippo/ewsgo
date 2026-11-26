// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"reflect"
	"testing"
)

func TestInitialTableFlagDiagnosticsRemainMetadataOnly(t *testing.T) {
	for _, tc := range []struct {
		id, known byte
		names     map[byte]string
	}{
		{4, 0xfc, map[byte]string{4: "table_depth", 8: "table_deferred_errors", 0x10: "table_no_notifications", 0x20: "table_soft_deletes", 0x40: "table_unicode", 0x80: "table_suppress_notifications"}},
		{5, 0x7a, map[byte]string{2: "table_associated", 8: "table_deferred_errors", 0x10: "table_no_notifications", 0x20: "table_soft_deletes", 0x40: "table_unicode"}},
		{0x3e, 2, map[byte]string{2: "table_include_free_busy"}},
	} {
		for flag := 0; flag < 256; flag++ {
			op := []byte{tc.id, 0xfe, 0, 1, byte(flag)}
			wire, _ := (ROPBuffer{Operations: append(op, []byte("private-data")...), Handles: []uint32{0x12345678, 0xffffffff}}).Marshal()
			got := InspectROP(wire)
			var want []string
			if flag == 0 {
				want = []string{"table_flags_zero"}
			} else {
				for bit := byte(1); bit != 0; bit <<= 1 {
					if byte(flag)&bit != 0 && tc.names[bit] != "" {
						want = append(want, tc.names[bit])
					}
				}
				if byte(flag)&^tc.known != 0 {
					want = append(want, "table_other_bits")
				}
			}
			if got.First != publicROPLabels[tc.id] || !reflect.DeepEqual(got.Flags, want) {
				t.Fatalf("flags for %02x/%02x: %v", tc.id, flag, got)
			}
			for n := 1; n < 5; n++ {
				truncated, _ := (ROPBuffer{Operations: op[:n], Handles: []uint32{1, 2}}).Marshal()
				if got := InspectROP(truncated); got.First != publicROPLabels[tc.id] || len(got.Flags) != 0 {
					t.Fatal("truncated header exposed flags")
				}
			}
			for _, slot := range []int{2, 3} {
				invalid := bytes.Clone(op)
				invalid[slot] = 2
				wire, _ = (ROPBuffer{Operations: invalid, Handles: []uint32{1, 2}}).Marshal()
				if len(InspectROP(wire).Flags) != 0 {
					t.Fatal("invalid slot exposed flags")
				}
			}
		}
	}
	// A supported prefix does not authorize looking for a later Table opcode.
	wire, _ := (ROPBuffer{Operations: []byte{1, 4, 0, 0x3e, 4, 0, 1, 2}, Handles: []uint32{1, 2}}).Marshal()
	if got := InspectROP(wire); got.First != "release" || len(got.Flags) != 0 {
		t.Fatal("scanned beyond first operation")
	}
	// Opcode classification does not enable PermissionsTable execution or a prefix.
	wire, _ = (ROPBuffer{Operations: []byte{1, 4, 0, 0x3e, 4, 0, 1, 2}, Handles: []uint32{1, 2}}).Marshal()
	b, _ := ParseROP(wire)
	if ops, err := b.DecodeOperations(); err == nil || ops != nil {
		t.Fatal("diagnostics enabled execution")
	}
}
