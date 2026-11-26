// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"encoding/xml"
	"reflect"
	"slices"
	"sort"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"ewsgo/internal/core"
)

type aclFolder struct {
	ID, Parent, Flags, View string
	Revision                *uint64
	Present                 bool
	UserACLExtraFields      bool
	Grants                  []core.AccessGrant
	InternalExpiry          uint64
	GuestExpiry             uint64
	GrantExpiry             map[aclPrincipal]uint64
}

type aclPrincipal struct {
	Kind    core.AccessSubjectKind
	Subject string
}

func (aclFolder) String() string   { return "aclFolder{private}" }
func (aclFolder) GoString() string { return "aclFolder{private}" }

// Only the authenticated own mailbox is addressed. System root 11 is read as
// the inheritance boundary, not exposed as a selectable client folder. The
// owner decision comes from Authenticate/GetInfo, never a client account ID.
func (m *mailbox) ReadFolderAccess(ctx context.Context, id core.ID) (core.FolderAccess, error) {
	snapshot, _, err := m.readFolderAccess(ctx, id)
	return snapshot, err
}

// The native revision comes from the same folder tree as the validated ACL.
// It guards the target folder only and is deliberately absent from the token.
func (m *mailbox) readFolderAccess(ctx context.Context, id core.ID) (core.FolderAccess, aclFolder, error) {
	if !strings.HasPrefix(string(id), "f:") || !nativeID.MatchString(string(id)[2:]) || id == "f:11" {
		return core.FolderAccess{}, aclFolder{}, core.ErrInvalid
	}
	if m.scope.Backend != "zimbra" || m.scope.Account == "" {
		return core.FolderAccess{}, aclFolder{}, core.ErrAccessDenied
	}
	target := string(id)[2:]
	first, err := m.native.readACLTree(ctx)
	if err != nil {
		return core.FolderAccess{}, aclFolder{}, semanticError(err)
	}
	snapshot, err := resolveACL(first, target)
	if err != nil {
		return core.FolderAccess{}, aclFolder{}, err
	}
	second, err := m.native.readACLTree(ctx)
	if err != nil {
		return core.FolderAccess{}, aclFolder{}, semanticError(err)
	}
	validated, err := resolveACL(second, target)
	if err != nil {
		return core.FolderAccess{}, aclFolder{}, err
	}
	if !reflect.DeepEqual(snapshot, validated) {
		return core.FolderAccess{}, aclFolder{}, core.ErrConflict
	}
	if err = ctx.Err(); err != nil {
		return core.FolderAccess{}, aclFolder{}, err
	}
	snapshot.Scope = m.scope
	raw, err := json.Marshal(snapshot)
	if err != nil {
		return core.FolderAccess{}, aclFolder{}, core.ErrInvalid
	}
	snapshot.Token = sha256.Sum256(raw)
	snapshot.ObservedAt = time.Now().UTC()
	return snapshot, second[target], nil
}

func (s *Session) readACLTree(ctx context.Context) (map[string]aclFolder, error) {
	result := map[string]aclFolder{}
	grantCount := 0
	for _, selector := range []struct{ id, depth string }{{"1", "-1"}, {"11", "0"}} {
		folder := attr(element(mailNS, "folder", ""), "l", selector.id)
		req := element(mailNS, "GetFolderRequest", "", folder)
		for _, a := range [][2]string{{"depth", selector.depth}, {"tr", "0"}, {"visible", "0"}, {"needGranteeName", "0"}} {
			req = attr(req, a[0], a[1])
		}
		response, err := s.request(ctx, mailNS, "GetFolder", "", req)
		if err != nil {
			return nil, err
		}
		roots := 0
		var walk func(node) error
		walk = func(n node) error {
			if n.XMLName.Space != mailNS {
				return ErrProtocol
			}
			if n.XMLName.Local == "link" {
				return nil
			} // Never traverse a mountpoint.
			if n.XMLName.Local != "folder" && n.XMLName.Local != "search" {
				return nil
			}
			if len(result) >= 1001 {
				return core.ErrUnsupported
			}
			f, err := aclFolderFromNode(n)
			if err != nil {
				return err
			}
			grantCount += len(f.Grants)
			if grantCount > 1024 {
				return core.ErrUnsupported
			}
			if _, exists := result[f.ID]; exists {
				return core.ErrConflict
			}
			result[f.ID] = f
			for _, child := range n.Children {
				if child.XMLName.Local == "folder" || child.XMLName.Local == "search" || child.XMLName.Local == "link" {
					if err := walk(child); err != nil {
						return err
					}
				}
			}
			return nil
		}
		for _, n := range response.Children {
			if n.XMLName != (xml.Name{Space: mailNS, Local: "folder"}) {
				return nil, ErrProtocol
			}
			roots++
			if n.get("id") != selector.id {
				return nil, ErrProtocol
			}
			if err := walk(n); err != nil {
				return nil, err
			}
		}
		if roots != 1 {
			return nil, ErrProtocol
		}
	}
	return result, nil
}

// Copy a small allowlist of attributes into the semantic result. In particular
// pw/key/d are never retained. Unknown subject types/rights cannot become an
// accidentally complete list; malformed XML is an error, not an empty ACL.
func aclFolderFromNode(n node) (aclFolder, error) {
	if err := uniqueACLAttributes(n); err != nil {
		return aclFolder{}, err
	}
	f := aclFolder{ID: n.get("id"), Parent: n.get("l"), Flags: n.get("f"), View: n.get("view")}
	if !nativeID.MatchString(f.ID) || f.Parent != "" && !nativeID.MatchString(f.Parent) || len(f.Flags) > 128 {
		return f, ErrProtocol
	}
	var err error
	f.Revision, err = optionalFolderNumber(n, "ms")
	if err != nil {
		return f, err
	}
	seen := false
	for _, acl := range n.Children {
		if acl.XMLName != (xml.Name{Space: mailNS, Local: "acl"}) {
			continue
		}
		if seen {
			return f, ErrProtocol
		}
		seen = true
		if err := uniqueACLAttributes(acl); err != nil {
			return f, err
		}
		f.Present = true
		f.UserACLExtraFields = hasOtherACLAttributes(acl, "internalGrantExpiry", "guestGrantExpiry")
		internal, e := optionalFolderNumber(acl, "internalGrantExpiry")
		if e != nil {
			return f, e
		}
		guest, e := optionalFolderNumber(acl, "guestGrantExpiry")
		if e != nil {
			return f, e
		}
		if internal != nil {
			f.InternalExpiry = *internal
		}
		if guest != nil {
			f.GuestExpiry = *guest
		}
		f.GrantExpiry = make(map[aclPrincipal]uint64)
		subjects := map[string]bool{}
		for _, grant := range acl.Children {
			if grant.XMLName != (xml.Name{Space: mailNS, Local: "grant"}) || len(f.Grants) >= core.MaxFolderAccessGrants {
				return f, ErrProtocol
			}
			if err := uniqueACLAttributes(grant); err != nil {
				return f, err
			}
			kind := map[string]core.AccessSubjectKind{"usr": core.AccessUser, "grp": core.AccessGroup, "dom": core.AccessDomain, "cos": core.AccessCOS, "all": core.AccessAuthenticated, "pub": core.AccessPublic, "guest": core.AccessGuest, "key": core.AccessKey}[grant.get("gt")]
			subject := grant.get("zid")
			if kind == core.AccessSubjectUnknown {
				return f, core.ErrUnsupported
			}
			if kind == core.AccessUser {
				f.UserACLExtraFields = f.UserACLExtraFields || len(grant.Children) != 0 ||
					strings.TrimSpace(grant.Text) != "" || hasOtherACLAttributes(grant, "gt", "zid", "perm", "expiry")
			}
			if len(subject) > 512 || !utf8.ValidString(subject) || strings.ContainsFunc(subject, unicode.IsControl) || subject == "" && kind != core.AccessPublic && kind != core.AccessAuthenticated {
				return f, ErrProtocol
			}
			identity := grant.get("gt") + ":" + subject
			if subjects[identity] {
				return f, ErrProtocol
			}
			subjects[identity] = true
			rights, e := aclRights(grant.get("perm"))
			if e != nil {
				return f, e
			}
			expiry, e := optionalFolderNumber(grant, "expiry")
			if e != nil {
				return f, e
			}
			var millis uint64
			if expiry != nil {
				millis = *expiry
			}
			f.GrantExpiry[aclPrincipal{kind, subject}] = millis
			if millis == 0 {
				if kind == core.AccessGuest || kind == core.AccessKey {
					millis = f.GuestExpiry
				} else if kind != core.AccessPublic {
					millis = f.InternalExpiry
				}
			}
			var at time.Time
			if millis != 0 {
				if millis >= 253402300799999 {
					return f, ErrProtocol
				}
				at = time.UnixMilli(int64(millis)).UTC().Add(time.Millisecond) // Native expiry is strictly greater than the last valid millisecond.
			}
			f.Grants = append(f.Grants, core.AccessGrant{Kind: kind, Subject: subject, Rights: rights, ExpiresAt: at})
		}
	}
	sort.Slice(f.Grants, func(i, j int) bool {
		a, b := f.Grants[i], f.Grants[j]
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		return a.Subject < b.Subject
	})
	return f, nil
}

func hasOtherACLAttributes(n node, known ...string) bool {
	for _, a := range n.Attrs {
		if a.Name.Space == "xmlns" || a.Name.Space == "" && a.Name.Local == "xmlns" {
			continue
		}
		if a.Name.Space != "" || !slices.Contains(known, a.Name.Local) {
			return true
		}
	}
	return false
}

func aclRights(raw string) (core.AccessRights, error) {
	var rights core.AccessRights
	if len(raw) > 16 {
		return 0, ErrProtocol
	}
	for _, ch := range raw {
		switch ch {
		case 'r':
			rights |= core.AccessReadItems | core.AccessVisible
		case 'i':
			rights |= core.AccessCreateItems
		case 'w':
			rights |= core.AccessEditOwned | core.AccessEditAny
		case 'd':
			rights |= core.AccessDeleteOwned | core.AccessDeleteAny
		case 'a':
			rights |= core.AccessAdminister
		case 'p':
			rights |= core.AccessPrivate
		case 'x':
			rights |= core.AccessWorkflow
		case 'f':
			rights |= core.AccessFreeBusySimple
		case 'c': // Derived below, never accepted as an independent grant.
		default:
			return 0, core.ErrUnsupported
		}
	}
	if rights&(core.AccessReadItems|core.AccessCreateItems) == core.AccessReadItems|core.AccessCreateItems {
		rights |= core.AccessCreateSubfolders
	}
	if strings.ContainsRune(raw, 'c') && rights&core.AccessCreateSubfolders == 0 {
		return 0, ErrProtocol
	}
	return rights, nil
}

func resolveACL(tree map[string]aclFolder, target string) (core.FolderAccess, error) {
	result := core.FolderAccess{Composition: core.AccessReadCreateSubfolders, Folder: core.ID("f:" + target), CanViewList: true, EffectiveRights: core.AccessVisible | core.AccessAdminister, Completeness: core.AccessUnknown}
	if _, ok := tree[target]; !ok {
		return result, core.ErrNotFound
	}
	// Validate the entire target-to-own-root path independently of ACL stopping.
	visited := map[string]bool{}
	for at := target; at != "1"; {
		n, ok := tree[at]
		if !ok || visited[at] || at == "11" {
			return result, ErrProtocol
		}
		visited[at] = true
		at = n.Parent
	}
	root, ok := tree["1"]
	if !ok || root.Parent != "11" {
		return result, ErrProtocol
	}
	visited = map[string]bool{}
	for at := target; ; {
		n, ok := tree[at]
		if !ok || visited[at] {
			return result, ErrProtocol
		}
		visited[at] = true
		stop := strings.Contains(n.Flags, "i") || at == "11"
		if stop {
			result.Source = core.ID("f:" + at)
			result.Provenance = core.AccessInherited
			if at == target {
				result.Provenance = core.AccessExplicit
			}
			if !n.Present && strings.Contains(n.Flags, "i") {
				return result, nil
			} // Explicit but hidden/unknown is not empty.
			// Zimbra GetFolder uses NOTIFY_FIELDS and suppresses ACL when NO_INHERIT
			// is absent. ACL mutations set NO_INHERIT; the unmodified system root is
			// the private empty terminus. A root with NO_INHERIT must expose its ACL.
			result.Completeness = core.AccessComplete
			result.Grants = append([]core.AccessGrant(nil), n.Grants...)
			return result, nil
		}
		if n.Present {
			return result, ErrProtocol
		} // Inconsistent NOTIFY_FIELDS source.
		at = n.Parent
	}
}

func uniqueACLAttributes(n node) error {
	seen := map[string]bool{}
	for _, a := range n.Attrs {
		if a.Name.Space != "" {
			continue
		}
		if seen[a.Name.Local] {
			return ErrProtocol
		}
		seen[a.Name.Local] = true
	}
	return nil
}
