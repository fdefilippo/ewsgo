// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestNativeMailFolderPlacement(t *testing.T) {
	name, parent, revision := "", "1", 0
	created, updated := 0, 0
	ctx := context.Background()
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var request node
		if err := xml.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error("invalid SOAP request")
			w.WriteHeader(400)
			return
		}
		header := request.child(soapNS, "Header").child("urn:zimbra", "context")
		if header == nil || header.child("urn:zimbra", "account") != nil {
			t.Error("folder operation used a foreign mailbox")
		}
		body := request.child(soapNS, "Body")
		if body == nil || len(body.Children) != 1 {
			t.Error("invalid SOAP body")
			w.WriteHeader(400)
			return
		}
		op := body.Children[0]
		switch op.XMLName.Local {
		case "GetFolderRequest":
			if op.get("tr") != "0" {
				t.Error("folder operation traversed a mount")
			}
			entry := ""
			if name != "" {
				entry = fmt.Sprintf(`<folder id="101" l="%s" name="%s" view="message" ms="%d"/>`, parent, name, revision)
			}
			top, child := entry, ""
			if parent == "120" {
				top, child = "", entry
			}
			response := `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1" name="Root">` +
				top + `<folder id="120" l="1" name="Target" view="message">` + child + `</folder>` +
				`<folder id="130" l="1" name="Calendar" view="appointment"/><link id="140"><folder id="141" l="140"/></link></folder></GetFolderResponse>`
			envelope(w, response)
		case "CreateFolderRequest":
			created++
			folder := op.child(mailNS, "folder")
			if folder == nil || folder.get("fie") != "0" || folder.get("view") != "message" || folder.get("l") != "1" {
				t.Error("CreateFolder omitted placement or allowed adoption")
				w.WriteHeader(400)
				return
			}
			if name != "" {
				fault(w, "mail.ALREADY_EXISTS")
				return
			}
			name, revision = folder.get("name"), 7
			envelope(w, `<CreateFolderResponse xmlns="urn:zimbraMail"><folder id="101"/></CreateFolderResponse>`)
		case "FolderActionRequest":
			updated++
			action := op.child(mailNS, "action")
			if action == nil || action.get("id") != "101" || action.get("op") != "update" ||
				action.get("name") != "Renamed" || action.get("l") != "120" {
				t.Error("FolderAction did not combine rename and move")
				w.WriteHeader(400)
				return
			}
			name, parent, revision = action.get("name"), action.get("l"), 8
			envelope(w, `<FolderActionResponse xmlns="urn:zimbraMail"><action id="101" op="update"/></FolderActionResponse>`)
		default:
			t.Error("unexpected native operation")
			w.WriteHeader(500)
		}
	}, Config{})
	box := &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}}
	request := core.MailFolderPlacement{Parent: "f:1", Name: "Initial"}
	folder, err := box.CreateMailFolder(ctx, request)
	if err != nil || folder.ID != "f:101" || folder.Parent != request.Parent || folder.Properties[core.DisplayName] != core.Text(request.Name) || folder.Revision != 7 {
		t.Fatal("create readback unavailable", err)
	}
	if _, err := box.CreateMailFolder(ctx, request); !errors.Is(err, core.ErrConflict) {
		t.Fatal("existing folder was adopted", err)
	}
	to := core.MailFolderPlacement{Parent: "f:120", Name: "Renamed"}
	moved, err := box.UpdateMailFolder(ctx, folder, to)
	if err != nil || moved.ID != folder.ID || moved.Parent != to.Parent || moved.Properties[core.DisplayName] != core.Text(to.Name) || moved.Revision != 8 {
		t.Fatal("move and rename readback unavailable", err)
	}
	if _, err := box.UpdateMailFolder(ctx, moved, to); err != nil {
		t.Fatal("verified no-op refused", err)
	}
	if _, err := box.UpdateMailFolder(ctx, folder, to); !errors.Is(err, core.ErrConflict) {
		t.Fatal("stale folder precondition accepted", err)
	}
	for _, p := range []core.ID{"f:130", "f:141", "f:1:other"} {
		if _, err := box.CreateMailFolder(ctx, core.MailFolderPlacement{Parent: p, Name: "Forbidden"}); err == nil {
			t.Fatal("non-mail or outside parent accepted")
		}
	}
	if created != 2 || updated != 1 {
		t.Fatal("rejected or repeated action wrote to native mailbox")
	}
}

func TestAcknowledgedNativeFolderWithoutReadbackReturnsID(t *testing.T) {
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var request node
		if err := xml.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error("invalid SOAP")
			return
		}
		switch request.child(soapNS, "Body").Children[0].XMLName.Local {
		case "GetFolderRequest":
			envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1" name="Root"/></GetFolderResponse>`)
		case "CreateFolderRequest":
			envelope(w, `<CreateFolderResponse xmlns="urn:zimbraMail"><folder id="101"/></CreateFolderResponse>`)
		default:
			t.Error("unexpected native action")
		}
	}, Config{})
	box := &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}}
	folder, err := box.CreateMailFolder(context.Background(), core.MailFolderPlacement{Parent: "f:1", Name: "Pending"})
	if !errors.Is(err, core.ErrNotFound) || folder.ID != "f:101" || folder.Parent != "" {
		t.Fatal("acknowledged creation could not be reconciled", err)
	}
}
