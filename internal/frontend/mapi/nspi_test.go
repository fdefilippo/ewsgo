// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"ewsgo/internal/core"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"
)

func TestBindCodecSTATAndTruncation(t *testing.T) {
	v := Bind{Flags: 0x80000000, State: &STAT{SortType: 1, ContainerID: 2, CurrentRec: 3, Delta: -4, NumPos: 5, TotalRecs: 6, CodePage: 1252, TemplateLocale: 1040, SortLocale: 1040}, Auxiliary: []byte{0x80, 0xff}}
	data, err := v.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := ParseBind(data)
	if err != nil || !reflect.DeepEqual(decoded, v) {
		t.Fatal("STAT layout mismatch", err)
	}
	for offset := 0; offset < len(data); offset++ {
		if _, err := ParseBind(data[:offset]); err == nil {
			t.Fatal("truncated Bind accepted", offset)
		}
	}
	for _, flag := range []byte{1, 2, 255} {
		other := bytes.Clone(data)
		other[4] = flag
		if _, err = ParseBind(other); err != nil {
			t.Fatal("nonzero HasState rejected")
		}
	}
	if _, err = ParseBind(append(data, 0)); err == nil {
		t.Fatal("trailing Bind bytes accepted")
	}
	if _, err = ParseBind(make([]byte, MaxBody+1)); err == nil {
		t.Fatal("oversized Bind accepted")
	}
	data, _ = (Bind{}).Marshal()
	if len(data) != 9 {
		t.Fatal("absent STAT framing")
	}
	if v, err = ParseBind(data); err != nil || v.State != nil {
		t.Fatal("optional STAT not absent")
	}
	binary.LittleEndian.PutUint32(data[5:], MaxAuxiliary+1)
	if _, err = ParseBind(data); err == nil {
		t.Fatal("oversized Bind auxiliary accepted")
	}
}

func directoryRequest(t *testing.T, transport *SessionTransport, session core.Session, operation string, data []byte, cookies []*http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	identity, _ := transport.namespace.Mailbox(session.Identity())
	r := httptest.NewRequest("POST", "https://gateway.invalid/mapi/nspi/?MailboxId="+identity.RoutingID, bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/mapi-http")
	r.Header.Set("X-RequestId", "synthetic-request:1")
	r.Header.Set("X-RequestType", operation)
	for _, cookie := range cookies {
		r.AddCookie(cookie)
	}
	w := httptest.NewRecorder()
	transport.Directory(w, r, session)
	return w
}

func TestNSPIContextLifecycleIsolatedFromMailbox(t *testing.T) {
	transport, session, connect := transportFixture(t)
	native := scopedSession{session, session.Identity()}
	bind, _ := (Bind{Flags: 0x80000000, State: &STAT{CodePage: 1252, TemplateLocale: 1040, SortLocale: 1040}}).Marshal()
	w := directoryRequest(t, transport, native, "Bind", bind, nil)
	body := requireTransportCode(t, w, 0)
	guid, _ := hex.DecodeString("35be868f3181ab4a8d637f23212d0885")
	expected := append(append32(append32(nil, 0), 0), guid...)
	expected = append32(expected, 0)
	if !bytes.Equal(body, expected) {
		t.Fatal("Bind GUID/result framing")
	}
	old := w.Result().Cookies()
	for _, cookie := range old {
		if cookie.Path != "/mapi/nspi/" || !cookie.Secure || !cookie.HttpOnly {
			t.Fatal("unsafe directory cookie")
		}
	}
	// Supplying NSPI cookies to the mailbox handler never creates/accesses
	// mailbox state. The independent EMSMDB context is still required.
	requireTransportCode(t, transportRequest(t, transport, session, "PING", nil, old), 10)
	foreign := native
	foreign.identity.Scope.Account = "foreign"
	requireTransportCode(t, directoryRequest(t, transport, foreign, "PING", nil, old), 10)
	w = directoryRequest(t, transport, native, "PING", nil, old)
	if len(requireTransportCode(t, w, 0)) != 0 {
		t.Fatal("directory PING body")
	}
	current := w.Result().Cookies()
	requireTransportCode(t, directoryRequest(t, transport, native, "PING", nil, old), 15)
	for _, operation := range []string{"QueryRows", "GetSpecialTable", "DNToMId", "ResolveNames"} {
		requireTransportCode(t, directoryRequest(t, transport, native, operation, make([]byte, 4), current), 5)
	}
	// Rebind ignores stale sequence and revokes old state without touching EMSMDB.
	mailbox := transportRequest(t, transport, session, "Connect", connect, nil)
	requireTransportCode(t, mailbox, 0)
	w = directoryRequest(t, transport, native, "Bind", bind, old)
	requireTransportCode(t, w, 0)
	fresh := w.Result().Cookies()
	requireTransportCode(t, directoryRequest(t, transport, native, "PING", nil, current), 10)
	if len(transport.contexts.entries) != 2 {
		t.Fatal("NSPI replaced mailbox state")
	}
	reserved := append32(append32(nil, 0xffffffff), 0)
	w = directoryRequest(t, transport, native, "Unbind", reserved, fresh)
	if !bytes.Equal(requireTransportCode(t, w, 0), append32(append32(append32(nil, 0), 1), 0)) || len(w.Result().Cookies()) != 2 {
		t.Fatal("NSPI UnbindSuccess framing")
	}
	requireTransportCode(t, directoryRequest(t, transport, native, "PING", nil, fresh), 10)
	requireTransportCode(t, transportRequest(t, transport, session, "Disconnect", make([]byte, 4), mailbox.Result().Cookies()), 0)
}

func TestNSPIRejectsAnonymousAndUnsupportedCodepages(t *testing.T) {
	transport, session, _ := transportFixture(t)
	native := scopedSession{session, session.Identity()}
	for _, tc := range []struct {
		value             Bind
		transport, method uint32
	}{
		{Bind{Flags: 0x20, State: &STAT{CodePage: 1252}}, 8, 0},
		{Bind{}, 0, 0x80070057},
		{Bind{State: &STAT{CodePage: 1200}}, 0, 0x8004011e},
		{Bind{State: &STAT{CodePage: 999999}}, 0, 0x8004011e},
	} {
		data, _ := tc.value.Marshal()
		w := directoryRequest(t, transport, native, "Bind", data, nil)
		body := requireTransportCode(t, w, tc.transport)
		if tc.transport == 0 && binary.LittleEndian.Uint32(body[4:]) != tc.method {
			t.Fatal("wrong NSPI method error")
		}
		if len(w.Result().Cookies()) != 0 || len(transport.contexts.entries) != 0 {
			t.Fatal("rejected Bind created context")
		}
	}
	valid, _ := (Bind{State: &STAT{CodePage: 65001}}).Marshal()
	for offset := 0; offset < len(valid); offset++ {
		requireTransportCode(t, directoryRequest(t, transport, native, "Bind", valid[:offset], nil), 12)
	}
	w := directoryRequest(t, transport, native, "Bind", valid, nil)
	requireTransportCode(t, w, 0)
	cookies := w.Result().Cookies()
	state := transport.contexts.entries[cookies[0].Value]
	transport.contexts.now = func() time.Time { return state.expires.Add(time.Second) }
	requireTransportCode(t, directoryRequest(t, transport, native, "PING", nil, cookies), 10)
}

func TestUnbindIgnoresReservedAndBoundsAuxiliary(t *testing.T) {
	data := appendBlob(append32(nil, 0xffffffff), []byte{1, 2, 3})
	if parseUnbind(data) != nil {
		t.Fatal("reserved not ignored")
	}
	for offset := 0; offset < len(data); offset++ {
		if parseUnbind(data[:offset]) == nil {
			t.Fatal("truncated Unbind accepted")
		}
	}
	if parseUnbind(append(data, 0)) == nil || parseUnbind(append32(append32(nil, 0), MaxAuxiliary+1)) == nil {
		t.Fatal("unbounded Unbind")
	}
}

func FuzzNSPICodec(f *testing.F) {
	seed, _ := (Bind{State: &STAT{CodePage: 1252}}).Marshal()
	f.Add(seed)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > MaxBody+1 {
			return
		}
		if bind, err := ParseBind(data); err == nil {
			encoded, err := bind.Marshal()
			if err != nil {
				t.Fatal(err)
			}
			decoded, err := ParseBind(encoded)
			if err != nil || !reflect.DeepEqual(decoded, bind) {
				t.Fatal("Bind roundtrip")
			}
		}
		_ = parseUnbind(data)
	})
}

func TestNSPIQuotaReturnsLogonFailedWithoutFakeContext(t *testing.T) {
	transport, session, _ := transportFixture(t)
	native := scopedSession{session, session.Identity()}
	for range maxScopeContexts {
		state, code := transport.contexts.create(session.Identity().Scope, "/mapi/emsmdb/", "", Connect{})
		if code != 0 {
			t.Fatal(code)
		}
		transport.contexts.finish(state, false)
	}
	bind, _ := (Bind{State: &STAT{CodePage: 1252}}).Marshal()
	w := directoryRequest(t, transport, native, "Bind", bind, nil)
	body := requireTransportCode(t, w, 0)
	if binary.LittleEndian.Uint32(body[4:]) != 0x80040111 || len(w.Result().Cookies()) != 0 || len(transport.contexts.entries) != maxScopeContexts {
		t.Fatal("quota not rejected as LogonFailed")
	}
}
