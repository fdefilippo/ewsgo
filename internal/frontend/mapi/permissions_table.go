// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"sort"
	"time"
	"unicode"
	"unicode/utf8"

	"ewsgo/internal/core"
)

const maxPermissionMembers = 1024
const permissionReadPeak = 8 << 20

// A detached complete observation, not a cached authorization decision. Every
// Table operation rechecks the source, grant expiry and current GAL visibility.
type permissionsSnapshot struct {
	digest [32]byte
	flags  byte
	denied bool
}
type permissionsVisibilityDenied struct{}

func (permissionsVisibilityDenied) Error() string { return "permissions visibility denied" }
func (permissionsVisibilityDenied) Unwrap() error { return core.ErrAccessDenied }

type permissionCandidate struct {
	Scope   core.Scope
	Name    string
	EntryID []byte
	Rights  uint32
}

func (permissionsSnapshot) String() string   { return "permissionsSnapshot{private}" }
func (permissionsSnapshot) GoString() string { return "permissionsSnapshot{private}" }
func (permissionCandidate) String() string   { return "permissionCandidate{private}" }
func (permissionCandidate) GoString() string { return "permissionCandidate{private}" }

// permissionRows returns candidates without allocating stable IDs. Default
// and Anonymous are derived from the full ACL. Explicit users receive the
// union with all/public, matching the source's cumulative grant semantics.
func (t *SessionTransport) permissionRows(ctx context.Context, session core.Session, store *ramStore, fid uint64, flags byte) ([]permissionCandidate, [32]byte, error) {
	return t.permissionRowsAt(ctx, session, store, fid, flags, time.Time{})
}

func (t *SessionTransport) permissionRowsAt(ctx context.Context, session core.Session, store *ramStore, fid uint64, flags byte, now time.Time) ([]permissionCandidate, [32]byte, error) {
	var zero [32]byte
	if err := ctx.Err(); err != nil {
		return nil, zero, err
	}
	if err := authorizeHierarchyTable(ctx, session, store, fid); err != nil {
		return nil, zero, err
	}
	folder, err := store.folder(ctx, session, fid)
	if err != nil {
		return nil, zero, err
	}
	calendar := folder.properties[core.WellKnown] == core.Text("calendar")
	if flags&2 != 0 && !calendar {
		return nil, zero, core.ErrInvalid
	}
	access := core.FolderAccess{Scope: store.scope, Completeness: core.AccessComplete, Provenance: core.AccessServicePolicy, CanViewList: true}
	if folder.native != "" {
		reader, ok := session.Mailbox().(core.FolderAccessReader)
		if !ok {
			return nil, zero, core.ErrUnsupported
		}
		access, err = reader.ReadFolderAccess(ctx, folder.native)
		if errors.Is(err, core.ErrAccessDenied) {
			return nil, zero, permissionsVisibilityDenied{}
		}
		if err != nil {
			return nil, zero, err
		}
		if access.Scope != store.scope || access.Folder != folder.native {
			return nil, zero, core.ErrAccessDenied
		}
		if access.Provenance != core.AccessExplicit && access.Provenance != core.AccessInherited || access.Source == "" {
			return nil, zero, core.ErrUnsupported
		}
	}
	if !access.CanViewList {
		return nil, zero, permissionsVisibilityDenied{}
	}
	if access.Completeness != core.AccessComplete || len(access.Grants) > core.MaxFolderAccessGrants || access.Composition > core.AccessReadCreateSubfolders {
		return nil, zero, core.ErrUnsupported
	}
	liveClock := now.IsZero()
	if liveClock {
		now = time.Now()
	}
	var deadline time.Time
	var authenticated, public core.AccessRights
	users := map[core.Scope]core.AccessRights{}
	seen := map[struct {
		kind    core.AccessSubjectKind
		subject string
	}]bool{}
	active := make([]bool, len(access.Grants))
	for i, g := range access.Grants {
		key := struct {
			kind    core.AccessSubjectKind
			subject string
		}{g.Kind, g.Subject}
		// all/public designate one subject per kind; native sentinel IDs are not
		// exposed and cannot create multiple reserved rows.
		if g.Kind == core.AccessAuthenticated || g.Kind == core.AccessPublic {
			key.subject = ""
		}
		if seen[key] || !permissionText(g.Subject, 512) || g.Kind < core.AccessUser || g.Kind > core.AccessKey {
			return nil, zero, core.ErrUnsupported
		}
		seen[key] = true
		if !g.ExpiresAt.IsZero() && !now.Before(g.ExpiresAt) {
			continue
		}
		active[i] = true
		if !g.ExpiresAt.IsZero() && (deadline.IsZero() || g.ExpiresAt.Before(deadline)) {
			deadline = g.ExpiresAt
		}
		switch g.Kind {
		case core.AccessAuthenticated:
			authenticated |= g.Rights
		case core.AccessPublic:
			public |= g.Rights
		case core.AccessUser:
			if g.Subject == "" {
				return nil, zero, core.ErrUnsupported
			}
			users[core.Scope{Backend: store.scope.Backend, Account: g.Subject}] = g.Rights
		default:
			return nil, zero, core.ErrUnsupported // Never silently omit an unrepresentable grant.
		}
	}
	compose := func(r core.AccessRights) (uint32, error) {
		return permissionRights(composeFolderAccess(access.Composition, r))
	}
	defaultRights, err := compose(authenticated | public)
	if err != nil {
		return nil, zero, err
	}
	anonymousRights, err := compose(public)
	if err != nil {
		return nil, zero, err
	}
	rows := []permissionCandidate{{Rights: defaultRights}, {Name: "Anonymous", Rights: anonymousRights}}
	if len(users) > 0 {
		directory, code := t.visibleDirectory(ctx, session)
		if code != 0 {
			if code == accessDenied {
				return nil, zero, core.ErrAccessDenied
			}
			return nil, zero, core.ErrUnsupported
		}
		scopes := make([]core.Scope, 0, len(users))
		for scope := range users {
			scopes = append(scopes, scope)
		}
		sort.Slice(scopes, func(i, j int) bool { return scopes[i].Account < scopes[j].Account })
		for _, scope := range scopes {
			entry, ok := directory[scope]
			if !ok || entry.DisplayName == "" || !permissionText(entry.DisplayName, 2048) {
				return nil, zero, core.ErrUnsupported
			}
			// Native AccessUser proves recipient type; GAL proves current visibility.
			id, e := t.namespace.mailUserEntryID(entry.Identity)
			if e != nil {
				return nil, zero, e
			}
			rights, e := compose(users[scope] | authenticated | public)
			if e != nil {
				return nil, zero, e
			}
			rows = append(rows, permissionCandidate{Scope: scope, Name: entry.DisplayName, EntryID: id, Rights: rights})
		}
	}
	// Do not hash observation time or trust an opaque backend token alone. The
	// validity vector changes at expiry even when source metadata is unchanged.
	access.ObservedAt = time.Time{}
	access.Token = [32]byte{}
	raw, err := json.Marshal(struct {
		Access core.FolderAccess
		Active []bool
		Rows   []permissionCandidate
	}{access, active, rows})
	if err != nil {
		return nil, zero, core.ErrInvalid
	}
	if err = authorizeHierarchyTable(ctx, session, store, fid); err != nil {
		return nil, zero, err
	}
	if liveClock && !deadline.IsZero() && !time.Now().Before(deadline) {
		return nil, zero, core.ErrConflict
	}
	return rows, sha256.Sum256(raw), ctx.Err()
}

func permissionText(s string, limit int) bool {
	if len(s) > limit || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// Administration, private/workflow and free/busy have no qualified exact
// mapping yet. Reject the whole source instead of overstating a user's rights.
var permissionRightPairs = [...]struct {
	source core.AccessRights
	wire   uint32
}{
	{core.AccessReadItems, 1}, {core.AccessCreateItems, 2}, {core.AccessEditOwned, 8},
	{core.AccessDeleteOwned, 0x10}, {core.AccessEditAny, 0x20}, {core.AccessDeleteAny, 0x40},
	{core.AccessCreateSubfolders, 0x80}, {core.AccessVisible, 0x400},
}

func permissionRights(r core.AccessRights) (uint32, error) {
	var mapped uint32
	for _, p := range permissionRightPairs {
		if r&p.source != 0 {
			mapped |= p.wire
			r &^= p.source
		}
	}
	if r != 0 {
		return 0, core.ErrUnsupported
	}
	return mapped, nil
}

func composeFolderAccess(composition core.AccessComposition, rights core.AccessRights) core.AccessRights {
	const readCreate = core.AccessReadItems | core.AccessCreateItems
	if composition == core.AccessReadCreateSubfolders && rights&readCreate == readCreate {
		rights |= core.AccessCreateSubfolders
	}
	return rights
}

func (t *SessionTransport) capturePermissionsTable(ctx context.Context, session core.Session, store *ramStore, fid uint64, flags byte) (*contentsTable, error) {
	table := &contentsTable{permissions: &permissionsSnapshot{flags: flags}}
	fail := func(e error) (*contentsTable, error) { table.close(); return nil, e }
	var err error
	table.lease, err = store.reserveContentsPeak(ctx, permissionReadPeak)
	if err != nil {
		return nil, err
	}
	rows, digest, err := t.permissionRows(ctx, session, store, fid, flags)
	var denied permissionsVisibilityDenied
	if errors.As(err, &denied) {
		if err = authorizeHierarchyTable(ctx, session, store, fid); err != nil {
			return fail(err)
		}
		// MS-OXCPERM defers a visibility denial to QueryRows. This handle has no
		// retained rows and can never turn into a successful empty ACL result.
		table.permissions.denied = true
		if err = table.lease.shrink(1024 + 4*maxTableColumns); err != nil {
			return fail(err)
		}
		return table, nil
	}
	if err != nil {
		return fail(err)
	}
	_, confirmed, err := t.permissionRows(ctx, session, store, fid, flags)
	if err != nil {
		return fail(err)
	}
	if digest != confirmed {
		return fail(core.ErrConflict)
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	scopes := make([]core.Scope, 0, len(rows)-2)
	for _, r := range rows[2:] {
		scopes = append(scopes, r.Scope)
	}
	ids, err := store.permissionMemberIDs(scopes)
	if err != nil {
		return fail(err)
	}
	used := 1024 + 4*maxTableColumns
	for i, r := range rows {
		id := uint64(0)
		if i == 1 {
			id = ^uint64(0)
		} else if i > 1 {
			id = ids[r.Scope]
		}
		values := contentsValues{}
		putContentsValue(values, 0x66710014, core.Integer(id))
		putContentsValue(values, 0x6672001f, core.Text(r.Name))
		putContentsValue(values, 0x66730003, core.Integer(r.Rights))
		putContentsValue(values, 0x0fff0102, core.Binary(r.EntryID))
		table.rows = append(table.rows, values)
		used += 1024 + 3*len(r.Name) + len(r.EntryID)
	}
	table.permissions.digest = digest
	if err = table.lease.shrink(used); err != nil {
		return fail(err)
	}
	return table, nil
}

func (s *ramStore) permissionMemberIDs(scopes []core.Scope) (map[core.Scope]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	newKeys := map[core.Scope]bool{}
	for _, scope := range scopes {
		if scope.Backend != s.scope.Backend || scope.Account == "" || !permissionText(scope.Account, 512) {
			return nil, core.ErrInvalid
		}
		if s.permissionMembers[scope] == 0 {
			newKeys[scope] = true
		}
	}
	if len(s.permissionMembers)+len(newKeys) > maxPermissionMembers {
		return nil, core.ErrUnsupported
	}
	if len(newKeys) > 0 && s.permissionMemberLease == nil {
		// One bounded permanent reservation includes all 1024 keys/map overhead.
		// It survives Table Release/reconnect, exactly like the MemberId registry.
		lease, err := s.reserveContentsMemory(maxPermissionMembers * 1024)
		if err != nil {
			return nil, err
		}
		s.permissionMemberLease = lease
		if s.permissionMembers == nil {
			s.permissionMembers = make(map[core.Scope]uint64)
		}
	}
	ids := make(map[core.Scope]uint64, len(scopes))
	for _, scope := range scopes {
		if s.permissionMembers[scope] == 0 {
			s.permissionMembers[scope] = uint64(len(s.permissionMembers) + 1)
		}
		ids[scope] = s.permissionMembers[scope]
	}
	return ids, nil
}

func (t *SessionTransport) authorizePermissionsTable(ctx context.Context, session core.Session, object serverObject, op byte) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	p := object.table.permissions
	if session == nil || session.Identity().Scope != object.binding.store.scope {
		return core.ErrAccessDenied
	}
	lease, err := object.binding.store.reserveContentsPeak(ctx, permissionReadPeak)
	if err != nil {
		return err
	}
	defer lease.release()
	_, digest, err := t.permissionRows(ctx, session, object.binding.store, object.fid, p.flags)
	if p.denied {
		// SetColumns/status contain no ACL data. No row/position operation may
		// expose a denied snapshot, even if permission is later granted.
		if (err == nil || errors.Is(err, core.ErrAccessDenied)) && (op == 0x12 || op == 0x16) {
			return nil
		}
		return core.ErrAccessDenied
	}
	if err != nil {
		return err
	}
	if digest != p.digest {
		return core.ErrConflict
	}
	return ctx.Err()
}
