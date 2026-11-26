// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"errors"
	"math"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
)

var _ core.FolderAccessWriter = (*mailbox)(nil)

var unlimitedShareLifetime = regexp.MustCompile(`^0+(ms|[dhms])?$`)

func (m *mailbox) SetFolderAccessGrant(ctx context.Context, expected core.FolderAccess, grant core.AccessGrant) (core.FolderAccess, error) {
	return m.changeFolderAccess(ctx, expected, grant, false)
}

func (m *mailbox) RemoveFolderAccessGrant(ctx context.Context, expected core.FolderAccess, kind core.AccessSubjectKind, subject string) (core.FolderAccess, error) {
	return m.changeFolderAccess(ctx, expected, core.AccessGrant{Kind: kind, Subject: subject}, true)
}

func (m *mailbox) changeFolderAccess(ctx context.Context, expected core.FolderAccess, change core.AccessGrant, remove bool) (result core.FolderAccess, err error) {
	start := time.Now()
	operation := "SetFolderAccessGrant"
	if remove {
		operation = "RemoveFolderAccessGrant"
	}
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: operation, Result: core.ActivityOutcome(err), Duration: time.Since(start)})
	}()
	if expected.Scope != m.scope || !expected.CanViewList {
		return core.FolderAccess{}, core.ErrAccessDenied
	}
	if expected.Completeness != core.AccessComplete || expected.Token == [32]byte{} {
		return core.FolderAccess{}, core.ErrInvalid
	}
	principal := aclPrincipal{change.Kind, change.Subject}
	gt := ""
	switch change.Kind {
	case core.AccessUser:
		gt = "usr"
	case core.AccessGroup:
		gt = "grp"
	default:
		return core.FolderAccess{}, core.ErrUnsupported
	}
	if !galUUID.MatchString(change.Subject) || change.Subject == "00000000-0000-0000-0000-000000000000" {
		return core.FolderAccess{}, core.ErrInvalid
	}
	if strings.EqualFold(change.Subject, m.scope.Account) {
		return core.FolderAccess{}, core.ErrAccessDenied
	}
	var rights string
	var expiry uint64
	if !remove {
		rights, err = encodeACLGrantRights(change.Rights)
		if err != nil {
			return core.FolderAccess{}, err
		}
		if !change.ExpiresAt.IsZero() {
			ms := change.ExpiresAt.UnixMilli()
			if ms <= 1 || ms >= 253402300800000 || !change.ExpiresAt.Equal(time.UnixMilli(ms)) {
				return core.FolderAccess{}, core.ErrInvalid
			}
			expiry = uint64(ms - 1) // Native expiry is the last valid millisecond.
		}
	}

	current, native, err := m.readFolderAccess(ctx, expected.Folder)
	if err != nil {
		return core.FolderAccess{}, err
	}
	if current.Token != expected.Token {
		return core.FolderAccess{}, core.ErrConflict
	}
	if current.Completeness != core.AccessComplete {
		return core.FolderAccess{}, core.ErrUnsupported
	}
	if current.Provenance == core.AccessInherited && len(current.Grants) != 0 {
		return core.FolderAccess{}, core.ErrUnsupported
	}
	var previous core.AccessGrant
	for _, grant := range current.Grants {
		if strings.EqualFold(grant.Subject, change.Subject) {
			if grant.Kind != change.Kind || grant.Subject != change.Subject {
				return core.FolderAccess{}, core.ErrConflict
			}
			previous = grant
		}
	}
	found := previous.Kind != core.AccessSubjectUnknown
	keepExpiry := !remove && found && (expiry == 0 || previous.ExpiresAt.Equal(change.ExpiresAt))
	if keepExpiry {
		expiry = native.GrantExpiry[principal]
	}
	if remove && !found {
		return core.FolderAccess{}, core.ErrNotFound
	}
	if !remove && found && keepExpiry && previous.Rights == change.Rights {
		return current, nil
	}
	if native.Revision == nil || *native.Revision > math.MaxInt32 || !remove && !found && len(current.Grants) >= core.MaxFolderAccessGrants {
		return core.FolderAccess{}, core.ErrUnsupported
	}
	if err := m.native.checkACLExpiryEffects(ctx, native, principal, remove, keepExpiry); err != nil {
		return core.FolderAccess{}, err
	}
	action := attr(element(mailNS, "action", ""), "id", native.ID)
	op := "!grant"
	if remove {
		action = attr(action, "zid", change.Subject)
	} else {
		if err := m.native.confirmACLPrincipal(ctx, principal); err != nil {
			return core.FolderAccess{}, err
		}
		op = "grant"
		grant := attr(attr(attr(element(mailNS, "grant", ""), "gt", gt), "zid", change.Subject), "perm", rights)
		if expiry != 0 {
			grant = attr(grant, "expiry", strconv.FormatUint(expiry, 10))
		}
		action.Children = append(action.Children, grant)
	}
	if err := ctx.Err(); err != nil {
		return core.FolderAccess{}, err
	}
	request := element(mailNS, "FolderActionRequest", "", attr(action, "op", op))
	response, err := m.native.request(ctx, mailNS, "FolderAction", "", request, modifiedConstraint(core.Revision(*native.Revision)))
	if err != nil {
		return core.FolderAccess{}, aclWriteError(err)
	}
	ack := response.child(mailNS, "action")
	if len(response.Children) != 1 || ack == nil || ack.get("id") != native.ID || ack.get("op") != op ||
		!remove && ack.get("zid") != change.Subject {
		return core.FolderAccess{}, core.ErrOutcomeUnknown
	}
	after, updated, err := m.readFolderAccess(ctx, current.Folder)
	if err != nil || updated.Revision == nil || *updated.Revision <= *native.Revision ||
		after.Completeness != core.AccessComplete || after.Provenance != core.AccessExplicit ||
		!aclChangeVerified(native, updated, principal, change.Rights, expiry, remove, keepExpiry) {
		return core.FolderAccess{}, core.ErrOutcomeUnknown
	}
	return after, nil
}

func encodeACLGrantRights(rights core.AccessRights) (string, error) {
	const chars = "riwdapxf"
	var encoded strings.Builder
	for _, ch := range chars {
		mask, _ := aclRights(string(ch))
		if rights&mask != 0 {
			encoded.WriteRune(ch)
		}
	}
	permissions := encoded.String()
	decoded, err := aclRights(permissions)
	if err != nil || permissions == "" || decoded != rights {
		return "", core.ErrUnsupported
	}
	return permissions, nil
}

func (s *Session) confirmACLPrincipal(ctx context.Context, principal aclPrincipal) error {
	entries, err := s.GAL(ctx)
	if err != nil {
		return semanticError(err)
	}
	for _, entry := range entries {
		if entry.AccountID != principal.Subject {
			continue
		}
		switch entry.entryType {
		case "group":
			if principal.Kind == core.AccessGroup {
				return nil
			}
		case "", "account":
			if principal.Kind == core.AccessUser {
				return nil
			}
		}
		return core.ErrInvalid
	}
	return core.ErrNotFound
}

// Zimbra updates fallback expiry slots while applying native grants/revokes.
// Confirm that a slot change cannot alter another grant or extend one on revoke.
func (s *Session) checkACLExpiryEffects(ctx context.Context, folder aclFolder, target aclPrincipal, remove, keepExpiry bool) error {
	var users, guests int
	for _, grant := range folder.Grants {
		if remove && (aclPrincipal{grant.Kind, grant.Subject}) == target {
			continue
		}
		switch grant.Kind {
		case core.AccessUser:
			users++
		case core.AccessGuest:
			guests++
		}
	}
	var internal, external bool
	for _, grant := range folder.Grants {
		principal := aclPrincipal{grant.Kind, grant.Subject}
		if principal == target && !keepExpiry || folder.GrantExpiry[principal] != 0 {
			continue
		}
		switch grant.Kind {
		case core.AccessPublic:
		case core.AccessGuest, core.AccessKey:
			external = external || guests == 0 || folder.GuestExpiry == 0
		default:
			internal = internal || users == 0 || folder.InternalExpiry == 0
		}
	}
	if remove {
		if internal && users == 0 && folder.InternalExpiry != 0 ||
			external && guests == 0 && folder.GuestExpiry != 0 {
			return core.ErrUnsupported
		}
		return nil
	}
	if !internal && !external {
		return nil
	}
	return s.requireUnlimitedACLSharePolicy(ctx, folder.View, internal, external)
}

func (s *Session) requireUnlimitedACLSharePolicy(ctx context.Context, view string, internal, external bool) error {
	attrs, err := s.request(ctx, accountNS, "GetInfo", "", attr(element(accountNS, "GetInfoRequest", ""), "sections", "attrs"))
	if err != nil {
		return semanticError(err)
	}
	values := attrs.child(accountNS, "attrs")
	internalName, externalName := "zimbraShareLifetime", "zimbraExternalShareLifetime"
	if view == "document" {
		internalName, externalName = "zimbraFileShareLifetime", "zimbraFileExternalShareLifetime"
	}
	// For an unknown folder type, Zimbra takes the lower finite lifetime.
	if view == "" || view == "unknown" {
		if internal && !zeroACLShareLifetime(values, "zimbraFileShareLifetime") ||
			external && !zeroACLShareLifetime(values, "zimbraFileExternalShareLifetime") {
			return core.ErrUnsupported
		}
	}
	if internal && !zeroACLShareLifetime(values, internalName) || external && !zeroACLShareLifetime(values, externalName) {
		return core.ErrUnsupported
	}
	return nil
}

func zeroACLShareLifetime(attrs *node, name string) bool {
	if attrs == nil {
		return false
	}
	found := false
	for _, item := range attrs.Children {
		if item.XMLName.Space == accountNS && item.XMLName.Local == "attr" && item.get("name") == name {
			if found || !unlimitedShareLifetime.MatchString(item.Text) {
				return false
			}
			found = true
		}
	}
	return found
}

func aclChangeVerified(before, after aclFolder, target aclPrincipal, rights core.AccessRights, expiry uint64, remove, keepExpiry bool) bool {
	old := make(map[aclPrincipal]core.AccessGrant, len(before.Grants))
	for _, grant := range before.Grants {
		old[aclPrincipal{grant.Kind, grant.Subject}] = grant
	}
	matched := false
	for _, grant := range after.Grants {
		principal := aclPrincipal{grant.Kind, grant.Subject}
		if principal == target {
			if remove || grant.Rights != rights || after.GrantExpiry[principal] != expiry ||
				keepExpiry && !grant.ExpiresAt.Equal(old[principal].ExpiresAt) {
				return false
			}
			matched = true
		} else if grant != old[principal] || before.GrantExpiry[principal] != after.GrantExpiry[principal] {
			return false
		}
		delete(old, principal)
	}
	delete(old, target)
	return len(old) == 0 && matched != remove
}

func aclWriteError(err error) error {
	var fault *Fault
	if errors.As(err, &fault) {
		switch fault.Code {
		case "mail.CANNOT_GRANT":
			return core.ErrAccessDenied
		case "mail.GRANTEE_EXISTS":
			return core.ErrConflict
		case "account.NO_SUCH_ACCOUNT", "account.NO_SUCH_DISTRIBUTION_LIST":
			return core.ErrNotFound
		}
	}
	return messageWriteError(err)
}
