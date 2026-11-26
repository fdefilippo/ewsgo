// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"crypto/tls"
	"net/http/httptest"
	"strings"
	"testing"

	"ewsgo/internal/backend/memory"
	"ewsgo/internal/core"
)

type testSession struct {
	box    core.Mailbox
	closed int
}

func (*testSession) Identity() core.Identity {
	return core.Identity{Scope: core.Scope{Backend: "synthetic", Account: "one"}, Name: "lab@example.invalid"}
}
func (s *testSession) Mailbox() core.Mailbox       { return s.box }
func (s *testSession) Close(context.Context) error { s.closed++; return nil }

type testAuth struct {
	session *testSession
	calls   int
	fail    bool
}

func (a *testAuth) Authenticate(context.Context, string, string) (core.Session, error) {
	a.calls++
	if a.fail {
		return nil, core.ErrAuthentication
	}
	return a.session, nil
}
func TestTLSIdentityAndFreshAuthentication(t *testing.T) {
	provider := memory.New()
	box, err := provider.Open(context.Background(), core.Scope{Backend: "synthetic", Account: "one"})
	if err != nil {
		t.Fatal(err)
	}
	auth := &testAuth{session: &testSession{box: box}}
	handler := core.BasicTLS(auth, New().Serve)
	call := func(payload string, tlsOn, login bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://example.invalid/EWS/Exchange.asmx", strings.NewReader(payload))
		if tlsOn {
			r.TLS = &tls.ConnectionState{}
		} else {
			r.TLS = nil
		}
		if login {
			r.SetBasicAuth("lab@example.invalid", "synthetic")
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	payload := string(envelope(el(messages, "GetFolder", "", el(messages, "FolderIds", "", at(el(types, "DistinguishedFolderId", ""), "Id", "inbox")))))
	if w := call(payload, false, true); w.Code != 426 || auth.calls != 0 {
		t.Fatal("plaintext credentials reached backend")
	}
	if w := call(payload, true, false); w.Code != 401 || auth.calls != 0 {
		t.Fatal("missing credentials accepted")
	}
	auth.fail = true
	if w := call(payload, true, true); w.Code != 401 {
		t.Fatal("expired/rejected authentication accepted")
	}
	auth.fail = false
	for range 2 {
		if w := call(payload, true, true); w.Code != 200 {
			t.Fatal(w.Code)
		}
	}
	if auth.calls != 3 || auth.session.closed != 2 {
		t.Fatal("sessions were reused or not closed")
	}
	cross := strings.Replace(payload, `</DistinguishedFolderId>`, `<Mailbox xmlns="`+types+`"><EmailAddress>other@example.invalid</EmailAddress></Mailbox></DistinguishedFolderId>`, 1)
	if !strings.Contains(call(cross, true, true).Body.String(), "ErrorAccessDenied") {
		t.Fatal("mailbox override accepted")
	}
	if w := call(`<!DOCTYPE a [<!ENTITY x SYSTEM "file:///etc/passwd">]>`+payload, true, true); w.Code != 400 {
		t.Fatal("directive accepted")
	}
}
func TestOpaqueIDsBindAccountAndGeneration(t *testing.T) {
	h := New()
	scope := core.Scope{Backend: "synthetic", Account: "one"}
	value := h.encode(scope, "f:2")
	if id, err := h.decode(scope, value); err != nil || id != "f:2" {
		t.Fatal("round trip failed")
	}
	if _, err := h.decode(core.Scope{Backend: "synthetic", Account: "two"}, value); err == nil {
		t.Fatal("cross-account reference accepted")
	}
	if _, err := New().decode(scope, value); err == nil {
		t.Fatal("foreign generation accepted")
	}
}
