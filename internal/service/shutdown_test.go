// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"testing"
	"time"
)

func TestHTTPShutdownDrainsRequests(t *testing.T) {
	for _, tc := range []struct {
		name          string
		grace         time.Duration
		finishRequest bool
	}{
		{"completed within grace period", 3 * time.Second, true},
		{"canceled at grace deadline", 50 * time.Millisecond, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx, deadline := context.WithTimeout(t.Context(), 6*time.Second)
			defer deadline()
			stop, cancel := context.WithCancel(ctx)
			defer cancel()
			listener, err := (&net.ListenConfig{}).Listen(ctx, "tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer listener.Close()
			started, draining, finish := make(chan struct{}), make(chan struct{}), make(chan struct{})
			handlerDone := make(chan error, 1)
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				close(started)
				select {
				case <-r.Context().Done():
					handlerDone <- r.Context().Err()
				case <-finish:
					_, err := io.WriteString(w, "complete")
					handlerDone <- err
				}
			})}
			server.RegisterOnShutdown(func() { close(draining) })
			served := make(chan error, 1)
			go func() { served <- serveHTTP(stop, server, listener, tc.grace) }()

			req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://"+listener.Addr().String(), nil)
			if err != nil {
				t.Fatal(err)
			}
			received := make(chan error, 1)
			go func() {
				resp, err := http.DefaultClient.Do(req)
				if err == nil {
					defer resp.Body.Close()
					var body []byte
					body, err = io.ReadAll(resp.Body)
					if err == nil && (resp.StatusCode != http.StatusOK || string(body) != "complete") {
						err = fmt.Errorf("unexpected response: %d %q", resp.StatusCode, body)
					}
				}
				received <- err
			}()
			select {
			case <-started:
			case <-ctx.Done():
				t.Fatal("HTTP handler did not start")
			}
			cancel()
			select {
			case <-draining:
			case <-ctx.Done():
				t.Fatal("HTTP drain did not start")
			}
			conn, err := (&net.Dialer{}).DialContext(ctx, "tcp", listener.Addr().String())
			if err == nil {
				conn.Close()
				t.Fatal("listener accepted a new connection during drain")
			}
			if tc.finishRequest {
				close(finish)
			}
			if err := <-handlerDone; (tc.finishRequest && err != nil) || (!tc.finishRequest && !errors.Is(err, context.Canceled)) {
				t.Fatalf("unexpected handler result: %v", err)
			}
			if err := <-served; (tc.finishRequest && err != nil) || (!tc.finishRequest && (err == nil || err.Error() != "graceful shutdown timed out")) {
				t.Fatalf("unexpected server result: %v", err)
			}
			if err := <-received; (err == nil) != tc.finishRequest {
				t.Fatalf("unexpected client result: %v", err)
			}
		})
	}
}
