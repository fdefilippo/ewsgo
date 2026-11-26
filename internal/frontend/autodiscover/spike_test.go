// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package autodiscover

import (
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"ewsgo/internal/backend/memory"
	"ewsgo/internal/core"
)

func payload(address string) string {
	return `<Autodiscover xmlns="` + requestNS + `"><Request><EMailAddress>` + address + `</EMailAddress><AcceptableResponseSchema>` + responseNS + `</AcceptableResponseSchema></Request></Autodiscover>`
}

type session struct {
	box    core.Mailbox
	closed int
}

func (*session) Identity() core.Identity {
	return core.Identity{Scope: core.Scope{Backend: "synthetic", Account: "one"}, Name: "lab@example.invalid"}
}
func (s *session) Mailbox() core.Mailbox       { return s.box }
func (s *session) Close(context.Context) error { s.closed++; return nil }

type authenticator struct {
	session *session
	calls   int
}

func (a *authenticator) Authenticate(_ context.Context, user, password string) (core.Session, error) {
	a.calls++
	if user != "lab@example.invalid" || password != "synthetic" {
		return nil, core.ErrAuthentication
	}
	return a.session, nil
}

func TestDiagnosticAuthenticatesAndRejectsMailboxOverride(t *testing.T) {
	box, err := memory.New().Open(context.Background(), core.Scope{Backend: "synthetic", Account: "one"})
	if err != nil {
		t.Fatal(err)
	}
	auth := &authenticator{session: &session{box: box}}
	h := core.BasicTLS(auth, Spike)
	for _, tc := range []struct {
		address string
		login   bool
		code    int
	}{{"lab@example.invalid", false, 401}, {"lab@example.invalid", true, 501}, {"other@example.invalid", true, 403}} {
		r := httptest.NewRequest("POST", "https://example.invalid/autodiscover/autodiscover.xml", strings.NewReader(payload(tc.address)))
		r.Header.Set("Content-Type", "text/xml; charset=utf-8")
		if tc.login {
			r.SetBasicAuth("lab@example.invalid", "synthetic")
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Fatalf("status %d, want %d", w.Code, tc.code)
		}
		if w.Code == 501 {
			var result map[string]any
			if json.Unmarshal(w.Body.Bytes(), &result) != nil || result["nativeMailboxAccessible"] != true || result["outlookProfileCreated"] != false {
				t.Fatal("diagnostic faked an Outlook profile")
			}
			if strings.Contains(w.Body.String(), "example.invalid") || w.Header().Get("Cache-Control") != "no-store" {
				t.Fatal("private response or cache policy")
			}
		}
	}
	if auth.calls != 2 || auth.session.closed != 2 {
		t.Fatal("missing fresh authentication/session cleanup")
	}
}

func TestPOXParserBoundsAndSelectors(t *testing.T) {
	valid := payload("lab@example.invalid")
	if value, err := parsePOX([]byte(valid)); err != nil || value.Email != "lab@example.invalid" {
		t.Fatal("valid POX request rejected")
	}
	for _, invalid := range []string{
		valid + valid,
		`<!DOCTYPE x [<!ENTITY x SYSTEM "file:///etc/passwd">]>` + valid,
		strings.Replace(valid, "<EMailAddress>", "<EMailAddress><Nested/>", 1),
		strings.Replace(valid, "</EMailAddress>", "</EMailAddress><EMailAddress>other@example.invalid</EMailAddress>", 1),
		strings.ReplaceAll(valid, requestNS, "urn:foreign"),
		strings.ReplaceAll(valid, responseNS, "https://foreign.invalid"),
		strings.Replace(valid, "</Request>", "<LegacyDN>private DN</LegacyDN></Request>", 1),
		strings.Replace(valid, "lab@example.invalid", strings.Repeat("x", 321), 1),
	} {
		if _, err := parsePOX([]byte(invalid)); err == nil {
			t.Fatal("unsupported or unsafe POX request accepted")
		}
	}
	legacy := httptest.NewRequest("POST", "https://example.invalid", strings.NewReader(strings.ReplaceAll(valid, "EMailAddress", "LegacyDN")))
	legacy.Header.Set("Content-Type", "text/xml")
	legacyResponse := httptest.NewRecorder()
	Spike(legacyResponse, legacy, nil)
	if legacyResponse.Code != 400 {
		t.Fatal("legacy selector accepted by old authentication probe")
	}
	r := httptest.NewRequest("POST", "https://example.invalid", strings.NewReader(strings.Repeat("x", 16385)))
	r.Header.Set("Content-Type", "text/xml")
	w := httptest.NewRecorder()
	Spike(w, r, nil)
	if w.Code != 413 {
		t.Fatal("body limit missing")
	}
}
