// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const fixtureToken = "synthetic-secret-token"
const fixtureMIME = "From: sender@example.invalid\r\nDate: Sun, 15 Nov 2026 12:00:00 +0100\r\nMessage-ID: <synthetic@example.invalid>\r\nContent-Type: text/html; charset=utf-8\r\n\r\n<p>Synthetic HTML</p>"

func testClient(t *testing.T, handler http.HandlerFunc, config Config) *Client {
	t.Helper()
	server := httptest.NewTLSServer(handler)
	t.Cleanup(server.Close)
	pool := x509.NewCertPool()
	pool.AddCert(server.Certificate())
	config.RootCAs = pool
	config.Origin = server.URL
	client, err := New(config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	return client
}
func response(w http.ResponseWriter, ns, payload string) {
	w.Header().Set("Content-Type", "application/soap+xml")
	fmt.Fprintf(w, `<s:Envelope xmlns:s="%s"><s:Body><r xmlns="%s">%s</r></s:Body></s:Envelope>`, soapNS, ns, payload)
}
func envelope(w http.ResponseWriter, payload string) {
	w.Header().Set("Content-Type", "application/soap+xml")
	fmt.Fprintf(w, `<s:Envelope xmlns:s="%s"><s:Body>%s</s:Body></s:Envelope>`, soapNS, payload)
}
func fault(w http.ResponseWriter, code string) {
	w.WriteHeader(500)
	envelope(w, `<s:Fault xmlns:s="`+soapNS+`"><s:Detail><Error xmlns="urn:zimbra"><Code>`+code+`</Code><Trace>PRIVATE SERVER DETAILS</Trace></Error></s:Detail></s:Fault>`)
}
func fakeZimbra(t *testing.T) http.HandlerFunc {
	t.Helper()
	var mu sync.Mutex
	revoked := false
	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if r.Method == http.MethodGet {
			cookie, err := r.Cookie("ZM_AUTH_TOKEN")
			if err != nil || cookie.Value != fixtureToken || r.URL.Query().Get("authToken") != "" {
				t.Error("REST credentials were not confined to the cookie")
				w.WriteHeader(403)
				return
			}
			io.WriteString(w, fixtureMIME)
			return
		}
		raw, _ := io.ReadAll(r.Body)
		root, err := parseXML(raw)
		if err != nil {
			t.Error("invalid request envelope")
			w.WriteHeader(400)
			return
		}
		body := root.child(soapNS, "Body")
		if body == nil || len(body.Children) != 1 {
			t.Error("request body absent")
			return
		}
		request := body.Children[0]
		if target := root.find(contextNS, "account"); target != nil && target.Text == "other@example.invalid" {
			fault(w, "service.PERM_DENIED")
			return
		}
		switch request.XMLName.Local {
		case "AuthRequest":
			if password := request.child(accountNS, "password"); password != nil && password.Text != "synthetic-password" {
				fault(w, "account.AUTH_FAILED")
				return
			}
			if request.child(accountNS, "authToken") != nil && revoked {
				fault(w, "service.AUTH_EXPIRED")
				return
			}
			envelope(w, `<AuthResponse xmlns="urn:zimbraAccount"><authToken>`+fixtureToken+`</authToken><lifetime>60000</lifetime></AuthResponse>`)
		case "GetInfoRequest":
			envelope(w, `<GetInfoResponse xmlns="urn:zimbraAccount"><version>synthetic-test</version><id>synthetic-account</id><name>lab@example.invalid</name></GetInfoResponse>`)
		case "GetFolderRequest":
			envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1" name="root"><folder id="2" l="1" name="Inbox"/></folder></GetFolderResponse>`)
		case "SearchRequest":
			envelope(w, `<SearchResponse xmlns="urn:zimbraMail"><m id="9" l="2" f="u"/></SearchResponse>`)
		case "GetMsgRequest":
			m := request.child(mailNS, "m")
			if m == nil || m.get("read") != "0" {
				t.Error("message read status would change")
			}
			envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="9" l="2" f="u"><mp ct="text/html"/></m></GetMsgResponse>`)
		case "EndSessionRequest":
			if request.get("logoff") != "1" {
				t.Error("logout did not revoke token")
			}
			revoked = true
			envelope(w, `<EndSessionResponse xmlns="urn:zimbraAccount"/>`)
		default:
			t.Error("unexpected SOAP operation")
			w.WriteHeader(400)
		}
	}
}
func login(t *testing.T, c *Client) *Session {
	t.Helper()
	s, err := c.Login(context.Background(), "lab@example.invalid", "synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func TestNativeRoundTripAndAccessErrors(t *testing.T) {
	c := testClient(t, fakeZimbra(t), Config{})
	s := login(t, c)
	ctx := context.Background()
	info, err := s.Info(ctx)
	if err != nil || info.Version != "synthetic-test" {
		t.Fatalf("info failed: %v", err)
	}
	folders, err := s.Folders(ctx, "")
	if err != nil || len(folders) != 2 {
		t.Fatalf("folders failed: %v", err)
	}
	inbox, err := s.Inbox(ctx)
	if err != nil || len(inbox) != 1 {
		t.Fatalf("search failed: %v", err)
	}
	message, err := s.Message(ctx, inbox[0].ID)
	if err != nil || message.Flags != "u" {
		t.Fatalf("message failed: %v", err)
	}
	raw, err := s.MIME(ctx, message.ID)
	if err != nil || string(raw) != fixtureMIME {
		t.Fatalf("MIME failed: %v", err)
	}
	if _, err = s.Folders(ctx, "other@example.invalid"); !errors.Is(err, ErrDenied) {
		t.Fatalf("isolation failed: %v", err)
	}
	if _, err = c.Login(ctx, "lab@example.invalid", "incorrect"); !errors.Is(err, ErrAuth) {
		t.Fatalf("password failure not classified: %v", err)
	}
	if err = s.Logout(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.Validate(ctx); !errors.Is(err, ErrExpired) {
		t.Fatalf("revocation not checked remotely: %v", err)
	}
	if _, err = s.Info(ctx); !errors.Is(err, ErrExpired) {
		t.Fatalf("closed session accepted: %v", err)
	}
	if strings.Contains(fmt.Sprint(s), fixtureToken) {
		t.Fatal("session formatted a token")
	}
}
func TestTLSVerifiedByDefault(t *testing.T) {
	server := httptest.NewTLSServer(fakeZimbra(t))
	defer server.Close()
	c, err := New(Config{Origin: server.URL})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	if _, err = c.Login(context.Background(), "lab@example.invalid", "synthetic-password"); !errors.Is(err, ErrTransport) {
		t.Fatalf("untrusted TLS accepted: %v", err)
	}
}
func TestRedirectDoesNotForwardCredentials(t *testing.T) {
	var calls atomic.Int32
	destination := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { calls.Add(1) }))
	defer destination.Close()
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", destination.URL)
		w.WriteHeader(307)
	}, Config{})
	if _, err := c.Login(context.Background(), "lab@example.invalid", "synthetic-password"); !errors.Is(err, ErrRouting) {
		t.Fatalf("redirect accepted: %v", err)
	}
	if calls.Load() != 0 {
		t.Fatal("credentials forwarded to redirected origin")
	}
}
func TestLimitsNamespacesAndFaultRedaction(t *testing.T) {
	for _, tc := range []struct {
		name, body string
		limit      int64
		want       error
	}{
		{"size", strings.Repeat("x", 100), 20, ErrLimit},
		{"namespace", `<Envelope><Body><AuthResponse><authToken>bad</authToken></AuthResponse></Body></Envelope>`, 0, ErrProtocol},
		{"depth", strings.Repeat("<x>", 65) + strings.Repeat("</x>", 65), 0, ErrLimit},
		{"directive", `<!DOCTYPE x><x/>`, 0, ErrProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := testClient(t, func(w http.ResponseWriter, r *http.Request) { io.WriteString(w, tc.body) }, Config{MaxSOAPBytes: tc.limit})
			_, err := c.Login(context.Background(), "lab@example.invalid", "synthetic-password")
			if !errors.Is(err, tc.want) {
				t.Fatalf("got %v, want %v", err, tc.want)
			}
		})
	}
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) { fault(w, "account.AUTH_FAILED") }, Config{})
	_, err := c.Login(context.Background(), "lab@example.invalid", "synthetic-password")
	if strings.Contains(fmt.Sprint(err), "PRIVATE") || strings.Contains(fmt.Sprint(err), "synthetic-password") {
		t.Fatal("fault leaked private details")
	}
	c = testClient(t, fakeZimbra(t), Config{MaxMIMEBytes: 4})
	if _, err = login(t, c).MIME(context.Background(), "9"); !errors.Is(err, ErrLimit) {
		t.Fatalf("MIME not bounded: %v", err)
	}
}
func TestDeadlineCancellationAndLocalExpiry(t *testing.T) {
	c := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		// Bound the fixture itself: an unread HTTP/1 request body can delay the
		// server observing a client disconnect, independently of the client timeout.
		select {
		case <-r.Context().Done():
		case <-time.After(150 * time.Millisecond):
		}
	}, Config{Timeout: 40 * time.Millisecond})
	if _, err := c.Login(context.Background(), "lab@example.invalid", "synthetic-password"); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("timeout not classified: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := c.Login(ctx, "lab@example.invalid", "synthetic-password"); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancel not propagated: %v", err)
	}
	c = testClient(t, fakeZimbra(t), Config{})
	s := login(t, c)
	s.expires = time.Now().Add(-time.Second)
	if _, err := s.Info(context.Background()); !errors.Is(err, ErrExpired) {
		t.Fatalf("local expiration not enforced: %v", err)
	}
}
func TestRejectUnsafeOrigins(t *testing.T) {
	for _, origin := range []string{"http://example.invalid", "https://user:secret@example.invalid", "https://example.invalid/path", "https://example.invalid?authToken=secret"} {
		if _, err := New(Config{Origin: origin}); err == nil {
			t.Fatal("unsafe origin accepted")
		}
	}
}
