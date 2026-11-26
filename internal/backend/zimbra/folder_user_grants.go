// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"math"
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
)

var _ core.FolderUserGrantsWriter = (*mailbox)(nil)

func (m *mailbox) ReplaceFolderUserGrants(ctx context.Context, expected core.FolderAccess, grants []core.AccessGrant) (result core.FolderAccess, err error) {
	start := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: "ReplaceFolderUserGrants", Result: core.ActivityOutcome(err), Duration: time.Since(start)})
	}()
	if expected.Scope != m.scope || !expected.CanViewList {
		return core.FolderAccess{}, core.ErrAccessDenied
	}
	if expected.Completeness != core.AccessComplete || expected.Token == [32]byte{} || len(grants) > core.MaxFolderAccessGrants {
		return core.FolderAccess{}, core.ErrInvalid
	}
	current, folder, err := m.readFolderAccess(ctx, expected.Folder)
	if err != nil {
		return core.FolderAccess{}, err
	}
	if current.Token != expected.Token {
		return core.FolderAccess{}, core.ErrConflict
	}
	if !current.CanViewList || current.EffectiveRights&core.AccessAdminister == 0 {
		return core.FolderAccess{}, core.ErrAccessDenied
	}
	if current.Completeness != core.AccessComplete || current.Provenance != core.AccessExplicit ||
		current.Source != current.Folder || !folder.Present || folder.UserACLExtraFields ||
		folder.Revision == nil || *folder.Revision == 0 || *folder.Revision > math.MaxInt32 {
		return core.FolderAccess{}, core.ErrUnsupported
	}
	old := make(map[string]core.AccessGrant, len(current.Grants))
	for _, g := range current.Grants {
		if g.Kind != core.AccessUser || !validACLUserID(g.Subject) || g.Subject == m.scope.Account {
			return core.FolderAccess{}, core.ErrUnsupported
		}
		old[g.Subject] = g
	}
	acl := folderUserACL(folder)
	next := make(map[string]core.AccessGrant, len(grants))
	rawExpiries := make(map[aclPrincipal]uint64, len(grants))
	changedUsers := make(map[string]bool)
	for _, g := range grants {
		if g.Kind != core.AccessUser || !validACLUserID(g.Subject) || g.Subject == m.scope.Account {
			return core.FolderAccess{}, core.ErrInvalid
		}
		if _, duplicate := next[g.Subject]; duplicate {
			return core.FolderAccess{}, core.ErrInvalid
		}
		rights, err := encodeACLGrantRights(g.Rights)
		if err != nil {
			return core.FolderAccess{}, err
		}
		previous, exists := old[g.Subject]
		if !g.ExpiresAt.Equal(previous.ExpiresAt) {
			return core.FolderAccess{}, core.ErrInvalid
		}
		key := aclPrincipal{g.Kind, g.Subject}
		expiry := folder.GrantExpiry[key]
		if !exists && folder.InternalExpiry != 0 {
			if folder.InternalExpiry >= 253402300799999 {
				return core.FolderAccess{}, core.ErrUnsupported
			}
			g.ExpiresAt = time.UnixMilli(int64(folder.InternalExpiry)).UTC().Add(time.Millisecond)
			if !time.Now().Before(g.ExpiresAt) {
				return core.FolderAccess{}, core.ErrUnsupported
			}
		}
		if !exists || previous.Rights != g.Rights {
			changedUsers[g.Subject] = true
		}
		rawExpiries[key] = expiry
		next[g.Subject] = g
		grant := attr(attr(attr(element(mailNS, "grant", ""), "gt", "usr"), "zid", g.Subject), "perm", rights)
		if expiry != 0 {
			grant = attr(grant, "expiry", strconv.FormatUint(expiry, 10))
		}
		acl.Children = append(acl.Children, grant)
	}
	if len(changedUsers) == 0 && len(old) == len(next) {
		return current, nil
	}
	if err := m.native.requireUnlimitedACLSharePolicy(ctx, folder.View, true, true); err != nil {
		return core.FolderAccess{}, err
	}
	if len(changedUsers) != 0 {
		if err := m.confirmInternalACLUsers(ctx, changedUsers); err != nil {
			return core.FolderAccess{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return core.FolderAccess{}, err
	}
	action := attr(attr(element(mailNS, "action", "", acl), "id", folder.ID), "op", "update")
	reply, err := m.native.request(ctx, mailNS, "FolderAction", "", element(mailNS, "FolderActionRequest", "", action),
		modifiedConstraint(core.Revision(*folder.Revision)))
	if err != nil {
		return core.FolderAccess{}, aclWriteError(err)
	}
	ack := reply.child(mailNS, "action")
	if len(reply.Children) != 1 || ack == nil || ack.get("id") != folder.ID || ack.get("op") != "update" {
		return core.FolderAccess{}, core.ErrOutcomeUnknown
	}
	after, updated, err := m.readFolderAccess(ctx, current.Folder)
	if err != nil || updated.Revision == nil || *updated.Revision <= *folder.Revision ||
		after.Completeness != core.AccessComplete || after.Provenance != core.AccessExplicit ||
		updated.UserACLExtraFields || updated.Flags != folder.Flags || updated.View != folder.View ||
		updated.Parent != folder.Parent || len(updated.Grants) != len(next) {
		return core.FolderAccess{}, core.ErrOutcomeUnknown
	}
	if len(next) == 0 {
		// Zimbra drops the now-empty native ACL, including its expiry slots.
		folder.InternalExpiry, folder.GuestExpiry = 0, 0
	}
	if updated.InternalExpiry != folder.InternalExpiry || updated.GuestExpiry != folder.GuestExpiry {
		return core.FolderAccess{}, core.ErrOutcomeUnknown
	}
	for _, g := range updated.Grants {
		if want, exists := next[g.Subject]; !exists || want.Kind != g.Kind || want.Rights != g.Rights ||
			!want.ExpiresAt.Equal(g.ExpiresAt) ||
			updated.GrantExpiry[aclPrincipal{g.Kind, g.Subject}] != rawExpiries[aclPrincipal{g.Kind, g.Subject}] {
			return core.FolderAccess{}, core.ErrOutcomeUnknown
		}
	}
	return after, nil
}

func folderUserACL(folder aclFolder) node {
	acl := element(mailNS, "acl", "")
	if folder.InternalExpiry != 0 {
		acl = attr(acl, "internalGrantExpiry", strconv.FormatUint(folder.InternalExpiry, 10))
	}
	if folder.GuestExpiry != 0 {
		acl = attr(acl, "guestGrantExpiry", strconv.FormatUint(folder.GuestExpiry, 10))
	}
	return acl
}

func validACLUserID(id string) bool {
	return galUUID.MatchString(id) && id != "00000000-0000-0000-0000-000000000000"
}

func (m *mailbox) confirmInternalACLUsers(ctx context.Context, want map[string]bool) error {
	_, ownerDomain, found := strings.Cut(m.owner, "@")
	if !found || ownerDomain == "" {
		return core.ErrAccessDenied
	}
	entries, err := m.native.GAL(ctx)
	if err != nil {
		return semanticError(err)
	}
	for _, entry := range entries {
		if !want[entry.AccountID] {
			continue
		}
		_, domain, _ := strings.Cut(entry.Address, "@")
		if !strings.EqualFold(domain, ownerDomain) || entry.entryType != "" && entry.entryType != "account" {
			return core.ErrAccessDenied
		}
		delete(want, entry.AccountID)
	}
	if len(want) != 0 {
		return core.ErrNotFound
	}
	return nil
}
