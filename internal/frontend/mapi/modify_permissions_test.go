// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func permissionChangeWire(operation byte, memberID uint64, rights uint32, entryID []byte) []byte {
	out := []byte{0x40, 4, 1, 0, 1, 0, operation, 2, 0}
	if operation == permissionAdd {
		out = append32(out, 0x0fff0102)
		out = binary.LittleEndian.AppendUint16(out, uint16(len(entryID)))
		out = append(out, entryID...)
	} else {
		out = append32(out, 0x66710014)
		out = binary.LittleEndian.AppendUint64(out, memberID)
	}
	if operation == permissionRemove {
		out[7] = 1
		return out
	}
	return append32(append32(out, 0x66730003), rights)
}

func permissionBatchWire(flags byte, rows ...[]byte) []byte {
	out := binary.LittleEndian.AppendUint16([]byte{0x40, 4, 1, flags}, uint16(len(rows)))
	for _, row := range rows {
		out = append(out, row[6:]...)
	}
	return out
}

type permissionBatchFixture struct {
	*permissionsMailbox
	writes int
}

func (b *permissionBatchFixture) ReplaceFolderUserGrants(_ context.Context, expected core.FolderAccess, grants []core.AccessGrant) (core.FolderAccess, error) {
	if expected.Token != b.access.Token || expected.Folder != b.access.Folder {
		return core.FolderAccess{}, core.ErrConflict
	}
	b.writes++
	b.access.Grants = grants
	return b.access, nil
}

func TestAtomicModifyPermissionsThroughFolderHandle(t *testing.T) {
	f, b, d := permissionsFixture(t)
	b.access.Token = [32]byte{1}
	b.access.EffectiveRights = core.AccessAdminister
	b.access.Composition = core.AccessReadCreateSubfolders
	b.access.Provenance = core.AccessExplicit
	b.access.Folder = "any/not/zimbra/inbox"
	b.access.Source = b.access.Folder
	writer := &permissionBatchFixture{permissionsMailbox: b}
	f.session = directorySession{&testSession{box: writer}, d}
	scope := f.session.Identity().Scope
	var entryIDs [3][]byte
	for i, account := range []string{"first-grant", "second-grant", "third-grant"} {
		identity := core.Identity{Scope: core.Scope{Backend: scope.Backend, Account: account}, Name: account + "@example.invalid"}
		d.entries = append(d.entries, core.DirectoryEntry{Identity: identity, DisplayName: account})
		var err error
		entryIDs[i], err = f.transport.namespace.mailUserEntryID(identity)
		if err != nil {
			t.Fatal(err)
		}
	}
	requireResult := func(wire []byte, code uint32) {
		t.Helper()
		out, _ := f.run(wire)
		if !bytes.Equal(out, ropError(0x40, 1, code)) {
			t.Fatalf("ModifyPermissions result %x, want %x", out, code)
		}
	}
	a := permissionChangeWire(permissionAdd, 0, 0x401, entryIDs[0])
	bb := permissionChangeWire(permissionAdd, 0, 0x401, entryIDs[1])
	c := permissionChangeWire(permissionAdd, 0, 0x401, entryIDs[2])
	malformed := permissionBatchWire(0, a, bb)
	body, _ := f.execute(malformed[:len(malformed)-1], 65535)
	if !bytes.Equal(body, executeError(0x80070057, 65535)) || b.calls != 0 || writer.writes != 0 {
		t.Fatal("truncated second row reached the ACL")
	}
	d.entries = d.entries[:2]
	requireResult(permissionBatchWire(0, a, c), storeError(core.ErrNotFound))
	if writer.writes != 0 {
		t.Fatal("first row was written before validating the second")
	}
	d.entries = d.entries[:3]
	requireResult(permissionBatchWire(0, a, bb), 0)
	if writer.writes != 1 || len(b.access.Grants) != 2 {
		t.Fatal("did not submit both grants together")
	}
	expires := time.Now().Add(time.Hour)
	b.access.Grants[0].ExpiresAt = expires
	permissionsOpen(t, f, 0)
	rows := permissionsRows(t, f)
	if len(rows) != 4 {
		t.Fatal("two grants absent from ACL table")
	}
	ids := map[string]uint64{}
	for _, row := range rows[2:] {
		ids[row[1].(string)] = row[0].(uint64)
	}
	f.run([]byte{1, 4, 2})
	mod := permissionChangeWire(permissionModify, ids["first-grant"], 0x483, nil)
	del := permissionChangeWire(permissionRemove, ids["second-grant"], 0, nil)
	requireResult(permissionBatchWire(0, mod, del, c), 0)
	if writer.writes != 2 || len(b.access.Grants) != 2 || b.access.Grants[0].Subject != "first-grant" ||
		b.access.Grants[0].Rights != core.AccessReadItems|core.AccessVisible|core.AccessCreateItems|core.AccessCreateSubfolders ||
		!b.access.Grants[0].ExpiresAt.Equal(expires) || b.access.Grants[1].Subject != "third-grant" {
		t.Fatal("mixed batch lost rights, expiry or recipient")
	}
	requireResult(permissionBatchWire(1, a), 0)
	if writer.writes != 3 || len(b.access.Grants) != 1 || b.access.Grants[0].Subject != "first-grant" ||
		!b.access.Grants[0].ExpiresAt.Equal(expires) {
		t.Fatal("ReplaceRows failed to preserve retained expiry or remove the omitted grant")
	}
	requireResult(permissionBatchWire(1), 0)
	if writer.writes != 4 || len(b.access.Grants) != 0 {
		t.Fatal("empty ReplaceRows did not clear the grants")
	}
	permissionsOpen(t, f, 0)
	if rows := permissionsRows(t, f); len(rows) != 2 || rows[0][0] != uint64(0) || rows[0][2] != uint32(0) {
		t.Fatal("ReplaceRows removed or altered the default row")
	}
}

type permissionWriteFixture struct {
	*permissionsMailbox
	writes   int
	writeErr error
	expected core.FolderAccess
}

func (b *permissionWriteFixture) SetFolderAccessGrant(_ context.Context, expected core.FolderAccess, grant core.AccessGrant) (core.FolderAccess, error) {
	b.writes++
	b.expected = expected
	if b.writeErr != nil {
		return core.FolderAccess{}, b.writeErr
	}
	b.access.Grants = []core.AccessGrant{grant}
	return b.access, nil
}

func (b *permissionWriteFixture) RemoveFolderAccessGrant(_ context.Context, expected core.FolderAccess, kind core.AccessSubjectKind, subject string) (core.FolderAccess, error) {
	b.writes++
	b.expected = expected
	if len(b.access.Grants) != 1 || b.access.Grants[0].Kind != kind || b.access.Grants[0].Subject != subject {
		return core.FolderAccess{}, core.ErrNotFound
	}
	b.access.Grants = nil
	return b.access, nil
}

func TestModifyPermissionsThroughFolderHandle(t *testing.T) {
	f, b, d := permissionsFixture(t)
	b.access.Token = [32]byte{1}
	b.access.EffectiveRights = core.AccessAdminister
	b.access.Composition = core.AccessReadCreateSubfolders
	writer := &permissionWriteFixture{permissionsMailbox: b}
	f.session = directorySession{&testSession{box: writer}, d}
	other := core.Identity{Scope: core.Scope{Backend: f.session.Identity().Scope.Backend, Account: "grant-user"}, Name: "user@example.invalid"}
	d.entries = []core.DirectoryEntry{{Identity: other, DisplayName: "Grant User"}}
	entryID, err := f.transport.namespace.mailUserEntryID(other)
	if err != nil {
		t.Fatal("test EntryID")
	}
	add := permissionChangeWire(permissionAdd, 0, 0x401, entryID)
	multi := append(bytes.Clone(add), add[6:]...)
	multi[4] = 2
	replace := bytes.Clone(add)
	replace[3] = 1
	for _, tc := range []struct {
		wire []byte
		code uint32
	}{
		{multi, NotSupported},
		{replace, NotSupported},
		{append(bytes.Clone(add), 0x40), 0x80070057},
	} {
		body, _ := f.execute(tc.wire, 65535)
		if !bytes.Equal(body, executeError(tc.code, 65535)) || b.calls != 0 || writer.writes != 0 {
			t.Fatal("malformed or unsupported batch reached ACL")
		}
	}
	body, _ := f.execute(add, uint32(10+4*len(f.handles)+5))
	if storeResponse(t, body).Operations[0] != 0xff || b.calls != 0 || writer.writes != 0 {
		t.Fatal("insufficient output reached ACL")
	}
	requireResult := func(wire []byte, code uint32) {
		t.Helper()
		out, _ := f.run(wire)
		if !bytes.Equal(out, ropError(0x40, 1, code)) {
			t.Fatalf("ModifyPermissions result %x, want %x", out, code)
		}
	}
	b.access.EffectiveRights = 0
	requireResult(add, accessDenied)
	b.access.EffectiveRights = core.AccessAdminister
	b.access.Completeness = core.AccessUnknown
	requireResult(add, NotSupported)
	b.access.Completeness = core.AccessComplete
	requireResult(permissionChangeWire(permissionAdd, 0, 0x481, entryID), NotSupported)
	if writer.writes != 0 {
		t.Fatal("refused writes reached ACL writer")
	}

	requireResult(add, 0)
	read := core.AccessReadItems | core.AccessVisible
	if writer.writes != 1 || writer.expected.Token != b.access.Token || writer.expected.Folder == "" ||
		len(b.access.Grants) != 1 || b.access.Grants[0].Subject != other.Scope.Account || b.access.Grants[0].Rights != read {
		t.Fatal("grant translation or revision precondition")
	}
	permissionsOpen(t, f, 0)
	rows := permissionsRows(t, f)
	if len(rows) != 3 || rows[2][2] != uint32(0x401) || !bytes.Equal(rows[2][3].([]byte), entryID) {
		t.Fatal("added grant not projected")
	}
	memberID := rows[2][0].(uint64)
	f.run([]byte{1, 4, 2})
	modify := permissionChangeWire(permissionModify, memberID, 0x483, nil)
	requireResult(modify, 0)
	if writer.writes != 2 || len(b.access.Grants) != 1 || b.access.Grants[0].Rights != read|core.AccessCreateItems|core.AccessCreateSubfolders {
		t.Fatal("member update translation")
	}
	writer.writeErr = core.ErrOutcomeUnknown
	requireResult(modify, storeError(core.ErrOutcomeUnknown))
	if writer.writes != 3 {
		t.Fatal("uncertain write retried")
	}
	writer.writeErr = nil
	remove := permissionChangeWire(permissionRemove, memberID, 0, nil)
	requireResult(remove, 0)
	requireResult(remove, 0)
	if writer.writes != 4 || len(b.access.Grants) != 0 {
		t.Fatal("grant removal or absent row handling")
	}
}
