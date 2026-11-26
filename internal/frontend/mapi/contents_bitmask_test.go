// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"ewsgo/internal/core"
)

func TestContentsBitMaskIndependentWireAndInt32Semantics(t *testing.T) {
	// Literal ROP bytes, independent of an encoder. The top bit of an Int32
	// remains a bit in the unsigned mask, rather than a signed comparison.
	for _, tc := range []struct {
		wire          string
		value         int64
		present, want bool
	}{
		{"06 00 0300070e 08000000", 8, true, false},
		{"06 01 0300070e 08000000", 8, true, true},
		{"06 00 0300070e 08000000", 4, true, true},
		{"06 01 0300070e 08000000", 4, true, false},
		{"06 00 0300070e 00000000", -1, true, true},
		{"06 01 0300070e 00000000", -1, true, false},
		{"06 01 0300070e 00000080", -2147483648, true, true},
		{"06 00 0300070e 00000080", 2147483647, true, true},
		{"06 01 0300070e ffffffff", -1, true, true},
		{"06 00 0300070e 00000000", 0, false, false},
		{"06 01 0300070e ffffffff", 0, false, false},
	} {
		n := boundFilter(t, filterWire(t, tc.wire), nil)
		values := contentsValues{}
		if tc.present {
			values[contentsProperty{id: 0xe07, kind: 3}] = core.Integer(tc.value)
		}
		got, err := n.matches(values)
		if err != nil || got != tc.want {
			t.Fatal("BitMask semantic mismatch", err)
		}
	}
	for _, wire := range []string{
		"00 0200 08 0300070e 06 01 0300070e 08000000",
		"01 0200 06 01 0300070e 04000000 06 01 0300070e 08000000",
		"02 06 00 0300070e 08000000",
	} {
		n := boundFilter(t, filterWire(t, wire), nil)
		if ok, err := n.matches(contentsValues{{id: 0xe07, kind: 3}: core.Integer(8)}); err != nil || !ok {
			t.Fatal("composed BitMask")
		}
	}
	n := boundFilter(t, filterWire(t, "06 01 0300070e 08000000"), nil)
	for _, bad := range []core.Value{core.Text("8"), core.Integer(2147483648), core.Integer(-2147483649)} {
		if _, err := n.matches(contentsValues{{id: 0xe07, kind: 3}: bad}); !errors.Is(err, core.ErrInvalid) {
			t.Fatal("invalid Int32 projection accepted")
		}
	}
}

func TestContentsBitMaskFrameValidationSchemaAndPrivacy(t *testing.T) {
	wire := filterWire(t, "06 01 0300070e 08000000")
	for i := 1; i < len(wire); i++ {
		if n, err := parseContentsRestriction(wire[:i]); !errors.Is(err, ErrMalformed) || n != nil {
			t.Fatal("partial BitMask accepted")
		}
	}
	if n, err := parseContentsRestriction(append(bytes.Clone(wire), 0)); !errors.Is(err, ErrMalformed) || n != nil {
		t.Fatal("BitMask trailing data accepted")
	}
	for _, raw := range []string{"06 02 0300070e 08000000", "06 00 1400070e 08000000", "06 01 0310070e 08000000"} {
		n, err := parseContentsRestriction(filterWire(t, raw))
		var reason contentsRestrictionRejection
		if n != nil || !errors.As(err, &reason) || reason != restrictionBitMask {
			t.Fatal("invalid BitMask profile not rejected")
		}
	}
	// Bind every arm, even when another OR arm could match.
	n, err := parseContentsRestriction(filterWire(t, "01 0200 06 01 0300070e 08000000 06 01 03003412 08000000"))
	if err != nil {
		t.Fatal(err)
	}
	if bound, err := bindContentsRestriction(n, filterResolver(nil)); !errors.Is(err, core.ErrUnsupported) || bound != nil {
		t.Fatal("unknown BitMask property skipped")
	}
	// Existing namespace and schema restrictions apply to BitMask too.
	n, _ = parseContentsRestriction(filterWire(t, "06 01 03000180 08000000"))
	if _, err := bindContentsRestriction(n, filterResolver(nil)); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal("unregistered named property accepted")
	}
	for _, format := range []string{"%v", "%+v", "%#v"} {
		if got := fmt.Sprintf(format, n); got != "contentsRestriction{private}" {
			t.Fatal("BitMask formatting exposed private fields")
		}
	}
	// Complete Configure frame must remain valid before execution.
	frame := contentsConfigureOperation(fullContentsConfig(), wire)
	for i := 1; i < len(frame); i++ {
		_, err := (ROPBuffer{Operations: frame[:i], Handles: []uint32{1, 2, 3}}).decodeContentsOperations(true, true, false, false, false, false, true)
		if err == nil {
			t.Fatal("truncated BitMask Configure accepted")
		}
	}
}

func TestContentsBitMaskScopeIncludesMaskOperatorAndCanonicalIdentity(t *testing.T) {
	c := fullContentsConfig()
	fingerprint := func(wire string) [32]byte {
		n := boundFilter(t, filterWire(t, wire), nil)
		p := contentsFilterPlan{normal: true, restriction: n}
		scope, err := contentsScopeFingerprint(1, c, p)
		if err != nil {
			t.Fatal(err)
		}
		return scope
	}
	base := fingerprint("06 01 0300070e 08000000")
	for _, wire := range []string{"06 01 0300070e 04000000", "06 00 0300070e 08000000", "06 01 03000830 08000000"} {
		if fingerprint(wire) == base {
			t.Fatal("BitMask scope collision")
		}
	}
	name := propertyName{Set: [16]byte{7}, Kind: 0, LID: 9}
	var a, b namedPropertyRegistry
	x, _ := a.ids(context.Background(), []propertyName{name}, true)
	b.ids(context.Background(), []propertyName{{Set: [16]byte{8}, Kind: 0, LID: 1}}, true)
	y, _ := b.ids(context.Background(), []propertyName{name}, true)
	bind := func(id uint16, r *namedPropertyRegistry) *contentsRestriction {
		raw := binary.LittleEndian.AppendUint32([]byte{6, 1}, uint32(id)<<16|3)
		raw = binary.LittleEndian.AppendUint32(raw, 8)
		return boundFilter(t, raw, r)
	}
	left, right := bind(x[0], &a), bind(y[0], &b)
	if !reflect.DeepEqual(left, right) {
		t.Fatal("BitMask named allocation leaked into canonical tree")
	}
	la, _ := contentsScopeFingerprint(1, c, contentsFilterPlan{restriction: left})
	lb, _ := contentsScopeFingerprint(1, c, contentsFilterPlan{restriction: right})
	if la != lb {
		t.Fatal("canonical BitMask checkpoint differs")
	}
}

func TestContentsBitMaskCalendarFAIUsesOnlyLocalProjection(t *testing.T) {
	f, _ := newCalendarFixture(t) // This backend has no mail/event content port.
	f.transport.EnableHierarchyDownloadSpike()
	f.transport.EnableContentsDownloadSpike()
	f.handles = append(f.handles, 0xffffffff, 0xffffffff)
	entry := calendarEntry(t, f)
	resolved, _ := f.run(conversionOperation(0x44, entry[22:]))
	fid := binary.LittleEndian.Uint64(resolved[6:])
	f.run(folderOperation(fid, 4, 0, 1))
	o, _ := f.state.mailbox.object(f.handles[1], 4)
	seedFAI(t, o.binding.store, faiTestRecord(o.binding.store, fid, 1))
	c := fullContentsConfig()
	c.flags &^= 0x20
	for _, tc := range []struct {
		wire string
		want int
	}{
		{"06 01 03000770 08000000", 1},
		{"06 00 03000770 08000000", 0},
	} {
		faiRunOK(t, f, contentsConfigureOperation(c, filterWire(t, tc.wire)))
		raw := drainFX(t, f.run, 2, 79)
		count := 0
		for _, token := range decodeTestContents(t, raw) {
			if token.tag == 0x001a001f {
				count++
			}
		}
		if count != tc.want {
			t.Fatal("Calendar FAI BitMask not applied")
		}
		contentsFourTestSets(t, raw)
		faiRunOK(t, f, []byte{1, 4, 2})
	}
	c.flags |= 0x20
	response, counts := f.run(contentsConfigureOperation(c, filterWire(t, "06 01 03000770 08000000")))
	if counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) != NotSupported {
		t.Fatal("Calendar native Contents enabled")
	}
	f.state.mailbox.close()
}

func TestContentsBitMaskCheckpointCannotMoveBetweenMasks(t *testing.T) {
	f, _ := messageFixture(t)
	row := messageObserve(t, f, Inbox).messages[0]
	c := testContentsStateConfig()
	c.restriction = &contentsRestriction{op: 6, rel: 1, tag: 0x0e070003, mask: 1}
	s := contentsStateForTest(t, f, c)
	defer s.close()
	state := syntheticMessageState(row, f.store.replica)
	ticket, err := f.store.stageContentsCheckpoint(s.scope, state)
	if err != nil {
		t.Fatal(err)
	}
	ticket.finish(t.Context(), true)
	c.restriction = &contentsRestriction{op: 6, rel: 1, tag: 0x0e070003, mask: 2}
	other := contentsStateForTest(t, f, c)
	defer other.close()
	uploadContentsTestState(t, other, f.session, state)
	if _, err := other.beginPlanning(context.Background(), f.session); !errors.Is(err, core.ErrResync) {
		t.Fatal("checkpoint moved between masks")
	}
}

func FuzzContentsBitMaskInt32(f *testing.F) {
	f.Add(int32(-2147483648), uint32(0x80000000), byte(1))
	f.Add(int32(8), uint32(8), byte(0))
	f.Fuzz(func(t *testing.T, value int32, mask uint32, rel byte) {
		raw := binary.LittleEndian.AppendUint32([]byte{6, rel}, 0x0e070003)
		raw = binary.LittleEndian.AppendUint32(raw, mask)
		n, err := parseContentsRestriction(raw)
		if rel > 1 {
			if !errors.Is(err, ErrUnsupported) || n != nil {
				t.Fatal("operator accepted")
			}
			return
		}
		if err != nil {
			t.Fatal("valid frame rejected")
		}
		n, err = bindContentsRestriction(n, filterResolver(nil))
		if err != nil {
			t.Fatal(err)
		}
		got, err := n.matches(contentsValues{{id: 0xe07, kind: 3}: core.Integer(value)})
		want := (uint32(value)&mask == 0) == (rel == 0)
		if err != nil || got != want {
			t.Fatal("BitMask differs from Int32 oracle")
		}
	})
}
