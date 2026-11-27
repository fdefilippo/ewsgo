// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"time"

	"ewsgo/internal/core"
)

const (
	permissionAdd    = 0x01
	permissionModify = 0x02
	permissionRemove = 0x04
)

type permissionRequest struct {
	rows    []permissionChange
	replace bool
}

func (q permissionRequest) atomic() bool { return q.replace || len(q.rows) > 1 }

type permissionChange struct {
	operation byte
	entryID   []byte
	memberID  uint64
	rights    uint32
}

func (permissionChange) String() string   { return "permissionChange{private}" }
func (permissionChange) GoString() string { return "permissionChange{private}" }

func parsePermissionRequest(r *reader, request *permissionRequest) error {
	header := r.take(3)
	if r.err != nil {
		return r.err
	}
	flags, count := header[0], binary.LittleEndian.Uint16(header[1:])
	if flags&^byte(3) != 0 {
		return ErrMalformed
	}
	if count > core.MaxFolderAccessGrants || flags&2 != 0 {
		return ErrUnsupported
	}
	request.replace = flags&1 != 0
	request.rows = make([]permissionChange, count)
	for i := range request.rows {
		change := &request.rows[i]
		if err := parsePermissionChange(r, change); err != nil {
			return err
		}
		if request.replace && change.operation != permissionAdd {
			return ErrMalformed
		}
	}
	return nil
}

func parsePermissionChange(r *reader, change *permissionChange) error {
	row := r.take(3)
	if r.err != nil {
		return r.err
	}
	change.operation = row[0]
	propertyCount := binary.LittleEndian.Uint16(row[1:])
	var tags uint8
	switch change.operation {
	case permissionAdd, permissionModify, permissionRemove:
	default:
		return ErrMalformed
	}
	if propertyCount > 2 {
		return ErrUnsupported
	}
	for range propertyCount {
		var bit uint8
		tag := r.u32()
		if r.err != nil {
			return r.err
		}
		switch tag {
		case 0x0fff0102:
			bit = 1
			size := r.take(2)
			if r.err != nil {
				return r.err
			}
			change.entryID = r.take(int(binary.LittleEndian.Uint16(size)))
		case 0x66710014:
			bit = 2
			raw := r.take(8)
			if r.err == nil {
				change.memberID = binary.LittleEndian.Uint64(raw)
			}
		case 0x66730003:
			bit = 4
			change.rights = r.u32()
		default:
			return ErrUnsupported
		}
		if r.err != nil || tags&bit != 0 {
			return ErrMalformed
		}
		tags |= bit
	}
	expectedTags := map[byte]uint8{permissionAdd: 5, permissionModify: 6, permissionRemove: 2}
	if tags != expectedTags[change.operation] {
		return ErrMalformed
	}
	if change.operation == permissionAdd && !isMailUserEntryID(change.entryID) ||
		change.operation != permissionAdd && (change.memberID == 0 || change.memberID == ^uint64(0)) {
		return ErrUnsupported
	}
	rights := change.rights
	if rights&^uint32(0x1ffb) != 0 || rights&0x20 != 0 && rights&8 == 0 ||
		rights&0x40 != 0 && rights&0x10 == 0 || rights&0x101 != 0 && rights&0x400 == 0 ||
		rights&0x1000 != 0 && rights&0x800 == 0 {
		return ErrMalformed
	}
	if rights&0x1b00 != 0 {
		return ErrUnsupported
	}
	return nil
}

func isMailUserEntryID(id []byte) bool {
	return len(id) > len(mailUserEntryIDHeader)+1 && len(id) <= maxOwnAddressBookEntryID &&
		id[len(id)-1] == 0 && bytes.HasPrefix(id, []byte(mailUserEntryIDHeader)) &&
		bytes.IndexByte(id[len(mailUserEntryIDHeader):len(id)-1], 0) == -1
}

func (t *SessionTransport) modifyPermissions(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	result := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return result(code)
	}
	if object.isLogon || object.download != nil {
		return result(NotSupported)
	}
	store := object.binding.store
	if session == nil || session.Identity().Scope != store.scope {
		return result(accessDenied)
	}
	if err := authorizeHierarchyTable(ctx, session, store, object.fid); err != nil {
		return result(storeError(err))
	}
	folder, err := store.folder(ctx, session, object.fid)
	if err != nil {
		return result(storeError(err))
	}
	box := session.Mailbox()
	reader, hasReader := box.(core.FolderAccessReader)
	writer, hasWriter := box.(core.FolderAccessWriter)
	batchWriter, hasBatchWriter := box.(core.FolderUserGrantsWriter)
	request := op.PermissionChange
	if folder.native == "" || !hasReader || request.atomic() && !hasBatchWriter || !request.atomic() && !hasWriter {
		return result(NotSupported)
	}
	lease, err := store.reserveContentsPeak(ctx, permissionReadPeak)
	if err != nil {
		return result(storeError(err))
	}
	defer lease.release()
	access, err := reader.ReadFolderAccess(ctx, folder.native)
	if err != nil {
		return result(storeError(err))
	}
	if access.Scope != store.scope || access.Folder != folder.native || !access.CanViewList ||
		access.EffectiveRights&core.AccessAdminister == 0 {
		return result(accessDenied)
	}
	if access.Completeness != core.AccessComplete || access.Token == [32]byte{} || access.Source == "" ||
		access.Provenance != core.AccessExplicit && access.Provenance != core.AccessInherited ||
		access.Composition > core.AccessReadCreateSubfolders || len(access.Grants) > core.MaxFolderAccessGrants {
		return result(NotSupported)
	}
	if request.atomic() {
		return result(t.replaceFolderUserGrants(ctx, session, store, batchWriter, access, request))
	}
	if len(request.rows) == 0 {
		return result(0)
	}
	change := request.rows[0]
	var scope core.Scope
	if change.operation == permissionAdd {
		id := change.entryID[len(mailUserEntryIDHeader) : len(change.entryID)-1]
		scope, code = t.permissionEntryScope(ctx, session, id)
		if code != 0 {
			return result(code)
		}
	} else {
		scope, err = store.permissionMemberScope(ctx, change.memberID)
		if err != nil {
			return result(storeError(err))
		}
	}
	var previous core.AccessGrant
	var baseline core.AccessRights
	now := time.Now()
	for _, grant := range access.Grants {
		active := grant.ExpiresAt.IsZero() || now.Before(grant.ExpiresAt)
		switch grant.Kind {
		case core.AccessAuthenticated, core.AccessPublic:
			if active {
				baseline |= grant.Rights
			}
		case core.AccessUser:
			if scope.Account != "" && grant.Subject == scope.Account {
				previous = grant
			}
		}
	}
	present := previous.Subject != "" && (previous.ExpiresAt.IsZero() || now.Before(previous.ExpiresAt))
	if change.operation != permissionAdd && !present {
		return result(0) // MS-OXCPERM ignores modification/deletion of an absent row.
	}
	if change.operation == permissionRemove {
		_, err = writer.RemoveFolderAccessGrant(ctx, access, core.AccessUser, scope.Account)
		return result(permissionWriteCode(err))
	}
	if change.operation == permissionAdd && previous.Subject != "" && !present {
		return result(NotSupported) // The writer preserves the existing native expiry.
	}
	rights, err := nativePermissionRights(access.Composition, change.rights, baseline, previous.Rights)
	if err != nil {
		return result(storeError(err))
	}
	if present && change.operation == permissionAdd && rights != previous.Rights {
		return result(storeError(core.ErrConflict))
	}
	_, err = writer.SetFolderAccessGrant(ctx, access, core.AccessGrant{Kind: core.AccessUser, Subject: scope.Account, Rights: rights})
	return result(permissionWriteCode(err))
}

func (s *ramStore) permissionMemberScope(ctx context.Context, id uint64) (core.Scope, error) {
	if s.durable.store != nil {
		account, err := s.durable.store.MAPIPermissionMember(ctx, s.durable.mailbox, id)
		if errors.Is(err, core.ErrNotFound) {
			return core.Scope{}, nil
		}
		if err != nil {
			return core.Scope{}, err
		}
		return core.Scope{Backend: s.scope.Backend, Account: account}, nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for scope, member := range s.permissionMembers {
		if member == id {
			return scope, nil
		}
	}
	return core.Scope{}, nil
}

func (t *SessionTransport) permissionEntryScope(ctx context.Context, session core.Session, dn []byte) (core.Scope, uint32) {
	directory, code := t.visibleDirectory(ctx, session)
	if code != 0 {
		return core.Scope{}, code
	}
	return t.permissionDirectoryScope(directory, session.Identity().Scope, dn)
}

func (t *SessionTransport) permissionDirectoryScope(directory map[core.Scope]core.DirectoryEntry, owner core.Scope, dn []byte) (core.Scope, uint32) {
	for scope, entry := range directory {
		mapped, err := t.namespace.Mailbox(entry.Identity)
		if err == nil && mapped.LegacyDN == string(dn) {
			if scope == owner {
				return core.Scope{}, accessDenied
			}
			return scope, 0 // The native writer verifies the recipient type before mutation.
		}
	}
	return core.Scope{}, storeError(core.ErrNotFound)
}

func nativePermissionRights(composition core.AccessComposition, requested uint32, baseline, previous core.AccessRights) (core.AccessRights, error) {
	if _, err := permissionRights(baseline | previous); err != nil {
		return 0, err
	}
	var desired core.AccessRights
	for _, pair := range permissionRightPairs {
		if requested&pair.wire != 0 {
			desired |= pair.source
		}
	}
	// Preserve overlapping explicit grants while keeping the inherited
	// baseline out of new explicit rights. Zimbra derives subfolder creation
	// after the union of baseline and explicit rights.
	explicit := (desired &^ baseline) | (previous & baseline)
	if composition == core.AccessReadCreateSubfolders {
		explicit = composeFolderAccess(composition, explicit&^core.AccessCreateSubfolders)
	}
	wire, err := permissionRights(composeFolderAccess(composition, explicit|baseline))
	if err != nil || explicit == 0 || wire != requested {
		return 0, core.ErrUnsupported
	}
	return explicit, nil
}

func permissionWriteCode(err error) uint32 {
	if err == nil {
		return 0
	}
	return storeError(err)
}
