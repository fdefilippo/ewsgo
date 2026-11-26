// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func filterWire(t *testing.T, s string) []byte {
	t.Helper()
	v, err := hex.DecodeString(strings.ReplaceAll(s, " ", ""))
	if err != nil {
		t.Fatal("fixture hex")
	}
	return v
}
func filterResolver(registry *namedPropertyRegistry) contentsResolver {
	return func(tag uint32) (contentsProperty, error) {
		return resolveContentsProperty(tag, registry, func(p contentsProperty) bool {
			return p.name != (propertyName{}) || p.id == 0x37 || p.id == 0x1a || p.id == 0xe07 || p.id == 0xe69 || p.id == 0x3008 || p.id == 0x1009
		})
	}
}
func boundFilter(t *testing.T, wire []byte, registry *namedPropertyRegistry) *contentsRestriction {
	t.Helper()
	n, err := parseContentsRestriction(wire)
	if err != nil {
		t.Fatal("restriction fixture parse", err)
	}
	n, err = bindContentsRestriction(n, filterResolver(registry))
	if err != nil {
		t.Fatal("restriction fixture bind", err)
	}
	return n
}
func TestContentsRestrictionIndependentROPFixtures(t *testing.T) {
	// No production encoder: packet layouts below follow MS-OXCDATA 2.12.
	for _, tc := range []struct {
		name, wire string
		values     contentsValues
		want       bool
	}{
		{"exists", "08 1f003700", contentsValues{{id: 0x37, kind: 0x1f}: core.Text("Real subject")}, true},
		{"absent", "08 1f003700", contentsValues{}, false},
		{"int_equal_ignores_value_id", "04 04 0300070e 03000000 08000000", contentsValues{{id: 0xe07, kind: 3}: core.Integer(8)}, true},
		{"int_negative_less", "04 00 0300070e 0300ffff ffffffff", contentsValues{{id: 0xe07, kind: 3}: core.Integer(-2)}, true},
		{"int_not_equal", "04 05 0300070e 03000000 08000000", contentsValues{{id: 0xe07, kind: 3}: core.Integer(8)}, false},
		{"unicode_equal", "04 04 1f003700 1f000000 e9000000", contentsValues{{id: 0x37, kind: 0x1f}: core.Text("é")}, true},
		{"unicode_ordinal_case", "04 04 1f003700 1f000000 41000000", contentsValues{{id: 0x37, kind: 0x1f}: core.Text("a")}, false},
		{"message_class_equal_case_insensitive", "04 04 1f001a00 1f000000 41000000", contentsValues{{id: 0x1a, kind: 0x1f}: core.Text("a")}, true},
		{"message_class_not_equal_case_insensitive", "04 05 1f001a00 1f000000 41000000", contentsValues{{id: 0x1a, kind: 0x1f}: core.Text("a")}, false},
		{"message_class_content_full_case_insensitive", "03 00000000 1f001a00 1f000000 410062000000", contentsValues{{id: 0x1a, kind: 0x1f}: core.Text("aB")}, true},
		{"message_class_content_prefix_case_insensitive", "03 02000000 1f001a00 1f000000 410062000000", contentsValues{{id: 0x1a, kind: 0x1f}: core.Text("aBc")}, true},
		{"bool_equal", "04 04 0b00690e 0b000000 01", contentsValues{{id: 0xe69, kind: 0xb}: core.Bool(true)}, true},
		{"content_prefix", "03 02000000 1f003700 1f000000 610062000000", contentsValues{{id: 0x37, kind: 0x1f}: core.Text("abc")}, true},
		{"content_substring_fold", "03 01000100 1f003700 1f000000 610042000000", contentsValues{{id: 0x37, kind: 0x1f}: core.Text("xxAByy")}, true},
		{"content_full_not_substring", "03 00000000 1f003700 1f000000 610062000000", contentsValues{{id: 0x37, kind: 0x1f}: core.Text("abc")}, false},
		{"binary_content", "03 01000000 02010910 02010000 0200 00ff", contentsValues{{id: 0x1009, kind: 0x102}: core.Binary{4, 0, 255, 8}}, true},
		{"not", "02 08 1f003700", contentsValues{}, true},
		{"and", "00 0200 08 1f003700 08 0300070e", contentsValues{{id: 0x37, kind: 0x1f}: core.Text("subject")}, false},
		{"or", "01 0200 08 1f003700 08 0300070e", contentsValues{{id: 0x37, kind: 0x1f}: core.Text("subject")}, true},
		{"empty_and", "00 0000", contentsValues{}, true},
		{"empty_or", "01 0000", contentsValues{}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			n := boundFilter(t, filterWire(t, tc.wire), nil)
			got, err := n.matches(tc.values)
			if err != nil || got != tc.want {
				t.Fatal("restriction result", got, err)
			}
		})
	}
	for _, rel := range []byte{0, 1, 2, 3, 4, 5} {
		wire := filterWire(t, "04 00 0300070e 03000000 08000000")
		wire[1] = rel
		got, err := boundFilter(t, wire, nil).matches(contentsValues{{id: 0xe07, kind: 3}: core.Integer(8)})
		if err != nil || got != (rel == 1 || rel == 3 || rel == 4) {
			t.Fatal("relational operator")
		}
	}
	if !strings.EqualFold(foldContentsText("ΣςσKKk"), "ΣΣΣKKK") {
		t.Fatal("Unicode fold equivalents")
	}
}

func TestContentsRestrictionNativeDateAndScalarWidths(t *testing.T) {
	for _, kind := range []uint16{2, 3, 0x14, 0xb, 0x40, 0x1f, 0x102} {
		raw := []byte{4, 4}
		raw = binary.LittleEndian.AppendUint32(raw, 0x30080000|uint32(kind))
		raw = binary.LittleEndian.AppendUint32(raw, uint32(kind))
		var value core.Value
		switch kind {
		case 2:
			raw = append(raw, 0xff, 0xff)
			value = core.Integer(-1)
		case 3:
			raw = append(raw, 0xff, 0xff, 0xff, 0xff)
			value = core.Integer(-1)
		case 0x14:
			raw = binary.LittleEndian.AppendUint64(raw, ^uint64(0))
			value = core.Integer(-1)
		case 0xb:
			raw = append(raw, 1)
			value = core.Bool(true)
		case 0x40:
			raw = binary.LittleEndian.AppendUint64(raw, 116444736000000000)
			value = core.Timestamp(time.Unix(0, 0).UTC())
		case 0x1f:
			raw = append(raw, 0x3d, 0xd8, 0x00, 0xde, 0, 0)
			value = core.Text("😀")
		case 0x102:
			raw = append(raw, 2, 0, 1, 2)
			value = core.Binary{1, 2}
		}
		got, err := boundFilter(t, raw, nil).matches(contentsValues{{id: 0x3008, kind: kind}: value})
		if err != nil || !got {
			t.Fatal("scalar width", kind)
		}
		for n := 0; n < len(raw); n++ {
			if root, err := parseContentsRestriction(raw[:n]); n != 0 && (err == nil || root != nil) {
				t.Fatal("truncated scalar accepted", kind, n)
			}
		}
	}
}

func TestContentsRestrictionRejectsMalformedUnsupportedAndQuotas(t *testing.T) {
	for _, wire := range []string{
		"ff", "05", "07", "09", "0a", "0b", // Unimplemented operators, not ignored.
		"06",                               // Truncated BitMask.
		"04 64 0300070e 03000000 00000000", // MemberOfDL is not numeric comparison.
		"04 00 0b00690e 0b000000 01",       // Boolean supports EQ/NE only.
		"04 04 0300070e 14000000 0000000000000000", // Type mismatch.
		"04 04 0b00690e 0b000000 02",               // Invalid Boolean.
		"04 04 1f003700 1f000000 00d80000",         // Lone high surrogate.
		"04 04 40000830 40000000 ffffffffffffffff", // FILETIME overflow.
		"03 00000200 1f003700 1f000000 0000",       // Nonspacing not implemented.
		"03 00000100 02010910 02010000 0000",       // Binary case-fold is not meaningful.
		"03 03000000 1f003700 1f000000 0000",       // Unknown fuzzy mode.
		"08 1f103700",                              // Multivalue unsupported in this slice.
		"08 1f003700 00",                           // Trailing bytes.
	} {
		wire := filterWire(t, wire)
		if n, err := parseContentsRestriction(wire); err == nil || n != nil {
			t.Fatal("unsupported/malformed filter accepted")
		}
	}
	if _, err := parseContentsRestriction(make([]byte, 8193)); !errors.Is(err, ErrUnsupported) {
		t.Fatal("byte quota")
	}
	leaf := filterWire(t, "08 1f003700")
	for _, count := range []int{127, 128} {
		wire := []byte{1, byte(count), 0}
		for i := 0; i < count; i++ {
			wire = append(wire, leaf...)
		}
		_, err := parseContentsRestriction(wire)
		if (count == 127) != (err == nil) {
			t.Fatal("node quota")
		}
	}
	for _, depth := range []int{16, 17} {
		wire := append(bytes.Repeat([]byte{2}, depth-1), leaf...)
		_, err := parseContentsRestriction(wire)
		if (depth == 16) != (err == nil) {
			t.Fatal("depth quota")
		}
	}
	root, err := parseContentsRestriction(leaf)
	if err != nil {
		t.Fatal("fixture")
	}
	if _, err := root.matches(contentsValues{}); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("unbound filter silently matched")
	}
	// Unsupported property in an OR arm is rejected before evaluating anything.
	root, _ = parseContentsRestriction(filterWire(t, "01 0200 08 1f003700 08 1f003412"))
	if bound, err := bindContentsRestriction(root, filterResolver(nil)); !errors.Is(err, core.ErrUnsupported) || bound != nil {
		t.Fatal("unknown short-circuited property ignored")
	}
	n := boundFilter(t, leaf, nil)
	if _, err := n.matches(contentsValues{{id: 0x37, kind: 0x1f}: core.Integer(4)}); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("bad projected value ignored")
	}
}

func TestContentsRestrictionNamedIdentityDetachedAndRedacted(t *testing.T) {
	name := propertyName{Set: [16]byte{0x55, 0x44, 0x33}, Kind: 1, Name: "PRIVATE-property"}
	var first, second namedPropertyRegistry
	a, _ := first.ids(context.Background(), []propertyName{name}, true)
	second.ids(context.Background(), []propertyName{{Set: [16]byte{1}, Kind: 0, LID: 7}}, true)
	b, _ := second.ids(context.Background(), []propertyName{name}, true)
	if a[0] == b[0] {
		t.Fatal("fixture should vary named ID")
	}
	makeFilter := func(id uint16, registry *namedPropertyRegistry) *contentsRestriction {
		wire := []byte{4, 4}
		wire = binary.LittleEndian.AppendUint32(wire, uint32(id)<<16|0x102)
		wire = binary.LittleEndian.AppendUint32(wire, 0x102)
		wire = append(wire, 3, 0, 1, 2, 3)
		n := boundFilter(t, wire, registry)
		wire[len(wire)-1] = 99
		return n
	}
	x, y := makeFilter(a[0], &first), makeFilter(b[0], &second)
	if !reflect.DeepEqual(x, y) {
		t.Fatal("named identity depends on allocation order")
	}
	got, err := x.matches(contentsValues{{name: name, kind: 0x102}: core.Binary{1, 2, 3}})
	if err != nil || !got {
		t.Fatal("named filter or detached binary")
	}
	if _, err := resolveContentsProperty(0x80030102, &first, func(contentsProperty) bool { return true }); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal("unregistered name accepted")
	}
	for _, v := range []any{x, x.property, contentsValues{{name: name, kind: 0x102}: core.Binary{1, 2, 3}}} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			s := fmt.Sprintf(format, v)
			if strings.Contains(s, "PRIVATE") || strings.Contains(s, "0x55") || !strings.Contains(s, "private") {
				t.Fatal("filter formatting leaked data")
			}
		}
	}
}

func FuzzContentsRestrictionFrame(f *testing.F) {
	f.Add([]byte{8, 0x1f, 0, 0x37, 0})
	f.Add([]byte{6, 1, 3, 0, 7, 0x0e, 0, 0, 0, 0x80})
	f.Add([]byte{0, 0, 0})
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, wire []byte) {
		if len(wire) > 9000 {
			t.Skip()
		}
		n, err := parseContentsRestriction(wire)
		if err != nil {
			if n != nil {
				t.Fatal("partial tree on error")
			}
			return
		}
		bound, err := bindContentsRestriction(n, filterResolver(nil))
		if err != nil {
			return
		}
		if _, err := bound.matches(contentsValues{}); err != nil {
			t.Fatal("validated empty-value evaluation")
		}
	})
}
