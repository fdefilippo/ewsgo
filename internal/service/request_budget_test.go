// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"
)

func TestRequestBudgetSeparatesUploadOperationAndResponse(t *testing.T) {
	for _, tc := range []struct {
		name      string
		upload    time.Duration
		operation time.Duration
		status    int
	}{
		{"slow valid upload and backend", 120 * time.Millisecond, 120 * time.Millisecond, http.StatusOK},
		{"upload deadline", 350 * time.Millisecond, 0, http.StatusRequestTimeout},
		{"operation deadline", 0, time.Second, http.StatusGatewayTimeout},
	} {
		t.Run(tc.name, func(t *testing.T) {
			const response = "protocol reply"
			selectBudget := func(*http.Request) requestBudget {
				return requestBudget{250 * time.Millisecond, 500 * time.Millisecond, 300 * time.Millisecond}
			}
			server := httptest.NewUnstartedServer(withRequestBudget(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, err := io.ReadAll(r.Body)
				if errors.Is(err, os.ErrDeadlineExceeded) {
					http.Error(w, "upload expired", http.StatusRequestTimeout)
					return
				}
				if err != nil || string(body) != "test upload" {
					http.Error(w, "invalid request body", http.StatusBadRequest)
					return
				}
				select {
				case <-time.After(tc.operation):
				case <-r.Context().Done():
					if !errors.Is(r.Context().Err(), context.DeadlineExceeded) {
						t.Errorf("operation cancelled without its deadline: %v", r.Context().Err())
					}
					w.WriteHeader(http.StatusGatewayTimeout)
				}
				_, _ = io.WriteString(w, response)
			}), selectBudget))
			server.Config.ReadTimeout = 75 * time.Millisecond
			server.Config.WriteTimeout = 100 * time.Millisecond
			server.StartTLS()
			defer server.Close()
			reader, writer := io.Pipe()
			go func() {
				defer writer.Close()
				time.Sleep(tc.upload)
				_, _ = io.WriteString(writer, "test upload")
			}()
			r, err := http.NewRequestWithContext(t.Context(), http.MethodPost, server.URL+"/EWS/Exchange.asmx", reader)
			if err != nil {
				t.Fatal(err)
			}
			r.ContentLength = int64(len("test upload"))
			res, err := server.Client().Do(r)
			if err != nil {
				t.Fatal(err)
			}
			defer res.Body.Close()
			payload, err := io.ReadAll(res.Body)
			if err != nil || res.StatusCode != tc.status ||
				tc.status != http.StatusRequestTimeout && string(payload) != response {
				t.Fatalf("HTTP %d, body %q, err %v", res.StatusCode, payload, err)
			}
		})
	}
}
