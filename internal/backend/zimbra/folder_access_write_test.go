// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/xml"
	"errors"
	"net/http"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestFolderAccessWriterPreconditionsAndReadback(t *testing.T) {
	const subject = "47b8acb8-febd-4824-b27b-9e65d8b82040"
	const other = "e3da56ca-90b6-4636-a1ad-52435c5d6d02"
	const read = core.AccessReadItems | core.AccessVisible
	const readCreate = read | core.AccessCreateItems | core.AccessCreateSubfolders
	grant := `<grant gt="usr" zid="` + subject + `" perm="r" expiry="1999999999999"/>`
	preserved := `<grant gt="usr" zid="` + other + `" perm="r"/>`
	expiry := time.UnixMilli(2000000000000).UTC()
	for _, tc := range []struct {
		name, before, after, response, lifetime             string
		rights                                              core.AccessRights
		remove, stale, foreign, inherited, group, wrongKind bool
		writes                                              int
		want                                                error
	}{
		{name: "add", after: grant, rights: read, writes: 1},
		{name: "add confirmed group", after: `<grant gt="grp" zid="` + subject + `" perm="r" expiry="1999999999999"/>`, rights: read, group: true, writes: 1},
		{name: "group presented as a user", rights: read, wrongKind: true, want: core.ErrInvalid},
		{name: "change rights, preserve expiry", before: grant, rights: readCreate,
			after: `<grant gt="usr" zid="` + subject + `" perm="ri" expiry="1999999999999"/>`, writes: 1},
		{name: "add preserves another user with confirmed unlimited policy", before: preserved, after: preserved + grant, lifetime: "0ms", rights: read, writes: 1},
		{name: "policy would cap another grant", before: preserved, lifetime: "1d", rights: read, want: core.ErrUnsupported},
		{name: "unchanged", before: grant, rights: read},
		{name: "revoke", before: grant, remove: true, writes: 1},
		{name: "revoke would extend another grant",
			before: `<grant gt="grp" zid="` + other + `" perm="r"/>` + grant, remove: true, lifetime: "explicit default", want: core.ErrUnsupported},
		{name: "revocation absent", remove: true, want: core.ErrNotFound},
		{name: "inherited grant", before: preserved, inherited: true, rights: read, want: core.ErrUnsupported},
		{name: "foreign scope", foreign: true, rights: read, want: core.ErrAccessDenied},
		{name: "stale snapshot", stale: true, rights: read, want: core.ErrConflict},
		{name: "unrepresentable rights", rights: core.AccessEditOwned, want: core.ErrUnsupported},
		{name: "native concurrent modification", rights: read, writes: 1, response: "mail.MODIFY_CONFLICT", want: core.ErrConflict},
		{name: "native access denied", rights: read, writes: 1, response: "service.PERM_DENIED", want: core.ErrAccessDenied},
		{name: "unclassified write fault", rights: read, writes: 1, response: "service.FAILURE", want: core.ErrOutcomeUnknown},
		{name: "malformed acknowledgement", rights: read, writes: 1, response: "bad ack", want: core.ErrOutcomeUnknown},
		{name: "readback reports stale content", rights: read, writes: 1, want: core.ErrOutcomeUnknown},
		{name: "readback loses another grant", before: preserved, after: grant, lifetime: "0", rights: read, writes: 1, want: core.ErrOutcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			writes, reads := 0, 0
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				var req node
				if err := xml.NewDecoder(r.Body).Decode(&req); err != nil {
					t.Error("invalid request", err)
					return
				}
				header := req.child(soapNS, "Header").child(contextNS, "context")
				if header.child(contextNS, "account") != nil {
					t.Error("delegated account selector")
				}
				body := req.child(soapNS, "Body")
				if get := body.child(mailNS, "GetFolderRequest"); get != nil {
					reads++
					if get.child(mailNS, "folder").get("l") == "11" {
						envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="11"/></GetFolderResponse>`)
						return
					}
					folder := `<folder id="24" l="1" ms="41" view="message"/>`
					if tc.inherited {
						envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1" l="11" f="i"><acl>`+tc.before+`</acl>`+folder+`</folder></GetFolderResponse>`)
						return
					}
					if writes > 0 {
						folder = `<folder id="24" l="1" ms="44" view="message" f="i"><acl>` + tc.after + `</acl></folder>`
					} else if tc.before != "" {
						acl := `<acl>`
						if tc.lifetime == "explicit default" {
							acl = `<acl internalGrantExpiry="1999999999999">`
						}
						folder = `<folder id="24" l="1" ms="41" view="message" f="i">` + acl + tc.before + `</acl></folder>`
					}
					envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1" l="11">`+folder+`</folder></GetFolderResponse>`)
					return
				}
				if body.child(accountNS, "SearchGalRequest") != nil {
					entryType := ""
					if tc.group || tc.wrongKind {
						entryType = `<a n="type">group</a>`
					}
					envelope(w, `<SearchGalResponse xmlns="urn:zimbraAccount" more="0"><cn><a n="zimbraId">`+subject+`</a><a n="email">fixture@example.test</a>`+entryType+`</cn></SearchGalResponse>`)
					return
				}
				if body.child(accountNS, "GetInfoRequest") != nil {
					envelope(w, `<GetInfoResponse xmlns="urn:zimbraAccount"><attrs><attr name="zimbraShareLifetime">`+tc.lifetime+`</attr></attrs></GetInfoResponse>`)
					return
				}
				mutation := body.child(mailNS, "FolderActionRequest")
				change := header.child(contextNS, "change")
				if mutation == nil || change == nil || change.get("type") != "mod" || change.get("token") != "41" {
					t.Error("mutation without target revision")
					fault(w, "service.INVALID_REQUEST")
					return
				}
				writes++
				op := "grant"
				a := mutation.child(mailNS, "action")
				if tc.remove {
					op = "!grant"
					if a == nil || a.get("zid") != subject || len(a.Children) != 0 {
						t.Error("wrong revoke request")
					}
				} else {
					g := a.child(mailNS, "grant")
					perm := "r"
					if tc.rights == readCreate {
						perm = "ri"
					}
					gt := "usr"
					if tc.group {
						gt = "grp"
					}
					if g == nil || g.get("gt") != gt || g.get("zid") != subject || g.get("perm") != perm ||
						g.get("expiry") != "1999999999999" || g.get("pw") != "" || g.get("key") != "" {
						t.Error("wrong native grant")
					}
				}
				if a == nil || a.get("id") != "24" || a.get("op") != op {
					t.Error("wrong mutation target")
				}
				switch tc.response {
				case "":
					envelope(w, `<FolderActionResponse xmlns="urn:zimbraMail"><action id="24" op="`+op+`" zid="`+subject+`"/></FolderActionResponse>`)
				case "bad ack":
					envelope(w, `<FolderActionResponse xmlns="urn:zimbraMail"><action id="25" op="grant"/></FolderActionResponse>`)
				default:
					fault(w, tc.response)
				}
			}, Config{})
			box := &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}, scope: core.Scope{Backend: "zimbra", Account: "owner"}}
			ctx := context.Background()
			current, err := box.ReadFolderAccess(ctx, "f:24")
			if err != nil {
				t.Fatal("fixture ACL unavailable", err)
			}
			if tc.foreign {
				current.Scope.Account = "different"
			}
			if tc.stale {
				current.Token[0] ^= 1
			}
			var got core.FolderAccess
			if tc.remove {
				got, err = box.RemoveFolderAccessGrant(ctx, current, core.AccessUser, subject)
			} else {
				input := core.AccessGrant{Kind: core.AccessUser, Subject: subject, Rights: tc.rights, ExpiresAt: expiry}
				if tc.group {
					input.Kind = core.AccessGroup
				}
				if tc.rights == readCreate {
					input.ExpiresAt = time.Time{}
				}
				got, err = box.SetFolderAccessGrant(ctx, current, input)
			}
			if !errors.Is(err, tc.want) || writes != tc.writes {
				t.Fatalf("got %v with %d writes, want %v with %d writes", err, writes, tc.want, tc.writes)
			}
			if tc.foreign && reads != 4 {
				t.Fatal("foreign scope reached the backend")
			}
			if tc.want == nil && (got.Folder != current.Folder || got.Scope != current.Scope ||
				got.Completeness != core.AccessComplete || got.Provenance != core.AccessExplicit ||
				tc.writes == 1 && got.Token == current.Token) {
				t.Fatal("expected a confirmed ACL snapshot")
			}
		})
	}
}
