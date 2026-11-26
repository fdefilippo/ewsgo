// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
)

func setListOperation(logon, input byte) []byte {
	values := append32(nil, 0x3613001e)
	values = append(values, []byte("PRIVATE-string-opcode-\x0a\x1c")...)
	values = append(values, 0)
	values = append(append32(values, 0x1234000b), 1)
	values = append(append32(values, 0x12350102), 4, 0, 0x0a, 0x1c, 0xff, 0x70)
	header := binary.LittleEndian.AppendUint16([]byte{0x0a, logon, input}, uint16(2+len(values)))
	return append(append(header, 3, 0), values...)
}

func TestSetListScalarTraversalPrivacyAndBoundaries(t *testing.T) {
	operation := setListOperation(4, 1)
	wire := setPropertyWire(t, operation)
	d := InspectSetList(wire)
	a, tags, n, hi, res := d.Fields()
	if a != "list_only" || !reflect.DeepEqual(tags, []string{"0x3613001e", "0x1234000b", "0x12350102"}) || n != "3" || hi != "0" || res != "0" {
		t.Fatal("scalar boundaries")
	}
	for i := range wire {
		wire[i] = 0
	}
	if strings.Contains(fmt.Sprintf("%v %#v", d, d), "PRIVATE") {
		t.Fatal("retained/formatted value")
	}
	a2, tags2, n2, _, _ := d.Fields()
	if a2 != a || !reflect.DeepEqual(tags, tags2) || n2 != n {
		t.Fatal("aliased input")
	}
	for i := 1; i < len(operation); i++ {
		a, ts, n, h, r := InspectSetList(setPropertyWire(t, operation[:i])).Fields()
		if a != "malformed" || !reflect.DeepEqual(ts, []string{"none"}) || n != "none" || h != "none" || r != "none" {
			t.Fatal("malformed exposed partial tags", i)
		}
	}
	if a, _, _, _, _ := InspectSetList(setPropertyWire(t, append([]byte{1, 4, 1}, operation...))).Fields(); a != "none" {
		t.Fatal("scanned beyond first ROP")
	}
	// Every ID boundary is filtered before storage, not just when formatted.
	for _, id := range []uint16{0, 1, 0x7fff, 0x8000, 0xfffe, 0xffff} {
		d := InspectSetList(setPropertyWire(t, setPropertyBinaryFixture(4, 1, uint32(id)<<16|0x0102)))
		a, ts, n, h, r := d.Fields()
		if a != "list_only" || n != "1" {
			t.Fatal("ID fixture")
		}
		if id == 0 || id == 0xffff {
			if len(ts) != 0 || r != "1" || d.tags[0] != 0 {
				t.Fatal("reserved retained")
			}
		} else if id >= 0x8000 {
			if len(ts) != 0 || h != "1" || d.tags[0] != 0 {
				t.Fatal("high retained")
			}
		} else if len(ts) != 1 {
			t.Fatal("low tag filtered")
		}
	}
	// Unknown types stop at their tag, even if their payload resembles more tags.
	bad := append32([]byte{0x0a, 4, 1, 15, 0, 2, 0}, 0x1234000d)
	bad = append(bad, 1, 2, 3, 4, 5, 6, 7, 8, 9)
	a, ts, n, _, _ := InspectSetList(setPropertyWire(t, bad)).Fields()
	if a != "stopped_type" || n != "1" || !reflect.DeepEqual(ts, []string{"0x1234000d"}) {
		t.Fatal("unknown type scanned")
	}
	for _, kind := range []uint16{2, 3, 4, 5, 6, 7, 10, 11, 20, 64, 72, 0x1e, 0x1f, 0x102} {
		sizes := map[uint16]int{2: 2, 3: 4, 4: 4, 5: 8, 6: 8, 7: 8, 10: 4, 11: 1, 20: 8, 64: 8, 72: 16, 0x1e: 1, 0x1f: 2, 0x102: 2}
		payload := make([]byte, sizes[kind])
		op := append32([]byte{0x0a, 4, 1, 0, 0, 1, 0}, 0x12340000|uint32(kind))
		op = append(op, payload...)
		binary.LittleEndian.PutUint16(op[3:], uint16(len(op)-5))
		if a, _, _, _, _ := InspectSetList(setPropertyWire(t, op)).Fields(); a != "list_only" {
			t.Fatal("scalar cursor")
		}
	}
	for _, count := range []int{17, 257} {
		op := make([]byte, 7+count*4)
		copy(op, []byte{0x0a, 4, 1})
		binary.LittleEndian.PutUint16(op[3:], uint16(len(op)-5))
		binary.LittleEndian.PutUint16(op[5:], uint16(count))
		if a, _, _, _, _ := InspectSetList(setPropertyWire(t, op)).Fields(); a != "limit" {
			t.Fatal("count limit")
		}
	}
	op := make([]byte, 5+maxSetListDiagnosticBytes+1)
	copy(op, []byte{0x0a, 4, 1})
	binary.LittleEndian.PutUint16(op[3:], uint16(len(op)-5))
	binary.LittleEndian.PutUint16(op[5:], 1)
	if a, _, _, _, _ := InspectSetList(setPropertyWire(t, op)).Fields(); a != "limit" {
		t.Fatal("byte limit")
	}
}

type setListRecorder struct {
	*httptest.ResponseRecorder
	calls      int
	diagnostic SetListDiagnostic
}

func (w *setListRecorder) RecordSetListDiagnostic(d SetListDiagnostic) { w.calls++; w.diagnostic = d }

func TestSetListTransportGateAndNoEffects(t *testing.T) {
	transport, session, box, cookies := storeFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	body, cookies := storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 4, 0), []uint32{0xffffffff, 0xffffffff}, 4096)
	logon := storeResponse(t, body)
	fid := binary.LittleEndian.Uint64(logon.Operations[7+int(Inbox)*8:])
	body, cookies = storeExecute(t, transport, session, cookies, folderOperation(fid, 4, 0, 1), logon.Handles, 4096)
	opened := storeResponse(t, body)
	call := func(valid bool, operations ...[]byte) ([]byte, *setListRecorder) {
		ops := append(setListOperation(4, 1), 1, 4, 1)
		if len(operations) != 0 {
			ops = operations[0]
		}
		wire, _ := (ROPBuffer{Operations: ops, Handles: opened.Handles}).Marshal()
		request, _ := (Execute{ROP: wire, MaxOutput: 4096}).Marshal()
		r := httptest.NewRequest("POST", "https://gateway.invalid/mapi/emsmdb/?MailboxId="+identity.RoutingID, bytes.NewReader(request))
		r.Header.Set("Content-Type", "application/mapi-http")
		r.Header.Set("X-RequestType", "Execute")
		r.Header.Set("X-RequestId", "synthetic-request:1")
		if valid {
			for _, c := range cookies {
				r.AddCookie(c)
			}
		}
		w := &setListRecorder{ResponseRecorder: httptest.NewRecorder()}
		transport.Mailbox(w, r, session)
		if !valid {
			return nil, w
		}
		cookies = w.Result().Cookies()
		return requireTransportCode(t, w.ResponseRecorder, 0), w
	}
	before := box.calls
	off, w := call(true)
	if w.calls != 0 {
		t.Fatal("default enabled")
	}
	transport.EnableSetListTrace()
	_, w = call(false)
	if w.calls != 0 {
		t.Fatal("invalid session inspected")
	}
	on, w := call(true)
	a, _, _, _, _ := w.diagnostic.Fields()
	if w.calls != 1 || a != "list_only" || !bytes.Equal(on, off) || binary.LittleEndian.Uint32(on[4:]) != NotSupported || box.calls != before {
		t.Fatal("diagnostic changed execution/queries")
	}
	_, w = call(true, append([]byte{1, 4, 1}, setListOperation(4, 1)...))
	a, tags, n, _, _ := w.diagnostic.Fields()
	if w.calls != 1 || a != "list_only" || n != "3" || !reflect.DeepEqual(tags, []string{"0x3613001e", "0x1234000b", "0x12350102"}) || box.calls != before {
		t.Fatal("prefix diagnostic callback")
	}
	for _, state := range transport.contexts.entries {
		if _, code := state.mailbox.object(opened.Handles[1], 4); code != 0 {
			t.Fatal("suffix Release executed")
		}
	}
}

func FuzzSetListDiagnostic(f *testing.F) {
	f.Add(setPropertyWire(f, setListOperation(4, 1)))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		d := InspectSetList(raw)
		a, tags, n, h, r := d.Fields()
		switch a {
		case "none", "malformed", "limit":
			if len(tags) != 1 || tags[0] != "none" || n != "none" || h != "none" || r != "none" {
				t.Fatal("partial output")
			}
		case "list_only", "stopped_type":
			visited, err := strconv.Atoi(n)
			if err != nil || visited > 16 || len(tags) > visited {
				t.Fatal("bounds")
			}
			for _, tag := range d.tags[:d.low] {
				if id := uint16(tag >> 16); id == 0 || id >= 0x8000 {
					t.Fatal("private tag retained")
				}
			}
		default:
			t.Fatal("vocabulary")
		}
	})
}
