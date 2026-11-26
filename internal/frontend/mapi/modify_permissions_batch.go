// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"cmp"
	"context"
	"maps"
	"slices"
	"time"

	"ewsgo/internal/core"
)

func (t *SessionTransport) replaceFolderUserGrants(ctx context.Context, session core.Session, store *ramStore, writer core.FolderUserGrantsWriter, access core.FolderAccess, request permissionRequest) uint32 {
	if access.Provenance != core.AccessExplicit || access.Source != access.Folder {
		return NotSupported
	}
	before := make(map[string]core.AccessGrant, len(access.Grants))
	for _, grant := range access.Grants {
		if grant.Kind != core.AccessUser || grant.Subject == "" {
			return NotSupported
		}
		if _, duplicate := before[grant.Subject]; duplicate {
			return NotSupported
		}
		before[grant.Subject] = grant
	}
	next := make(map[string]core.AccessGrant, len(before))
	if !request.replace {
		maps.Copy(next, before)
	}

	now := time.Now()
	var directory map[core.Scope]core.DirectoryEntry
	for _, change := range request.rows {
		scope := store.permissionMemberScope(change.memberID)
		if change.operation == permissionAdd {
			if directory == nil {
				var code uint32
				directory, code = t.visibleDirectory(ctx, session)
				if code != 0 {
					return code
				}
			}
			dn := change.entryID[len(mailUserEntryIDHeader) : len(change.entryID)-1]
			var code uint32
			scope, code = t.permissionDirectoryScope(directory, session.Identity().Scope, dn)
			if code != 0 {
				return code
			}
		}
		previous, present := next[scope.Account]
		active := present && (previous.ExpiresAt.IsZero() || now.Before(previous.ExpiresAt))
		if change.operation != permissionAdd && !active {
			continue
		}
		if change.operation == permissionRemove {
			delete(next, scope.Account)
			continue
		}
		expiry := before[scope.Account].ExpiresAt
		if !expiry.IsZero() && !now.Before(expiry) {
			return NotSupported
		}
		rights, err := nativePermissionRights(access.Composition, change.rights, 0, previous.Rights)
		if err != nil {
			return storeError(err)
		}
		if present && change.operation == permissionAdd && rights != previous.Rights {
			return storeError(core.ErrConflict)
		}
		next[scope.Account] = core.AccessGrant{Kind: core.AccessUser, Subject: scope.Account, Rights: rights, ExpiresAt: expiry}
	}
	if len(next) > core.MaxFolderAccessGrants {
		return NotSupported
	}
	grants := slices.SortedFunc(maps.Values(next), func(a, b core.AccessGrant) int { return cmp.Compare(a.Subject, b.Subject) })
	_, err := writer.ReplaceFolderUserGrants(ctx, access, grants)
	return permissionWriteCode(err)
}
