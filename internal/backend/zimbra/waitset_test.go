// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"errors"
	"io"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestMailboxWatchRejectsOtherAccountsAndPreservesOpaqueSequence(t *testing.T) {
	var waits atomic.Int32
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		root, err := parseXML(raw)
		body := root.child(soapNS, "Body")
		if err != nil || body == nil || len(body.Children) != 1 {
			w.WriteHeader(400)
			t.Error("missing WaitSet request body")
			return
		}
		req := body.Children[0]
		switch req.XMLName.Local {
		case "CreateWaitSetRequest":
			account := req.find(mailNS, "a")
			if account == nil || account.get("id") != "own" || req.get("defTypes") != "f,m" {
				t.Error("unexpected WaitSet scope")
			}
			envelope(w, `<CreateWaitSetResponse xmlns="urn:zimbraMail" waitSet="set" seq="start-id"/>`)
		case "WaitSetRequest":
			if req.get("waitSet") != "set" || req.get("block") != "1" {
				t.Error("unexpected wait")
			}
			switch attempt := waits.Add(1); attempt {
			case 1:
				if req.get("seq") != "start-id" {
					t.Error("creation sequence missing")
				}
				envelope(w, `<WaitSetResponse xmlns="urn:zimbraMail" waitSet="set" seq="sequence/2"><a id="own"/></WaitSetResponse>`)
			case 2, 3:
				if req.get("seq") != "sequence/2" {
					t.Error("unvalidated or altered sequence")
				}
				if attempt == 2 {
					envelope(w, `<WaitSetResponse xmlns="urn:zimbraMail" waitSet="set" seq="unauthorized"><a id="other"/></WaitSetResponse>`)
				} else {
					fault(w, "admin.NO_SUCH_WAITSET")
				}
			default:
				t.Error("extra WaitSet call")
			}
		case "DestroyWaitSetRequest":
			fault(w, "admin.NO_SUCH_WAITSET")
		default:
			t.Error("unexpected request")
			w.WriteHeader(400)
		}
	}, Config{})
	session := &accountSession{
		native:   &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Minute)},
		identity: core.Identity{Scope: core.Scope{Backend: "zimbra", Account: "own"}},
	}
	watch, err := session.OpenMailboxWatch(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if changed, err := watch.Wait(t.Context()); !changed || err != nil {
		t.Fatalf("own notification: changed=%t, err=%v", changed, err)
	}
	if changed, err := watch.Wait(t.Context()); changed || !errors.Is(err, ErrProtocol) {
		t.Fatalf("cross-account notification: changed=%t, err=%v", changed, err)
	}
	if changed, err := watch.Wait(t.Context()); changed || !errors.Is(err, core.ErrResync) {
		t.Fatalf("missing set: changed=%t, err=%v", changed, err)
	}
	if err := watch.Close(t.Context()); err != nil {
		t.Fatalf("destroy of missing set: %v", err)
	}
}
