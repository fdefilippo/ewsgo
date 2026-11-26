// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package autodiscover

import (
	"context"
	"encoding/xml"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"ewsgo/internal/backend/memory"
	"ewsgo/internal/core"
	"ewsgo/internal/frontend/mapi"
)

const deployment = "8f86be35-8131-4aab-8d63-7f23212d0885"

func TestProfileXMLUsesConfiguredOriginAndStableIdentity(t *testing.T) {
	n, _ := mapi.NewNamespace(deployment)
	p, err := NewProfile("https://gateway.invalid/", n)
	if err != nil {
		t.Fatal(err)
	}
	box, _ := memory.New().Open(context.Background(), core.Scope{Backend: "synthetic", Account: "one"})
	s := &session{box: box}
	auth := &authenticator{session: s}
	h := core.BasicTLS(auth, p.Serve)
	mailbox, _ := n.Mailbox(s.Identity())
	var previous string
	for _, selector := range []string{payload("lab@example.invalid"), strings.ReplaceAll(payload(mailbox.LegacyDN), "EMailAddress", "LegacyDN")} {
		r := httptest.NewRequest("POST", "https://poison.invalid/autodiscover/autodiscover.xml", strings.NewReader(selector))
		r.Host = "poison.invalid"
		r.Header.Set("Forwarded", "host=poison.invalid;proto=http")
		r.Header.Set("X-Forwarded-Host", "poison.invalid")
		r.Header.Set("Content-Type", "text/xml")
		r.Header.Set("X-MapiHttpCapability", "1")
		r.SetBasicAuth("lab@example.invalid", "synthetic")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		var parsed profileResponse
		if w.Code != 200 || xml.Unmarshal(w.Body.Bytes(), &parsed) != nil || parsed.XMLName.Space != "http://schemas.microsoft.com/exchange/autodiscover/responseschema/2006" {
			t.Fatalf("invalid profile XML: status %d", w.Code)
		}
		decoder := xml.NewDecoder(strings.NewReader(w.Body.String()))
		for {
			token, err := decoder.Token()
			if err != nil {
				break
			}
			if start, ok := token.(xml.StartElement); ok && start.Name.Local != "Autodiscover" && start.Name.Space != responseNS {
				t.Fatal("response child has wrong namespace")
			}
		}
		if previous != "" && previous != w.Body.String() {
			t.Fatal("selectors produce different identities")
		}
		previous = w.Body.String()
		if parsed.Response.User.LegacyDN != mailbox.LegacyDN || parsed.Response.User.Deployment != deployment || parsed.Response.User.SMTP != s.Identity().Name || parsed.Response.Account.Type != "email" || parsed.Response.Account.Action != "settings" {
			t.Fatal("identity/profile mismatch")
		}
		protocol := parsed.Response.Account.Protocol
		if protocol.Type != "mapiHttp" || protocol.Version != "1" {
			t.Fatal("wrong protocol")
		}
		for i, endpoint := range []endpointURLs{protocol.MailStore, protocol.AddressBook} {
			u, err := url.Parse(endpoint.Internal)
			expected := []string{"/mapi/emsmdb/", "/mapi/nspi/"}[i]
			if err != nil || u.Scheme != "https" || u.Host != "gateway.invalid" || u.Path != expected || u.Query().Get("MailboxId") != mailbox.RoutingID || endpoint.Internal != endpoint.External {
				t.Fatal("endpoint mismatch or Host poisoning")
			}
		}
		if strings.Contains(w.Body.String(), "poison.invalid") || w.Header().Get("Cache-Control") != "no-store" || len(w.Header().Values("Set-Cookie")) != 0 {
			t.Fatal("untrusted origin, cache or fake session")
		}
	}
	if auth.calls != 2 || s.closed != 2 {
		t.Fatal("native sessions not closed")
	}
}

func TestProfileRejectsCrossMailboxSelectorsAndMissingCapability(t *testing.T) {
	n, _ := mapi.NewNamespace(deployment)
	p, _ := NewProfile("https://gateway.invalid", n)
	// No mailbox: every rejected request must stop before accessing native data.
	s := &session{}
	for _, tc := range []struct {
		body, capability string
		status           int
	}{
		{payload("other@example.invalid"), "1", 403},
		{strings.ReplaceAll(payload("/o=Other/cn=Another"), "EMailAddress", "LegacyDN"), "1", 403},
		{payload("lab@example.invalid"), "", 501},
		{payload("lab@example.invalid"), "0", 501},
		{payload("lab@example.invalid"), "1,1", 501},
		{strings.Replace(payload("lab@example.invalid"), "</Request>", "<LegacyDN>/o=Other</LegacyDN></Request>", 1), "1", 400},
		{strings.ReplaceAll(payload(""), "EMailAddress", "LegacyDN"), "1", 400},
		{`<!DOCTYPE x>` + payload("lab@example.invalid"), "1", 400},
		{strings.Repeat("x", 16385), "1", 413},
	} {
		r := httptest.NewRequest("POST", "https://gateway.invalid", strings.NewReader(tc.body))
		r.Header.Set("Content-Type", "text/xml")
		if tc.capability != "" {
			r.Header.Set("X-MapiHttpCapability", tc.capability)
		}
		w := httptest.NewRecorder()
		p.Serve(w, r, s)
		if w.Code != tc.status {
			t.Fatalf("got %d want %d", w.Code, tc.status)
		}
	}
}

func TestProfileNegotiatesSupportedVersion(t *testing.T) {
	n, _ := mapi.NewNamespace(deployment)
	p, _ := NewProfile("https://gateway.invalid", n)
	box, _ := memory.New().Open(context.Background(), core.Scope{Backend: "synthetic", Account: "one"})
	s := &session{box: box}
	var previous string
	for _, capability := range []string{"1", "2", "2147483648", "18446744073709551616", "0001", " +02\t"} {
		r := httptest.NewRequest("POST", "https://gateway.invalid", strings.NewReader(payload("lab@example.invalid")))
		r.Header.Set("Content-Type", "text/xml")
		r.Header.Set("X-MapiHttpCapability", capability)
		w := httptest.NewRecorder()
		p.Serve(w, r, s)
		var parsed profileResponse
		if w.Code != 200 || xml.Unmarshal(w.Body.Bytes(), &parsed) != nil || parsed.Response.Account.Protocol.Version != "1" {
			t.Fatalf("capability %q did not negotiate v1: status %d", capability, w.Code)
		}
		if previous != "" && previous != w.Body.String() {
			t.Fatal("capability changed the profile instead of negotiating v1")
		}
		previous = w.Body.String()
	}
}

func TestProfileRejectsInvalidOrAmbiguousCapability(t *testing.T) {
	n, _ := mapi.NewNamespace(deployment)
	p, _ := NewProfile("https://gateway.invalid", n)
	// No mailbox: rejection must occur before native data access.
	s := &session{}
	for _, values := range [][]string{nil, {""}, {" "}, {"0"}, {"000"}, {"-1"}, {"1.0"}, {"1e2"}, {"1,2"}, {"1", "2"}, {"1", "1"}, {"+"}, {"++1"}, {"1 2"}, {"\u0661"}} {
		r := httptest.NewRequest("POST", "https://gateway.invalid", strings.NewReader(payload("lab@example.invalid")))
		r.Header.Set("Content-Type", "text/xml")
		for _, value := range values {
			r.Header.Add("X-MapiHttpCapability", value)
		}
		w := httptest.NewRecorder()
		p.Serve(w, r, s)
		if w.Code != 501 {
			t.Fatalf("invalid capability %q accepted: status %d", values, w.Code)
		}
	}
}

func TestProfileOriginValidation(t *testing.T) {
	n, _ := mapi.NewNamespace(deployment)
	for _, origin := range []string{"http://gateway.invalid", "https://user:secret@gateway.invalid", "https://gateway.invalid/path", "https://gateway.invalid?", "https://gateway.invalid?next=evil", "https://gateway.invalid#frag", "https://gateway.invalid:0", "https://gateway.invalid:65536", "https://", "https://gateway.invalid/%2f"} {
		if _, err := NewProfile(origin, n); err == nil {
			t.Fatal("unsafe public origin accepted")
		}
	}
	if _, err := NewProfile("https://gateway.invalid", mapi.Namespace{}); err == nil {
		t.Fatal("missing namespace accepted")
	}
}
