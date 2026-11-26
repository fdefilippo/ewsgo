// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"net/http/httptest"
	"reflect"
	"testing"
)

func testLogonROP(name string, logonFlags byte, openFlags uint32) []byte {
	out := []byte{0xfe, 0x9a, 0, logonFlags}
	out = append32(append32(out, openFlags), 0)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(name)+1))
	return append(append(out, name...), 0)
}

type ropSummaryWriter struct {
	*httptest.ResponseRecorder
	layout, first string
	flags         []string
	calls         int
}

func (w *ropSummaryWriter) RecordROPDiagnostic(layout, first string, flags []string) {
	w.layout, w.first, w.flags = layout, first, flags
	w.calls++
}

func TestExecuteDiagnosticDoesNotExecuteOrReturnROPSuccess(t *testing.T) {
	transport, session, connect := transportFixture(t)
	w := transportRequest(t, transport, session, "Connect", connect, nil)
	requireTransportCode(t, w, 0)
	current := w.Result().Cookies()
	identity, _ := transport.namespace.Mailbox(session.Identity())
	plain, _ := (ROPBuffer{Operations: testLogonROP(identity.LegacyDN, 1, 0x01000004), Handles: []uint32{0xffffffff}}).Marshal()
	compressed := bytes.Clone(plain)
	binary.LittleEndian.PutUint16(compressed[2:], 7)
	binary.LittleEndian.PutUint16(compressed[6:], uint16(len(compressed)-7))
	for _, tc := range []struct {
		wire          []byte
		layout, first string
	}{{plain, "plain", "logon"}, {compressed, "compressed_xor", "none"}, {[]byte{0xff}, "malformed", "none"}} {
		data, _ := (Execute{ROP: tc.wire, MaxOutput: 4096}).Marshal()
		r := httptest.NewRequest("POST", "https://gateway.invalid/mapi/emsmdb/?MailboxId="+identity.RoutingID, bytes.NewReader(data))
		r.Header.Set("Content-Type", "application/mapi-http")
		r.Header.Set("X-RequestId", "synthetic-request:1")
		r.Header.Set("X-RequestType", "Execute")
		for _, cookie := range current {
			r.AddCookie(cookie)
		}
		recorder := &ropSummaryWriter{ResponseRecorder: httptest.NewRecorder()}
		transport.Mailbox(recorder, r, session)
		body := requireTransportCode(t, recorder.ResponseRecorder, 0)
		if recorder.calls != 1 || recorder.layout != tc.layout || recorder.first != tc.first {
			t.Fatal("Execute diagnostic missing/incorrect")
		}
		if !bytes.Equal(body, []byte{0, 0, 0, 0, 2, 1, 4, 128, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}) {
			t.Fatal("diagnostic introduced fake ROP success")
		}
		current = recorder.Result().Cookies()
	}
}

func TestInspectROPLogonIsLayoutOnlyAndNeverExposesIdentifiers(t *testing.T) {
	op := testLogonROP("/o=Private/cn=PrivateMailbox", 1, 0x21000004)
	// The output slot is unallocated. Logon has no input handle, and its
	// arbitrary LogonId must not be mistaken for a handle index or logged.
	data, _ := (ROPBuffer{Operations: append(op, 0xff), Handles: []uint32{0xffffffff}}).Marshal()
	got := InspectROP(data)
	want := ROPInspection{Layout: "plain", First: "logon", Flags: []string{"private", "home_logon", "per_mdb_replid_mapping", "support_progress"}}
	if !reflect.DeepEqual(got, want) {
		t.Fatal("first Logon framing/flag identification", got)
	}
	// Invalid trailing ROP is deliberately not validated: this diagnostic
	// does not claim an executable/valid batch.
	if _, err := ParseROP(data); err != nil {
		t.Fatal(err)
	}
	for _, n := range []int{0, 1, 2, 3, 4, 11, 12, 13, len(op) - 1} {
		wire, _ := (ROPBuffer{Operations: op[:n], Handles: []uint32{0xffffffff}}).Marshal()
		if n == 0 {
			if InspectROP(wire).First != "empty" {
				t.Fatal("empty operations")
			}
			continue
		}
		if InspectROP(wire).First != "malformed" {
			t.Fatal("truncated first Logon identified", n)
		}
	}
	for _, alter := range []func([]byte){
		func(v []byte) { v[2] = 1 },
		func(v []byte) { v[len(v)-1] = 'x' },
		func(v []byte) { v[15] = 0 },
		func(v []byte) { v[15] = 0x80 },
		func(v []byte) { binary.LittleEndian.PutUint16(v[12:], 4097) },
	} {
		invalid := bytes.Clone(op)
		alter(invalid)
		wire, _ := (ROPBuffer{Operations: invalid, Handles: []uint32{0xffffffff}}).Marshal()
		if InspectROP(wire).First != "malformed" {
			t.Fatal("malformed Logon accepted")
		}
	}
	// EssdnSize==0 is explicitly allowed by MS-OXCSTOR 2.2.1.1.1.
	public := testLogonROP("", 0, 2)
	public = public[:14]
	binary.LittleEndian.PutUint16(public[12:], 0)
	wire, _ := (ROPBuffer{Operations: public, Handles: []uint32{0xffffffff}}).Marshal()
	if got := InspectROP(wire); got.First != "logon" || !reflect.DeepEqual(got.Flags, []string{"public"}) {
		t.Fatal("empty Essdn public request")
	}
}

func TestInspectROPFramingLimitsAreReportedWithoutFallback(t *testing.T) {
	op := testLogonROP("/o=Private", 0xf1, 0xffffffff)
	plain, _ := (ROPBuffer{Operations: op, Handles: []uint32{0xffffffff}}).Marshal()
	for _, flags := range []uint16{5, 6, 7} {
		wire := bytes.Clone(plain)
		binary.LittleEndian.PutUint16(wire[2:], flags)
		if flags&1 != 0 {
			binary.LittleEndian.PutUint16(wire[6:], uint16(len(wire)-7))
		}
		want := map[uint16]string{5: "compressed", 6: "xor", 7: "compressed_xor"}[flags]
		if got := InspectROP(wire); got.Layout != want || got.First != "none" || len(got.Flags) != 0 {
			t.Fatal("unsupported framing was decoded", got)
		}
		first := bytes.Clone(wire)
		binary.LittleEndian.PutUint16(first[2:], flags & ^uint16(4))
		packed := append(first, wire...)
		if got := InspectROP(packed); got.Layout != "packed_"+want || got.First != "none" {
			t.Fatal("packed unsupported framing", got)
		}
	}
	first := bytes.Clone(plain)
	binary.LittleEndian.PutUint16(first[2:], 0)
	if got := InspectROP(append(first, plain...)); got.Layout != "packed" || got.First != "none" {
		t.Fatal("packed request was decoded")
	}
	for _, wire := range [][]byte{nil, {}, plain[:8], plain[:len(plain)-1], append(bytes.Clone(plain), 0), make([]byte, MaxBody+1), first} {
		if InspectROP(wire).Layout != "malformed" {
			t.Fatal("bad extended framing accepted")
		}
	}
	for _, offset := range []int{0, 2, 4, 6} {
		wire := bytes.Clone(plain)
		binary.LittleEndian.PutUint16(wire[offset:], 0xffff)
		if InspectROP(wire).Layout != "malformed" {
			t.Fatal("invalid header accepted")
		}
	}
	if got := InspectROP(append(bytes.Repeat(first, 256), plain...)); got.Layout != "limit" {
		t.Fatal("packed buffer quota")
	}
}

func TestInspectROPKnownAndUnknownFirstOperations(t *testing.T) {
	for _, tc := range []struct {
		op      []byte
		handles []uint32
		first   string
	}{
		{[]byte{1, 0, 0}, []uint32{12}, "release"},
		{[]byte{1, 0, 1}, []uint32{12}, "malformed"},
		{[]byte{2, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0}, []uint32{12, 0xffffffff}, "open_folder"},
		{[]byte{2, 0, 0, 1, 0, 0, 0, 0, 0, 0, 0, 0}, []uint32{12, 0xffffffff}, "malformed"},
		{[]byte{0xfd}, nil, "other"},
	} {
		wire, _ := (ROPBuffer{Operations: tc.op, Handles: tc.handles}).Marshal()
		if got := InspectROP(wire); got.Layout != "plain" || got.First != tc.first {
			t.Fatal("incorrect first operation diagnostic", got)
		}
	}
}

func FuzzROPInspection(f *testing.F) {
	wire, _ := (ROPBuffer{Operations: testLogonROP("/o=Synthetic", 1, 4), Handles: []uint32{0xffffffff}}).Marshal()
	f.Add(wire)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		diagnostic := InspectROP(data)
		if len(diagnostic.Layout) > 32 || len(diagnostic.First) > 64 || len(diagnostic.Flags) > 15 {
			t.Fatal("unbounded diagnostic")
		}
	})
}

func TestPublicROPDiagnosticsIdentifyOpcodeWithoutClaimingLayoutOrExecution(t *testing.T) {
	for _, tc := range []struct {
		id    byte
		label string
	}{
		{0x04, "RopGetHierarchyTable"}, {0x07, "RopGetPropertiesSpecific"},
		{0x26, "RopSetReceiveFolder"}, {0x27, "RopGetReceiveFolder"},
		{0x43, "RopLongTermIdFromId"}, {0x49, "RopGetAddressTypes"},
		{0x7b, "RopGetStoreState"}, {0x76, "RopSynchronizationUploadStateStreamContinue"},
		{0x00, "other"}, {0x28, "other"}, {0xfd, "other"},
	} {
		// A single opcode is deliberately insufficient for semantic decoding.
		// Only its name is observed; no body, handle or private string leaves.
		for _, tail := range [][]byte{nil, []byte("private-mailbox-and-token")} {
			wire, _ := (ROPBuffer{Operations: append([]byte{tc.id}, tail...)}).Marshal()
			got := InspectROP(wire)
			if got.Layout != "plain" || got.First != tc.label || len(got.Flags) != 0 || SanitizeROPLabel(got.First) != tc.label {
				t.Fatal("unsafe or inaccurate opcode classification", tc.id)
			}
			buffer, _ := ParseROP(wire)
			if ops, err := buffer.DecodeOperations(); err == nil || ops != nil {
				t.Fatal("diagnostic added unsupported execution")
			}
		}
	}
	for _, value := range []string{"private-DN", "RopGetPropertiesSpecific/private-token", "RopGetPropertiesSpecific\nprivate", "ropGetPropertiesSpecific", ""} {
		if SanitizeROPLabel(value) != "other" {
			t.Fatal("non-vocabulary string accepted")
		}
	}
	// The finite public table remains bounded and unique, including names of
	// server-only ROPs: classification itself grants no request support.
	names := map[string]bool{}
	for _, name := range publicROPLabels {
		if name == "" {
			continue
		}
		if len(name) > 64 || names[name] {
			t.Fatal("unbounded or ambiguous vocabulary")
		}
		names[name] = true
	}
}
