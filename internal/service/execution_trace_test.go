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

func TestExecutionTraceSharesExplicitSyncOptionsGate(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		var out bytes.Buffer
		h := laboratoryOptionsTrace(slog.New(slog.NewJSONHandler(&out, nil)), http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.(interface {
				RecordExecutionDiagnostic(mapi.ExecutionDiagnostic)
			}).RecordExecutionDiagnostic(mapi.ExecutionDiagnostic{})
			_, _ = w.Write([]byte("PRIVATE-response"))
		}), false, false, false, enabled)
		h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://PRIVATE.invalid/mapi/emsmdb/?PRIVATE-id", strings.NewReader("PRIVATE-data")))
		var e map[string]any
		if json.Unmarshal(out.Bytes(), &e) != nil || strings.Contains(out.String(), "PRIVATE") {
			t.Fatal("private data escaped")
		}
		for key, want := range map[string]string{"store_execute_stage": "none", "store_first_rop_error": "none", "store_getbuffer_calls": "0"} {
			got, ok := e[key]
			if ok != enabled || enabled && got != want {
				t.Fatal("gate/default", key, got)
			}
		}
		for _, key := range []string{"store_executed_rops", "store_getbuffer_outcomes"} {
			got, ok := e[key]
			raw, _ := json.Marshal(got)
			if ok != enabled || enabled && string(raw) != `["none"]` {
				t.Fatal("bounded default", key, got)
			}
		}
	}
}

func TestTableProfileTraceRequiresBothExplicitGates(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		for _, options := range []bool{false, true} {
			var out bytes.Buffer
			h := laboratoryOptionsTrace(slog.New(slog.NewJSONHandler(&out, nil)), http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.(interface {
					RecordExecutionDiagnostic(mapi.ExecutionDiagnostic)
				}).RecordExecutionDiagnostic(mapi.ExecutionDiagnostic{})
			}), unknown, false, false, options)
			h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "/mapi/emsmdb/", strings.NewReader("PRIVATE-value")))
			var event map[string]any
			if json.Unmarshal(out.Bytes(), &event) != nil || strings.Contains(out.String(), "PRIVATE") {
				t.Fatal("request content escaped")
			}
			for _, key := range []string{"store_table_profile", "store_folder_open_profile"} {
				value, exists := event[key]
				encoded, _ := json.Marshal(value)
				if exists != (unknown && options) || exists && string(encoded) != `["none"]` {
					t.Fatal("diagnostic gate/default", key)
				}
			}
		}
	}
}
