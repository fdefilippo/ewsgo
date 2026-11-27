// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"ewsgo/internal/core"
)

type activityProbe struct{ events []core.Activity }

func (*activityProbe) Authenticated(string)         {}
func (p *activityProbe) Record(event core.Activity) { p.events = append(p.events, event) }

func TestSOAPAndRESTActivitiesNeverContainValues(t *testing.T) {
	client := testClient(t, fakeZimbra(t), Config{})
	probe := &activityProbe{}
	ctx := core.WithActivityObserver(context.Background(), probe)
	session, err := client.Authenticate(ctx, "PRIVATE-attempted-user", "synthetic-password")
	if err != nil {
		t.Fatal("fixture authentication failed")
	}
	if _, err := session.Mailbox().Query(ctx, core.Query{Kind: core.Folder, Limit: 100}); err != nil {
		t.Fatal("fixture folders failed")
	}
	native := session.(*accountSession).native
	if _, err := native.MIME(ctx, "PRIVATE-native-id"); err != nil {
		t.Fatal("fixture MIME failed")
	}
	if err := session.Close(ctx); err != nil {
		t.Fatal("fixture logout failed")
	}
	want := []string{"Auth", "GetInfo", "GetFolder", "GetMIME", "EndSession"}
	if len(probe.events) != len(want) {
		t.Fatal("missing backend activity")
	}
	for i, event := range probe.events {
		if event.Operation != want[i] || event.Layer != "backend" || event.Result != core.ActivitySuccess || event.Duration <= 0 {
			t.Fatal("incorrect backend activity")
		}
	}
	raw, _ := json.Marshal(probe.events)
	for _, private := range []string{"PRIVATE", fixtureToken, "synthetic-password", "lab@example.invalid", "Synthetic HTML", "urn:zimbra", "fmt=raw"} {
		if strings.Contains(string(raw), private) {
			t.Fatal("private value in backend activity")
		}
	}
}

func TestEndSessionFaultRecordsOnlyTechnicalOutcome(t *testing.T) {
	fixture := fakeZimbra(t)
	client := testClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		if strings.Contains(string(raw), "EndSessionRequest") {
			fault(w, "service.FAILURE")
			return
		}
		r.Body = io.NopCloser(strings.NewReader(string(raw)))
		fixture(w, r)
	}), Config{})
	session := login(t, client)
	probe := &activityProbe{}
	ctx := core.WithActivityObserver(t.Context(), probe)
	if err := session.Logout(ctx); err == nil {
		t.Fatal("SOAP fault did not reach the caller")
	}
	events, _ := json.Marshal(probe.events)
	if len(probe.events) != 1 || probe.events[0].Operation != "EndSession" || probe.events[0].Result != core.ActivityFailure ||
		strings.Contains(string(events), "PRIVATE") || strings.Contains(string(events), fixtureToken) {
		t.Fatalf("incorrect logout diagnostic: %s", events)
	}
}
