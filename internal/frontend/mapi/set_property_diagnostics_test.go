// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
)

func setPropertyBinaryFixture(logon, input byte, tag uint32) []byte {
	// MS-OXCROPS fixture: size8 includes count2, tag4, empty Binary length2.
	out := binary.LittleEndian.AppendUint32([]byte{0x0a, logon, input, 8, 0, 1, 0}, tag)
	return append(out, 0, 0)
}

func setPropertyWire(t testing.TB, ops []byte) []byte {
	t.Helper()
	wire, err := (ROPBuffer{Operations: ops, Handles: []uint32{1, 2}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	return wire
}

func TestSetPropertyHeaderFixturesAndBoundaries(t *testing.T) {
	valid := []byte{0x0a, 0x91, 1, 8, 0, 1, 0, 2, 1, 4, 0x7c, 0, 0}
	for _, tc := range []struct {
		ops  []byte
		want [3]string
	}{
		{valid, [3]string{"header_only", "1", "0x7c040102"}},
		{[]byte{0x0a, 0, 1, 6, 0, 1, 0, 0xff, 0xff, 1, 0x30}, [3]string{"header_only", "1", "0x3001ffff"}}, // Value type is not validated.
		{[]byte{0x0a, 0, 1, 2, 0, 0, 0}, [3]string{"header_only", "0", "none"}},
		// Two Binary values; only the first tag is observed, even with hostile suffix.
		{[]byte{0x0a, 0, 1, 14, 0, 2, 0, 2, 1, 1, 0x30, 0, 0, 2, 1, 0xff, 0xff, 0, 0, 0xff}, [3]string{"header_only", "2", "0x30010102"}},
		{append([]byte{1, 0, 1}, valid...), [3]string{"none", "none", "none"}},
		{[]byte{0x0a, 0, 2, 2, 0, 0, 0}, [3]string{"malformed", "none", "none"}},
		{[]byte{0x0a, 0, 1, 1, 0, 0, 0}, [3]string{"malformed", "none", "none"}},
		{[]byte{0x0a, 0, 1, 3, 0, 0, 0, 0}, [3]string{"malformed", "none", "none"}},
		{[]byte{0x0a, 0, 1, 6, 0, 2, 0, 2, 1, 4, 0x7c}, [3]string{"malformed", "none", "none"}},
	} {
		d := InspectSetProperty(setPropertyWire(t, tc.ops))
		a, b, c := d.Fields()
		if [3]string{a, b, c} != tc.want {
			t.Fatalf("header fixture: %v", [3]string{a, b, c})
		}
	}
	for n := 1; n < len(valid); n++ {
		d := InspectSetProperty(setPropertyWire(t, valid[:n]))
		a, b, c := d.Fields()
		if a != "malformed" || b != "none" || c != "none" {
			t.Fatal("truncated header exposed fields", n)
		}
	}
	for _, n := range []uint16{256, 257, 65535} {
		size := 2 + 4*256
		if n == 257 {
			size += 4
		}
		ops := make([]byte, 5+size)
		copy(ops, []byte{0x0a, 0, 1})
		binary.LittleEndian.PutUint16(ops[3:], uint16(size))
		binary.LittleEndian.PutUint16(ops[5:], n)
		binary.LittleEndian.PutUint32(ops[7:], 0x7c040102)
		a, b, c := InspectSetProperty(setPropertyWire(t, ops)).Fields()
		if n == 256 {
			if a != "header_only" || b != "256" || c != "0x7c040102" {
				t.Fatal("256 bound")
			}
		} else if a != "limit" || b != "limit" || c != "none" {
			t.Fatal("limit disclosed count/tag")
		}
	}
	// All ID boundaries, including recognized public IDs; no high numeric IDs.
	for id := 0; id <= 0xffff; id++ {
		a, b, c := InspectSetProperty(setPropertyWire(t, setPropertyBinaryFixture(0, 1, uint32(id)<<16|0x0102))).Fields()
		want := fmt.Sprintf("0x%04x0102", id)
		if id == 0 || id == 0xffff {
			want = "reserved_id"
		} else if id >= 0x8000 {
			want = "high_id"
		}
		if a != "header_only" || b != "1" || c != want {
			t.Fatal("ID filter", id)
		}
	}
}

func TestSetPropertyHeaderDoesNotInspectOrRetainValues(t *testing.T) {
	ops := append(setPropertyBinaryFixture(0, 1, 0x7c040102)[:11], []byte("private-sentinel-value")...)
	binary.LittleEndian.PutUint16(ops[3:], uint16(len(ops)-5))
	wire := setPropertyWire(t, ops)
	d := InspectSetProperty(wire)
	for i := range wire {
		wire[i] = 0
	}
	a, b, c := d.Fields()
	if a != "header_only" || b != "1" || c != "0x7c040102" || strings.Contains(fmt.Sprint(d), "private-") {
		t.Fatal("borrowed value or tag escaped snapshot")
	}
	// ParseROP still owns its operation/handle copies after the framing extraction.
	wire = setPropertyWire(t, setPropertyBinaryFixture(0, 1, 0x7c040102))
	parsed, err := ParseROP(wire)
	if err != nil {
		t.Fatal(err)
	}
	for i := range wire {
		wire[i] = 0
	}
	if !bytes.Equal(parsed.Operations, setPropertyBinaryFixture(0, 1, 0x7c040102)) || parsed.Handles[1] != 2 {
		t.Fatal("ParseROP snapshot aliases input")
	}
}

func TestSetPropertyHeaderFramingNoFallback(t *testing.T) {
	wire := setPropertyWire(t, setPropertyBinaryFixture(0, 1, 0x7c040102))
	for _, flags := range []uint16{0, 2, 3, 5, 6, 7} {
		altered := bytes.Clone(wire)
		binary.LittleEndian.PutUint16(altered[2:], flags)
		if flags&1 != 0 {
			binary.LittleEndian.PutUint16(altered[6:], binary.LittleEndian.Uint16(altered[4:])+1)
		}
		a, b, c := InspectSetProperty(altered).Fields()
		if a != "none" || b != "none" || c != "none" {
			t.Fatal("non-plain request inspected")
		}
	}
	for _, altered := range [][]byte{nil, wire[:9], append(bytes.Clone(wire), 0), append(bytes.Clone(wire), wire...)} {
		a, b, c := InspectSetProperty(altered).Fields()
		if a != "malformed" || b != "none" || c != "none" {
			t.Fatal("malformed framing")
		}
	}
}

type setPropertyRecorder struct {
	*httptest.ResponseRecorder
	calls      int
	diagnostic SetPropertyDiagnostic
}

func (w *setPropertyRecorder) RecordSetPropertyDiagnostic(d SetPropertyDiagnostic) {
	w.calls++
	w.diagnostic = d
}

func TestSetPropertyTraceTransportGateWireAndNoPrefixEffects(t *testing.T) {
	transport, session, _, cookies := storeFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	body, cookies := storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 0, 0), []uint32{0xffffffff, 0xffffffff}, 4096)
	logon := storeResponse(t, body)
	fid := binary.LittleEndian.Uint64(logon.Operations[7+int(Inbox)*8:])
	body, cookies = storeExecute(t, transport, session, cookies, folderOperation(fid, 0, 0, 1), logon.Handles, 4096)
	opened := storeResponse(t, body)
	call := func(ops []byte, withCookies bool) ([]byte, *setPropertyRecorder) {
		t.Helper()
		wire, _ := (ROPBuffer{Operations: ops, Handles: opened.Handles}).Marshal()
		request, _ := (Execute{ROP: wire, MaxOutput: 4096}).Marshal()
		r := httptest.NewRequest("POST", "https://gateway.invalid/mapi/emsmdb/?MailboxId="+identity.RoutingID, bytes.NewReader(request))
		r.Header.Set("Content-Type", "application/mapi-http")
		r.Header.Set("X-RequestId", "synthetic-request:1")
		r.Header.Set("X-RequestType", "Execute")
		if withCookies {
			for _, c := range cookies {
				r.AddCookie(c)
			}
		}
		w := &setPropertyRecorder{ResponseRecorder: httptest.NewRecorder()}
		transport.Mailbox(w, r, session)
		if !withCookies {
			return nil, w
		}
		cookies = w.Result().Cookies()
		return requireTransportCode(t, w.ResponseRecorder, 0), w
	}
	set := setPropertyBinaryFixture(0, 1, 0x7c040102)
	off, w := call(set, true)
	if w.calls != 0 || transport.setPropertyTrace {
		t.Fatal("trace default enabled")
	}
	transport.EnableSetPropertyTrace()
	_, w = call(set, false)
	if w.calls != 0 {
		t.Fatal("unvalidated context exposed header")
	}
	on, w := call(set, true)
	a, b, c := w.diagnostic.Fields()
	if !bytes.Equal(off, on) || len(on) != 20 || binary.LittleEndian.Uint32(on[4:]) != NotSupported || w.calls != 1 || a != "header_only" || b != "1" || c != "0x7c040102" {
		t.Fatal("wire or gated callback")
	}
	_, w = call(append([]byte{1, 0, 1}, set...), true)
	a, b, c = w.diagnostic.Fields()
	if a != "none" || b != "none" || c != "none" {
		t.Fatal("scanned a later ROP")
	}
	for _, s := range transport.contexts.entries {
		if _, code := s.mailbox.object(opened.Handles[1], 0); code != 0 {
			t.Fatal("unsupported batch executed prefix Release")
		}
	}
}

func FuzzSetPropertyHeader(f *testing.F) {
	f.Add(setPropertyWire(f, setPropertyBinaryFixture(0, 1, 0x7c040102)))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		d := InspectSetProperty(data)
		a, b, c := d.Fields()
		switch a {
		case "none", "malformed":
			if b != "none" || c != "none" {
				t.Fatal("partial invalid header")
			}
		case "limit":
			if b != "limit" || c != "none" {
				t.Fatal("limit fields")
			}
		case "header_only":
			n, err := strconv.Atoi(b)
			if err != nil || n < 0 || n > 256 {
				t.Fatal("count bound")
			}
			if strings.HasPrefix(c, "0x") {
				id, err := strconv.ParseUint(c[2:6], 16, 16)
				if err != nil || id == 0 || id >= 0x8000 || len(c) != 10 {
					t.Fatal("tag filter")
				}
			} else if c != "none" && c != "high_id" && c != "reserved_id" {
				t.Fatal("tag vocabulary")
			}
		default:
			t.Fatal("inspection vocabulary")
		}
	})
}
