// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"ewsgo/internal/backend/memory"
	"ewsgo/internal/core"
	"ewsgo/internal/frontend/ews"
	"ewsgo/internal/frontend/mapi"
)

func logEvents(t *testing.T, data []byte) []map[string]any {
	t.Helper()
	var events []map[string]any
	decoder := json.NewDecoder(bytes.NewReader(data))
	for {
		var event map[string]any
		err := decoder.Decode(&event)
		if err == io.EOF {
			break
		}
		if err != nil {
			t.Fatal("invalid JSON log")
		}
		events = append(events, event)
	}
	return events
}

func TestLogLevelsAreCumulative(t *testing.T) {
	for _, tc := range []struct {
		level string
		want  int
	}{{"none", 0}, {"err", 1}, {"error", 1}, {"warn", 2}, {"info", 3}, {"", 3}, {"debug", 4}, {" DEBUG ", 4}} {
		var out bytes.Buffer
		logger, err := NewLogger(&out, tc.level)
		if err != nil {
			t.Fatal(err)
		}
		logger.Debug("debug")
		logger.Info("info")
		logger.Warn("warn")
		logger.Error("error")
		if len(logEvents(t, out.Bytes())) != tc.want {
			t.Fatal("threshold", tc.level)
		}
	}
	for _, value := range []string{"trace", "PRIVATE-secret", "off", "info\nPRIVATE"} {
		if _, err := NewLogger(io.Discard, value); err == nil || strings.Contains(err.Error(), "PRIVATE") {
			t.Fatal("invalid level not rejected safely")
		}
		if (Config{Listen: "127.0.0.1:0", LogLevel: value}).Validate() == nil {
			t.Fatal("configuration accepted invalid level")
		}
	}
}

type activityAuth struct {
	session core.Session
	err     error
}

func (a activityAuth) Authenticate(ctx context.Context, _, _ string) (core.Session, error) {
	core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: "Auth", Result: core.ActivityOutcome(a.err), Duration: time.Millisecond})
	return a.session, a.err
}

type activitySession struct{ core.Session }

func (s activitySession) Close(ctx context.Context) error {
	core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: "EndSession", Result: core.ActivitySuccess})
	return s.Session.Close(ctx)
}
func activityFixture(t *testing.T) core.Session {
	t.Helper()
	box, err := memory.New().Open(context.Background(), core.Scope{Backend: "synthetic", Account: "one"})
	if err != nil {
		t.Fatal(err)
	}
	return activitySession{&importProbeSession{box: box}}
}

const activitySOAP = `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/"><s:Body><m:GetFolder xmlns:m="http://schemas.microsoft.com/exchange/services/2006/messages"><m:FolderIds><t:DistinguishedFolderId xmlns:t="http://schemas.microsoft.com/exchange/services/2006/types" Id="PRIVATE-unknown-folder"/></m:FolderIds></m:GetFolder></s:Body></s:Envelope>`

func TestOperationalHTTPSLogsAreIndependentOfLaboratoryFlags(t *testing.T) {
	session := activityFixture(t)
	for _, level := range []string{"none", "err", "warn", "info", "debug"} {
		t.Run(level, func(t *testing.T) {
			var out bytes.Buffer
			logger, _ := NewLogger(&out, level)
			namespace, _ := mapi.NewNamespace("8f86be35-8131-4aab-8d63-7f23212d0885")
			transport := mapi.NewSessionTransport(namespace)
			transport.EnableRAMStore()
			transport.EnableContentsDownloadSpike() // Expose the ROP without enabling diagnostic tracing.
			mailbox, _ := namespace.Mailbox(session.Identity())
			mux := http.NewServeMux()
			mux.Handle("/EWS/Exchange.asmx", core.BasicTLS(activityAuth{session: session}, ews.New().Serve))
			mux.Handle("/mapi/nspi/", core.BasicTLS(activityAuth{session: session}, transport.Directory))
			mux.Handle("/mapi/emsmdb/", core.BasicTLS(activityAuth{session: session}, transport.Mailbox))
			mux.Handle("/failure", core.BasicTLS(activityAuth{err: errors.New("PRIVATE-backend-error")}, func(http.ResponseWriter, *http.Request, core.Session) { t.Error("failed auth entered handler") }))
			mux.Handle("/rejected", core.BasicTLS(activityAuth{err: core.ErrAuthentication}, func(http.ResponseWriter, *http.Request, core.Session) { t.Error("failed auth entered handler") }))
			done := make(chan struct{}, 1)
			traced := operationalTrace(logger, mux)
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { traced.ServeHTTP(w, r); done <- struct{}{} }))
			defer server.Close()
			var cookies []*http.Cookie
			call := func(path, op string, data []byte) ([]map[string]any, *http.Response) {
				t.Helper()
				out.Reset()
				request, _ := http.NewRequest("POST", server.URL+path, bytes.NewReader(data))
				request.SetBasicAuth("PRIVATE-attempted-alias", "PRIVATE-password")
				request.Header.Set("Content-Type", "application/mapi-http")
				request.Header.Set("X-RequestType", op)
				request.Header.Set("X-RequestId", "PRIVATE-request-id")
				request.Header.Set("Subject", "PRIVATE-subject")
				for _, cookie := range cookies {
					request.AddCookie(cookie)
				}
				response, err := server.Client().Do(request)
				if err != nil {
					t.Fatal("HTTPS request failed")
				}
				_, _ = io.Copy(io.Discard, response.Body)
				response.Body.Close()
				<-done
				if strings.Contains(out.String(), "PRIVATE") {
					t.Fatal("private request/error data escaped")
				}
				events := logEvents(t, out.Bytes())
				if len(events) > 0 {
					id := events[0]["request_id"]
					if len(id.(string)) != 32 {
						t.Fatal("missing generated correlation")
					}
					for _, e := range events {
						if e["request_id"] != id {
							t.Fatal("uncorrelated steps")
						}
					}
				}
				return events, response
			}
			assert := func(events []map[string]any, wantLevel, operation, result, account string) {
				t.Helper()
				threshold, _ := ParseLogLevel(level)
				expected, _ := ParseLogLevel(wantLevel)
				if threshold > expected {
					if len(events) != 0 {
						t.Fatal("event below threshold")
					}
					return
				}
				if len(events) == 0 {
					t.Fatal("missing event")
				}
				event := events[0]
				if event["msg"] != "user_operation" || event["level"] != strings.ToUpper(wantLevel) || event["operation"] != operation || event["result"] != result || event["account"] != account {
					t.Fatalf("incorrect operation/result attribution: %v", event)
				}
				if level == "debug" {
					if len(events) < 2 {
						t.Fatal("missing debug steps")
					}
					for _, e := range events[1:] {
						if e["level"] != "DEBUG" || e["account"] != account {
							t.Fatal("step identity/level")
						}
					}
				} else if len(events) != 1 {
					t.Fatal("debug steps below threshold")
				}
			}
			events, _ := call("/EWS/Exchange.asmx", "PRIVATE-header", []byte(activitySOAP))
			assert(events, "warn", "GetFolder", "rejected", session.Identity().Name)
			for _, operation := range strings.Fields("FindItem CreateItem MoveItem CopyItem DeleteItem CreateFolder UpdateFolder MoveFolder DeleteFolder CreateAttachment DeleteAttachment") {
				body := `<s:Envelope xmlns:s="http://schemas.xmlsoap.org/soap/envelope/" xmlns:m="http://schemas.microsoft.com/exchange/services/2006/messages"><s:Body><m:` + operation + `/></s:Body></s:Envelope>`
				events, _ = call("/EWS/Exchange.asmx", "PRIVATE-header", []byte(body))
				assert(events, "warn", operation, "rejected", session.Identity().Name)
			}
			events, _ = call("/mapi/nspi/?MailboxId="+mailbox.RoutingID, "PING", nil)
			assert(events, "info", "PING", "success", session.Identity().Name)
			connect, _ := (mapi.Connect{UserDN: mailbox.LegacyDN, CodePage: 1252, SortLCID: 1033, StringLCID: 1033}).Marshal()
			events, response := call("/mapi/emsmdb/?MailboxId="+mailbox.RoutingID, "Connect", connect)
			assert(events, "info", "Connect", "success", session.Identity().Name)
			cookies = response.Cookies()
			rop, _ := (mapi.ROPBuffer{Operations: []byte{1, 0, 0}, Handles: []uint32{0xffffffff}}).Marshal()
			execute, _ := (mapi.Execute{ROP: rop, MaxOutput: 4096}).Marshal()
			events, response = call("/mapi/emsmdb/?MailboxId="+mailbox.RoutingID, "Execute", execute)
			cookies = response.Cookies()
			assert(events, "info", "Execute", "success", session.Identity().Name)
			if len(events) > 0 {
				ops := events[0]["operations"].([]any)
				if len(ops) != 1 || ops[0] != "release" {
					t.Fatal("ROP logging still depends on lab flags")
				}
			}
			rop, _ = (mapi.ROPBuffer{Operations: []byte{0x66, 0, 0, 0, 1, 0, 0}, Handles: []uint32{0xffffffff}}).Marshal()
			execute, _ = (mapi.Execute{ROP: rop, MaxOutput: 4096}).Marshal()
			events, _ = call("/mapi/emsmdb/?MailboxId="+mailbox.RoutingID, "Execute", execute)
			assert(events, "warn", "Execute", "rejected", session.Identity().Name)
			if len(events) > 0 {
				ops := events[0]["operations"].([]any)
				if len(ops) != 1 || ops[0] != "set_read_flags" {
					t.Fatal("missing named read-state ROP in the request summary")
				}
			}
			if level == "debug" {
				var found bool
				for _, event := range events[1:] {
					if event["layer"] == "rop" && event["operation"] == "set_read_flags" &&
						event["rop_id"] == "0x66" && event["rop_return_value"] == "0x000004b9" {
						found = true
					}
				}
				if !found {
					t.Fatal("missing read-state ROP and return value at debug")
				}
			}
			events, _ = call("/failure", "PRIVATE-header", nil)
			assert(events, "error", "request", "failure", "unauthenticated")
			events, _ = call("/rejected", "PRIVATE-header", nil)
			assert(events, "warn", "request", "authentication", "unauthenticated")
		})
	}
}

func TestActivityRetentionIsBoundedAndClosed(t *testing.T) {
	a := &requestActivity{operations: make(map[string]bool)}
	a.Authenticated("PRIVATE\nidentity")
	if a.account != "unavailable" {
		t.Fatal("invalid canonical identity accepted")
	}
	a.Authenticated("canonical@example.invalid")
	var wg sync.WaitGroup
	for range 16 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range 64 {
				a.Record(core.Activity{Layer: "PRIVATE-layer", Operation: "PRIVATE-operation", Result: core.ActivitySuccess, Operations: []string{"release", "PRIVATE-value"}})
			}
		}()
	}
	wg.Wait()
	a.Record(core.Activity{Layer: "ews", Operation: "GetItem", Result: core.ActivityDenied})
	if len(a.events) != 128 || a.dropped != 897 || a.result != core.ActivityDenied || a.operation != "GetItem" {
		t.Fatal("bounded details lost the final semantic failure")
	}
	raw, _ := json.Marshal(a.events)
	if strings.Contains(string(raw), "PRIVATE") {
		t.Fatal("operation vocabulary was not enforced")
	}
}

func TestLaboratoryTraceUsesDebugInServiceComposition(t *testing.T) {
	for _, level := range []string{"none", "info", "debug"} {
		var out bytes.Buffer
		logger, _ := NewLogger(&out, level)
		handler := operationalTrace(logger, detailedLaboratoryTraceAtLevel(logger, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { w.WriteHeader(204) }), slog.LevelDebug, laboratoryTraceOptions{}))
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://example.invalid/mapi/nspi/", nil))
		events := logEvents(t, out.Bytes())
		want := 0
		if level == "info" {
			want = 1
		}
		if level == "debug" {
			want = 2
		}
		if len(events) != want {
			t.Fatal("lab trace bypassed threshold")
		}
	}
}

func TestBackendCleanupFailureRaisesSeverityWithoutChangingClientResult(t *testing.T) {
	var out bytes.Buffer
	logger, _ := NewLogger(&out, "err")
	handler := operationalTrace(logger, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		core.RecordAuthenticated(r.Context(), "canonical@example.invalid")
		for range 140 {
			core.RecordActivity(r.Context(), core.Activity{Layer: "rop", Operation: "release"})
		}
		core.RecordActivity(r.Context(), core.Activity{Layer: "backend", Operation: "EndSession", Result: core.ActivityFailure})
		w.WriteHeader(204)
	}))
	handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("POST", "https://example.invalid/", nil))
	events := logEvents(t, out.Bytes())
	if len(events) != 1 || events[0]["level"] != "ERROR" || events[0]["result"] != "success" || events[0]["backend_failures"] != float64(1) || events[0]["detail_events_dropped"] != float64(13) {
		t.Fatal("cleanup failure hidden or client outcome changed")
	}
}

func TestOperationalTraceRecordsPanicWithoutPanicValue(t *testing.T) {
	var out bytes.Buffer
	logger, _ := NewLogger(&out, "err")
	handler := operationalTrace(logger, http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("PRIVATE-panic-value") }))
	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic unexpectedly consumed")
			}
		}()
		handler.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest("GET", "https://example.invalid/", nil))
	}()
	events := logEvents(t, out.Bytes())
	if len(events) != 1 || events[0]["completed"] != false || events[0]["result"] != "failure" || strings.Contains(out.String(), "PRIVATE") {
		t.Fatal("aborted request missing or leaked panic")
	}
}
