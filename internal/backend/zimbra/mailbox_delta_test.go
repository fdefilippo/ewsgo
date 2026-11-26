// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestMailboxDeltaAnchorAndCompoundContinuation(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(500)
			return
		}
		request := string(raw)
		switch {
		case strings.Contains(request, "NoOpRequest") && strings.Contains(request, `wait="0"`):
			fmt.Fprintf(w, `<s:Envelope xmlns:s="%s">
				<s:Header><context xmlns="urn:zimbra"><change acct="delegated" token="99"/><change token="10"/></context></s:Header>
				<s:Body><NoOpResponse xmlns="urn:zimbraMail"/></s:Body></s:Envelope>`, soapNS)
		case strings.Contains(request, "SyncRequest"):
			if !strings.Contains(request, `l="1"`) || !strings.Contains(request, `typed="1"`) ||
				!strings.Contains(request, `changeLimit="4"`) || !strings.Contains(request, `deleteLimit="4"`) {
				t.Error("expected mailbox root, typed deletions and bounded limits")
				w.WriteHeader(400)
				return
			}
			switch {
			case strings.Contains(request, `token="10"`):
				envelope(w, `<SyncResponse xmlns="urn:zimbraMail" token="10-21:d17-35" more="true">
					<m id="20"/><m id="20"/><folder id="2" l="1"/><link id="3"/>
					<deleted ids="30,31,32,-19"><folder ids="30"/><m ids="31"/><c ids="32,-19"/></deleted>
				</SyncResponse>`)
			case strings.Contains(request, `token="10-21:d17-35"`):
				envelope(w, `<SyncResponse xmlns="urn:zimbraMail" token="18"><m id="21"/></SyncResponse>`)
			default:
				t.Error("continuation was not sent unchanged")
				w.WriteHeader(400)
			}
		default:
			t.Error("unexpected SOAP request")
			w.WriteHeader(400)
		}
	}, Config{})
	session := &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Minute)}
	anchor, err := session.mailboxDeltaAnchor(t.Context())
	if err != nil || anchor != "10" {
		t.Fatalf("read own marker: %q, %v", anchor, err)
	}
	first, err := session.readMailboxDelta(t.Context(), anchor, 4)
	if err != nil || !first.More || first.Next != "10-21:d17-35" {
		t.Fatalf("first delta: %+v, %v", first, err)
	}
	if !slices.Equal(first.ChangedMessages, []string{"20"}) || !slices.Equal(first.ChangedFolders, []string{"2"}) ||
		!slices.Equal(first.DeletedMessages, []string{"31"}) || !slices.Equal(first.DeletedFolders, []string{"30"}) ||
		!slices.Equal(first.OtherRemovals, []string{"32"}) {
		t.Fatal("changed, typed and unclassified references were not kept distinct")
	}
	second, err := session.readMailboxDelta(t.Context(), first.Next, 4)
	if err != nil || second.More || second.Next != "18" || !slices.Equal(second.ChangedMessages, []string{"21"}) {
		t.Fatalf("continuation delta: %+v, %v", second, err)
	}
}

func TestMailboxDeltaRefusesAmbiguousOrRegressivePages(t *testing.T) {
	for _, tc := range []struct {
		name, cursor, attributes, body string
		want                           error
	}{
		{"initial request forbidden", "", "", "", core.ErrResync},
		{"zero cursor forbidden", "0", "", "", core.ErrResync},
		{"invalid delete position", "12:d5-0", "", "", core.ErrResync},
		{"expired", "15", "", "", core.ErrResync},
		{"rewound change cursor", "15-26:d20-91", `token="15-25:d20-93" more="1"`, "", core.ErrResync},
		{"rewound delete cursor", "15-26:d20-91", `token="16:d20-90" more="1"`, "", core.ErrResync},
		{"no page progress", "15", `token="15:d15" more="1"`, "", ErrProtocol},
		{"no token", "15", `more="1"`, "", ErrProtocol},
		{"unknown more value", "15", `token="16" more="maybe"`, "", ErrProtocol},
		{"unexpected initial snapshot", "15", `token="16" s="120"`, `<folder id="1"><m ids="8,9"/></folder>`, ErrProtocol},
		{"inconsistent deletion", "15", `token="16"`, `<deleted ids="9"><m ids="8"/></deleted>`, ErrProtocol},
		{"conflicting typed deletion", "15", `token="16"`, `<deleted ids="9"><m ids="9"/><folder ids="9"/></deleted>`, ErrProtocol},
		{"message changed and deleted", "15", `token="16"`, `<deleted ids="9"><m ids="9"/></deleted><m id="9"/>`, core.ErrResync},
		{"partial list with invalid message", "15", `token="16"`, `<deleted ids="9"><m ids="9"/></deleted><m id="other:12"/>`, ErrProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
				if tc.attributes == "" {
					if tc.name != "expired" {
						t.Error("invalid local cursor reached the server")
					}
					fault(w, "mail.MUST_RESYNC")
					return
				}
				envelope(w, `<SyncResponse xmlns="urn:zimbraMail" `+tc.attributes+`>`+tc.body+`</SyncResponse>`)
			}, Config{})
			session := &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Minute)}
			page, err := session.readMailboxDelta(t.Context(), tc.cursor, 2)
			if !errors.Is(err, tc.want) || page.Next != "" || len(page.DeletedMessages) != 0 {
				t.Fatalf("page leaked data or wrong result: %+v, %v", page, err)
			}
		})
	}
}

func TestMailboxDeltaAnchorRequiresOneOwnChangeMarker(t *testing.T) {
	for _, marker := range []string{"", `<change acct="other" token="12"/>`, `<change token="4"/><change token="5"/>`} {
		t.Run(marker, func(t *testing.T) {
			client := testClient(t, func(w http.ResponseWriter, _ *http.Request) {
				fmt.Fprintf(w, `<s:Envelope xmlns:s="%s">
					<s:Header><context xmlns="urn:zimbra">%s</context></s:Header>
					<s:Body><NoOpResponse xmlns="urn:zimbraMail"/></s:Body></s:Envelope>`, soapNS, marker)
			}, Config{})
			session := &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Minute)}
			if cursor, err := session.mailboxDeltaAnchor(t.Context()); cursor != "" || !errors.Is(err, ErrProtocol) {
				t.Fatalf("ambiguous scope yielded a marker: %v", err)
			}
		})
	}
}
