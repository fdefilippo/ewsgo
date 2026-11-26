// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"strconv"
	"strings"

	"ewsgo/internal/core"
)

const (
	publicRootCounter = 1
	publicNativeBase  = 1 << 32
	publicFolderDepth = 64
)

type publicStore struct {
	owner    core.Scope
	root     core.ID
	rootFID  uint64
	storeKey string
	dn       string
	replica  [16]byte
	memory   *ramStore
	states   core.PublicReadStateStore
	bindUser func(context.Context, core.Scope) (string, error)
}

func (t *SessionTransport) EnablePublicZimbraStore(qualifiedRoot string, states core.PublicReadStateStore, bindUser func(context.Context, core.Scope) (string, error)) error {
	owner, folder, ok := strings.Cut(qualifiedRoot, ":")
	guid, err := NewNamespace(owner)
	anchor, valid := publicNativeCounter(core.ID("f:" + folder))
	if t.stores == nil || states == nil || bindUser == nil || !ok || err != nil || !valid || anchor <= 15 {
		return core.ErrInvalid
	}
	publicScope := core.Scope{Backend: "zimbra", Account: guid.id}
	identity, err := t.namespace.Mailbox(core.Identity{
		Scope: core.Scope{Backend: "ewsgo-public-zimbra/v1", Account: guid.id + ":" + folder},
		Name:  "public-folders@invalid.test",
	})
	if err != nil {
		return err
	}
	replica, err := mailboxGUIDWire(identity.GUID)
	if err != nil {
		return err
	}
	t.publicStore = &publicStore{
		owner: publicScope, root: core.ID("f:" + folder), rootFID: folderID(publicNativeBase + anchor),
		storeKey: identity.GUID, dn: "/o=EWSGo/ou=" + t.namespace.id + "/cn=PublicStores/cn=" + identity.GUID,
		replica: replica, states: states, bindUser: bindUser, memory: &ramStore{allocator: t.stores},
	}
	return nil
}

func publicNativeCounter(id core.ID) (uint64, bool) {
	raw, ok := strings.CutPrefix(string(id), "f:")
	n, err := strconv.ParseUint(raw, 10, 32)
	return n, ok && err == nil && n > 0 && strconv.FormatUint(n, 10) == raw
}

func publicFIDCounter(fid uint64) uint64 {
	if uint16(fid) != 1 {
		return 0
	}
	wire := binary.LittleEndian.AppendUint64(nil, fid)
	return binary.BigEndian.Uint64(append([]byte{0, 0}, wire[2:]...))
}

func (p *publicStore) authorize(ctx context.Context, session core.Session, fid uint64) error {
	if session == nil || session.Mailbox() == nil || session.Identity().Scope.Backend != p.owner.Backend {
		return core.ErrAccessDenied
	}
	shared, ok := session.Mailbox().(core.SharedFolderMailbox)
	if !ok {
		return core.ErrUnsupported
	}
	counter := publicFIDCounter(fid)
	switch {
	case counter == publicRootCounter || fid == p.rootFID:
		_, err := shared.ReadSharedMailFolder(ctx, p.owner, p.root)
		return err
	case counter <= publicNativeBase || counter >= publicNativeBase+(1<<32):
		return core.ErrNotFound
	}
	current := core.ID("f:" + strconv.FormatUint(counter-publicNativeBase, 10))
	for range publicFolderDepth {
		folder, err := shared.ReadSharedMailFolder(ctx, p.owner, current)
		if err != nil {
			return err
		}
		if folder.ID != current || folder.Parent == current {
			return core.ErrInvalid
		}
		if current == p.root {
			return nil
		}
		parent, ok := publicNativeCounter(folder.Parent)
		if !ok || parent <= 15 {
			return core.ErrNotFound
		}
		current = folder.Parent
	}
	return core.ErrNotFound
}

func (p *publicStore) longTermID(fid uint64) [24]byte {
	var id [24]byte
	copy(id[:], p.replica[:])
	wire := binary.LittleEndian.AppendUint64(nil, fid)
	copy(id[16:22], wire[2:])
	return id
}

func (p *publicStore) publicFID(id [24]byte) (uint64, error) {
	if !bytes.Equal(id[:16], p.replica[:]) {
		return 0, core.ErrNotFound
	}
	var wire [8]byte
	wire[0] = 1
	copy(wire[2:], id[16:22]) // The two input padding bytes have no meaning.
	fid := binary.LittleEndian.Uint64(wire[:])
	if publicFIDCounter(fid) == 0 {
		return 0, core.ErrInvalid
	}
	return fid, nil
}

func (p *publicStore) logon(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Output, code) }
	request := op.LogonRequest
	if request.Flags & ^byte(0x0e) != 0 || request.OpenFlags&2 == 0 ||
		request.OpenFlags & ^uint32(0x2100060e) != 0 {
		return fail(NotSupported)
	}
	if request.ESSDN != "" && !strings.EqualFold(request.ESSDN, p.dn) {
		return fail(accessDenied)
	}
	if err := p.authorize(ctx, session, p.rootFID); err != nil {
		return fail(storeError(err))
	}
	handle, err := objects.addStoreLogon(serverObject{logon: op.Logon, isLogon: true, public: p})
	if err != nil {
		return fail(storeError(err))
	}
	response := append(fail(0), request.Flags&6)
	// The root and interpersonal subtree are exposed. Reserved/deprecated
	// roles that have no counterpart in this public store are zero.
	for _, fid := range [13]uint64{folderID(publicRootCounter), p.rootFID} {
		response = binary.LittleEndian.AppendUint64(response, fid)
	}
	response = binary.LittleEndian.AppendUint16(response, 1)
	response = append(response, p.replica[:]...)
	response = append(response, make([]byte, 16)...)
	handles[op.Output] = handle
	return response
}

func (p *publicStore) folderOperation(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	slot := op.Input
	if op.ID == 2 {
		slot = op.Output
	}
	fail := func(code uint32) []byte { return ropError(op.ID, slot, code) }
	object, code := objects.publicLookup(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if op.ID != 2 && !object.isLogon {
		return fail(NotSupported)
	}
	switch op.ID {
	case 2, ropGetOwningServers, ropPublicFolderIsGhosted, 0x43, 0x44:
	default:
		return fail(NotSupported)
	}
	fid := op.Folder
	if op.ID == 0x44 {
		var err error
		fid, err = p.publicFID(op.LongTermID)
		if err != nil {
			return fail(storeError(err))
		}
	}
	if err := p.authorize(ctx, session, fid); err != nil {
		return fail(storeError(err))
	}
	switch op.ID {
	case 2:
		if op.Flags != 0 || len(objects.objects) >= maxLiveStoreHandles || objects.allocated >= maxContextHandleAllocations {
			return fail(NotSupported)
		}
		handle, err := objects.allocator.handle()
		if err != nil {
			return fail(storeError(err))
		}
		objects.objects[handle] = serverObject{logon: op.Logon, fid: fid, public: p}
		objects.allocated++
		handles[op.Output] = handle
		return append(fail(0), 0, 0) // HasRules and IsGhosted.
	case ropGetOwningServers:
		out := append(fail(0), 1, 0, 1, 0) // One active replica at lowest cost.
		return append(append(out, p.dn...), 0)
	case ropPublicFolderIsGhosted:
		return append(fail(0), 0)
	case 0x43:
		id := p.longTermID(fid)
		return append(fail(0), id[:]...)
	default: // Long-term ID to FID.
		return binary.LittleEndian.AppendUint64(fail(0), fid)
	}
}

func (s *mailboxObjects) publicLookup(handle uint32, logon byte) (serverObject, uint32) {
	object, code := s.rawLookup(handle, logon)
	if code == 0 && object.public == nil {
		code = NotSupported
	}
	return object, code
}
