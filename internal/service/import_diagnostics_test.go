// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/backend/memory"
	"ewsgo/internal/core"
	"ewsgo/internal/frontend/mapi"
)

type importProbeSession struct{ box core.Mailbox }

func (s *importProbeSession) Identity() core.Identity {
	return core.Identity{Scope: core.Scope{Backend: "synthetic", Account: "one"}, Name: "synthetic@example.invalid"}
}
func (s *importProbeSession) Mailbox() core.Mailbox     { return s.box }
func (*importProbeSession) Close(context.Context) error { return nil }
func (s *importProbeSession) Authenticate(context.Context, string, string) (core.Session, error) {
	return s, nil
}

func TestImportDiagnosticsHTTPSLogGates(t *testing.T) {
	box, err := memory.New().Open(context.Background(), core.Scope{Backend: "synthetic", Account: "one"})
	if err != nil {
		t.Fatal("fixture unavailable")
	}
	session := &importProbeSession{box: box}
	runImportDiagnosticsHTTPS(t, session, session.Identity(), "synthetic", "correct")
}

// Opt-in: a separate loopback TLS gateway with native authentication. Only
// Connect reads the root-folder metadata; all Execute batches are rejected
// during decoding, before any prefix, mailbox operation or payload capture.
func runImportDiagnosticsHTTPS(t *testing.T, auth core.Authenticator, identity core.Identity, user, password string) (map[string]bool, int) {
	t.Helper()
	checks := map[string]bool{}
	namespace, err := mapi.NewNamespace("8f86be35-8131-4aab-8d63-7f23212d0885")
	if err != nil {
		t.Fatal("invalid test namespace")
	}
	mailbox, err := namespace.Mailbox(identity)
	if err != nil {
		t.Fatal("mailbox mapping failed; details suppressed")
	}
	requests := 0
	for _, transportTrace := range []bool{false, true} {
		for _, logTrace := range []bool{false, true} {
			mode := "transport_off_log_off"
			if transportTrace {
				mode = "transport_on_log_off"
			}
			if logTrace {
				mode = strings.TrimSuffix(mode, "off") + "on"
			}
			transport := mapi.NewSessionTransport(namespace)
			transport.EnableRAMStore()
			transport.EnableHierarchyDownloadSpike()
			transport.EnableContentsDownloadSpike()
			transport.EnableFAICollectorSpike()
			if transportTrace {
				transport.EnableSynchronizationOptionsTrace()
			}
			var logs bytes.Buffer
			logger := slog.New(slog.NewJSONHandler(&logs, nil))
			// Synchronize completion of the log callback before examining/resetting
			// the buffer. Reading the full HTTP body alone is not that barrier.
			completed := make(chan struct{}, 1)
			handler := detailedLaboratoryTrace(logger, core.BasicTLS(auth, transport.Mailbox), laboratoryTraceOptions{true, true, true, logTrace, true, true, true})
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { handler.ServeHTTP(w, r); completed <- struct{}{} }))
			defer server.Close()
			client := server.Client()
			defer client.CloseIdleConnections()
			client.Timeout = 20 * time.Second
			client.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
			var cookies []*http.Cookie
			call := func(op string, data []byte, withContext bool) ([]byte, map[string]any) {
				t.Helper()
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, "POST", server.URL+"/mapi/emsmdb/?MailboxId="+mailbox.RoutingID, bytes.NewReader(data))
				if err != nil {
					t.Fatal("invalid probe request")
				}
				req.Header.Set("Content-Type", "application/mapi-http")
				req.Header.Set("X-RequestId", "synthetic-import-probe:1")
				req.Header.Set("X-RequestType", op)
				req.SetBasicAuth(user, password)
				if withContext {
					for _, cookie := range cookies {
						req.AddCookie(cookie)
					}
				}
				response, err := client.Do(req)
				if err != nil {
					t.Fatal("probe HTTPS failed; details suppressed")
				}
				raw, readErr := io.ReadAll(io.LimitReader(response.Body, mapi.MaxBody+1))
				response.Body.Close()
				select {
				case <-completed:
				case <-ctx.Done():
					t.Fatal("probe log completion timed out")
				}
				requests++
				if readErr != nil || len(raw) > mapi.MaxBody || response.StatusCode != 200 {
					t.Fatal("unexpected probe HTTP response; details suppressed")
				}
				var event map[string]any
				if json.Unmarshal(logs.Bytes(), &event) != nil {
					t.Fatal("probe did not emit one JSON event")
				}
				text := logs.String()
				logs.Reset()
				if strings.Contains(text, "PRIVATE") || strings.Contains(text, password) || strings.Contains(text, user) || strings.Contains(text, mailbox.RoutingID) {
					t.Fatal("private request data reached diagnostic log")
				}
				schema := 55
				if logTrace {
					schema = 69
				}
				if len(event) != schema {
					t.Fatalf("unexpected log field count: %d, want %d", len(event), schema)
				}
				if op == "Execute" && !withContext {
					if response.Header.Get("X-ResponseCode") != "13" {
						t.Fatal("invalid context was not rejected before decoding")
					}
					return nil, event
				}
				if response.Header.Get("X-ResponseCode") != "0" {
					t.Fatal("probe transport rejected request")
				}
				cookies = response.Cookies()
				parts := bytes.SplitN(raw, []byte("\r\n\r\n"), 2)
				if len(parts) != 2 {
					t.Fatal("invalid probe envelope")
				}
				return parts[1], event
			}
			connect, _ := (mapi.Connect{UserDN: mailbox.LegacyDN, CodePage: 1252, SortLCID: 1033, StringLCID: 1033}).Marshal()
			payload, _ := call("Connect", connect, false)
			if len(payload) < 8 || binary.LittleEndian.Uint32(payload[4:]) != 0 {
				t.Fatal("probe Connect failed")
			}
			for _, tc := range []struct {
				name, reason, operation string
				wire                    []byte
				code                    uint32
			}{
				{"truncated_normal", "none", "none", []byte{0x72, 0, 0, 0, 0, 4, 0}, 0x80070057},
				{"unknown_flags", "import_unknown_flags", "RopSynchronizationImportMessageChange", []byte{0x72, 0, 0, 0, 0x11, 4, 0}, mapi.NotSupported},
				{"property_count", "import_property_count_profile", "RopSynchronizationImportMessageChange", []byte{0x72, 0, 0, 0, 0x10, 5, 0}, mapi.NotSupported},
				{"hierarchy", "import_hierarchy_not_supported", "RopSynchronizationImportHierarchyChange", hierarchyImportWithUnsupportedType(), mapi.NotSupported},
				{"contents_table", "none", "RopGetContentsTable", []byte{0x05, 0, 0}, mapi.NotSupported},
				{"after_save", "commit_nonfinal_batch", "RopGetAddressTypes", []byte{0x0c, 0, 0, 0, 0x0a, 0x49, 0, 0}, mapi.NotSupported},
				{"truncated", "none", "none", []byte{0x72, 0, 0, 0, 0x10}, 0x80070057},
			} {
				// Rejection before the private tail means the complete operation
				// has not been validated; the reason names only that parser branch.
				batch := append([]byte{1, 0, 0}, tc.wire...)
				if tc.name != "truncated" && tc.name != "truncated_normal" && tc.name != "after_save" {
					batch = append(batch, []byte("PRIVATE-unparsed-import-tail")...)
				}
				rop, _ := (mapi.ROPBuffer{Operations: batch, Handles: []uint32{0xffffffff}}).Marshal()
				execute, _ := (mapi.Execute{ROP: rop, MaxOutput: 4096}).Marshal()
				body, event := call("Execute", execute, true)
				want, _ := mapi.ExecuteResponse(tc.code, nil, nil, 4096)
				if !bytes.Equal(body, want) || event["rop_unsupported"] != tc.operation {
					t.Fatalf("%s: probe diagnostic changed rejected response or label", tc.name)
				}
				if logTrace {
					reason := "none"
					if transportTrace {
						reason = tc.reason
					}
					if !reflect.DeepEqual(event["store_sync_profile_rejections"], []any{reason}) {
						t.Fatal("incorrect gated import reason")
					}
					for _, key := range []string{"store_sync_flags", "store_sync_send_options", "store_sync_extra_flags"} {
						if !reflect.DeepEqual(event[key], []any{"none"}) {
							t.Fatal("import implied Configure flags")
						}
					}
					if event["store_sync_restriction"] != "none" || event["store_sync_tag_count"] != "none" {
						t.Fatal("import implied Configure payload")
					}
				} else if _, ok := event["store_sync_profile_rejections"]; ok {
					t.Fatal("diagnostic emitted with logging gate off")
				}
				checks[mode+"_"+tc.name] = true
				if tc.name == "truncated_normal" {
					_, event = call("Execute", execute, false)
					if logTrace && !reflect.DeepEqual(event["store_sync_profile_rejections"], []any{"none"}) {
						t.Fatal("invalid context emitted import reason")
					}
					checks[mode+"_invalid_context_no_reason"] = true
				}
			}
			call("PING", nil, true)
			call("Disconnect", make([]byte, 4), true)
			checks[mode+"_context_survives_rejected_batches"] = true
			client.CloseIdleConnections()
			server.Close()
		}
	}
	return checks, requests
}

func hierarchyImportWithUnsupportedType() []byte {
	source := append([]byte{1}, make([]byte, 20)...)
	source = append(source, 1)
	wire := []byte{0x73, 0, 0, 6, 0}
	addBinary := func(tag uint32, value []byte) {
		wire = binary.LittleEndian.AppendUint32(wire, tag)
		wire = binary.LittleEndian.AppendUint16(wire, uint16(len(value)))
		wire = append(wire, value...)
	}
	addBinary(0x65e10102, nil)
	addBinary(0x65e00102, source)
	wire = binary.LittleEndian.AppendUint32(wire, 0x30080040)
	wire = binary.LittleEndian.AppendUint64(wire, 134000000000000000)
	addBinary(0x65e20102, source)
	addBinary(0x65e30102, append([]byte{22}, source...))
	wire = binary.LittleEndian.AppendUint32(wire, 0x3001001f)
	wire = append(wire, 'T', 0, 0, 0)
	wire = binary.LittleEndian.AppendUint16(wire, 1)
	return binary.LittleEndian.AppendUint32(wire, 0x667900fd)
}
