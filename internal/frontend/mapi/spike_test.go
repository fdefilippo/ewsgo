// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"ewsgo/internal/backend/memory"
	"ewsgo/internal/core"
	"net/http/httptest"
	"testing"
)

type testSession struct{ box core.Mailbox }

func (*testSession) Identity() core.Identity {
	return core.Identity{Scope: core.Scope{Backend: "synthetic", Account: "one"}, Name: "lab@example.invalid"}
}
func (s *testSession) Mailbox() core.Mailbox     { return s.box }
func (*testSession) Close(context.Context) error { return nil }
func TestSpikeNeverReportsMailboxSuccess(t *testing.T) {
	box, err := memory.New().Open(context.Background(), core.Scope{Backend: "synthetic", Account: "one"})
	if err != nil {
		t.Fatal(err)
	}
	connect, _ := (Connect{UserDN: "/o=EWSGo/cn=Synthetic"}).Marshal()
	for _, test := range []struct {
		name, method, content, operation, id string
		body                                 []byte
		status                               int
	}{
		{"parsed but unimplemented", "POST", "application/mapi-http", "Connect", "one", connect, 501},
		{"malformed", "POST", "application/mapi-http", "Connect", "one", connect[:2], 400},
		{"unknown operation", "POST", "application/mapi-http", "Bind", "one", nil, 501},
		{"wrong content", "POST", "text/plain", "Connect", "one", connect, 415},
		{"missing ID", "POST", "application/mapi-http", "Connect", "", connect, 400},
		{"method", "GET", "application/mapi-http", "Connect", "one", nil, 405},
		{"bounded body", "POST", "application/mapi-http", "Connect", "one", make([]byte, MaxBody+1), 413},
	} {
		t.Run(test.name, func(t *testing.T) {
			r := httptest.NewRequest(test.method, "https://example.invalid/mapi/emsmdb/", bytes.NewReader(test.body))
			r.Header.Set("Content-Type", test.content)
			r.Header.Set("X-RequestType", test.operation)
			r.Header.Set("X-RequestId", test.id)
			w := httptest.NewRecorder()
			Spike(w, r, &testSession{box})
			if w.Code != test.status {
				t.Fatal(w.Code)
			}
			if len(w.Header().Values("Set-Cookie")) != 0 {
				t.Fatal("fake session issued")
			}
		})
	}
}
