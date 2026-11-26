// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"bytes"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestHTTPServerErrorsHonorThresholdAndExcludePrivateValues(t *testing.T) {
	for _, level := range []string{"none", "info", "debug"} {
		t.Run(level, func(t *testing.T) {
			var out bytes.Buffer
			logger, _ := NewLogger(&out, level)
			server := httptest.NewUnstartedServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("PRIVATE-panic-request-value") }))
			server.Config.ErrorLog = httpServerLogger(logger)
			server.StartTLS()
			defer server.Close()
			response, err := server.Client().Get(server.URL + "/PRIVATE-path")
			if response != nil {
				response.Body.Close()
			}
			if err == nil {
				t.Fatal("panic unexpectedly returned a successful response")
			}
			conn, err := net.DialTimeout("tcp", server.Listener.Addr().String(), time.Second)
			if err != nil {
				t.Fatal("TLS error fixture unavailable")
			}
			_ = conn.SetDeadline(time.Now().Add(2 * time.Second))
			_, _ = io.WriteString(conn, "GET /PRIVATE-request HTTP/1.0\r\nHost: PRIVATE-host\r\n\r\n")
			_, _ = io.Copy(io.Discard, conn)
			conn.Close()
			server.Close()
			if strings.Contains(out.String(), "PRIVATE") || strings.Contains(out.String(), "127.0.0.1") {
				t.Fatal("HTTP server leaked raw error/peer details")
			}
			events := logEvents(t, out.Bytes())
			if level == "none" {
				if len(events) != 0 {
					t.Fatal("standard HTTP server bypassed none")
				}
				return
			}
			if len(events) != 2 {
				t.Fatal("HTTP server diagnostics missing")
			}
			seen := map[string]bool{}
			for _, event := range events {
				if event["msg"] != "http_server_error" || event["level"] != "ERROR" || len(event) != 4 {
					t.Fatal("HTTP server error schema is not closed")
				}
				seen[event["failure"].(string)] = true
			}
			if !seen["panic"] || !seen["tls_handshake"] {
				t.Fatal("HTTP server categories incorrect")
			}
		})
	}
}
