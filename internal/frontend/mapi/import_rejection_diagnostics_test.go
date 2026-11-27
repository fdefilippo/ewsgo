// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func importDiagnosticWire(source, change, pcl []byte) []byte {
	wire := appendFAIRopBinary([]byte{3, 0x10, 4, 0}, 0x65e00102, source)
	wire = binary.LittleEndian.AppendUint32(wire, 0x30080040)
	wire = binary.LittleEndian.AppendUint64(wire, fileTime(hierarchyTestTime))
	wire = appendFAIRopBinary(wire, 0x65e20102, change)
	return appendFAIRopBinary(wire, 0x65e30102, pcl)
}

func requireImportDiagnostic(t *testing.T, d SynchronizationOptionsDiagnostic, reason string) {
	t.Helper()
	flags, send, extra, reasons, restriction, tags := d.Fields()
	if d.parsed || !reflect.DeepEqual(flags, []string{"none"}) || !reflect.DeepEqual(send, []string{"none"}) || !reflect.DeepEqual(extra, []string{"none"}) || !reflect.DeepEqual(reasons, []string{reason}) || restriction != "none" || tags != "none" {
		t.Fatal("import diagnostic implied Configure metadata or lost its closed reason")
	}
}

func TestImportRejectionBranchesAndNoBatchEffects(t *testing.T) {
	r := syntheticFAIImport()
	flags := func(flag byte) []byte { wire := faiImportWire(r); wire[1] = flag; return wire }
	count := func(n uint16) []byte {
		wire := faiImportWire(r)
		binary.LittleEndian.PutUint16(wire[2:], n)
		return wire
	}
	for _, tc := range []struct {
		name, reason string
		wire         []byte
	}{
		{"unknown_flags", "import_unknown_flags", flags(0x11)},
		{"unknown_normal_flags", "import_unknown_flags", flags(1)},
		{"zero_properties", "import_property_count_profile", count(0)},
		{"extra_properties", "import_property_count_profile", count(5)},
		{"many_properties", "import_property_count_profile", count(65535)},
		{"source_size", "import_xid_profile", importDiagnosticWire(r.source[:21], []byte(r.change), r.predecessors.marshal())},
		{"change_size", "import_xid_profile", importDiagnosticWire(r.source[:], []byte(r.change[:16]), r.predecessors.marshal())},
		{"pcl_xid_profile", "import_pcl_profile", importDiagnosticWire(r.source[:], []byte(r.change), []byte{16, 1})},
		{"pcl_budget", "import_pcl_profile", importDiagnosticWire(r.source[:], []byte(r.change), make([]byte, 25*maxFAIPCLReplicas+1))},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, box, store := newFAIExecuteFixture(t)
			handle := f.handles[1]
			allocated, retained, calls := f.state.mailbox.allocated, f.transport.stores.contents.used, box.sourceCalls
			// Release and a valid import precede the rejected operation. Neither
			// may run; the private tail is deliberately never interpreted.
			ops := append([]byte{1, 4, 1}, faiImportOperation(r)...)
			ops = append(ops, 0x72, 4, 2)
			ops = append(ops, tc.wire...)
			ops = append(ops, []byte("PRIVATE-unparsed-tail")...)
			body, counts := f.execute(ops, 65535)
			if !bytes.Equal(body, executeError(NotSupported, 65535)) || counts.unsupported != "RopSynchronizationImportMessageChange" {
				t.Fatal("rejection changed wire result or operation label")
			}
			requireImportDiagnostic(t, counts.synchronizationOptions, tc.reason)
			if f.state.mailbox.allocated != allocated || f.transport.stores.contents.used != retained || box.sourceCalls != calls || len(store.fai.records) != 0 {
				t.Fatal("rejected batch allocated, captured, retained or saved data")
			}
			if _, code := f.state.mailbox.object(handle, 4); code != 0 {
				t.Fatal("rejected batch released prefix")
			}
			f.state.mailbox.close()
			if f.transport.stores.contents.used != 0 || f.transport.stores.transfers.Load() != 0 {
				t.Fatal("fixture cleanup leaked")
			}
		})
	}
}

func TestImportRejectionMalformedDisabledAndSupportedRemainDistinct(t *testing.T) {
	r := syntheticFAIImport()
	valid := faiImportWire(r)
	check := func(wire []byte, enabled bool) ([]Operation, error) {
		return (ROPBuffer{Operations: append([]byte{0x72, 4, 2}, wire...), Handles: []uint32{1, 2, 3, 4}}).decodeFAIOperations(true, true, false, false, false, false, true, enabled)
	}
	for n := 0; n < len(valid); n++ {
		_, err := check(valid[:n], true)
		var u unsupportedOperation
		if !errors.Is(err, ErrMalformed) || errors.As(err, &u) {
			t.Fatal("truncated supported frame promoted to profile rejection", n)
		}
	}
	bad := bytes.Clone(valid)
	bad[4] = 0xe2
	if _, err := check(bad, true); !errors.Is(err, ErrMalformed) {
		t.Fatal("wrong fixed property promoted")
	}
	bad = bytes.Clone(valid)
	bad[0] = 4
	if _, err := check(bad, true); !errors.Is(err, ErrMalformed) {
		t.Fatal("invalid output handle promoted")
	}
	for _, flag := range []byte{0, 0x10, 0x40, 0x50} {
		wire := bytes.Clone(valid)
		wire[1] = flag
		ops, err := check(wire, true)
		if err != nil || len(ops) != 1 || ops[0].FAIImport == nil || ops[0].FAIImport.flags != flag {
			t.Fatal("valid message import framing changed")
		}
	}
	bad = bytes.Clone(valid)
	bad[1] = 0
	_, err := check(bad, false)
	var u unsupportedOperation
	if !errors.As(err, &u) || u.synchronizationOptions.importRejection != 0 {
		t.Fatal("disabled collector inspected import")
	}
	for _, enabled := range []bool{false, true} {
		_, err := (ROPBuffer{Operations: []byte{0x73, 4, 1}, Handles: []uint32{1, 2}}).decodeFAIOperations(true, true, false, false, false, false, true, enabled)
		if enabled {
			if !errors.Is(err, ErrMalformed) {
				t.Fatal("truncated hierarchy import was not rejected as malformed")
			}
			continue
		}
		if !errors.As(err, &u) || u.label != "RopSynchronizationImportHierarchyChange" {
			t.Fatal("lost unsupported hierarchy label")
		}
		if u.synchronizationOptions.importRejection != 0 {
			t.Fatal("disabled collector promoted hierarchy diagnostic")
		}
	}
}

func TestImportDeletesRejectionThroughExecute(t *testing.T) {
	for _, tc := range []struct {
		index int
		value byte
		label string
	}{
		{3, 1, "import_delete_hierarchy_not_supported"},
		{3, 4, "import_unknown_flags"},
		{4, 0, "import_delete_property_profile"},
		{6, 0, "import_delete_property_profile"},
		{10, 0, "import_delete_count_profile"},
		{14, 21, "import_delete_gid_profile"},
	} {
		t.Run(tc.label, func(t *testing.T) {
			f, _, store := newFAIExecuteFixture(t)
			wire := faiDeleteOperation(faiTestXID(1, 1))
			wire[tc.index] = tc.value
			body, counts := f.execute(append([]byte{1, 4, 1}, wire...), 65535)
			if !bytes.Equal(body, executeError(NotSupported, 65535)) || counts.unsupported != "RopSynchronizationImportDeletes" {
				t.Fatal("rejection changed wire result or operation label")
			}
			requireImportDiagnostic(t, counts.synchronizationOptions, tc.label)
			if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 || len(store.fai.absent) != 0 {
				t.Fatal("rejected batch changed folder or collector state")
			}
		})
	}
}

func TestImportRejectionVocabularyAndFormattingAreClosed(t *testing.T) {
	for n := 0; n < 256; n++ {
		r := importRejection(n)
		label := r.label()
		allowed := map[string]bool{
			"import_unknown_flags": true, "import_normal_not_supported": true, "import_property_count_profile": true,
			"import_xid_profile": true, "import_pcl_profile": true, "import_hierarchy_not_supported": true,
			"import_delete_hierarchy_not_supported": true, "import_delete_property_profile": true,
			"import_delete_count_profile": true, "import_delete_gid_profile": true, "import_other_profile": true,
			"import_fai_collector": true, "import_fai_draft_profile": true, "import_fai_handle_capacity": true,
			"import_normal_folder_profile": true, "import_normal_backend_profile": true,
			"import_normal_source_replica": true, "import_normal_source_reservation": true,
			"import_normal_existing_message":                 true,
			"import_appointment_save_mode":                   true,
			"import_appointment_save_empty":                  true,
			"import_appointment_save_decode":                 true,
			"import_appointment_save_profile":                true,
			"import_appointment_save_message_flags":          true,
			"import_appointment_save_meeting_state":          true,
			"import_appointment_save_reminder":               true,
			"import_appointment_save_native_profile":         true,
			"import_appointment_recurrence_decode":           true,
			"import_appointment_recurrence_rule":             true,
			"import_appointment_recurrence_dates":            true,
			"import_appointment_recurrence_exception":        true,
			"import_appointment_recurrence_attachment":       true,
			"import_appointment_exception_wall_time":         true,
			"import_appointment_exception_bounds":            true,
			"import_appointment_exception_embedded_anchor":   true,
			"import_appointment_exception_attachment_anchor": true,
			"import_appointment_save_start_whole":            true,
			"import_appointment_save_end_whole":              true,
			"import_appointment_save_range":                  true,
			"import_appointment_save_fixed_dates":            true,
			"import_appointment_save_common_dates":           true,
			"import_appointment_save_duration":               true,
			"import_appointment_save_text":                   true,
			"import_appointment_save_collector":              true,
			"import_appointment_save_native":                 true,
			"import_appointment_save_zone":                   true,
		}
		if !allowed[label] || !errors.Is(r, ErrUnsupported) {
			t.Fatal("open label vocabulary or broken error class")
		}
		if fmt.Sprintf("%v %+v %#v", r, r, r) != "importRejection{redacted} importRejection{redacted} importRejection{redacted}" {
			t.Fatal("diagnostic formatting exposed a number")
		}
	}
	// The same wire flags apply to normal and associated messages.
	r := syntheticFAIImport()
	for n := 0; n < 256; n++ {
		wire := faiImportWire(r)
		wire[1] = byte(n)
		_, err := parseFAIImport(&reader{data: wire}, 4)
		if n & ^0x50 == 0 {
			if err != nil {
				t.Fatal("supported flag rejected")
			}
			continue
		}
		var rejection importRejection
		if !errors.As(err, &rejection) || rejection != importUnknownFlags {
			t.Fatal("unknown flag classification changed")
		}
	}
}

func TestAssociatedImportExecutionRejectionTracePreservesBatch(t *testing.T) {
	for _, tc := range []struct {
		reason string
		input  byte
		fill   bool
	}{
		{"import_fai_collector", 1, false},
		{"import_fai_handle_capacity", 2, true},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			wire := faiImportOperation(syntheticFAIImport())
			wire[2] = tc.input
			// Release must still execute after the rejected import, even at the allocation ceiling.
			batch := append(wire, 1, 4, 1)
			want := ropError(0x72, 3, NotSupported)
			for _, enabled := range []bool{false, true} {
				f, _, store := newFAIExecuteFixture(t)
				if tc.fill {
					f.state.mailbox.allocated = maxContextHandleAllocations
				}
				folder := f.handles[1]
				budget, transfers := f.transport.stores.contents.used, f.transport.stores.transfers.Load()
				f.transport.synchronizationOptionsTrace = enabled
				response, counts := f.run(batch)
				if !bytes.Equal(response, want) || counts.errors != 1 || counts.unsupported != "" {
					t.Fatalf("unexpected runtime result % x", response)
				}
				if enabled {
					requireImportDiagnostic(t, counts.synchronizationOptions, tc.reason)
				} else if counts.synchronizationOptions.messageImportRefusal != 0 {
					t.Fatal("trace emitted by default")
				}
				if f.transport.stores.contents.used != budget || f.transport.stores.transfers.Load() != transfers || len(store.fai.records) != 0 {
					t.Fatal("failed import retained a draft or modified the store")
				}
				if !f.state.mailbox.released[folder] {
					t.Fatal("batch suffix did not release the folder")
				}
				f.state.mailbox.close()
				if f.transport.stores.contents.used != 0 || f.transport.stores.transfers.Load() != 0 {
					t.Fatal("fixture cleanup leaked")
				}
			}
		})
	}
}

func TestNormalImportRejectionPreservesBatch(t *testing.T) {
	for _, flag := range []byte{0, 0x40} {
		f, box, store := newFAIExecuteFixture(t)
		t.Cleanup(f.state.mailbox.close)
		f.transport.synchronizationOptionsTrace = true
		r := syntheticFAIImport()
		r.flags = flag
		importWire := faiImportOperation(r)
		suffix := []byte{0x09, 4, 1}
		before := f.handles[3]
		allocated, retained, calls := f.state.mailbox.allocated, f.transport.stores.contents.used, box.sourceCalls
		batch := append(importWire, suffix...)
		out, counts := f.run(batch)
		want := ropError(0x72, 3, ropNotImplemented)
		if !bytes.HasPrefix(out, want) || counts.errors != 1 || counts.unsupported != "" {
			t.Fatalf("normal import did not fail independently: %x", out)
		}
		if _, rest := listedPropertyTags(t, out[len(want):], 0x09, 1); len(rest) != 0 {
			t.Fatal("batch suffix not preserved")
		}
		requireImportDiagnostic(t, counts.synchronizationOptions, "import_normal_not_supported")
		if f.handles[3] != before || f.state.mailbox.allocated != allocated || f.transport.stores.contents.used != retained ||
			box.sourceCalls != calls || len(store.fai.records) != 0 {
			t.Fatal("normal import allocated a message or changed the store")
		}
		for n := 1; n < len(importWire); n++ {
			body, _ := f.execute(append([]byte{1, 4, 1}, importWire[:n]...), 65535)
			if !bytes.Equal(body, executeError(0x80070057, 65535)) {
				t.Fatalf("normal import accepted a truncated request at byte %d", n)
			}
			if _, code := f.state.mailbox.lookup(f.handles[1], 4); code != 0 {
				t.Fatal("truncated normal import released the prefix handle")
			}
		}
	}
}

func TestImportRejectionTransportTraceGateDoesNotChangeResponse(t *testing.T) {
	f, _, _ := newFAIExecuteFixture(t)
	identity, _ := f.transport.namespace.Mailbox(f.session.Identity())
	r := syntheticFAIImport()
	r.flags = 0x11
	batch := append([]byte{1, 4, 1}, faiImportOperation(r)...)
	wire, _ := (ROPBuffer{Operations: batch, Handles: f.handles}).Marshal()
	request, _ := (Execute{ROP: wire, MaxOutput: 4096}).Marshal()
	// The fixture context is internal. Exercise the actual HTTP path with a
	// separate Connect, without relying on its private cookie. The parser
	// rejects the whole batch before looking up any operation's object.
	f.transport.EnableSynchronizationOptionsTrace()
	connect, _ := (Connect{UserDN: identity.LegacyDN, CodePage: 1252, SortLCID: 1033, StringLCID: 1033}).Marshal()
	connected := transportRequest(t, f.transport, f.session, "Connect", connect, nil)
	requireTransportCode(t, connected, 0)
	cookies := connected.Result().Cookies()
	var response []byte
	for _, enabled := range []bool{false, true} {
		f.transport.synchronizationOptionsTrace = enabled
		req := httptest.NewRequest("POST", "https://gateway.invalid/mapi/emsmdb/?MailboxId="+identity.RoutingID, bytes.NewReader(request))
		req.Header.Set("Content-Type", "application/mapi-http")
		req.Header.Set("X-RequestId", "synthetic-request:1")
		req.Header.Set("X-RequestType", "Execute")
		for _, cookie := range cookies {
			req.AddCookie(cookie)
		}
		w := &syncOptionsRecorder{ResponseRecorder: httptest.NewRecorder()}
		f.transport.Mailbox(w, req, f.session)
		cookies = w.Result().Cookies()
		body := requireTransportCode(t, w.ResponseRecorder, 0)
		if !enabled {
			response = bytes.Clone(body)
			if w.calls != 0 {
				t.Fatal("trace emitted by default")
			}
		} else {
			if w.calls != 1 || !bytes.Equal(response, body) {
				t.Fatal("trace changed response or missed callback")
			}
			requireImportDiagnostic(t, w.diagnostic, "import_unknown_flags")
		}
		if strings.Contains(fmt.Sprintf("%v %+v %#v", w.diagnostic, w.diagnostic, w.diagnostic), "PRIVATE") {
			t.Fatal("private diagnostic")
		}
	}
	req := httptest.NewRequest("POST", "https://gateway.invalid/mapi/emsmdb/?MailboxId="+identity.RoutingID, bytes.NewReader(request))
	req.Header.Set("Content-Type", "application/mapi-http")
	req.Header.Set("X-RequestId", "synthetic-request:1")
	req.Header.Set("X-RequestType", "Execute")
	w := &syncOptionsRecorder{ResponseRecorder: httptest.NewRecorder()}
	f.transport.Mailbox(w, req, f.session)
	if w.calls != 0 {
		t.Fatal("invalid context reached import trace")
	}
}

func FuzzImportRejectionClosedVocabulary(f *testing.F) {
	f.Add(faiImportWire(syntheticFAIImport()))
	f.Add([]byte{3, 0, 4, 0})
	f.Add([]byte{3, 0x11, 4, 0})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 65500 {
			return
		}
		_, err := (ROPBuffer{Operations: append([]byte{0x72, 4, 2}, raw...), Handles: []uint32{1, 2, 3, 4}}).decodeFAIOperations(true, true, false, false, false, false, true, true)
		var u unsupportedOperation
		if !errors.As(err, &u) {
			return
		}
		if u.label != "RopSynchronizationImportMessageChange" {
			if u.synchronizationOptions.importRejection != 0 {
				t.Fatal("later operation acquired an import reason")
			}
			return
		}
		if u.synchronizationOptions.parsed {
			t.Fatal("misattributed diagnostic")
		}
		r := u.synchronizationOptions.importRejection
		if r == 0 || r > importPCLProfile {
			t.Fatal("unbounded import reason")
		}
		requireImportDiagnostic(t, u.synchronizationOptions, r.label())
	})
}
