// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestMailFolderObservationPaginatesLargeMembership(t *testing.T) {
	total := core.MaxMailContentMessages + 1
	sequence, searchReads := 8, 0
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		var request node
		if err := xml.NewDecoder(r.Body).Decode(&request); err != nil {
			t.Error("invalid SOAP request", err)
			w.WriteHeader(500)
			return
		}
		req := request.child(soapNS, "Body").Children[0]
		switch req.XMLName.Local {
		case "GetFolderRequest":
			envelope(w, fmt.Sprintf(`<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1"><folder id="3" l="1" view="message" n="%d" i4ms="%d"/></folder></GetFolderResponse>`, total, sequence))
		case "SearchRequest":
			searchReads++
			offset, err := strconv.Atoi(req.get("offset"))
			if err != nil || offset < 0 || offset >= total || req.get("types") != "message" || req.get("limit") != "100" ||
				req.child(mailNS, "query").Text != "inid:3" {
				t.Error("unexpected folder observation search scope")
				w.WriteHeader(500)
				return
			}
			var rows strings.Builder
			end := min(offset+100, total)
			for index := offset; index < end; index++ {
				fmt.Fprintf(&rows, `<m id="%d" l="3" rev="1"/>`, 1000+index)
			}
			envelope(w, fmt.Sprintf(`<SearchResponse xmlns="urn:zimbraMail" offset="%d" more="%t">%s</SearchResponse>`, offset, end < total, rows.String()))
		default:
			t.Error("folder observation requested message metadata or MIME")
			w.WriteHeader(500)
		}
	}, Config{})
	box := &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}}
	first, err := box.ObserveMailFolder(t.Context(), "f:3", nil)
	if err != nil || len(first.MessageIDs) != total || searchReads != 3 {
		t.Fatalf("observation could not resolve all pages: %v", err)
	}
	same, err := box.ObserveMailFolder(t.Context(), "f:3", &first.Sequence)
	if err != nil || same.Sequence != first.Sequence || len(same.MessageIDs) != 0 || same.MessageCount != first.MessageCount || searchReads != 3 {
		t.Fatal("unchanged marker caused another paginated scan")
	}
}

func TestMailContentIndexVerifiesCompleteStableMembership(t *testing.T) {
	for _, scenario := range []string{"complete", "missing_member", "sequence_changed", "misbound_metadata"} {
		t.Run(scenario, func(t *testing.T) {
			folderReads, searchReads := 0, 0
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				var request node
				if r.Method != http.MethodPost || xml.NewDecoder(r.Body).Decode(&request) != nil {
					t.Error("unexpected body read or malformed SOAP")
					w.WriteHeader(500)
					return
				}
				req := request.child(soapNS, "Body").Children[0]
				switch req.XMLName.Local {
				case "GetFolderRequest":
					folderReads++
					seq := 8
					if scenario == "sequence_changed" {
						seq += folderReads
					}
					envelope(w, fmt.Sprintf(`<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1"><folder id="2" l="1" view="message" n="2" i4ms="%d"/></folder></GetFolderResponse>`, seq))
				case "SearchRequest":
					searchReads++
					offset := req.get("offset")
					if req.get("types") != "message" || req.get("limit") != "100" {
						t.Error("unexpected search scope")
					}
					if offset == "0" {
						more := 1
						if scenario == "missing_member" {
							more = 0
						}
						envelope(w, fmt.Sprintf(`<SearchResponse xmlns="urn:zimbraMail" offset="0" more="%d"><m id="91" l="2" rev="3"/></SearchResponse>`, more))
					} else {
						if offset != "1" {
							t.Error("invalid second offset")
						}
						envelope(w, `<SearchResponse xmlns="urn:zimbraMail" offset="1" more="0"><m id="92" l="2" rev="6"/></SearchResponse>`)
					}
				case "BatchRequest":
					if len(req.Children) != 2 || req.Children[0].child(mailNS, "m").get("read") != "0" {
						t.Error("unexpected metadata request")
					}
					id := 92
					if scenario == "misbound_metadata" {
						id = 93
					}
					envelope(w, fmt.Sprintf(`<BatchResponse xmlns="urn:zimbra">
<GetMsgResponse xmlns="urn:zimbraMail" requestId="1"><m id="91" l="2" ms="7" rev="3" md="1794736800" d="1794736800000" s="120"/></GetMsgResponse>
<GetMsgResponse xmlns="urn:zimbraMail" requestId="2"><m id="%d" l="2" ms="8" rev="6" md="1794736800" d="1794736800000" s="120"/></GetMsgResponse></BatchResponse>`, id))
				default:
					t.Error("unexpected backend operation")
					w.WriteHeader(500)
				}
			}, Config{})
			box := &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}}
			got, err := box.ReadMailContentIndex(t.Context(), "f:2")
			switch scenario {
			case "complete":
				if err != nil || len(got.Messages) != 2 || searchReads != 2 || folderReads != 2 {
					t.Fatalf("complete index failed: %v", err)
				}
				for _, message := range got.Messages {
					if len(message.MIME) != 0 || message.Properties[core.VersionedMIME] != core.Bool(true) {
						t.Fatal("index retained MIME or omitted its revision guarantee")
					}
				}
			case "misbound_metadata":
				if !errors.Is(err, ErrProtocol) || len(got.Messages) != 0 {
					t.Fatal("misbound metadata produced an index")
				}
			default:
				if !errors.Is(err, core.ErrConflict) || len(got.Messages) != 0 {
					t.Fatal("incomplete or changing membership produced an index")
				}
			}
		})
	}
}
