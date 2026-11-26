// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"io"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"ewsgo/internal/core"
)

const attachmentFixture = "MIME-Version: 1.0\r\nContent-Type: multipart/mixed; boundary=fixture\r\n\r\n" +
	"--fixture\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nCaffè ☕\r\n" +
	"--fixture\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=fixture.bin\r\n" +
	"Content-Transfer-Encoding: base64\r\n\r\nAP8Q\r\n--fixture--\r\n"

type profileMailbox struct{ fixtureMailbox }

func (*profileMailbox) ReadMIME(_ context.Context, _ core.ID) ([]byte, error) {
	return []byte(attachmentFixture), nil
}

func TestMailCraftShapesAndVersion(t *testing.T) {
	h := New()
	object := core.Object{ID: "m:100", Parent: "f:2", Kind: core.Message, Revision: 8,
		Properties: core.Properties{core.Subject: core.Text("Caffè ☕"), core.Preview: core.Text("Résumé"),
			core.MailCategories: core.Strings{"Verde, blu"}, core.IsFlagged: core.Bool(true), core.HasAttachments: core.Bool(true),
			core.ContentRevision: core.Integer(5), core.ConversationID: core.Text("-100")},
		Recipients: []core.Recipient{{Address: "to@example.invalid", DisplayName: "Zoë", Kind: core.RecipientTo}}}
	folder := core.Object{ID: "f:2", Parent: "f:1", Kind: core.Folder, Revision: 12, Properties: core.Properties{
		core.DisplayName: core.Text("Ricevuta"), core.FolderItemCount: core.Integer(3), core.FolderUnreadCount: core.Integer(0),
	}}
	box := &profileMailbox{fixtureMailbox{objects: map[core.ID]core.Object{object.ID: object, folder.ID: folder}}}
	session := &testSession{box: box}
	scope := session.Identity().Scope
	call := func(version string, operation node) (int, node) {
		t.Helper()
		header := el(soap, "Header", "", at(el(types, "RequestServerVersion", ""), "Version", version))
		request, err := xml.Marshal(el(soap, "Envelope", "", header, el(soap, "Body", "", operation)))
		if err != nil {
			t.Fatal(err)
		}
		w := httptest.NewRecorder()
		h.Serve(w, httptest.NewRequest("POST", "https://example.invalid/EWS/Exchange.asmx", bytes.NewReader(request)), session)
		reply, err := parse(w.Body.Bytes())
		if err != nil {
			t.Fatalf("invalid response XML: %v", err)
		}
		return w.Code, reply
	}
	get := func(children ...node) node {
		return el(messages, "GetItem", "", el(messages, "ItemShape", "", children...),
			el(messages, "ItemIds", "", at(el(types, "ItemId", ""), "Id", h.itemID(scope, object))))
	}
	uri := func(value string) node { return at(el(types, "FieldURI", ""), "FieldURI", value) }

	status, reply := call("invalid", get())
	if status != http.StatusInternalServerError || reply.find(soap, "Fault") == nil || reply.find(ewsErrors, "ResponseCode").Text != "ErrorInvalidServerVersion" {
		t.Fatal("unsupported version must use a SOAP fault")
	}
	extra := el(types, "AdditionalProperties", "", uri("item:Flag"), uri("item:Preview"))
	_, reply = call("Exchange2010_SP2", get(extra))
	if reply.find(messages, "ResponseCode").Text != "ErrorInvalidPropertyRequest" {
		t.Fatal("fallback version accepted newer fields")
	}
	_, reply = call("Exchange2010_SP2", get())
	if reply.find(messages, "ResponseCode").Text != "NoError" || reply.find(types, "Flag") != nil {
		t.Fatal("fallback version did not return a compatible message")
	}
	_, reply = call("Exchange2013", get())
	for name, value := range map[string]string{"Subject": "Caffè ☕", "Name": "Zoë", "String": "Verde, blu", "FlagStatus": "Flagged", "HasAttachments": "true"} {
		wire := reply.find(types, name)
		if wire == nil || wire.Text != value {
			t.Fatalf("missing or incorrect %s", name)
		}
	}
	_, reply = call("Exchange2013", get(el(types, "BaseShape", "IdOnly"),
		el(types, "AdditionalProperties", "", uri("item:ConversationId"), uri("message:ToRecipients")), el(types, "IncludeMimeContent", "true")))
	response := reply.find(types, "Message")
	if response == nil || len(response.Children) != 4 || response.find(types, "ConversationId").attr("Id") == "-100" {
		t.Fatal("IdOnly shape did not return just the requested properties and MIME")
	}
	raw, err := base64.StdEncoding.DecodeString(response.find(types, "MimeContent").Text)
	if err != nil || string(raw) != attachmentFixture {
		t.Fatal("MIME changed on the wire")
	}
	body := strings.SplitN(string(raw), "\r\n\r\n", 2)[1]
	parts := multipart.NewReader(strings.NewReader(body), "fixture")
	if _, err := parts.NextPart(); err != nil {
		t.Fatal(err)
	}
	part, err := parts.NextPart()
	if err != nil {
		t.Fatal(err)
	}
	attachment, err := io.ReadAll(base64.NewDecoder(base64.StdEncoding, part))
	if err != nil || !bytes.Equal(attachment, []byte{0, 255, 16}) {
		t.Fatal("binary attachment did not round-trip")
	}
	changeKey := response.find(types, "ItemId").attr("ChangeKey")
	object.Properties[core.ContentRevision] = core.Integer(6)
	if changeKey == h.changeKey(scope, object) {
		t.Fatal("content edit did not update ChangeKey")
	}
	changeKey = h.changeKey(scope, object)
	object.Revision++
	if changeKey == h.changeKey(scope, object) {
		t.Fatal("metadata edit did not update ChangeKey")
	}

	_, reply = call("Exchange2013", el(messages, "GetFolder", "",
		el(messages, "FolderIds", "", at(el(types, "FolderId", ""), "Id", h.encode(scope, folder.ID)))))
	if reply.find(types, "TotalCount").Text != "3" || reply.find(types, "UnreadCount").Text != "0" || reply.find(types, "ChildFolderCount") != nil ||
		reply.find(types, "FolderId").attr("ChangeKey") != h.changeKey(scope, folder) {
		t.Fatal("folder did not expose native counts and revision")
	}
	status, reply = call("Exchange2013", el(messages, "NotImplemented", ""))
	if status != http.StatusInternalServerError || reply.find(ewsErrors, "ResponseCode").Text != "ErrorInvalidRequest" {
		t.Fatal("unsupported operation did not report SOAP fault")
	}
}
