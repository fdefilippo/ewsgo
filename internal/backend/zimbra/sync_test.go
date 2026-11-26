// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"errors"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestNativeSyncParsingAndScope(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		switch {
		case strings.Contains(string(raw), "SyncRequest"):
			if strings.Contains(string(raw), `token="expired"`) {
				fault(w, "mail.MUST_RESYNC")
				return
			}
			envelope(w, `<SyncResponse xmlns="urn:zimbraMail" token="synthetic-next" more="1"><folder id="2"><m ids="9,10"/></folder><m id="11" l="2"/><deleted><m ids="12,13"/><folder ids="99"/></deleted></SyncResponse>`)
		case strings.Contains(string(raw), "GetMsgRequest"):
			envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="9" l="2" ms="7" rev="3" f="u" d="1794736800000"><su>Synthetic</su></m></GetMsgResponse>`)
		default:
			t.Fatal("unexpected operation")
		}
	}, Config{})
	session := &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Minute)}
	result, err := session.Sync(context.Background(), "", "2", 1)
	if err != nil || len(result.Messages) != 3 || len(result.Deleted) != 2 || !result.More {
		t.Fatal("typed sync parsing")
	}
	box := &mailbox{native: session, scope: core.Scope{Backend: "synthetic", Account: "one"}}
	if _, err = box.SyncFolder(context.Background(), "f:3", core.Cursor(`{"Scope":{"Backend":"synthetic","Account":"one"},"Folder":"f:2","Token":"x"}`), 1); !errors.Is(err, core.ErrResync) {
		t.Fatal("foreign folder cursor accepted")
	}
	if _, err = session.Sync(context.Background(), "expired", "2", 1); !errors.Is(semanticError(err), core.ErrResync) {
		t.Fatal("MUST_RESYNC not normalized")
	}
	message, err := session.Message(context.Background(), "9")
	if err != nil || message.MetadataRevision != 7 || message.ContentRevision != 3 {
		t.Fatal("revision fields conflated")
	}
	for _, id := range []string{"0", "2:other-account", "../3"} {
		if _, err = session.Sync(context.Background(), "", id, 1); err == nil {
			t.Fatal("invalid scope sent")
		}
	}
}
