// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/backend/memory"
	"ewsgo/internal/core"
)

type scopedSession struct {
	core.Session
	identity core.Identity
}

func (s scopedSession) Identity() core.Identity { return s.identity }

func transportFixture(t *testing.T) (*SessionTransport, core.Session, []byte) {
	t.Helper()
	n, err := NewNamespace("8f86be35-8131-4aab-8d63-7f23212d0885")
	if err != nil {
		t.Fatal(err)
	}
	box, err := memory.New().Open(context.Background(), core.Scope{Backend: "synthetic", Account: "one"})
	if err != nil {
		t.Fatal(err)
	}
	session := &testSession{box}
	id, _ := n.Mailbox(session.Identity())
	data, _ := (Connect{UserDN: id.LegacyDN, CodePage: 1252, SortLCID: 1033, StringLCID: 1033}).Marshal()
	return NewSessionTransport(n), session, data
}

func transportRequest(t *testing.T, transport *SessionTransport, session core.Session, operation string, data []byte, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	identity, _ := transport.namespace.Mailbox(session.Identity())
	r := httptest.NewRequest("POST", "https://gateway.invalid/mapi/emsmdb/?MailboxId="+identity.RoutingID, bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/mapi-http")
	r.Header.Set("X-RequestId", "synthetic-request:1")
	r.Header.Set("X-RequestType", operation)
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	transport.Mailbox(w, r, session)
	return w
}

func requireTransportCode(t *testing.T, w *httptest.ResponseRecorder, expected uint32) []byte {
	t.Helper()
	if w.Code != 200 || w.Header().Get("X-ResponseCode") != strconv.Itoa(int(expected)) {
		t.Fatalf("HTTP %d; transport %q, want %d", w.Code, w.Header().Get("X-ResponseCode"), expected)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("Content-Length") != strconv.Itoa(w.Body.Len()) {
		t.Fatal("invalid response headers")
	}
	if expected != 0 {
		if w.Header().Get("Content-Type") != "text/html" || len(w.Result().Cookies()) != 0 {
			t.Fatal("invalid failure framing")
		}
		return nil
	}
	if w.Header().Get("Content-Type") != "application/mapi-http" || w.Header().Get("X-RequestId") != "synthetic-request:1" {
		t.Fatal("invalid success headers")
	}
	parts := bytes.SplitN(w.Body.Bytes(), []byte("\r\n\r\n"), 2)
	if len(parts) != 2 || !bytes.HasPrefix(parts[0], []byte("PROCESSING\r\nDONE\r\n")) || !bytes.Contains(parts[0], []byte("X-ResponseCode: 0\r\n")) {
		t.Fatal("missing metadata framing")
	}
	return parts[1]
}

func TestSessionTransportLifecycleAndExplicitROPFailure(t *testing.T) {
	transport, session, connect := transportFixture(t)
	w := transportRequest(t, transport, session, "Connect", connect, nil)
	body := requireTransportCode(t, w, 0)
	if len(body) < 27 || binary.LittleEndian.Uint32(body) != 0 || binary.LittleEndian.Uint32(body[4:]) != 0 {
		t.Fatal("Connect did not succeed")
	}
	cookies := w.Result().Cookies()
	if len(cookies) != 2 || w.Header().Get("X-ExpirationInfo") != "300000" {
		t.Fatal("context missing")
	}
	for _, cookie := range cookies {
		if !cookie.Secure || !cookie.HttpOnly || cookie.Path != "/mapi/emsmdb/" || cookie.Domain != "" || !validCookie(cookie.Value) {
			t.Fatal("unsafe context cookie")
		}
	}
	state := transport.contexts.entries[cookies[0].Value]
	if state == nil || state.codePage != 1252 || state.sortLCID != 1033 || state.stringLCID != 1033 {
		t.Fatal("connection settings lost")
	}

	rop, _ := (ROPBuffer{Operations: []byte{0xfe, 0, 0}, Handles: []uint32{0xffffffff}}).Marshal()
	execute, _ := (Execute{ROP: rop, MaxOutput: 4096}).Marshal()
	w = transportRequest(t, transport, session, "Execute", execute, cookies)
	body = requireTransportCode(t, w, 0)
	if len(body) != 20 || binary.LittleEndian.Uint32(body[4:]) != NotSupported || binary.LittleEndian.Uint32(body[12:]) != 0 {
		t.Fatal("ROP failure replaced by fake success")
	}
	next := w.Result().Cookies()
	if cookies[0].Value != next[0].Value || cookies[1].Value == next[1].Value {
		t.Fatal("sequence did not rotate")
	}
	requireTransportCode(t, transportRequest(t, transport, session, "PING", nil, cookies), 15)
	w = transportRequest(t, transport, session, "PING", nil, next)
	if len(requireTransportCode(t, w, 0)) != 0 {
		t.Fatal("PING binary body")
	}
	next = w.Result().Cookies()
	w = transportRequest(t, transport, session, "Disconnect", make([]byte, 4), next)
	body = requireTransportCode(t, w, 0)
	if len(body) != 12 || !bytes.Equal(body, make([]byte, 12)) || len(w.Result().Cookies()) != 2 {
		t.Fatal("invalid disconnect response")
	}
	requireTransportCode(t, transportRequest(t, transport, session, "PING", nil, next), 10)
	if len(transport.contexts.entries) != 0 {
		t.Fatal("disconnect did not revoke")
	}
}

func TestTransportRejectsCrossScopeAndEndpointCookieReuse(t *testing.T) {
	transport, session, connect := transportFixture(t)
	w := transportRequest(t, transport, session, "Connect", connect, nil)
	requireTransportCode(t, w, 0)
	cookies := w.Result().Cookies()
	foreign := session.Identity()
	foreign.Scope.Account = "another"
	requireTransportCode(t, transportRequest(t, transport, scopedSession{session, foreign}, "PING", nil, cookies), 10)
	foreign = session.Identity()
	foreign.Scope.Backend = "another"
	requireTransportCode(t, transportRequest(t, transport, scopedSession{session, foreign}, "PING", nil, cookies), 10)
	state, code := transport.contexts.acquire(session.Identity().Scope, "/mapi/nspi/", cookies[0].Value, cookies[1].Value)
	if state != nil || code != 10 {
		t.Fatal("cross-endpoint reuse accepted")
	}
	requireTransportCode(t, transportRequest(t, transport, session, "PING", nil, cookies), 0)
}

func TestReconnectExpiresAndRevokesOldContext(t *testing.T) {
	transport, session, connect := transportFixture(t)
	now := time.Unix(1700000000, 0)
	transport.contexts.now = func() time.Time { return now }
	first := transportRequest(t, transport, session, "Connect", connect, nil)
	requireTransportCode(t, first, 0)
	old := first.Result().Cookies()
	old[1].Value = "sequence ignored on reconnect"
	replacement := transportRequest(t, transport, session, "Connect", connect, old)
	requireTransportCode(t, replacement, 0)
	fresh := replacement.Result().Cookies()
	if fresh[0].Value == old[0].Value || len(transport.contexts.entries) != 1 {
		t.Fatal("reconnect not replaced")
	}
	requireTransportCode(t, transportRequest(t, transport, session, "Connect", connect, old), 10)
	now = now.Add(sessionTTL - time.Second)
	ping := transportRequest(t, transport, session, "PING", nil, fresh)
	requireTransportCode(t, ping, 0)
	now = now.Add(sessionTTL)
	requireTransportCode(t, transportRequest(t, transport, session, "PING", nil, ping.Result().Cookies()), 10)
	if len(transport.contexts.entries) != 0 {
		t.Fatal("expired state retained")
	}
}

func TestContextExcludesSimultaneousRequestsAndBoundsAllocation(t *testing.T) {
	s := newContexts()
	scope := core.Scope{Backend: "synthetic", Account: "one"}
	state, code := s.create(scope, "emsmdb", "", Connect{})
	if code != 0 {
		t.Fatal(code)
	}
	if _, code = s.acquire(scope, "emsmdb", state.id, state.sequence); code != 15 {
		t.Fatal("simultaneous request accepted")
	}
	if _, code = s.create(scope, "emsmdb", state.id, Connect{}); code != 15 {
		t.Fatal("in-flight reconnect accepted")
	}
	s.finish(state, false)
	id, sequence := state.id, state.sequence
	results := make(chan uint32, 16)
	for range 16 {
		go func() { _, code := s.acquire(scope, "emsmdb", id, sequence); results <- code }()
	}
	accepted := 0
	for range 16 {
		code := <-results
		if code == 0 {
			accepted++
		} else if code != 15 {
			t.Fatal(code)
		}
	}
	if accepted != 1 {
		t.Fatal("context not exclusive", accepted)
	}
	s.finish(state, true)
	for range maxScopeContexts {
		state, code = s.create(scope, "emsmdb", "", Connect{})
		if code != 0 {
			t.Fatal(code)
		}
		s.finish(state, false)
	}
	if _, code = s.create(scope, "emsmdb", "", Connect{}); code == 0 {
		t.Fatal("unbounded per-account allocation")
	}
	for index := maxScopeContexts; index < maxContexts; index++ {
		state, code = s.create(core.Scope{Backend: "synthetic", Account: strconv.Itoa(index)}, "emsmdb", "", Connect{})
		if code != 0 {
			t.Fatal(code)
		}
		s.finish(state, false)
	}
	if _, code = s.create(core.Scope{Backend: "synthetic", Account: "last"}, "emsmdb", "", Connect{}); code == 0 {
		t.Fatal("unbounded global allocation")
	}
}

func TestTransportInputFailuresNeverCreateContext(t *testing.T) {
	transport, session, connect := transportFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	base := "https://gateway.invalid/mapi/emsmdb/?MailboxId=" + identity.RoutingID
	for _, tc := range []struct {
		name   string
		change func(*http.Request)
		code   uint32
	}{
		{"method", func(r *http.Request) { r.Method = "GET" }, 2},
		{"path", func(r *http.Request) { r.URL.Path += "extra" }, 3},
		{"unknown length", func(r *http.Request) { r.ContentLength = -1 }, 7},
		{"missing header", func(r *http.Request) { r.Header.Del("X-RequestId") }, 7},
		{"duplicate header", func(r *http.Request) { r.Header.Add("X-RequestId", "another") }, 4},
		{"header length", func(r *http.Request) { r.Header.Set("X-RequestId", strings.Repeat("x", 257)) }, 4},
		{"header injection", func(r *http.Request) { r.Header.Set("X-RequestId", "invalid\r\nInjected: yes") }, 4},
		{"content type", func(r *http.Request) { r.Header.Set("Content-Type", "text/plain") }, 4},
		{"oversized body", func(r *http.Request) { r.Body = io.NopCloser(bytes.NewReader(make([]byte, MaxBody+1))) }, 9},
		{"truncated connect", func(r *http.Request) { r.Body = io.NopCloser(bytes.NewReader(connect[:4])) }, 12},
		{"unknown operation", func(r *http.Request) { r.Header.Set("X-RequestType", "Fake") }, 5},
		{"foreign route", func(r *http.Request) { r.URL.RawQuery = "MailboxId=foreign" }, 11},
		{"duplicate route", func(r *http.Request) { r.URL.RawQuery += "&MailboxId=foreign" }, 4},
		{"malformed context", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: contextCookie, Value: "invalid"}) }, 6},
		{"unknown context", func(r *http.Request) { r.AddCookie(&http.Cookie{Name: contextCookie, Value: strings.Repeat("a", 64)}) }, 10},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest("POST", base, bytes.NewReader(connect))
			r.Header.Set("Content-Type", "application/mapi-http")
			r.Header.Set("X-RequestId", "synthetic-request:1")
			r.Header.Set("X-RequestType", "Connect")
			tc.change(r)
			w := httptest.NewRecorder()
			transport.Mailbox(w, r, session)
			requireTransportCode(t, w, tc.code)
			if len(transport.contexts.entries) != 0 {
				t.Fatal("state allocated for rejected request")
			}
		})
	}
	for _, flags := range []uint32{1, 2, 0x80000000} {
		v, _ := ParseConnect(connect)
		v.Flags = flags
		data, _ := v.Marshal()
		w := transportRequest(t, transport, session, "Connect", data, nil)
		body := requireTransportCode(t, w, 0)
		if binary.LittleEndian.Uint32(body[4:]) == 0 || len(w.Result().Cookies()) != 0 {
			t.Fatal("unsupported privilege accepted")
		}
	}
	v, _ := ParseConnect(connect)
	v.Flags = 0x8000
	data, _ := v.Marshal()
	requireTransportCode(t, transportRequest(t, transport, session, "Connect", data, nil), 0)
}

func TestMissingSequenceAndDuplicateCookies(t *testing.T) {
	transport, session, connect := transportFixture(t)
	w := transportRequest(t, transport, session, "Connect", connect, nil)
	cookies := w.Result().Cookies()
	for _, tc := range []struct {
		cookies []*http.Cookie
		code    uint32
	}{
		{nil, 13}, {cookies[:1], 13},
		{[]*http.Cookie{cookies[0], cookies[0], cookies[1]}, 6},
		{[]*http.Cookie{cookies[0], cookies[1], cookies[1]}, 15},
	} {
		requireTransportCode(t, transportRequest(t, transport, session, "Disconnect", make([]byte, 4), tc.cookies), tc.code)
	}
	w = transportRequest(t, transport, session, "PING", nil, nil)
	if len(requireTransportCode(t, w, 0)) != 0 || len(w.Result().Cookies()) != 0 {
		t.Fatal("reachability PING allocated state")
	}
}

func TestDisconnectCodecBounds(t *testing.T) {
	valid := appendBlob(nil, []byte{1, 2, 3})
	if parseDisconnect(valid) != nil {
		t.Fatal("valid disconnect rejected")
	}
	for offset := 0; offset < len(valid); offset++ {
		if parseDisconnect(valid[:offset]) == nil {
			t.Fatal("truncated disconnect accepted", offset)
		}
	}
	if parseDisconnect(append(valid, 0)) == nil || parseDisconnect(append32(nil, MaxAuxiliary+1)) == nil {
		t.Fatal("unbounded disconnect")
	}
}

type revocableAuth struct {
	session core.Session
	denied  bool
	calls   int
	failure error
}

func (a *revocableAuth) Authenticate(context.Context, string, string) (core.Session, error) {
	a.calls++
	if a.denied {
		return nil, core.ErrAccessDenied
	}
	if a.failure != nil {
		return nil, a.failure
	}
	return a.session, nil
}

func TestPINGBackendFailurePrecedesContextLookup(t *testing.T) {
	for _, directory := range []bool{false, true} {
		for _, disconnected := range []bool{false, true} {
			for _, failure := range []struct {
				name string
				err  error
			}{
				{"backend", errors.New("PRIVATE backend failure")},
				{"canceled", context.Canceled},
				{"deadline", context.DeadlineExceeded},
			} {
				t.Run(strconv.FormatBool(directory)+"/"+strconv.FormatBool(disconnected)+"/"+failure.name, func(t *testing.T) {
					transport, session, connect := transportFixture(t)
					path, start, stop := "/mapi/emsmdb/", "Connect", "Disconnect"
					next := transport.Mailbox
					if directory {
						path, start, stop, next = "/mapi/nspi/", "Bind", "Unbind", transport.Directory
						connect, _ = (Bind{State: &STAT{CodePage: 1252}}).Marshal()
					}
					identity, _ := transport.namespace.Mailbox(session.Identity())
					auth := &revocableAuth{session: session}
					entered := 0
					handler := core.BasicTLS(auth, func(w http.ResponseWriter, r *http.Request, s core.Session) {
						entered++
						next(w, r, s)
					})
					call := func(op string, data []byte, cookies []*http.Cookie) *httptest.ResponseRecorder {
						r := httptest.NewRequest("POST", "https://gateway.invalid"+path+"?MailboxId="+identity.RoutingID, bytes.NewReader(data))
						r.Header.Set("Content-Type", "application/mapi-http")
						r.Header.Set("X-RequestId", "synthetic-request:1")
						r.Header.Set("X-RequestType", op)
						r.SetBasicAuth("synthetic", "correct")
						for _, cookie := range cookies {
							r.AddCookie(cookie)
						}
						w := httptest.NewRecorder()
						handler.ServeHTTP(w, r)
						return w
					}
					w := call(start, connect, nil)
					requireTransportCode(t, w, 0)
					cookies := w.Result().Cookies()
					if disconnected {
						stopData := make([]byte, 4)
						if directory {
							stopData = make([]byte, 8)
						}
						requireTransportCode(t, call(stop, stopData, cookies), 0)
					}
					before, contexts := entered, len(transport.contexts.entries)
					calls := auth.calls
					auth.failure = failure.err
					w = call("PING", nil, cookies)
					if w.Code != http.StatusBadGateway || w.Body.String() != "backend unavailable\n" || w.Header().Get("X-ResponseCode") != "" {
						t.Fatal("backend failure was confused with a MAPI context result or exposed private details")
					}
					if entered != before || auth.calls != calls+1 || len(transport.contexts.entries) != contexts {
						t.Fatal("failed authentication reached the transport or changed its contexts")
					}
					auth.failure = nil
					w = call("PING", nil, cookies)
					if disconnected {
						if w.Code != http.StatusOK || w.Header().Get("X-ResponseCode") == "0" || len(transport.contexts.entries) != 0 {
							t.Fatal("closed context was recreated or reported as backend failure")
						}
					} else {
						requireTransportCode(t, w, 0)
						if len(transport.contexts.entries) != contexts {
							t.Fatal("authentication failure discarded the live context")
						}
					}
				})
			}
		}
	}
}

func TestContextCookieNeverBypassesFreshBackendAuthorization(t *testing.T) {
	transport, session, connect := transportFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	auth := &revocableAuth{session: session}
	handler := core.BasicTLS(auth, transport.Mailbox)
	call := func(op string, data []byte, cookies []*http.Cookie, login bool) *httptest.ResponseRecorder {
		r := httptest.NewRequest("POST", "https://gateway.invalid/mapi/emsmdb/?MailboxId="+identity.RoutingID, bytes.NewReader(data))
		r.Header.Set("Content-Type", "application/mapi-http")
		r.Header.Set("X-RequestType", op)
		r.Header.Set("X-RequestId", "synthetic-request:1")
		if login {
			r.SetBasicAuth("synthetic", "correct")
		}
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		return w
	}
	w := call("Connect", connect, nil, true)
	requireTransportCode(t, w, 0)
	cookies := w.Result().Cookies()
	if w = call("PING", nil, cookies, false); w.Code != 401 {
		t.Fatal("cookie bypassed authentication")
	}
	auth.denied = true
	if w = call("PING", nil, cookies, true); w.Code != 403 {
		t.Fatal("revoked account retained access")
	}
	if auth.calls != 2 {
		t.Fatal("backend not rechecked")
	}
}

type failedWriter struct{ *httptest.ResponseRecorder }

func (failedWriter) Write([]byte) (int, error) { return 0, errors.New("client disconnected") }
func TestAbortedConnectDoesNotLeakAContext(t *testing.T) {
	transport, session, connect := transportFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	r := httptest.NewRequest("POST", "https://gateway.invalid/mapi/emsmdb/?MailboxId="+identity.RoutingID, bytes.NewReader(connect))
	r.Header.Set("Content-Type", "application/mapi-http")
	r.Header.Set("X-RequestId", "synthetic")
	r.Header.Set("X-RequestType", "Connect")
	transport.Mailbox(failedWriter{httptest.NewRecorder()}, r, session)
	if len(transport.contexts.entries) != 0 {
		t.Fatal("failed response leaked a context")
	}
}
