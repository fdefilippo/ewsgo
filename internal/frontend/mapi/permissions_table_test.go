// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"ewsgo/internal/core"
)

type permissionsMailbox struct {
	core.Mailbox
	state  *stateMailbox
	access core.FolderAccess
	err    error
	calls  int
	hook   func(int)
}

func (b *permissionsMailbox) MailboxState(ctx context.Context) (core.MailboxState, error) {
	return b.state.MailboxState(ctx)
}
func (b *permissionsMailbox) SpecialFolder(ctx context.Context, kind core.SpecialFolderKind) (core.Object, error) {
	if r, ok := b.Mailbox.(core.SpecialFolderReader); ok {
		return r.SpecialFolder(ctx, kind)
	}
	return core.Object{}, core.ErrUnsupported
}
func (b *permissionsMailbox) ReadFolderAccess(_ context.Context, id core.ID) (core.FolderAccess, error) {
	b.calls++
	if b.hook != nil {
		b.hook(b.calls)
	}
	a := b.access
	a.Folder = id
	return a, b.err
}
func permissionsFixture(t *testing.T) (*ostFixture, *permissionsMailbox, *directoryFixture) {
	t.Helper()
	f := newOSTFixture(t)
	f.handles = append(f.handles, 0xffffffff, 0xffffffff)
	f.transport.EnableContentsTableSpike()
	b := &permissionsMailbox{Mailbox: f.box, state: f.box, access: core.FolderAccess{Scope: f.session.Identity().Scope, Source: f.box.Root(), CanViewList: true, Completeness: core.AccessComplete, Provenance: core.AccessInherited}}
	d := &directoryFixture{}
	f.session = directorySession{&testSession{box: b}, d}
	t.Cleanup(f.state.mailbox.close)
	return f, b, d
}
func permissionsOpen(t *testing.T, f *ostFixture, flags byte) (*contentsTable, *ramStore) {
	t.Helper()
	out, c := f.run([]byte{0x3e, 4, 1, 2, flags})
	if !bytes.Equal(out, []byte{0x3e, 2, 0, 0, 0, 0}) || c.errors != 0 {
		t.Fatal("Permissions Table open wire/error", c.errors)
	}
	o, code := f.state.mailbox.lookup(f.handles[2], 4)
	if code != 0 || o.table == nil || o.table.permissions == nil {
		t.Fatal("Permissions handle missing")
	}
	return o.table, o.binding.store
}

var permissionTags = []uint32{0x66710014, 0x6672001f, 0x66730003, 0x0fff0102, 0x66740003}

func permissionsRows(t *testing.T, f *ostFixture) [][]any {
	t.Helper()
	out, c := f.run(tableColumnsWire(2, permissionTags...))
	if c.errors != 0 || len(out) != 7 {
		t.Fatal("Permissions columns")
	}
	out, c = f.run(tableQueryWire(2, 0, 1, 65535))
	if c.errors != 0 {
		t.Fatal("Permissions query", c.errors)
	}
	return decodeTableRows(t, out, permissionTags)
}
func TestPermissionsTableCodecAndAtomicPreflight(t *testing.T) {
	f, b, _ := permissionsFixture(t)
	for flag := 0; flag < 256; flag++ {
		_, err := (ROPBuffer{Operations: []byte{0x3e, 4, 1, 2, byte(flag)}, Handles: f.handles}).decodeTableOperations(true, true, true, true, false, false, false, false, false, true, true, true)
		if (err == nil) != (flag == 0 || flag == 2) {
			t.Fatal("Permissions flags")
		}
	}
	for _, bad := range [][]byte{{0x3e, 4, 1}, {0x3e, 4, 1, 2}, {0x3e, 4, 1, 9, 0}, {0x3e, 4, 9, 2, 0}, {0x3e, 4, 1, 2, 1}, {0x40, 4, 1}} {
		f.execute(append([]byte{1, 4, 1}, bad...), 65535)
		if _, c := f.state.mailbox.object(f.handles[1], 4); c != 0 || b.calls != 0 || f.transport.stores.contents.used != 0 {
			t.Fatal("bad suffix executed prefix")
		}
	}
	// 6 bytes, not the Contents/Hierarchy 10-byte response. Exact capacity succeeds.
	body, _ := f.execute([]byte{0x3e, 4, 1, 2, 0}, uint32(10+4*len(f.handles)+5))
	if storeResponse(t, body).Operations[0] != 0xff || b.calls != 0 {
		t.Fatal("small output caused reads")
	}
	body, c := f.execute([]byte{0x3e, 4, 1, 2, 0}, uint32(10+4*len(f.handles)+6))
	if c.errors != 0 || !bytes.Equal(storeResponse(t, body).Operations, []byte{0x3e, 2, 0, 0, 0, 0}) {
		t.Fatal("exact output response")
	}
	f.state.mailbox.close()
	f.transport.contentsTableSpike = false
	f.execute([]byte{0x3e, 4, 1, 2, 0}, 65535)
	if b.calls != 2 {
		t.Fatal("default-off parser reached backend")
	}
}
func TestPermissionsTableReservedRowsUsersAndReconnect(t *testing.T) {
	f, b, d := permissionsFixture(t)
	other := core.Identity{Scope: core.Scope{Backend: f.session.Identity().Scope.Backend, Account: "grant-user"}, Name: "user@example.invalid"}
	d.entries = []core.DirectoryEntry{{Identity: other, DisplayName: "User É"}}
	b.access.Composition = core.AccessReadCreateSubfolders
	b.access.Grants = []core.AccessGrant{
		{Kind: core.AccessUser, Subject: other.Scope.Account, Rights: core.AccessCreateItems | core.AccessEditOwned | core.AccessEditAny | core.AccessDeleteOwned | core.AccessDeleteAny},
		{Kind: core.AccessAuthenticated, Rights: core.AccessCreateItems},
		{Kind: core.AccessPublic, Rights: core.AccessReadItems | core.AccessVisible},
	}
	table, store := permissionsOpen(t, f, 0)
	rows := permissionsRows(t, f)
	mapped, _ := f.transport.namespace.Mailbox(other)
	expected := [][]any{
		{uint64(0), "", uint32(0x483), []byte(nil), uint32(PropertyNotFound)},
		{^uint64(0), "Anonymous", uint32(0x401), []byte(nil), uint32(PropertyNotFound)},
		{uint64(1), "User É", uint32(0x4fb), addressBookEntryFixture(mapped.LegacyDN), uint32(PropertyNotFound)},
	}
	if !reflect.DeepEqual(rows, expected) || table.cursor != 3 {
		t.Fatal("Reserved/user rights or EntryID wire differ", len(rows))
	}
	f.run(tableSeekWire(2, 0, 0))
	tags := []uint32{0x6672001e, 0x66710014}
	f.run(tableColumnsWire(2, tags...))
	out, c := f.run(tableQueryWire(2, 1, 1, 3))
	if c.errors != 0 || decodeTableRows(t, out, tags)[2][0] != "User \xc9" || table.cursor != 0 {
		t.Fatal("String8/noadvance")
	}
	retained := maxPermissionMembers * 1024
	f.run([]byte{1, 4, 2})
	if store.allocator.contents.used != retained {
		t.Fatal("Release snapshot/registry budget")
	}
	oldID := store.permissionMembers[other.Scope]
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	f.open(int(Inbox))
	permissionsOpen(t, f, 0)
	if permissionsRows(t, f)[2][0] != oldID {
		t.Fatal("MemberId changed on reconnect")
	}
	f.run([]byte{1, 4, 2})
	// Removed grants cannot cause MemberId reuse for another user.
	next := other
	next.Scope.Account = "next-user"
	next.Name = "next@example.invalid"
	b.access.Grants = []core.AccessGrant{{Kind: core.AccessUser, Subject: next.Scope.Account}}
	d.entries = []core.DirectoryEntry{{Identity: next, DisplayName: "Next"}}
	permissionsOpen(t, f, 0)
	if permissionsRows(t, f)[2][0] != uint64(2) {
		t.Fatal("MemberId reused")
	}
	for _, private := range []any{b.access, permissionCandidate{Scope: other.Scope, Name: "secret-marker"}, table.permissions} {
		if bytes.Contains([]byte(fmt.Sprintf("%+v %#v", private, private)), []byte("secret-marker")) {
			t.Fatal("Private formatting")
		}
	}
}
func TestPermissionsTableDeniedUnknownAndUnrepresentable(t *testing.T) {
	for _, mode := range []string{"visibility", "native_denied", "unknown", "group", "domain", "cos", "guest", "key", "admin", "private", "workflow", "freebusy", "unresolved", "duplicate", "source", "too_many", "flags", "backend"} {
		t.Run(mode, func(t *testing.T) {
			f, b, _ := permissionsFixture(t)
			flags := byte(0)
			grant := core.AccessGrant{Kind: core.AccessUser, Subject: "unresolved"}
			switch mode {
			case "visibility":
				b.access.CanViewList = false
			case "native_denied":
				b.err = core.ErrAccessDenied
			case "unknown":
				b.access.Completeness = core.AccessUnknown
			case "group":
				grant.Kind = core.AccessGroup
			case "domain":
				grant.Kind = core.AccessDomain
			case "cos":
				grant.Kind = core.AccessCOS
			case "guest":
				grant.Kind = core.AccessGuest
			case "key":
				grant.Kind = core.AccessKey
			case "admin":
				grant.Rights = core.AccessAdminister
			case "private":
				grant.Rights = core.AccessPrivate
			case "workflow":
				grant.Rights = core.AccessWorkflow
			case "freebusy":
				grant.Rights = core.AccessFreeBusySimple
			case "source":
				b.access.Source = ""
			case "too_many":
				b.access.Grants = make([]core.AccessGrant, core.MaxFolderAccessGrants+1)
			case "flags":
				flags = 2
			case "backend":
				b.err = core.ErrConflict
			}
			if mode == "duplicate" {
				b.access.Grants = []core.AccessGrant{grant, grant}
			} else if len(b.access.Grants) == 0 && mode != "visibility" && mode != "native_denied" {
				b.access.Grants = []core.AccessGrant{grant}
			}
			if mode == "visibility" || mode == "native_denied" {
				table, store := permissionsOpen(t, f, flags)
				f.run(tableColumnsWire(2, permissionTags...))
				out, c := f.run(tableQueryWire(2, 0, 1, 1))
				if c.errors != 1 || !bytes.Equal(out, ropError(0x15, 2, accessDenied)) || table.cursor != 0 {
					t.Fatal("Denied QueryRows reported fake empty result")
				}
				b.err = nil
				b.access.CanViewList = true
				out, _ = f.run(tableQueryWire(2, 0, 1, 1))
				if !bytes.Equal(out, ropError(0x15, 2, accessDenied)) {
					t.Fatal("Denied snapshot upgraded without reopening")
				}
				f.run([]byte{1, 4, 2})
				if store.allocator.contents.used != 0 {
					t.Fatal("Denied table leaked")
				}
				return
			}
			out, c := f.run([]byte{0x3e, 4, 1, 2, flags})
			if c.errors != 1 || binary.LittleEndian.Uint32(out[2:]) == 0 || f.handles[2] != 0xffffffff || f.transport.stores.contents.used != 0 {
				t.Fatal("Unknown/nonrepresentable ACL became success or leaked")
			}
		})
	}
}
func TestPermissionsTableRevalidationAndFailedCapture(t *testing.T) {
	for _, mode := range []string{"acl_change", "expiry", "owner", "scope", "folder", "cancel", "directory_revoke", "directory_failure", "read_error", "capture_change", "capture_owner", "budget", "member_quota", "live_limit", "allocation_limit"} {
		t.Run(mode, func(t *testing.T) {
			f, b, d := permissionsFixture(t)
			user := core.Identity{Scope: core.Scope{Backend: f.session.Identity().Scope.Backend, Account: "stable-user"}, Name: "stable@example.invalid"}
			d.entries = []core.DirectoryEntry{{Identity: user, DisplayName: "Visible"}}
			b.access.Grants = []core.AccessGrant{{Kind: core.AccessUser, Subject: user.Scope.Account, Rights: core.AccessReadItems | core.AccessVisible}}
			o, _ := f.state.mailbox.object(f.handles[1], 4)
			store := o.binding.store
			switch mode {
			case "capture_change":
				b.hook = func(n int) {
					if n == 2 {
						b.access.Grants = nil
					}
				}
			case "capture_owner":
				b.hook = func(int) { f.box.state.Owner = false }
			case "budget":
				l, e := store.reserveContentsMemory(maxStoreContentsMemory)
				if e != nil {
					t.Fatal(e)
				}
				defer l.release()
			case "member_quota":
				store.permissionMembers = map[core.Scope]uint64{}
				for i := 0; i < maxPermissionMembers; i++ {
					store.permissionMembers[core.Scope{Backend: user.Scope.Backend, Account: fmt.Sprint(i)}] = uint64(i + 1)
				}
			case "live_limit":
				for len(f.state.mailbox.objects) < maxLiveStoreHandles {
					h, _ := f.transport.stores.handle()
					f.state.mailbox.objects[h] = o
				}
			case "allocation_limit":
				f.state.mailbox.allocated = maxContextHandleAllocations
			}
			if mode == "capture_change" || mode == "capture_owner" || mode == "budget" || mode == "member_quota" || mode == "live_limit" || mode == "allocation_limit" {
				used := store.allocator.contents.used
				out, c := f.run([]byte{0x3e, 4, 1, 2, 0})
				if c.errors != 1 || binary.LittleEndian.Uint32(out[2:]) == 0 || f.handles[2] != 0xffffffff || store.allocator.contents.used != used {
					t.Fatal("Failed capture published handle/leaked")
				}
				if len(store.permissionMembers) != 0 && mode != "member_quota" {
					t.Fatal("Failed capture allocated stable members")
				}
				return
			}
			if mode == "expiry" {
				b.access.Grants[0].ExpiresAt = time.Now().Add(time.Hour)
			}
			table, _ := permissionsOpen(t, f, 0)
			f.run(tableColumnsWire(2, permissionTags...))
			before := store.allocator.contents.used
			switch mode {
			case "acl_change":
				b.access.Grants[0].Rights = 0
			case "expiry":
				b.access.Grants[0].ExpiresAt = time.Now().Add(-time.Second)
			case "owner":
				f.box.state.Owner = false
			case "scope":
				id := f.session.Identity()
				id.Scope.Account = "foreign"
				f.session = scopedSession{f.session, id}
			case "folder":
				delete(store.byFID, f.fids[Inbox])
			case "directory_revoke":
				d.entries = nil
			case "directory_failure":
				d.err = core.ErrAccessDenied
			case "read_error":
				b.err = core.ErrConflict
			}
			ctx := context.Background()
			if mode == "cancel" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			wire, _ := (ROPBuffer{Operations: tableQueryWire(2, 0, 1, 1), Handles: f.handles}).Marshal()
			body, c := f.transport.executeStore(ctx, f.session, f.state, f.identity, Execute{ROP: wire, MaxOutput: 65535})
			out := storeResponse(t, body).Operations
			if c.errors != 1 || len(out) != 6 || binary.LittleEndian.Uint32(out[2:]) == 0 || table.cursor != 0 || store.allocator.contents.used != before {
				t.Fatal("Stale/unauthorized data exposed or cursor/ledger changed")
			}
		})
	}
}
func TestPermissionsTableServiceCalendarAndSameBatch(t *testing.T) {
	f, b, _ := permissionsFixture(t)
	f.open(int(Outbox))
	table, store := permissionsOpen(t, f, 0)
	if len(permissionsRows(t, f)) != 2 || b.calls != 0 {
		t.Fatal("Service policy claims native ACL")
	}
	f.run([]byte{1, 4, 2})
	if table.lease != nil || store.allocator.contents.used != 0 {
		t.Fatal("Service table leaked")
	}
	f.open(int(Inbox))
	batch := append([]byte{0x3e, 4, 1, 2, 0}, tableColumnsWire(2, permissionTags...)...)
	batch = append(batch, tableQueryWire(2, 0, 1, 3)...)
	out, c := f.run(batch)
	if c.errors != 0 || len(decodeTableRows(t, out[13:], permissionTags)) != 2 {
		t.Fatal("Same batch open/set/query")
	}
	f.run([]byte{1, 4, 2})
	cal, native := newCalendarFixture(t)
	cal.handles = append(cal.handles, 0xffffffff, 0xffffffff)
	cal.transport.EnableContentsTableSpike()
	acl := &permissionsMailbox{Mailbox: native, state: cal.box, access: core.FolderAccess{Scope: cal.session.Identity().Scope, Source: native.Root(), CanViewList: true, Completeness: core.AccessComplete, Provenance: core.AccessInherited}}
	cal.session = &testSession{box: acl}
	entry := calendarEntry(t, cal)
	resolved, c := cal.run(conversionOperation(0x44, entry[22:]))
	if c.errors != 0 {
		t.Fatal("Calendar resolve")
	}
	fid := binary.LittleEndian.Uint64(resolved[6:])
	cal.run(folderOperation(fid, 4, 0, 1))
	table, store = permissionsOpen(t, cal, 2)
	if len(permissionsRows(t, cal)) != 2 || native.specialCalls == 0 || len(table.rows) != 2 {
		t.Fatal("Calendar IncludeFreeBusy empty complete ACL")
	}
	cal.state.mailbox.close()
	if store.allocator.contents.used != 0 {
		t.Fatal("Calendar table leak")
	}
}
func TestPermissionsRightsAndExpiryWithoutSourceTokenChange(t *testing.T) {
	// Exact per-bit mapping, including unsupported rights rather than loss.
	for bit := 0; bit < 16; bit++ {
		r := core.AccessRights(1 << bit)
		wire, err := permissionRights(r)
		if bit < 8 && (err != nil || wire == 0) || bit >= 8 && !errors.Is(err, core.ErrUnsupported) {
			t.Fatal("Rights bit mapping", bit)
		}
	}
	f, b, _ := permissionsFixture(t)
	o, _ := f.state.mailbox.object(f.handles[1], 4)
	store := o.binding.store
	deadline := time.Date(2026, 11, 17, 12, 0, 0, 1000000, time.UTC)
	b.access.Grants = []core.AccessGrant{{Kind: core.AccessPublic, Rights: core.AccessReadItems | core.AccessVisible, ExpiresAt: deadline}}
	rows, first, e := f.transport.permissionRowsAt(context.Background(), f.session, store, f.fids[Inbox], 0, deadline.Add(-time.Nanosecond))
	if e != nil || rows[0].Rights != 0x401 || rows[1].Rights != 0x401 {
		t.Fatal("Grant expired before exclusive boundary")
	}
	rows, second, e := f.transport.permissionRowsAt(context.Background(), f.session, store, f.fids[Inbox], 0, deadline)
	if e != nil || first == second || rows[0].Rights != 0 || rows[1].Rights != 0 {
		t.Fatal("Expired public grant persisted with unchanged source")
	}
}

func TestPermissionsTableDiagnosticAndWrongObjectGuards(t *testing.T) {
	f, _, _ := permissionsFixture(t)
	f.transport.EnableSynchronizationOptionsTrace()
	out, c := f.run([]byte{0x3e, 4, 1, 2, 0})
	stage, ops, first, _, _ := c.execution.Fields()
	if !bytes.Equal(out, ropError(0x3e, 2, 0)) || stage != "executed" || !reflect.DeepEqual(ops, []string{"get_permissions_table"}) || first != "none" {
		t.Fatal("Permissions execution evidence missing")
	}
	for _, wire := range [][]byte{{0x3e, 4, 2, 3, 0}, {0x3e, 4, 0, 3, 0}, {0x16, 5, 2}} {
		out, c = f.run(wire)
		if c.errors != 1 || binary.LittleEndian.Uint32(out[2:]) == 0 {
			t.Fatal("Wrong object/logon accepted")
		}
	}
	f.run([]byte{1, 4, 2})
	out, c = f.run(tableQueryWire(2, 0, 1, 1))
	if c.errors != 1 || !bytes.Equal(out, ropError(0x15, 2, invalidObject)) {
		t.Fatal("Released handle remained usable")
	}
}
