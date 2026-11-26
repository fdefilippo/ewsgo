// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"

	"ewsgo/internal/core"
)

func TestMailboxOwnerIndependentOfPreferenceFailure(t *testing.T) {
	var preferences atomic.Int32
	fallback := fakeZimbra(t)
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			http.Error(w, "invalid request", http.StatusBadRequest)
			return
		}
		if bytes.Contains(raw, []byte("GetPrefsRequest")) {
			preferences.Add(1)
			http.Error(w, "unavailable", http.StatusServiceUnavailable)
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(raw))
		fallback(w, r)
	}, Config{})
	ctx := context.Background()
	session, err := client.Authenticate(ctx, "lab@example.invalid", "synthetic-password")
	if err != nil {
		t.Fatal(err)
	}
	box := session.Mailbox().(*mailbox)
	if owner, err := box.MailboxOwner(ctx); !owner || err != nil || preferences.Load() != 0 {
		t.Fatalf("owner=%v err=%v preference requests=%d", owner, err, preferences.Load())
	}
	if _, err := box.MailboxState(ctx); err == nil || preferences.Load() != 1 {
		t.Fatalf("expected preference failure, err=%v requests=%d", err, preferences.Load())
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if owner, err := box.MailboxOwner(canceled); owner || !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled owner=%v err=%v", owner, err)
	}
	if err := session.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if owner, err := box.MailboxOwner(ctx); owner || !errors.Is(err, core.ErrAuthentication) {
		t.Fatalf("closed owner=%v err=%v", owner, err)
	}
}

func TestMailboxStateReadsOnlyRequestedPreferenceAndRestrictsProviderRights(t *testing.T) {
	for _, value := range []string{"TRUE", "false", "1", "0", "missing", "invalid", "", "duplicate", "unrequested"} {
		t.Run(value, func(t *testing.T) {
			fallback := fakeZimbra(t)
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				raw, _ := io.ReadAll(r.Body)
				root, err := parseXML(raw)
				if err != nil {
					t.Error("request XML")
					http.Error(w, "invalid request", 400)
					return
				}
				body := root.child(soapNS, "Body")
				if body.Children[0].XMLName.Local != "GetPrefsRequest" {
					r.Body = io.NopCloser(bytes.NewReader(raw))
					fallback(w, r)
					return
				}
				request := body.Children[0]
				if len(request.Children) != 1 || request.Children[0].get("name") != "zimbraPrefOutOfOfficeReplyEnabled" || root.find(contextNS, "account") != nil {
					t.Error("unbounded preferences or other mailbox selector")
				}
				pref := `<pref name="zimbraPrefOutOfOfficeReplyEnabled">` + value + `</pref>`
				if value == "missing" {
					pref = ""
				}
				if value == "duplicate" {
					pref += pref
				}
				if value == "unrequested" {
					pref = `<pref name="unrequested">FALSE</pref>`
				}
				envelope(w, `<GetPrefsResponse xmlns="urn:zimbraAccount">`+pref+`</GetPrefsResponse>`)
			}, Config{})
			session, err := client.Authenticate(context.Background(), "lab@example.invalid", "synthetic-password")
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close(context.Background())
			state, err := session.Mailbox().(core.MailboxStateReader).MailboxState(context.Background())
			if value == "invalid" || value == "" || value == "duplicate" || value == "unrequested" {
				if err == nil {
					t.Fatal("invalid preference accepted")
				}
				return
			}
			if err != nil || !state.Owner || state.CanSend || state.OutOfOffice != (value == "TRUE" || value == "1") {
				t.Fatal("mailbox state or exposed rights", err)
			}
		})
	}
}
