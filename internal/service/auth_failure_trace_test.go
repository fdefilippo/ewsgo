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
	"testing"
	"time"

	"ewsgo/internal/core"
)

type authFailureProbe struct{ err error }

func (a authFailureProbe) Authenticate(context.Context, string, string) (core.Session, error) {
	return nil, a.err
}

func TestAuthenticationFailureTraceHTTPSAndPrivacy(t *testing.T) {
	for _, tc := range []struct {
		name, failure, request string
		err                    error
		status                 int
		contextFailure         bool
		trace                  bool
	}{
		{"backend", "other", "active", errors.New("PRIVATE backend details"), 502, false, true},
		{"auth", "authentication", "active", errors.Join(errors.New("PRIVATE account"), core.ErrAuthentication), 401, false, true},
		{"denied", "access_denied", "active", core.ErrAccessDenied, 403, false, true},
		{"canceled", "canceled", "canceled", context.Canceled, 502, true, true},
		{"backend_deadline", "deadline_exceeded", "active", context.DeadlineExceeded, 502, false, true},
		{"request_deadline", "deadline_exceeded", "deadline_exceeded", context.DeadlineExceeded, 502, true, true},
		{"trace_off", "", "", errors.New("PRIVATE backend details"), 502, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var logs bytes.Buffer
			h := core.BasicTLS(authFailureProbe{tc.err}, func(http.ResponseWriter, *http.Request, core.Session) {
				t.Error("failed authentication entered protocol handler")
			})
			if tc.trace {
				h = laboratoryTrace(slog.New(slog.NewJSONHandler(&logs, nil)), h, false, false, false)
			}
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if tc.contextFailure {
					var ctx context.Context
					var cancel context.CancelFunc
					if tc.name == "canceled" {
						ctx, cancel = context.WithCancel(r.Context())
					} else {
						ctx, cancel = context.WithDeadline(r.Context(), time.Unix(1, 0))
					}
					cancel()
					r = r.WithContext(ctx)
				}
				h.ServeHTTP(w, r)
			}))
			defer server.Close()
			r, _ := http.NewRequest("POST", server.URL+"/mapi/nspi/?MailboxId=PRIVATE-selector", strings.NewReader("PRIVATE-body"))
			r.SetBasicAuth("PRIVATE-user", "PRIVATE-password")
			r.Header.Set("X-RequestType", "PING")
			r.Header.Set("Cookie", "PRIVATE-cookie")
			response, err := server.Client().Do(r)
			if err != nil {
				t.Fatal("HTTPS fixture request failed")
			}
			body, _ := io.ReadAll(response.Body)
			response.Body.Close()
			server.CloseClientConnections()
			if response.StatusCode != tc.status || response.Header.Get("X-ResponseCode") != "" || strings.Contains(string(body), "PRIVATE") {
				t.Fatal("diagnostic changed status, transport result or response privacy")
			}
			if !tc.trace {
				if logs.Len() != 0 {
					t.Fatal("diagnostic emitted outside lab opt-in")
				}
				return
			}
			var event map[string]any
			if json.Unmarshal(logs.Bytes(), &event) != nil || strings.Contains(logs.String(), "PRIVATE") || len(event) != 36 || event["authentication_failure"] != tc.failure || event["authentication_request_context"] != tc.request {
				t.Fatal("diagnostic leaked data or lost closed failure/context categories")
			}
		})
	}
}
