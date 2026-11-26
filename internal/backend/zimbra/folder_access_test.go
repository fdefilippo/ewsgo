// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/xml"
	"errors"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func accessFixture(t *testing.T, user, system string, change func(int) string) (*mailbox, *int) {
	t.Helper()
	calls := 0
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		calls++
		var e node
		if xml.NewDecoder(r.Body).Decode(&e) != nil {
			t.Fatal("request framing")
		}
		header := e.child(soapNS, "Header").child(contextNS, "context")
		if header.child(contextNS, "account") != nil {
			t.Fatal("delegated selector")
		}
		req := e.child(soapNS, "Body").child(mailNS, "GetFolderRequest")
		if req == nil || req.get("tr") != "0" || req.get("needGranteeName") != "0" || req.get("visible") != "0" {
			t.Fatal("unsafe ACL request")
		}
		selector := req.child(mailNS, "folder")
		if selector == nil {
			t.Fatal("selector missing")
		}
		body := user
		if selector.get("l") == "11" {
			body = system
		} else if selector.get("l") != "1" {
			t.Fatal("unexpected selector")
		}
		if change != nil {
			if v := change(calls); v != "" {
				body = v
			}
		}
		envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail">`+body+`</GetFolderResponse>`)
	}, Config{})
	return &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}, scope: core.Scope{Backend: "zimbra", Account: "owner"}}, &calls
}

func TestFolderAccessInheritanceEmptyExplicitPositiveAndUnknown(t *testing.T) {
	grant := `<grant gt="usr" zid="recipient" perm="riw" pw="private-password" key="private-key" d="private-name"/>`
	for _, tc := range []struct {
		name, user, system, source string
		complete                   core.AccessCompleteness
		count                      int
	}{
		{"private boundary", `<folder id="1" l="11"><folder id="2" l="1"/></folder>`, `<folder id="11"/>`, "f:11", core.AccessComplete, 0},
		{"explicit empty", `<folder id="1" l="11"><folder id="2" l="1" f="i"><acl/></folder></folder>`, `<folder id="11"/>`, "f:2", core.AccessComplete, 0},
		{"inherited grant", `<folder id="1" l="11" f="i"><acl>` + grant + `</acl><folder id="2" l="1"/></folder>`, `<folder id="11"/>`, "f:1", core.AccessComplete, 1},
		{"system grant", `<folder id="1" l="11"><folder id="2" l="1"/></folder>`, `<folder id="11" f="i"><acl>` + grant + `</acl></folder>`, "f:11", core.AccessComplete, 1},
		{"hidden explicit", `<folder id="1" l="11"><folder id="2" l="1" f="i"/></folder>`, `<folder id="11"/>`, "f:2", core.AccessUnknown, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box, calls := accessFixture(t, tc.user, tc.system, nil)
			got, err := box.ReadFolderAccess(context.Background(), "f:2")
			if err != nil || got.Completeness != tc.complete || got.Source != core.ID(tc.source) || len(got.Grants) != tc.count || !got.CanViewList || got.Scope != box.scope || *calls != 4 {
				t.Fatal("snapshot mismatch", err)
			}
			if got.Token == [32]byte{} || got.ObservedAt.IsZero() {
				t.Fatal("observation missing")
			}
			if tc.count != 0 {
				if got.Grants[0].Rights != core.AccessReadItems|core.AccessVisible|core.AccessCreateItems|core.AccessCreateSubfolders|core.AccessEditOwned|core.AccessEditAny {
					t.Fatal("rights lost")
				}
			}
			if strings.Contains(fmt.Sprintf("%+v %#v", got, got), "recipient") || strings.Contains(fmt.Sprintf("%+v", got.Grants), "recipient") {
				t.Fatal("private formatting")
			}
		})
	}
}

func TestFolderAccessRejectsMalformedUnknownAndCrossScope(t *testing.T) {
	for _, tc := range []struct {
		name, user, system string
		want               error
	}{
		{"cycle", `<folder id="1" l="11"><folder id="2" l="3"/><folder id="3" l="2"/></folder>`, `<folder id="11"/>`, ErrProtocol},
		{"missing parent", `<folder id="1" l="11"><folder id="2" l="3"/></folder>`, `<folder id="11"/>`, ErrProtocol},
		{"missing target", `<folder id="1" l="11"/>`, `<folder id="11"/>`, core.ErrNotFound},
		{"duplicate", `<folder id="1" l="11"><folder id="2" l="1"/><folder id="2" l="1"/></folder>`, `<folder id="11"/>`, core.ErrConflict},
		{"foreign", `<folder id="1" l="11"><folder id="foreign:2" l="1"/></folder>`, `<folder id="11"/>`, ErrProtocol},
		{"mountpoint only", `<folder id="1" l="11"><link id="8"><folder id="2" l="1"/></link></folder>`, `<folder id="11"/>`, core.ErrNotFound},
		{"unknown subject", `<folder id="1" l="11"><folder id="2" l="1" f="i"><acl><grant gt="new" zid="x" perm="r"/></acl></folder></folder>`, `<folder id="11"/>`, core.ErrUnsupported},
		{"unknown right", `<folder id="1" l="11"><folder id="2" l="1" f="i"><acl><grant gt="usr" zid="x" perm="rz"/></acl></folder></folder>`, `<folder id="11"/>`, core.ErrUnsupported},
		{"duplicate ACL", `<folder id="1" l="11"><folder id="2" l="1" f="i"><acl/><acl/></folder></folder>`, `<folder id="11"/>`, ErrProtocol},
		{"duplicate principal", `<folder id="1" l="11"><folder id="2" l="1" f="i"><acl><grant gt="usr" zid="x" perm="r"/><grant gt="usr" zid="x" perm="w"/></acl></folder></folder>`, `<folder id="11"/>`, ErrProtocol},
	} {
		t.Run(tc.name, func(t *testing.T) {
			box, _ := accessFixture(t, tc.user, tc.system, nil)
			if _, err := box.ReadFolderAccess(context.Background(), "f:2"); !errors.Is(err, tc.want) {
				t.Fatal("invalid snapshot accepted", err)
			}
		})
	}
	box, calls := accessFixture(t, `<folder id="1" l="11"/>`, `<folder id="11"/>`, nil)
	for _, id := range []core.ID{"m:2", "f:11", "f:foreign:2"} {
		if _, err := box.ReadFolderAccess(context.Background(), id); !errors.Is(err, core.ErrInvalid) {
			t.Fatal("invalid selector")
		}
	}
	box.scope = core.Scope{}
	if _, err := box.ReadFolderAccess(context.Background(), "f:2"); !errors.Is(err, core.ErrAccessDenied) || *calls != 0 {
		t.Fatal("untrusted owner")
	}
}

func TestFolderAccessChangeRevocationAndExpirySemantics(t *testing.T) {
	user := `<folder id="1" l="11"><folder id="2" l="1" f="i"><acl internalGrantExpiry="10000" guestGrantExpiry="20000"><grant gt="usr" zid="u" perm="r"/><grant gt="guest" zid="g" perm="r"/><grant gt="pub" perm="r"/><grant gt="all" perm="r" expiry="30000"/></acl></folder></folder>`
	box, _ := accessFixture(t, user, `<folder id="11"/>`, nil)
	got, err := box.ReadFolderAccess(context.Background(), "f:2")
	if err != nil {
		t.Fatal(err)
	}
	want := map[core.AccessSubjectKind]int64{core.AccessUser: 10000, core.AccessGuest: 20000, core.AccessPublic: 0, core.AccessAuthenticated: 30000}
	for _, g := range got.Grants {
		millis := int64(0)
		if !g.ExpiresAt.IsZero() {
			millis = g.ExpiresAt.Add(-time.Millisecond).UnixMilli()
		}
		if millis != want[g.Kind] {
			t.Fatal("expiry inheritance")
		}
	}
	changed := strings.Replace(user, `perm="r"`, `perm="rw"`, 1)
	box, _ = accessFixture(t, user, `<folder id="11"/>`, func(n int) string {
		if n == 3 {
			return changed
		}
		return ""
	})
	if _, err := box.ReadFolderAccess(context.Background(), "f:2"); !errors.Is(err, core.ErrConflict) {
		t.Fatal("changing ACL published", err)
	}
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		fault(w, "service.PERM_DENIED")
	}, Config{})
	box = &mailbox{native: &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Hour)}, scope: core.Scope{Backend: "zimbra", Account: "owner"}}
	if _, err := box.ReadFolderAccess(context.Background(), "f:2"); !errors.Is(err, core.ErrAccessDenied) {
		t.Fatal("denied became empty", err)
	}
}

func TestFolderAccessRevisionComesFromValidatedTarget(t *testing.T) {
	first := `<folder id="1" l="11" ms="40"><folder id="2" l="1" ms="25"/></folder>`
	second := strings.Replace(first, `ms="25"`, `ms="31"`, 1)
	box, _ := accessFixture(t, first, `<folder id="11" ms="7"/>`, func(call int) string {
		if call == 3 {
			return second
		}
		return ""
	})
	got, native, err := box.readFolderAccess(context.Background(), "f:2")
	if err != nil || native.Revision == nil || *native.Revision != 31 {
		t.Fatal("missing validated revision on target folder", err)
	}
	again, err := box.ReadFolderAccess(context.Background(), "f:2")
	if err != nil || got.Token != again.Token {
		t.Fatal("unrelated folder metadata changed the ACL token", err)
	}
}

func TestACLGrantSecretsCannotChangeSemanticSnapshot(t *testing.T) {
	raw := `<folder xmlns="urn:zimbraMail" id="2" l="1" f="i"><acl><grant gt="key" zid="external-user" perm="r" key="private-first" pw="private-pw"/></acl></folder>`
	a, _ := parseXML([]byte(raw))
	b, _ := parseXML([]byte(strings.ReplaceAll(raw, "private-", "different-")))
	first, e1 := aclFolderFromNode(a)
	second, e2 := aclFolderFromNode(b)
	if e1 != nil || e2 != nil || !reflect.DeepEqual(first, second) {
		t.Fatal("grant secret retained")
	}
}

func TestFolderAccessExpiryNormalizedExclusiveBoundary(t *testing.T) {
	box, _ := accessFixture(t, `<folder id="1" l="11"><folder id="2" l="1" f="i"><acl><grant gt="usr" zid="u" perm="r" expiry="10000"/></acl></folder></folder>`, `<folder id="11"/>`, nil)
	a, err := box.ReadFolderAccess(context.Background(), "f:2")
	if err != nil || len(a.Grants) != 1 || a.Grants[0].ExpiresAt.UnixNano() != int64(10001)*int64(time.Millisecond) {
		t.Fatal("Native last valid millisecond must become exclusive instant")
	}
	box, _ = accessFixture(t, `<folder id="1" l="11"><folder id="2" l="1" f="i"><acl><grant gt="usr" zid="u" perm="r" expiry="253402300799999"/></acl></folder></folder>`, `<folder id="11"/>`, nil)
	if _, err = box.ReadFolderAccess(context.Background(), "f:2"); err == nil {
		t.Fatal("Out-of-range normalized expiry must fail explicitly")
	}
}
