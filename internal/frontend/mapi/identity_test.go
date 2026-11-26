// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"ewsgo/internal/backend/memory"
	"ewsgo/internal/core"
)

const testDeployment = "8f86be35-8131-4aab-8d63-7f23212d0885"

func memoryBox(t *testing.T) core.Mailbox {
	t.Helper()
	box, err := memory.New().Open(context.Background(), core.Scope{Backend: "synthetic", Account: "one"})
	if err != nil {
		t.Fatal(err)
	}
	return box
}

func TestAddressBookGUIDScopeDomainAndRestartStability(t *testing.T) {
	n, _ := NewNamespace(testDeployment)
	scope := core.Scope{Backend: "synthetic", Account: "one"}
	guid, err := n.AddressBookGUID(scope)
	if err != nil {
		t.Fatal(err)
	}
	// Independent Python hashlib/uuid fixture, including GUID integer order.
	want, _ := hex.DecodeString("3ae5e713e9f444539c26cf6123331b19")
	if !bytes.Equal(guid[:], want) {
		t.Fatal("address book UUIDv5 wire vector")
	}
	reloaded, _ := NewNamespace(strings.ToUpper(testDeployment))
	again, _ := reloaded.AddressBookGUID(scope)
	if again != guid {
		t.Fatal("address book GUID changed on reload")
	}
	for _, otherScope := range []core.Scope{{Backend: "synthetic", Account: "two"}, {Backend: "other", Account: "one"}, {Backend: "synthetico", Account: "ne"}} {
		other, _ := n.AddressBookGUID(otherScope)
		if other == guid {
			t.Fatal("directory scope collision")
		}
	}
	otherInstall, _ := NewNamespace("9f86be35-8131-4aab-8d63-7f23212d0885")
	other, _ := otherInstall.AddressBookGUID(scope)
	if other == guid {
		t.Fatal("directory GUID ignores deployment")
	}
	box, _ := n.Mailbox(core.Identity{Scope: scope, Name: "lab@example.invalid"})
	boxUUID, _ := NewNamespace(box.GUID)
	if guid == boxUUID.ServerGUID() || guid == n.ServerGUID() {
		t.Fatal("directory identity aliases another protocol domain")
	}
	for _, bad := range []core.Scope{{}, {Backend: "synthetic"}, {Account: "one"}, {Backend: strings.Repeat("x", 1025), Account: "one"}, {Backend: "synthetic", Account: strings.Repeat("x", 4097)}} {
		if _, err := n.AddressBookGUID(bad); err == nil {
			t.Fatal("invalid native scope accepted")
		}
	}
	if _, err := (Namespace{}).AddressBookGUID(scope); err == nil {
		t.Fatal("missing deployment accepted")
	}
}

func TestNamespaceScopeAndRestartStability(t *testing.T) {
	n, err := NewNamespace(testDeployment)
	if err != nil {
		t.Fatal(err)
	}
	owner := core.Identity{Scope: core.Scope{Backend: "synthetic", Account: "one"}, Name: "lab@example.invalid"}
	first, err := n.Mailbox(owner)
	if err != nil {
		t.Fatal(err)
	}
	restarted, _ := NewNamespace(strings.ToUpper(testDeployment))
	again, _ := restarted.Mailbox(owner)
	if first != again || !strings.HasPrefix(first.LegacyDN, "/o=EWSGo/ou=") || first.GUID[14] != '5' {
		t.Fatal("identity changed with namespace reload")
	}
	renamed := owner
	renamed.Name = "renamed@other.invalid"
	afterRename, _ := n.Mailbox(renamed)
	if first.GUID != afterRename.GUID || first.LegacyDN != afterRename.LegacyDN {
		t.Fatal("SMTP rename changed mailbox identity")
	}
	for _, scope := range []core.Scope{{Backend: "synthetic", Account: "two"}, {Backend: "other", Account: "one"}} {
		other, _ := n.Mailbox(core.Identity{Scope: scope, Name: owner.Name})
		if other.GUID == first.GUID || other.LegacyDN == first.LegacyDN {
			t.Fatal("cross-scope collision")
		}
	}
	otherInstallation, _ := NewNamespace("9f86be35-8131-4aab-8d63-7f23212d0885")
	other, _ := otherInstallation.Mailbox(owner)
	if other.GUID == first.GUID {
		t.Fatal("installations share namespace")
	}
	for _, id := range []string{"", "00000000-0000-0000-0000-000000000000", strings.Replace(testDeployment, "8", "z", 1), strings.Replace(testDeployment, "-", "", 1)} {
		if _, err := NewNamespace(id); err == nil {
			t.Fatal("invalid installation UUID accepted")
		}
	}
	for _, invalid := range []core.Identity{{}, {Scope: owner.Scope, Name: "Display <lab@example.invalid>"}, {Scope: owner.Scope, Name: "not an address"}, {Scope: owner.Scope, Name: "lab@example.invalid\r\nheader"}} {
		if _, err := n.Mailbox(invalid); err == nil {
			t.Fatal("invalid backend identity accepted")
		}
	}
}

func TestProfileRoutesAndDNDoNotSelectAnotherMailbox(t *testing.T) {
	n, _ := NewNamespace(testDeployment)
	s := &testSession{box: nil} // Rejected routes must not touch the backend.
	identity, _ := n.Mailbox(s.Identity())
	valid := url.Values{"MailboxId": {identity.RoutingID}}.Encode()
	for _, tc := range []struct {
		query, dn string
		code      int
	}{
		{"", identity.LegacyDN, 400},
		{"MailboxId=%zz", identity.LegacyDN, 400},
		{valid + "&" + valid, identity.LegacyDN, 400},
		{valid + "&Other=one", identity.LegacyDN, 400},
		{"MailboxId=other", identity.LegacyDN, 403},
		{valid, "/o=Other/cn=AnotherAccount", 403},
	} {
		body, _ := (Connect{UserDN: tc.dn}).Marshal()
		r := httptest.NewRequest("POST", "https://gateway.invalid/mapi/emsmdb/?"+tc.query, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/mapi-http")
		r.Header.Set("X-RequestType", "Connect")
		r.Header.Set("X-RequestId", "synthetic")
		w := httptest.NewRecorder()
		n.MailboxSpike(w, r, s)
		if w.Code != tc.code {
			t.Fatalf("got %d want %d", w.Code, tc.code)
		}
		w = httptest.NewRecorder()
		if tc.code == 400 || tc.query == "MailboxId=other" {
			n.DirectorySpike(w, r, s)
			if w.Code != tc.code {
				t.Fatalf("NSPI route got %d want %d", w.Code, tc.code)
			}
		}
	}
}

func TestDirectorySpikeNeverCreatesAContext(t *testing.T) {
	n, _ := NewNamespace(testDeployment)
	s := &testSession{box: memoryBox(t)}
	identity, _ := n.Mailbox(s.Identity())
	for _, size := range []int{0, MaxBody + 1} {
		r := httptest.NewRequest("POST", "https://gateway.invalid/mapi/nspi/?"+url.Values{"MailboxId": {identity.RoutingID}}.Encode(), bytes.NewReader(make([]byte, size)))
		r.Header.Set("Content-Type", "application/mapi-http")
		r.Header.Set("X-RequestId", "synthetic")
		r.Header.Set("X-RequestType", "Bind")
		w := httptest.NewRecorder()
		n.DirectorySpike(w, r, s)
		if size > MaxBody {
			if w.Code != 413 {
				t.Fatal("NSPI body limit")
			}
			continue
		}
		var body map[string]any
		if w.Code != 501 || json.Unmarshal(w.Body.Bytes(), &body) != nil || body["nativeMailboxAccessible"] != true || body["directoryOpened"] != false || body["sessionContextCreated"] != false || len(w.Header().Values("Set-Cookie")) != 0 {
			t.Fatal("NSPI diagnostic faked a context")
		}
	}
}
