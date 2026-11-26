// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"ewsgo/internal/core"
)

const galContact = `<cn id="GAL-cache:42" ref="uid=synthetic,dc=example,dc=invalid"><a n="zimbraId">12345678-1234-4234-8234-123456789abc</a><a n="email">recipient@example.invalid</a><a n="fullName">Native Recipient</a></cn>`

func galClient(t *testing.T, payload string, faultCode string) *Client {
	t.Helper()
	base := fakeZimbra(t)
	return testClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		root, err := parseXML(raw)
		if err != nil {
			t.Fatal("malformed outgoing XML")
		}
		request := root.find(accountNS, "SearchGalRequest")
		if request == nil {
			r.Body = io.NopCloser(strings.NewReader(string(raw)))
			base(w, r)
			return
		}
		if root.find(contextNS, "account") != nil || root.find(contextNS, "authToken").Text != fixtureToken || request.get("name") != "" || request.get("type") != "all" || request.get("limit") != "1000" || request.get("offset") != "0" || request.get("sortBy") != "nameAsc" || len(request.Children) != 0 {
			t.Error("GAL request widened or changed native account scope")
		}
		if faultCode != "" {
			fault(w, faultCode)
		} else {
			envelope(w, `<SearchGalResponse xmlns="urn:zimbraAccount" `+payload+`</SearchGalResponse>`)
		}
	}, Config{})
}

func TestGALUsesNativeIdentityNotCacheContactID(t *testing.T) {
	c := galClient(t, `more="0" offset="0" paginationSupported="1">`+galContact, "")
	s, err := c.Authenticate(context.Background(), "lab@example.invalid", "synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close(context.Background())
	entries, err := s.(core.DirectorySession).Directory().Entries(context.Background())
	if err != nil || len(entries) != 1 {
		t.Fatal("native directory read", err)
	}
	e := entries[0]
	if e.Identity.Scope != (core.Scope{Backend: "zimbra", Account: "12345678-1234-4234-8234-123456789abc"}) || e.Identity.Name != "recipient@example.invalid" || e.DisplayName != "Native Recipient" {
		t.Fatal("native ID mapping changed")
	}
	if fmt.Sprint(GALEntry{AccountID: "private-id", Address: "private-address"}) != "Zimbra GAL entry [private fields redacted]" {
		t.Fatal("native GAL formatting exposes private values")
	}
}

func TestGALRejectsIncompleteAndAmbiguousData(t *testing.T) {
	for _, tc := range []struct {
		name, payload, fault string
		want                 error
	}{
		{"empty", `more="0">`, "", nil},
		{"no-paging-needed", `more="0" paginationSupported="0">` + galContact, "", nil},
		{"truncated", `more="1">` + galContact, "", ErrLimit},
		{"missing-completeness", `>` + galContact, "", ErrProtocol},
		{"bad-more", `more="unknown">`, "", ErrProtocol},
		{"wrong-offset", `more="0" offset="10">` + galContact, "", ErrProtocol},
		{"duplicate-id", `more="0">` + galContact + galContact, "", ErrProtocol},
		{"cache-id-only", `more="0"><cn id="cache:42"><a n="email">recipient@example.invalid</a></cn>`, "", ErrProtocol},
		{"bad-address", `more="0">` + strings.Replace(galContact, "recipient@example.invalid", "Recipient &lt;recipient@example.invalid&gt;", 1), "", ErrProtocol},
		{"duplicate-field", `more="0">` + strings.Replace(galContact, "</cn>", `<a n="email">other@example.invalid</a></cn>`, 1), "", ErrProtocol},
		{"wrong-namespace", `more="0"><cn xmlns="urn:wrong"/>`, "", ErrProtocol},
		{"denied", "", "service.PERM_DENIED", ErrDenied},
		{"expired", "", "service.AUTH_EXPIRED", ErrExpired},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := login(t, galClient(t, tc.payload, tc.fault))
			entries, err := s.GAL(context.Background())
			if !errors.Is(err, tc.want) || (err != nil && entries != nil) {
				t.Fatal("partial data or incorrect error", err)
			}
		})
	}
}

func TestGALBoundAndClosedSession(t *testing.T) {
	var payload strings.Builder
	payload.WriteString(`more="0">`)
	for i := 1; i <= core.MaxDirectoryEntries+1; i++ {
		fmt.Fprintf(&payload, `<cn><a n="zimbraId">%08x-1234-4234-8234-123456789abc</a><a n="email">recipient@example.invalid</a></cn>`, i)
	}
	s := login(t, galClient(t, payload.String(), ""))
	if entries, err := s.GAL(context.Background()); !errors.Is(err, ErrLimit) || entries != nil {
		t.Fatal("oversized catalog accepted", err)
	}
	if err := s.Logout(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GAL(context.Background()); !errors.Is(err, ErrExpired) {
		t.Fatal("closed native session read GAL", err)
	}
}
