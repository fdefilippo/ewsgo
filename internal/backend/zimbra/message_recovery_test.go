// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"errors"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestRecoverableMailContinuationKeepsFolderScope(t *testing.T) {
	for _, tt := range []struct {
		name, secondPage string
		want             error
	}{
		{name: "in scope", secondPage: `<m id="10" l="2" sf="10"/>`},
		{name: "other folder", secondPage: `<m id="10" l="5" sf="10"/>`, want: ErrProtocol},
		{name: "repeated cursor", secondPage: `<m id="9" l="2" sf="9"/>`, want: ErrProtocol},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var pages, dates atomic.Int32
			fallback := fakeZimbra(t)
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodHead {
					dates.Add(1)
					query := r.URL.Query()
					cookie, err := r.Cookie("ZM_AUTH_TOKEN")
					if r.URL.Path != "/service/content/get" || query.Get("dumpster") != "1" || query.Get("sync") != "1" || err != nil || cookie.Value != fixtureToken {
						t.Error("unexpected recoverable date request")
					}
					switch query.Get("id") {
					case "9":
						w.Header().Set("X-Zimbra-Modified", "1794736800123")
						w.Header().Set("X-Zimbra-Flags", "u")
					case "10":
						w.WriteHeader(http.StatusNotFound)
					default:
						t.Error("unexpected native date selector")
						w.WriteHeader(http.StatusBadRequest)
					}
					return
				}
				switch req := readMessageRequest(t, r); req.XMLName.Local {
				case "GetInfoRequest":
					if req.get("sections") != "attrs" {
						fallback(w, r)
						return
					}
					envelope(w, `<GetInfoResponse xmlns="urn:zimbraAccount"><attrs><attr name="zimbraDumpsterEnabled">TRUE</attr></attrs></GetInfoResponse>`)
				case "GetFolderRequest":
					envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1" name="root"><folder id="2" l="1" view="message" name="Inbox"/></folder></GetFolderResponse>`)
				case "SearchRequest":
					if textChild(req, "query") != "inid:2" || req.get("inDumpster") != "1" || req.get("types") != "message" || req.get("sortBy") != "idAsc" || req.get("recip") != "2" {
						t.Error("unexpected recoverable search scope")
					}
					cursor := req.child(mailNS, "cursor")
					switch pages.Add(1) {
					case 1:
						if cursor != nil || req.get("offset") != "0" {
							t.Error("unexpected first-page cursor")
						}
						envelope(w, `<SearchResponse xmlns="urn:zimbraMail" sortBy="idAsc" offset="0" more="1"><m id="9" l="2" sf="9"><e t="f" a="sender@example.test" p="Sender Name"/><e t="t" a="first@example.test" p="First Recipient"/><e t="t" a="second@example.test"/><e t="c" a="cc@example.test"/></m></SearchResponse>`)
					case 2:
						if cursor == nil || cursor.get("id") != "9" || cursor.get("sortVal") != "9" || req.get("offset") != "" {
							t.Error("unexpected continuation cursor")
						}
						envelope(w, `<SearchResponse xmlns="urn:zimbraMail" sortBy="idAsc" more="0">`+tt.secondPage+`</SearchResponse>`)
					default:
						t.Error("unexpected further search page")
						fault(w, "service.FAILURE")
					}
				default:
					fallback(w, r)
				}
			}, Config{})
			box := &mailbox{native: login(t, client)}
			items, err := box.ReadRecoverableMail(t.Context(), "f:2")
			if !errors.Is(err, tt.want) || pages.Load() != 2 {
				t.Fatalf("search pages = %d, error = %v, want 2 pages and %v", pages.Load(), err, tt.want)
			}
			if tt.want != nil {
				if len(items) != 0 || dates.Load() != 0 {
					t.Fatal("failed search returned or enriched a partial result")
				}
				return
			}
			if len(items) != 2 || items[0].ID != "m:9" || items[1].ID != "m:10" || items[0].Parent != "f:2" || items[1].Parent != "f:2" {
				t.Fatal("unexpected recoverable messages")
			}
			if items[0].Properties[core.SentRepresentingName] != core.Text("Sender Name") || items[0].Properties[core.DisplayTo] != core.Text("First Recipient; second@example.test") ||
				items[1].Properties[core.SentRepresentingName] != core.Text("") || items[1].Properties[core.DisplayTo] != core.Text("") {
				t.Fatal("unexpected recoverable envelope")
			}
			if dates.Load() != 2 || items[0].Properties[core.DeletedOn] != core.Timestamp(time.UnixMilli(1794736800123).UTC()) || items[1].Properties[core.DeletedOn] != nil {
				t.Fatal("unexpected recoverable deletion times")
			}
		})
	}
}

func TestRecoverMailMessageRechecksExactOriginAndAcknowledgement(t *testing.T) {
	for _, tt := range []struct {
		name, origin, ack string
		want              error
	}{
		{"known message", "f:2", "recover", nil},
		{"wrong origin", "f:5", "recover", core.ErrNotFound},
		{"uncertain acknowledgement", "f:2", "copy", core.ErrOutcomeUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var writes, searches atomic.Int32
			fallback := fakeZimbra(t)
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch req := readMessageRequest(t, r); req.XMLName.Local {
				case "GetInfoRequest":
					if req.get("sections") != "attrs" {
						fallback(w, r)
						return
					}
					envelope(w, `<GetInfoResponse xmlns="urn:zimbraAccount"><attrs><attr name="zimbraDumpsterEnabled">TRUE</attr></attrs></GetInfoResponse>`)
				case "GetFolderRequest":
					envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1"><folder id="5" l="1" view="message"/></folder></GetFolderResponse>`)
				case "SearchRequest":
					if req.get("inDumpster") != "1" || req.get("types") != "message" || textChild(req, "query") != "item:9" {
						t.Error("recovery search was not scoped to the original message")
					}
					searches.Add(1)
					matches := `<m id="9" l="2"/>`
					if writes.Load() > 0 {
						matches = ""
					}
					envelope(w, `<SearchResponse xmlns="urn:zimbraMail" more="0">`+matches+`</SearchResponse>`)
				case "ItemActionRequest":
					action := req.child(mailNS, "action")
					if action == nil || action.get("op") != "recover" || action.get("id") != "9" || action.get("l") != "5" {
						t.Error("recovery affected an unexpected message or destination")
					}
					writes.Add(1)
					envelope(w, `<ItemActionResponse xmlns="urn:zimbraMail"><action id="9" op="`+tt.ack+`"/></ItemActionResponse>`)
				default:
					fallback(w, r)
				}
			}, Config{})
			box := &mailbox{native: login(t, client)}
			err := box.RecoverMailMessage(t.Context(), "m:9", core.ID(tt.origin), "f:5")
			wantSearches, wantWrites := int32(1), int32(1)
			switch tt.want {
			case nil:
				wantSearches = 2
			case core.ErrNotFound:
				wantWrites = 0
			}
			if !errors.Is(err, tt.want) || searches.Load() != wantSearches || writes.Load() != wantWrites {
				t.Fatalf("recovery error=%v, searches=%d, mutations=%d", err, searches.Load(), writes.Load())
			}
		})
	}
}
