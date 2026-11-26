// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"encoding/xml"
	"errors"
	"net/http"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestReplaceFolderUserGrants(t *testing.T) {
	const first = "47b8acb8-febd-4824-b27b-9e65d8b82040"
	const second = "e3da56ca-90b6-4636-a1ad-52435c5d6d02"
	const read = core.AccessReadItems | core.AccessVisible
	firstRow := `<grant gt="usr" zid="` + first + `" perm="r" expiry="1999999999999"/>`
	secondRow := `<grant gt="usr" zid="` + second + `" perm="r"/>`
	newFirst := `<grant gt="usr" zid="` + first + `" perm="ri" expiry="1999999999999"/>`
	expiry := time.UnixMilli(2000000000000).UTC()
	for _, tc := range []struct {
		name, fault, extra                                string
		finite, remote, stale, inherited, mismatch, clear bool
		want                                              error
		calls                                             int
	}{
		{name: "update the first grant and add the second", calls: 1},
		{name: "clear all user grants", clear: true, calls: 1},
		{name: "stale observation", stale: true, want: core.ErrConflict},
		{name: "stale native revision", fault: "mail.MODIFY_CONFLICT", want: core.ErrConflict, calls: 1},
		{name: "uncertain native reply", fault: "service.FAILURE", want: core.ErrOutcomeUnknown, calls: 1},
		{name: "different ACL on readback", mismatch: true, want: core.ErrOutcomeUnknown, calls: 1},
		{name: "finite policy", finite: true, want: core.ErrUnsupported},
		{name: "cross domain subject", remote: true, want: core.ErrAccessDenied},
		{name: "inherited ACL", inherited: true, want: core.ErrUnsupported},
		{name: "raw grant metadata", extra: ` other="present"`, want: core.ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes := 0
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				var req node
				if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error("invalid SOAP fixture request")
					return
				}
				body := req.child(soapNS, "Body")
				switch {
				case body.child(mailNS, "GetFolderRequest") != nil:
					get := body.child(mailNS, "GetFolderRequest")
					if get.child(mailNS, "folder").get("l") == "11" {
						envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="11"/></GetFolderResponse>`)
						return
					}
					start := `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1" l="11">`
					end := `</folder></GetFolderResponse>`
					if tc.inherited {
						envelope(w, start+`<folder id="24" l="1" view="message" ms="41"/>`+end)
						return
					}
					if writes == 0 {
						old := firstRow[:len(firstRow)-2] + tc.extra + "/>"
						envelope(w, start+`<folder id="24" l="1" view="message" ms="41" f="i"><acl>`+old+`</acl></folder>`+end)
						return
					}
					rows := newFirst + secondRow
					if tc.mismatch {
						rows = firstRow + secondRow
					}
					if tc.clear {
						rows = ""
					}
					envelope(w, start+`<folder id="24" l="1" view="message" ms="42" f="i"><acl>`+rows+`</acl></folder>`+end)
				case body.child(accountNS, "GetInfoRequest") != nil:
					lifetime := "0ms"
					if tc.finite {
						lifetime = "30d"
					}
					envelope(w, `<GetInfoResponse xmlns="urn:zimbraAccount"><attrs><attr name="zimbraShareLifetime">`+lifetime+`</attr><attr name="zimbraExternalShareLifetime">0ms</attr></attrs></GetInfoResponse>`)
				case body.child(accountNS, "SearchGalRequest") != nil:
					address := "second@example.test"
					if tc.remote {
						address = "second@example.invalid"
					}
					envelope(w, `<SearchGalResponse xmlns="urn:zimbraAccount" more="0"><cn><a n="zimbraId">`+first+`</a><a n="email">first@example.test</a></cn><cn><a n="zimbraId">`+second+`</a><a n="email">`+address+`</a></cn></SearchGalResponse>`)
				default:
					writes++
					header := req.child(soapNS, "Header").child(contextNS, "context")
					change := header.child(contextNS, "change")
					a := body.child(mailNS, "FolderActionRequest").child(mailNS, "action")
					if change == nil || change.get("token") != "41" || change.get("type") != "mod" ||
						a == nil || a.get("id") != "24" || a.get("op") != "update" || hasOtherACLAttributes(*a, "id", "op") || len(a.Children) != 1 {
						t.Error("write was not a single revision-guarded ACL update")
						fault(w, "service.INVALID_REQUEST")
						return
					}
					acl := a.child(mailNS, "acl")
					if acl == nil || tc.clear && len(acl.Children) != 0 || !tc.clear && (len(acl.Children) != 2 ||
						acl.Children[0].get("perm") != "ri" || acl.Children[0].get("expiry") != "1999999999999" ||
						acl.Children[1].get("zid") != second) {
						t.Error("unexpected replacement ACL")
					}
					if tc.fault != "" {
						fault(w, tc.fault)
						return
					}
					envelope(w, `<FolderActionResponse xmlns="urn:zimbraMail"><action id="24" op="update"/></FolderActionResponse>`)
				}
			}, Config{})
			box := &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)},
				scope: core.Scope{Backend: "zimbra", Account: "owner"}, owner: "owner@example.test"}
			ctx := t.Context()
			before, err := box.ReadFolderAccess(ctx, "f:24")
			if err != nil {
				t.Fatal(err)
			}
			if tc.stale {
				before.Token[0] ^= 1
			}
			desired := []core.AccessGrant{
				{Kind: core.AccessUser, Subject: first, Rights: read | core.AccessCreateItems | core.AccessCreateSubfolders, ExpiresAt: expiry},
				{Kind: core.AccessUser, Subject: second, Rights: read},
			}
			if tc.clear {
				desired = nil
			}
			after, err := box.ReplaceFolderUserGrants(ctx, before, desired)
			if !errors.Is(err, tc.want) || writes != tc.calls {
				t.Fatalf("got %v with %d writes, want %v with %d", err, writes, tc.want, tc.calls)
			}
			if err == nil && (after.Token == before.Token || len(after.Grants) != len(desired)) {
				t.Fatal("missing confirmed ACL change")
			}
		})
	}
}
