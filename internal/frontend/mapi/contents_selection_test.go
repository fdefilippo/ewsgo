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
	"strings"
	"testing"

	"ewsgo/internal/core"
)

func TestContentsSelectionIncludeExcludeFAIAndSubobjects(t *testing.T) {
	subject := contentsProperty{id: 0x37, kind: 0x1f}
	class := contentsProperty{id: 0x1a, kind: 0x1f}
	recipients := contentsProperty{id: 0xe12, kind: 0xd}
	for _, tc := range []struct {
		flags                      uint16
		associated                 bool
		subject, class, recipients bool
	}{
		{0, false, false, true, false},
		{0, true, false, true, false},
		{0x80, false, true, false, true},
		{0x80, true, true, false, true},
		{0x4080, true, true, true, true},
		{0x4080, false, true, false, true},
		{0x4000, true, false, true, false}, // IgnoreSpecifiedOnFAI ignored without OnlySpecified.
	} {
		selection, err := newContentsSelection(synchronizationConfigure{flags: tc.flags, tags: []uint32{0x0037001f, 0x0e12000d}}, nil)
		if err != nil || selection.includes(subject, tc.associated) != tc.subject || selection.includes(class, tc.associated) != tc.class || selection.includes(recipients, tc.associated) != tc.recipients {
			t.Fatal("selection flags", tc.flags, tc.associated)
		}
	}
	for _, flags := range []uint16{0, 0x80} {
		s, err := newContentsSelection(synchronizationConfigure{flags: flags}, nil)
		if err != nil || s.includes(subject, false) != (flags == 0) {
			t.Fatal("empty include/exclude list")
		}
	}
	s, err := newContentsSelection(synchronizationConfigure{flags: 0x80, tags: []uint32{0x00370000, 0x00370000}}, nil)
	if err != nil || len(s.properties) != 1 || !s.includes(subject, false) {
		t.Fatal("unspecified selector and duplicate")
	}
	for _, tags := range [][]uint32{{0x003700fd}, {0x0037101f}, {0x0e120003, 0x1234000d}, {0x0000001f}, {0x8000001f}, {0xffff001f}, make([]uint32, 257)} {
		if s, err := newContentsSelection(synchronizationConfigure{tags: tags}, nil); err == nil || !reflect.DeepEqual(s, contentsSelection{}) {
			t.Fatal("invalid selector accepted")
		}
	}
}

func TestContentsConfigureRetainsPrivateASTAndCompilesSeparateFlags(t *testing.T) {
	restriction := filterWire(t, "08 1f003700")
	frame := synchronizationConfigure{output: 1, kind: 1, sendOptions: 0x1d, flags: 0x39, extraFlags: 0xd, tags: []uint32{0x0037001f}}
	wire := configureOptionsWire(frame, restriction)
	r := reader{data: append(bytes.Clone(wire[3:]), 1, 4, 1)}
	c, err := parseContentsConfigure(&r, 2)
	if err != nil || c.restriction == nil || c.frame.restrictionBytes != 5 || !bytes.Equal(r.data[r.pos:], []byte{1, 4, 1}) {
		t.Fatal("Contents framing or restriction lost")
	}
	wire[len(wire)-1] ^= 0xff
	if c.frame.tags[0] != 0x0037001f {
		t.Fatal("tag aliases request")
	}
	for _, flags := range []uint16{0, 8, 0x10, 0x20, 0x18, 0x38} {
		c.frame.flags = flags
		plan, err := compileContentsFilter(c, nil, func(p contentsProperty) bool { return p.id == 0x37 })
		if err != nil || plan.normal != (flags&0x20 != 0) || plan.associated != (flags&0x10 != 0) || plan.readState != (flags&8 != 0) {
			t.Fatal("Normal/FAI/ReadState conflated")
		}
		pass, err := plan.restriction.matches(contentsValues{{id: 0x37, kind: 0x1f}: core.Text("subject")})
		if err != nil || !pass {
			t.Fatal("filter not retained")
		}
	}
	for n := 0; n < len(wire)-3; n++ {
		r := reader{data: wire[3 : 3+n]}
		if c, err := parseContentsConfigure(&r, 2); !errors.Is(err, ErrMalformed) || !reflect.DeepEqual(c, contentsConfigure{}) {
			t.Fatal("partial Configure on truncation", n)
		}
	}
	c.frame.kind = 2
	if plan, err := compileContentsFilter(c, nil, func(contentsProperty) bool { return true }); !errors.Is(err, core.ErrUnsupported) || !reflect.DeepEqual(plan, contentsFilterPlan{}) {
		t.Fatal("Hierarchy accepted as Contents")
	}
	// Existing runtime still rejects a valid Contents Configure before Release.
	f := newOSTFixture(t)
	valid := configureOptionsWire(frame, restriction)
	body, counts := f.execute(append([]byte{1, 4, 1}, valid...), 4096)
	if binary.LittleEndian.Uint32(body[4:]) != NotSupported || counts.unsupported != "RopSynchronizationConfigure" {
		t.Fatal("prerequisite enabled runtime")
	}
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
		t.Fatal("rejected Contents executed Release prefix")
	}
}

func TestContentsSelectionNamedResolutionAndPrivacy(t *testing.T) {
	name := propertyName{Set: [16]byte{0x55}, Kind: 1, Name: "PRIVATE-name"}
	var registry namedPropertyRegistry
	ids, _ := registry.ids(context.Background(), []propertyName{name}, true)
	c := contentsConfigure{frame: synchronizationConfigure{kind: 1, flags: 0xb0, tags: []uint32{uint32(ids[0])<<16 | 0x102}}}
	plan, err := compileContentsFilter(c, &registry, nil)
	if err != nil || !plan.selection.includes(contentsProperty{name: name, kind: 0x102}, false) || !plan.normal || !plan.associated {
		t.Fatal("named selection identity")
	}
	for _, v := range []any{plan, plan.selection, c, c.frame} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			out := fmt.Sprintf(format, v)
			if strings.Contains(out, "PRIVATE") || strings.Contains(out, "8001") || !strings.Contains(out, "private") {
				t.Fatal("private Configure/selection formatting")
			}
		}
	}
	c.frame.tags = []uint32{0x80020102}
	if plan, err := compileContentsFilter(c, &registry, nil); !errors.Is(err, core.ErrUnsupported) || !reflect.DeepEqual(plan, contentsFilterPlan{}) {
		t.Fatal("unknown name accepted")
	}
}
