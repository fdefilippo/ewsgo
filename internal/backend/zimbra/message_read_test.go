// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"bytes"
	"encoding/xml"
	"errors"
	"fmt"
	"io"
	"net/http"
	"slices"
	"sync/atomic"
	"testing"

	"ewsgo/internal/core"
)

func TestMessageQueryRejectsIncompletePages(t *testing.T) {
	const hit = `<m id="9" l="2" sf="9"/>`
	const message = `<m id="9" l="2" rev="3" ms="7"><su>café 日本語</su></m>`
	tests := []struct {
		name, page, item string
		notFound         bool
		want             error
	}{
		{name: "second page", page: `more="0"`, item: message},
		{name: "omitted page marker", item: message, want: ErrProtocol},
		{name: "crossed folder", page: `more="0"`, item: `<m id="9" l="5" rev="3" ms="7"/>`, want: core.ErrConflict},
		{name: "disappearing hit", page: `more="0"`, notFound: true, want: core.ErrConflict},
		{name: "missing revision", page: `more="0"`, item: `<m id="9" l="2" ms="7"/>`, want: ErrProtocol},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			fallback := fakeZimbra(t)
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch req := readMessageRequest(t, r); req.XMLName.Local {
				case "GetFolderRequest":
					envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1" name="root"><folder id="2" l="1" view="message" name="Inbox"/><link id="21" l="1" view="message" name="Shared"/></folder></GetFolderResponse>`)
				case "SearchRequest":
					if textChild(req, "query") != "inid:2" || req.get("offset") != "1" || req.get("limit") != "1" || req.get("types") != "message" {
						t.Error("unexpected native search scope or continuation")
					}
					envelope(w, `<SearchResponse xmlns="urn:zimbraMail" sortBy="idAsc" offset="1" `+tt.page+`>`+hit+`</SearchResponse>`)
				case "GetMsgRequest":
					checkMessageSelector(t, req)
					if tt.notFound {
						fault(w, "mail.NO_SUCH_MSG")
					} else {
						envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail">`+tt.item+`</GetMsgResponse>`)
					}
				default:
					fallback(w, r)
				}
			}, Config{})
			box := &mailbox{native: login(t, client)}
			page, err := box.Query(t.Context(), core.Query{Kind: core.Message, Parent: "f:2", Offset: 1, Limit: 1})
			if !errors.Is(err, tt.want) {
				t.Fatalf("query error = %v, want %v", err, tt.want)
			}
			if tt.want != nil {
				if len(page.Objects) != 0 || page.NextOffset != 0 || page.More {
					t.Fatal("failed query returned a partial page")
				}
			} else if page.NextOffset != 2 || page.More || len(page.Objects) != 1 ||
				page.Objects[0].Revision != 7 || page.Objects[0].Parent != "f:2" ||
				page.Objects[0].Properties[core.ContentRevision] != core.Integer(3) ||
				page.Objects[0].Properties[core.Subject] != core.Text("café 日本語") {
				t.Fatal("unexpected second page")
			}
			if _, err := box.Query(t.Context(), core.Query{Kind: core.Message, Parent: "f:21"}); !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("shared folder must be outside the search scope: %v", err)
			}
		})
	}
}

func TestAttachmentReadRequiresCompleteCurrentContent(t *testing.T) {
	const parts = `<mp part="TEXT" ct="multipart/mixed"><mp part="1" ct="text/html"/><mp part="2" ct="application/octet-stream" filename="café.bin" cd="attachment"/></mp>`
	blob := []byte{0, 13, 128, 255, 0}
	tests := []struct {
		name, partID, metadata string
		status                 int
		truncated, revised     bool
		want                   error
	}{
		{name: "decoded bytes", partID: "m:9:3:2", metadata: parts},
		{name: "wrong message", partID: "m:8:3:2", metadata: parts, want: core.ErrNotFound},
		{name: "stale attachment ID", partID: "m:9:2:2", metadata: parts, want: core.ErrNotFound},
		{name: "missing multipart tree", partID: "m:9:3:2", metadata: `<mp part="TEXT" ct="multipart/mixed"/>`, want: ErrProtocol},
		{name: "missing part", partID: "m:9:3:2", metadata: parts, status: http.StatusNotFound, want: core.ErrNotFound},
		{name: "partial status", partID: "m:9:3:2", metadata: parts, status: http.StatusPartialContent, want: ErrProtocol},
		{name: "truncated body", partID: "m:9:3:2", metadata: parts, truncated: true, want: ErrTransport},
		{name: "revision changes", partID: "m:9:3:2", metadata: parts, revised: true, want: core.ErrConflict},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var fetched atomic.Bool
			fallback := fakeZimbra(t)
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					fetched.Store(true)
					q := r.URL.Query()
					cookie, err := r.Cookie("ZM_AUTH_TOKEN")
					if err != nil || cookie.Value != fixtureToken || r.URL.Path != "/service/home/~/" ||
						len(q) != 3 || q.Get("id") != "9" || q.Get("part") != "2" || q.Get("auth") != "co" {
						t.Error("unexpected REST attachment route or credential")
					}
					if tt.truncated {
						w.Header().Set("Content-Length", "50")
					}
					if tt.status != 0 {
						w.WriteHeader(tt.status)
					}
					_, _ = w.Write(blob)
					return
				}
				req := readMessageRequest(t, r)
				if req.XMLName.Local != "GetMsgRequest" {
					fallback(w, r)
					return
				}
				checkMessageSelector(t, req)
				revision := 3
				if tt.revised && fetched.Load() {
					revision = 4
				}
				envelope(w, fmt.Sprintf(`<GetMsgResponse xmlns="urn:zimbraMail"><m id="9" l="2" rev="%d">%s</m></GetMsgResponse>`, revision, tt.metadata))
			}, Config{})
			box := &mailbox{native: login(t, client)}
			attachment, err := box.ReadMailAttachment(t.Context(), "m:9", tt.partID)
			if !errors.Is(err, tt.want) {
				t.Fatalf("attachment error = %v, want %v", err, tt.want)
			}
			if tt.want == nil {
				if !bytes.Equal(attachment.Data, blob) || attachment.ID != tt.partID || attachment.MediaType != "application/octet-stream" {
					t.Fatal("unexpected decoded attachment")
				}
			} else if attachment.Data != nil {
				t.Fatal("failed read returned partial or stale attachment bytes")
			}
			if (tt.partID != "m:9:3:2" || tt.metadata != parts) && fetched.Load() {
				t.Fatal("an invalid selector reached the REST endpoint")
			}
		})
	}
}

func readMessageRequest(t *testing.T, r *http.Request) node {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	root, parseErr := parseXML(raw)
	body := root.child(soapNS, "Body")
	if err != nil || parseErr != nil || body == nil || len(body.Children) != 1 || root.find(contextNS, "account") != nil {
		t.Error("invalid SOAP envelope or explicit cross-mailbox selector")
		return node{}
	}
	r.Body = io.NopCloser(bytes.NewReader(raw))
	return body.Children[0]
}

func checkMessageSelector(t *testing.T, req node) {
	t.Helper()
	message := req.child(mailNS, "m")
	if message == nil || message.get("id") != "9" || message.get("read") != "0" || message.get("wantModSeq") != "1" {
		t.Error("unexpected GetMsg selector")
	}
}

func TestMessageSummaryMailCraftMetadata(t *testing.T) {
	var item node
	if err := xml.Unmarshal([]byte(`<m xmlns="urn:zimbraMail" id="9" l="2" rev="3" ms="7" cid="-9" f="ufa!" tn="Caffè,rosso\, verde,back\\slash"><e t="t" a="to@example.invalid" p="Zoë"/><e t="c" a="cc@example.invalid"/></m>`), &item); err != nil {
		t.Fatal(err)
	}
	got, err := messageSummary(item)
	if err != nil {
		t.Fatal(err)
	}
	if got.Properties[core.HasAttachments] != core.Bool(true) || got.Properties[core.IsFlagged] != core.Bool(true) ||
		got.Properties[core.IsRead] != core.Bool(false) || got.Properties[core.MessageImportance] != core.Integer(2) ||
		got.Properties[core.ConversationID] != core.Text("-9") {
		t.Fatal("message flags or conversation were not mapped")
	}
	if tags, ok := got.Properties[core.MailCategories].(core.Strings); !ok ||
		!slices.Equal(tags, []string{"Caffè", "rosso, verde", `back\slash`}) {
		t.Fatal("Zimbra tag separator or escaping was not respected")
	}
	if len(got.Recipients) != 2 || got.Recipients[0].DisplayName != "Zoë" || got.Recipients[1].Kind != core.RecipientCc {
		t.Fatal("recipient order and roles were not preserved")
	}
}
