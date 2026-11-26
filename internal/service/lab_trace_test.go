// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"bytes"
	"encoding/json"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ewsgo/internal/frontend/mapi"
)

func TestLaboratoryTraceDoesNotRecordPrivateRequestFields(t *testing.T) {
	var output bytes.Buffer
	h := laboratoryTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNotImplemented)
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("private-response"))
	}), false, false, false)
	r := httptest.NewRequest("POST", "https://private-host.invalid/MAPI/emsmdb/?MailboxId=private-account", strings.NewReader("private-body"))
	r.Header.Set("Authorization", "Basic private-authorization")
	r.Header.Set("Cookie", "private-cookie")
	r.Header.Set("User-Agent", "private-agent")
	r.Header.Set("X-RequestId", "private-request-id")
	r.Header.Set("X-RequestType", "private-operation")
	r.Header.Set("X-MapiHttpCapability", "private-capability")
	r.RemoteAddr = "private-remote"
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusNotImplemented || strings.Contains(output.String(), "private-") {
		t.Fatal("trace leaked private data or changed HTTP status")
	}
	var event map[string]any
	if err := json.Unmarshal(output.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if len(event) != 34 || event["store_property_requests"] != "none" || event["rop_unsupported"] != "none" || event["route"] != "mapi_emsmdb" || event["auth_scheme"] != "Basic" || event["request_type"] != "other" || event["status"] != float64(501) || event["nspi_names"] != "none" || event["nspi_resolved"] != "none" || event["store_logons"] != "none" {
		t.Fatal("unexpected diagnostic vocabulary")
	}
}

func TestFolderPropertyTraceOnlyContainsApprovedNamesKindsAndBoundedCounts(t *testing.T) {
	for _, tc := range []struct {
		d     mapi.StorePropertyDiagnostic
		valid bool
	}{
		{mapi.StorePropertyDiagnostic{Folder: true, Logon: true, Requests: 2, Requested: 6, Values: 3, Errors: 2, Other: 1}, true},
		{mapi.StorePropertyDiagnostic{Requests: 1024, Requested: 262144, Errors: 262144, Other: 262144}, true},
		{mapi.StorePropertyDiagnostic{Requests: 0}, false},
		{mapi.StorePropertyDiagnostic{Requests: 1025}, false},
		{mapi.StorePropertyDiagnostic{Requests: 1, Requested: 257}, false},
		{mapi.StorePropertyDiagnostic{Requests: 1, Requested: -1}, false},
		{mapi.StorePropertyDiagnostic{Requests: 1, Requested: 1, Values: 1, Errors: 1}, false},
		{mapi.StorePropertyDiagnostic{Requests: 1, Other: 1}, false},
		{mapi.StorePropertyDiagnostic{Requests: 1, Values: -1}, false},
		{mapi.StorePropertyDiagnostic{Requests: 1, Errors: -1}, false},
		{mapi.StorePropertyDiagnostic{Requests: 1, Other: -1}, false},
	} {
		var output bytes.Buffer
		h := laboratoryTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.(interface {
				RecordStoreProperties(mapi.StorePropertyDiagnostic)
			}).RecordStoreProperties(tc.d)
			w.Write([]byte("private-folder-name-and-FID"))
		}), false, false, false)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://private-server.invalid/mapi/emsmdb/?private-FID", strings.NewReader("private-tags")))
		var event map[string]any
		if json.Unmarshal(output.Bytes(), &event) != nil || strings.Contains(output.String(), "private-") {
			t.Fatal("private property data leaked")
		}
		if tc.valid {
			if event["store_property_requests"] == "other" {
				t.Fatal("valid counts rejected")
			}
			// An outside package cannot populate the opaque name set using
			// caller-supplied strings or numeric tags.
			if len(event["store_properties"].([]any)) != 0 {
				t.Fatal("names fabricated without requests")
			}
		} else if event["store_property_requests"] != "other" || event["store_property_values"] != "other" {
			t.Fatal("unbounded counts accepted")
		}
	}
}

func TestUnknownPropertyTraceSchemaIsExplicitAndDoesNotExposeRequestContent(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, valid := range []bool{false, true} {
			var output bytes.Buffer
			h := laboratoryTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				d := mapi.StorePropertyDiagnostic{Requests: 1, Requested: 2, Other: 2}
				if !valid {
					d.Requested = -1
				}
				w.(interface {
					RecordStoreProperties(mapi.StorePropertyDiagnostic)
				}).RecordStoreProperties(d)
				_, _ = w.Write([]byte("private-property-value"))
			}), enabled, false, false)
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://private.invalid/mapi/emsmdb/?private-id", strings.NewReader("private-tags")))
			var event map[string]any
			if json.Unmarshal(output.Bytes(), &event) != nil || strings.Contains(output.String(), "private-") {
				t.Fatal("request content leaked")
			}
			wantFields := 34
			if enabled {
				wantFields = 38
			}
			if len(event) != wantFields {
				t.Fatal("default or opt-in schema changed")
			}
			for _, key := range []string{"store_property_unknown_tags", "store_property_unknown_tag_overflow", "store_property_unknown_high_ids", "store_property_unknown_reserved_ids"} {
				if _, exists := event[key]; exists != enabled {
					t.Fatal("opt-in field presence")
				}
			}
			if enabled {
				tags := event["store_property_unknown_tags"].([]any)
				if valid {
					if len(tags) != 0 || event["store_property_unknown_high_ids"] != "0" {
						t.Fatal("opaque zero diagnostic fabricated tags")
					}
				} else if len(tags) != 1 || tags[0] != "other" || event["store_property_unknown_tag_overflow"] != "other" {
					t.Fatal("invalid counts accepted")
				}
			}
		}
	}
}

func TestContentsBindingDiagnosticRequiresBothGatesAndPreservesPropertyCounters(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		for _, options := range []bool{false, true} {
			var output bytes.Buffer
			h := laboratoryOptionsTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.(interface {
					RecordContentsBindingDiagnostic(mapi.StorePropertyDiagnostic)
				}).RecordContentsBindingDiagnostic(mapi.StorePropertyDiagnostic{})
				w.Write([]byte("private-response"))
			}), unknown, false, false, options)
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://private.invalid/mapi/emsmdb/?private-id", strings.NewReader("private-value")))
			var e map[string]any
			if json.Unmarshal(output.Bytes(), &e) != nil || strings.Contains(output.String(), "private-") {
				t.Fatal("binding log leaked request data")
			}
			if e["store_property_requests"] != "none" || e["store_property_requested"] != "none" || e["store_property_values"] != "none" {
				t.Fatal("binding log fabricated GetProperties requests")
			}
			if unknown {
				want := "none"
				if options {
					want = "0"
				}
				if e["store_property_unknown_high_ids"] != want {
					t.Fatal("binding receiver escaped combined gate")
				}
			}
			want := 34
			if unknown {
				want += 4
			}
			if options {
				want += 12
				if unknown {
					want += 2
				}
			}
			if len(e) != want {
				t.Fatal("binding changed log schema")
			}
		}
	}
}

func TestUnsupportedROPTraceHasClosedVocabulary(t *testing.T) {
	for _, label := range []string{"RopGetPropertiesSpecific", "RopGetPropertyIdsFromNames", "private-name", "RopGetPropertiesSpecific/private-name"} {
		var output bytes.Buffer
		h := laboratoryTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.(interface{ RecordUnsupportedROP(string) }).RecordUnsupportedROP(label)
		}), false, false, false)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://example.invalid/mapi/emsmdb/", nil))
		var event map[string]any
		want := label
		if strings.Contains(label, "private-") {
			want = "other"
		}
		if json.Unmarshal(output.Bytes(), &event) != nil || event["rop_unsupported"] != want || strings.Contains(output.String(), "private-") {
			t.Fatal("unsafe unsupported-opcode trace")
		}
	}
}

func TestStoreTraceCountsAreBounded(t *testing.T) {
	for _, tc := range []struct {
		counts [3]int
		want   [3]string
	}{
		{[3]int{1, 13, 0}, [3]string{"1", "13", "0"}},
		{[3]int{0, 0, 1024}, [3]string{"0", "0", "1024"}},
		{[3]int{-1, 1, 0}, [3]string{"other", "other", "other"}},
		{[3]int{1024, 1, 0}, [3]string{"other", "other", "other"}},
	} {
		var output bytes.Buffer
		h := laboratoryTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.(interface{ RecordStoreResult(int, int, int) }).RecordStoreResult(tc.counts[0], tc.counts[1], tc.counts[2])
			_, _ = w.Write([]byte("private-folder-handles"))
		}), false, false, false)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://example.invalid/mapi/emsmdb/?private-scope", nil))
		var event map[string]any
		if json.Unmarshal(output.Bytes(), &event) != nil || event["store_logons"] != tc.want[0] || event["store_folders"] != tc.want[1] || event["store_rop_errors"] != tc.want[2] || strings.Contains(output.String(), "private-") {
			t.Fatal("unsafe store trace")
		}
	}
}

func TestLaboratoryROPDiagnosticsHaveClosedVocabulary(t *testing.T) {
	for _, tc := range []struct {
		layout, first         string
		flags                 []string
		wantLayout, wantFirst string
		wantFlags             []string
	}{
		{"plain", "logon", []string{"private", "home_logon"}, "plain", "logon", []string{"private", "home_logon"}},
		{"plain", "RopGetPropertiesSpecific", nil, "plain", "RopGetPropertiesSpecific", []string{}},
		{"plain", "RopGetReceiveFolder", nil, "plain", "RopGetReceiveFolder", []string{}},
		{"plain", "RopGetPermissionsTable", []string{"table_include_free_busy", "table_flags_zero", "table_other_bits", "private-flag"}, "plain", "RopGetPermissionsTable", []string{"table_include_free_busy", "table_flags_zero", "table_other_bits", "other"}},
		{"plain", "RopGetHierarchyTable", []string{"table_depth", "table_deferred_errors", "table_no_notifications", "table_soft_deletes", "table_unicode", "table_suppress_notifications"}, "plain", "RopGetHierarchyTable", []string{"table_depth", "table_deferred_errors", "table_no_notifications", "table_soft_deletes", "table_unicode", "table_suppress_notifications"}},
		{"plain", "RopGetContentsTable", []string{"table_associated"}, "plain", "RopGetContentsTable", []string{"table_associated"}},
		{"plain", "RopGetPropertiesSpecific/private-DN", nil, "plain", "other", []string{}},
		{"compressed_xor", "none", nil, "compressed_xor", "none", []string{}},
		{"private-framing", "private-ROP", []string{"private-DN"}, "other", "other", []string{"other"}},
		{"plain", "logon", make([]string, 17), "plain", "logon", []string{"limit"}},
	} {
		var output bytes.Buffer
		h := laboratoryTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.(interface {
				RecordROPDiagnostic(string, string, []string)
			}).RecordROPDiagnostic(tc.layout, tc.first, tc.flags)
			_, _ = w.Write([]byte("private-ROP-body"))
		}), false, false, false)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://example.invalid/mapi/emsmdb/", strings.NewReader("private-ROP-request")))
		var event map[string]json.RawMessage
		if err := json.Unmarshal(output.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		var layout, first string
		var flags []string
		if json.Unmarshal(event["rop_layout"], &layout) != nil || json.Unmarshal(event["rop_first"], &first) != nil || json.Unmarshal(event["rop_flags"], &flags) != nil {
			t.Fatal("missing ROP diagnostic")
		}
		if layout != tc.wantLayout || first != tc.wantFirst || strings.Join(flags, ",") != strings.Join(tc.wantFlags, ",") || strings.Contains(output.String(), "private-") {
			t.Fatal("unsafe ROP diagnostic")
		}
	}
}

func TestLaboratoryCodepageDoesNotLogClientControlledNumbers(t *testing.T) {
	for _, tc := range []struct {
		cp      uint32
		present bool
		want    string
	}{
		{0, false, "none"}, {0, true, "zero"}, {1200, true, "winunicode"}, {1252, true, "windows_1252"}, {65001, true, "utf8"}, {0x70726976, true, "other"},
	} {
		var output bytes.Buffer
		h := laboratoryTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.(interface{ RecordNSPICodepage(uint32, bool) }).RecordNSPICodepage(tc.cp, tc.present)
		}), false, false, false)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://example.invalid/mapi/nspi/", nil))
		var event map[string]any
		if json.Unmarshal(output.Bytes(), &event) != nil || event["nspi_codepage"] != tc.want {
			t.Fatal("unsafe codepage vocabulary")
		}
	}
}

func TestLaboratoryDirectoryCountsBoundedWithoutIdentityCapture(t *testing.T) {
	for _, tc := range []struct {
		names, resolved         int
		wantNames, wantResolved string
	}{{5, 4, "5", "4"}, {0, 0, "0", "0"}, {256, 256, "256", "256"}, {-1, 0, "other", "other"}, {257, 1, "other", "other"}, {1, 2, "other", "other"}, {1, -1, "other", "other"}} {
		var output bytes.Buffer
		h := laboratoryTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.(interface{ RecordDirectoryResult(int, int) }).RecordDirectoryResult(tc.names, tc.resolved)
			_, _ = w.Write([]byte("private-response-with-identifiers"))
		}), false, false, false)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://example.invalid/mapi/nspi/?private-selector", strings.NewReader("private-DNs")))
		var event map[string]any
		if err := json.Unmarshal(output.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event["nspi_names"] != tc.wantNames || event["nspi_resolved"] != tc.wantResolved || strings.Contains(output.String(), "private-") {
			t.Fatal("directory trace leaked values or accepted unbounded counts")
		}
	}
}

func TestLaboratoryTraceRequiresTLS(t *testing.T) {
	if err := (Config{Listen: "127.0.0.1:8443", OutlookLab: true}).Validate(); err == nil {
		t.Fatal("plaintext laboratory tracing accepted")
	}
}

func TestLaboratoryPropertiesAreClosedNamesNotRawTags(t *testing.T) {
	for _, tc := range []struct {
		tags     []uint32
		defaults bool
		want     []string
	}{
		{[]uint32{0x3001001f, 0x8006001e, 0x70726976, 0x39feffff, 0x3001001f}, false, []string{"display_name:unicode", "home_mdb:string8", "other:other", "smtp_address:other", "display_name:unicode"}},
		{nil, true, []string{"defaults"}},
		{[]uint32{}, false, []string{}},
		{make([]uint32, 257), false, []string{"limit"}},
	} {
		var output bytes.Buffer
		h := laboratoryTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.(interface{ RecordNSPIProperties([]uint32, bool) }).RecordNSPIProperties(tc.tags, tc.defaults)
			w.(interface{ RecordNSPIPropertyResult(int, int) }).RecordNSPIPropertyResult(5, 2)
			_, _ = w.Write([]byte("private-properties-and-DN"))
		}), false, false, false)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://example.invalid/mapi/nspi/", nil))
		var event map[string]json.RawMessage
		if err := json.Unmarshal(output.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		var got []string
		if err := json.Unmarshal(event["nspi_properties"], &got); err != nil {
			t.Fatal(err)
		}
		if strings.Join(got, ",") != strings.Join(tc.want, ",") || strings.Contains(output.String(), "private-") || string(event["nspi_values"]) != `"5"` || string(event["nspi_errors"]) != `"2"` {
			t.Fatal("unsafe property diagnostics")
		}
	}
	for _, counts := range [][2]int{{-1, 0}, {257, 0}, {1, 2}, {1, -1}} {
		w := &labResponse{}
		w.RecordNSPIPropertyResult(counts[0], counts[1])
		if w.propertyValues != "other" || w.propertyErrors != "other" {
			t.Fatal("unbounded property counts")
		}
	}
}

func TestLaboratoryTraceRecordsClosedMAPIResultsWithoutBodyCapture(t *testing.T) {
	for _, tc := range []struct {
		status, method uint32
		result         string
	}{
		{0, 0, "success"}, {0, 1, "unbind_success"}, {0, 0x80040102, "not_supported"},
		{0, 0x8004011e, "invalid_codepage"}, {0, 0x80070057, "invalid_parameter"},
		{0, 0x80070005, "access_denied"}, {0, 0xffffffff, "other"}, {1, 0, "failure_status"},
	} {
		var output bytes.Buffer
		h := laboratoryTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.(interface{ RecordMAPIResult(uint32, uint32) }).RecordMAPIResult(tc.status, tc.method)
			w.Header().Set("X-ResponseCode", "0")
			w.Header().Set("Set-Cookie", "private-cookie")
			_, _ = w.Write([]byte("private-body"))
		}), false, false, false)
		r := httptest.NewRequest("POST", "https://example.invalid/mapi/nspi/", nil)
		r.Header.Set("X-RequestType", "Bind")
		h.ServeHTTP(httptest.NewRecorder(), r)
		var event map[string]any
		if err := json.Unmarshal(output.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event["mapi_operation_result"] != tc.result || event["mapi_response_code"] != "0" || strings.Contains(output.String(), "private-") {
			t.Fatal("incorrect or unsafe MAPI trace")
		}
	}
	var output bytes.Buffer
	h := laboratoryTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.Header().Set("X-ResponseCode", "private-header") }), false, false, false)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://example.invalid/mapi/nspi/", nil))
	if strings.Contains(output.String(), "private-header") || !strings.Contains(output.String(), `"mapi_response_code":"other"`) {
		t.Fatal("unbounded response header logged")
	}
}

func TestLaboratoryAuthClassifiesProbesWithoutInspectingParameters(t *testing.T) {
	for _, tc := range []struct {
		values               []string
		scheme               string
		parameters, multiple bool
	}{
		{nil, "none", false, false},
		{[]string{""}, "none", false, false},
		{[]string{"Basic"}, "Basic", false, false},
		{[]string{"basic private-credentials"}, "Basic", true, false},
		{[]string{"Anonymous"}, "Anonymous", false, false},
		{[]string{"Negotiate"}, "Negotiate", false, false},
		{[]string{"NTLM private-token"}, "NTLM", true, false},
		{[]string{"Nego2 private-token"}, "Nego2", true, false},
		{[]string{"Kerberos private-token"}, "Kerberos", true, false},
		{[]string{"Digest private-token"}, "Digest", true, false},
		{[]string{"Bearer private-token"}, "Bearer", true, false},
		{[]string{"private-scheme private-token"}, "other", true, false},
		{[]string{"Basic private-one", "NTLM private-two"}, "Basic", true, true},
		{[]string{"BasicPrivate"}, "other", false, false},
		{[]string{"Basic\tprivate-token"}, "Basic", true, false},
		{[]string{"Basic \t "}, "Basic", false, false},
	} {
		scheme, parameters, multiple := laboratoryAuth(tc.values)
		if scheme != tc.scheme || parameters != tc.parameters || multiple != tc.multiple {
			t.Fatal("incorrect safe auth classification")
		}
		var output bytes.Buffer
		h := laboratoryTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(401) }), false, false, false)
		r := httptest.NewRequest("POST", "https://example.invalid/mapi/nspi/", nil)
		for _, value := range tc.values {
			r.Header.Add("Authorization", value)
		}
		h.ServeHTTP(httptest.NewRecorder(), r)
		if strings.Contains(output.String(), "private-") {
			t.Fatal("authentication material leaked")
		}
		var event map[string]any
		if err := json.Unmarshal(output.Bytes(), &event); err != nil {
			t.Fatal(err)
		}
		if event["auth_scheme"] != tc.scheme || event["auth_parameters_present"] != tc.parameters || event["auth_header_multiple"] != tc.multiple {
			t.Fatal("incorrect trace auth fields")
		}
	}
}

func TestSetPropertyTraceSchemaAndNoValueCapture(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		for _, enabled := range []bool{false, true} {
			for _, tc := range []struct {
				ops  []byte
				want [3]string
			}{
				{[]byte{0x0a, 0, 1, 8, 0, 1, 0, 2, 1, 4, 0x7c, 0, 0}, [3]string{"header_only", "1", "0x7c040102"}},
				{[]byte{0x0a, 0, 1, 8, 0, 1, 0, 2, 1, 0, 0x80, 0, 0}, [3]string{"header_only", "1", "high_id"}},
				{[]byte{0x0a, 0, 1, 8, 0, 1, 0, 2, 1, 0xff, 0xff, 0, 0}, [3]string{"header_only", "1", "reserved_id"}},
				{[]byte{0x0a, 0, 1, 8, 0, 1, 0}, [3]string{"malformed", "none", "none"}},
				{[]byte{1, 0, 1}, [3]string{"none", "none", "none"}},
			} {
				wire, err := (mapi.ROPBuffer{Operations: tc.ops, Handles: []uint32{1, 2}}).Marshal()
				if err != nil {
					t.Fatal(err)
				}
				var output bytes.Buffer
				h := laboratoryTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
					w.(interface {
						RecordSetPropertyDiagnostic(mapi.SetPropertyDiagnostic)
					}).RecordSetPropertyDiagnostic(mapi.InspectSetProperty(wire))
					_, _ = w.Write([]byte("private-property-value"))
				}), unknown, enabled, false)
				h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://private.invalid/mapi/emsmdb/?private-handle", strings.NewReader("private-payload")))
				var event map[string]any
				if json.Unmarshal(output.Bytes(), &event) != nil || strings.Contains(output.String(), "private-") {
					t.Fatal("private value leaked")
				}
				wantFields := 34
				if unknown {
					wantFields += 4
				}
				if enabled {
					wantFields += 3
				}
				if len(event) != wantFields {
					t.Fatal("opt-in schema")
				}
				for i, key := range []string{"store_set_property_inspection", "store_set_property_count", "store_set_property_first_tag"} {
					value, present := event[key]
					if present != enabled || enabled && value != tc.want[i] {
						t.Fatal("SetProperties field gate or content")
					}
				}
			}
		}
	}
}

func TestSynchronizationTraceOptInSchemaAndPrivacy(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var output bytes.Buffer
		h := laboratoryTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.(interface {
				RecordSynchronizationDiagnostic(mapi.SynchronizationDiagnostic)
			}).RecordSynchronizationDiagnostic(mapi.SynchronizationDiagnostic{})
			_, _ = w.Write([]byte("private-response-body"))
		}), true, true, enabled)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://private.invalid/mapi/emsmdb/?private-handle", strings.NewReader("private-restrictions-tags-values")))
		var event map[string]any
		if json.Unmarshal(output.Bytes(), &event) != nil || strings.Contains(output.String(), "private-") {
			t.Fatal("private data leaked")
		}
		want := 41
		if enabled {
			want += 2
		}
		if len(event) != want {
			t.Fatal("explicit schema", len(event))
		}
		for _, key := range []string{"store_sync_inspection", "store_sync_type"} {
			value, present := event[key]
			if present != enabled || enabled && value != "none" {
				t.Fatal("trace gate")
			}
		}
	}
}

func TestSynchronizationOptionsSchemaAndExplicitGate(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var output bytes.Buffer
		h := laboratoryOptionsTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.(interface {
				RecordSynchronizationOptionsDiagnostic(mapi.SynchronizationOptionsDiagnostic)
			}).RecordSynchronizationOptionsDiagnostic(mapi.SynchronizationOptionsDiagnostic{})
			_, _ = w.Write([]byte("private-response"))
		}), true, true, true, enabled)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://private.invalid/mapi/emsmdb/?private-folder", strings.NewReader("private-filter-tags")))
		var event map[string]any
		if json.Unmarshal(output.Bytes(), &event) != nil || strings.Contains(output.String(), "private-") {
			t.Fatal("private input leaked")
		}
		want := 43
		if enabled {
			want = 57
		}
		if len(event) != want {
			t.Fatal("schema", len(event))
		}
		for _, key := range []string{"store_sync_flags", "store_sync_send_options", "store_sync_extra_flags", "store_sync_profile_rejections", "store_sync_restriction", "store_sync_tag_count", "store_rejected_table_flags"} {
			_, ok := event[key]
			if ok != enabled {
				t.Fatal("options escaped gate")
			}
		}
	}
}

func TestCreateFolderTraceSchemaAndPrivacyGate(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var output bytes.Buffer
		h := detailedLaboratoryTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.(interface {
				RecordCreateFolderDiagnostic(mapi.CreateFolderDiagnostic)
			}).RecordCreateFolderDiagnostic(mapi.CreateFolderDiagnostic{})
			_, _ = w.Write([]byte("PRIVATE-response"))
		}), laboratoryTraceOptions{true, true, true, true, enabled, false, false})
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://private.invalid/mapi/emsmdb/?PRIVATE-parent", strings.NewReader("PRIVATE-folder-name-and-comment")))
		var event map[string]any
		if json.Unmarshal(output.Bytes(), &event) != nil || strings.Contains(output.String(), "PRIVATE") || strings.Contains(output.String(), "private.invalid") {
			t.Fatal("identity/payload leaked")
		}
		want := 57
		if enabled {
			want = 62
		}
		if len(event) != want {
			t.Fatal("explicit schema", len(event))
		}
		for _, key := range []string{"store_create_folder_inspection", "store_create_folder_type", "store_create_folder_encoding", "store_create_folder_open_existing", "store_create_folder_reserved"} {
			v, ok := event[key]
			if ok != enabled || enabled && v != "none" {
				t.Fatal("trace gate")
			}
		}
	}
}

func TestSetListTraceSchemaAndPrivacyGate(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var output bytes.Buffer
		h := detailedLaboratoryTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			w.(interface{ RecordSetListDiagnostic(mapi.SetListDiagnostic) }).RecordSetListDiagnostic(mapi.SetListDiagnostic{})
			_, _ = w.Write([]byte("PRIVATE-response"))
		}), laboratoryTraceOptions{true, true, true, true, true, enabled, false})
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://private.invalid/mapi/emsmdb/?PRIVATE", strings.NewReader("PRIVATE-values")))
		var event map[string]any
		if json.Unmarshal(output.Bytes(), &event) != nil || strings.Contains(output.String(), "PRIVATE") || strings.Contains(output.String(), "private.invalid") {
			t.Fatal("private data leaked")
		}
		want := 62
		if enabled {
			want = 67
		}
		if len(event) != want {
			t.Fatal("schema", len(event))
		}
		for _, key := range []string{"store_set_list_inspection", "store_set_list_tags", "store_set_list_visited", "store_set_list_high_ids", "store_set_list_reserved_ids"} {
			if _, ok := event[key]; ok != enabled {
				t.Fatal("gate")
			}
		}
	}
}

func TestLaboratoryTraceNotificationWaitOutcome(t *testing.T) {
	for _, test := range []struct {
		name, want string
		trace      bool
		pending    bool
		record     bool
	}{
		{name: "pending", want: "event_pending", trace: true, pending: true, record: true},
		{name: "no event", want: "no_event", trace: true, record: true},
		{name: "trace disabled", pending: true, record: true},
		{name: "incomplete response", trace: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			var output bytes.Buffer
			h := laboratoryOptionsTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				if test.record {
					w.(*labResponse).RecordNotificationWait(test.pending)
				}
			}), false, false, false, test.trace)
			r := httptest.NewRequest("POST", "/mapi/emsmdb/", nil)
			r.Header.Set("X-RequestType", "NotificationWait")
			h.ServeHTTP(httptest.NewRecorder(), r)
			var event map[string]any
			if err := json.Unmarshal(output.Bytes(), &event); err != nil {
				t.Fatal(err)
			}
			got, present := event["store_notification_wait"]
			if present != (test.want != "") || present && got != test.want {
				t.Fatalf("store_notification_wait = %v, want %q", got, test.want)
			}
		})
	}
}

func TestLaboratoryTraceBindingNamesDoNotInventObjectTypes(t *testing.T) {
	var output bytes.Buffer
	h := laboratoryOptionsTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Binding-only diagnostics can report a known public name without
		// a GetProperties request or a Folder/Logon diagnostic object kind.
		response := w.(*labResponse)
		response.storePropertyNames = []string{"object_type"}
		response.RecordContentsBindingDiagnostic(mapi.StorePropertyDiagnostic{})
	}), true, false, false, true)
	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/mapi/emsmdb/", nil))
	var event map[string]any
	if err := json.Unmarshal(output.Bytes(), &event); err != nil {
		t.Fatal(err)
	}
	if raw, _ := json.Marshal(event["store_property_objects"]); string(raw) != `["none"]` {
		t.Fatal("binding-only object kinds must use the existing none sentinel")
	}
	for _, key := range []string{"store_property_requests", "store_property_requested", "store_property_values", "store_property_errors", "store_property_other"} {
		if event[key] != "none" {
			t.Fatal("binding-only diagnostic fabricated GetProperties counters")
		}
	}
}

func TestCalendarSetOutcomeTraceSchemaPrivacyAndGate(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		for _, diagnostic := range []mapi.CalendarReferenceSetDiagnostic{{}, {Requests: 1, Confirmed: 1}, {Requests: 1, PropertyProblems: 1}, {Requests: 1, OperationErrors: 1}, {Requests: 3, Confirmed: 1, PropertyProblems: 1, OperationErrors: 1}, {Requests: -1}} {
			var output bytes.Buffer
			h := detailedLaboratoryTrace(slog.New(slog.NewJSONHandler(&output, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.(interface {
					RecordCalendarReferenceSetDiagnostic(mapi.CalendarReferenceSetDiagnostic)
				}).RecordCalendarReferenceSetDiagnostic(diagnostic)
				_, _ = w.Write([]byte("PRIVATE-response"))
			}), laboratoryTraceOptions{true, true, true, true, true, true, enabled})
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://PRIVATE.invalid/mapi/emsmdb/?PRIVATE", strings.NewReader("PRIVATE-values")))
			var event map[string]any
			if json.Unmarshal(output.Bytes(), &event) != nil || strings.Contains(output.String(), "PRIVATE") {
				t.Fatal("private data leaked")
			}
			want := 67
			if enabled {
				want = 69
			}
			if len(event) != want {
				t.Fatal("schema", len(event))
			}
			outcomes, n := diagnostic.Fields()
			for _, key := range []string{"store_calendar_set_outcomes", "store_calendar_set_requests"} {
				if _, ok := event[key]; ok != enabled {
					t.Fatal("gate")
				}
			}
			if enabled {
				raw, _ := json.Marshal(outcomes)
				got, _ := json.Marshal(event["store_calendar_set_outcomes"])
				if !bytes.Equal(raw, got) || event["store_calendar_set_requests"] != n {
					t.Fatal("result vocabulary or sanitization")
				}
			}
		}
	}
}
