// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"

	"ewsgo/internal/core"
)

const (
	defaultPerUserChunk = 4096
	maxPerUserChunk     = 8192
)

type perUserRequest struct {
	offset   uint32
	finished byte
	maximum  uint16
	data     []byte
	replica  [16]byte
	limit    int
}

func readPerUserRequest(r *reader, op *Operation, public bool) {
	copy(op.LongTermID[:], r.take(24))
	header := r.take(7) // Reserved/HasFinished, offset, max output/data size.
	if header == nil {
		return
	}
	request := perUserRequest{offset: binary.LittleEndian.Uint32(header[1:5])}
	count := binary.LittleEndian.Uint16(header[5:])
	if op.ID == ropWritePerUserInformation {
		request.finished = header[0]
		request.data = r.take(int(count))
		if request.offset == 0 && !public {
			copy(request.replica[:], r.take(16))
		}
	} else {
		request.maximum = count
	}
	op.PerUser = request
}

func (r perUserRequest) desiredChunk() int {
	if r.maximum == 0 {
		return defaultPerUserChunk
	}
	return min(int(r.maximum), maxPerUserChunk)
}

type perUserTransferKey struct {
	logon   byte
	private bool
	folder  [24]byte
}

type perUserTransfer struct {
	key   perUserTransferKey
	state core.PublicReadState
	next  int
	lease *contentsLease
}

type perUserTransfers struct {
	read, write *perUserTransfer
	lastRead    perUserTransferKey
	lastVersion uint64
}

func (p *perUserTransfer) close() {
	if p != nil {
		p.lease.release()
	}
}

func (p *perUserTransfers) releaseLogon(logon byte) {
	if p.read != nil && p.read.key.logon == logon {
		p.read.close()
		p.read = nil
	}
	if p.write != nil && p.write.key.logon == logon {
		p.write.close()
		p.write = nil
	}
	if p.lastRead.logon == logon {
		p.lastRead = perUserTransferKey{}
		p.lastVersion = 0
	}
}

func (p *publicStore) perUserOperation(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32, private bool) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	object, code := objects.rawLookup(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if !object.isLogon || private != (object.public == nil) {
		return fail(NotSupported)
	}
	if op.ID == ropGetPerUserGUID && !private {
		return fail(NotSupported)
	}
	if op.ID == ropGetPerUserLongTermIDs {
		return p.listPrivateReadFolders(ctx, session, op)
	}
	fid, err := p.publicFID(op.LongTermID)
	if err != nil {
		return fail(storeError(err))
	}
	if err := p.authorize(ctx, session, fid); err != nil {
		return fail(storeError(err))
	}
	user, err := p.bindUser(ctx, session.Identity().Scope)
	if err != nil {
		return fail(storeError(err))
	}
	key := perUserTransferKey{logon: op.Logon, private: private, folder: p.longTermID(fid)}
	switch op.ID {
	case ropGetPerUserGUID:
		state, err := p.states.PublicReadState(ctx, user, p.storeKey, key.folder, true)
		if err != nil {
			return fail(storeError(err))
		}
		if state.Revision == 0 {
			return fail(PropertyNotFound)
		}
		return append(fail(0), state.Replica[:]...)
	case ropReadPerUserInformation:
		return p.readPerUser(ctx, user, key, op, &objects.perUser)
	default:
		return p.writePerUser(ctx, user, key, op, &objects.perUser)
	}
}

func (p *publicStore) listPrivateReadFolders(ctx context.Context, session core.Session, op Operation) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	if op.PerUser.replica != p.replica {
		return append(fail(0), 0, 0)
	}
	user, err := p.bindUser(ctx, session.Identity().Scope)
	if err != nil {
		return fail(storeError(err))
	}
	capacity := op.PerUser.limit / 24
	folders, err := p.states.PrivatePublicReadFolders(ctx, user, p.storeKey, capacity+1)
	if err != nil {
		return fail(storeError(err))
	}
	if len(folders) > capacity {
		return fail(tableBufferTooSmall)
	}
	visible := make([][24]byte, 0, len(folders))
	for _, folder := range folders {
		fid, err := p.publicFID(folder)
		if err != nil {
			return fail(storeError(err))
		}
		if err := p.authorize(ctx, session, fid); err != nil {
			if errors.Is(err, core.ErrAccessDenied) || errors.Is(err, core.ErrNotFound) {
				continue
			}
			return fail(storeError(err))
		}
		visible = append(visible, folder)
	}
	response := binary.LittleEndian.AppendUint16(fail(0), uint16(len(visible)))
	for _, folder := range visible {
		response = append(response, folder[:]...)
	}
	return response
}

func (p *publicStore) readPerUser(ctx context.Context, user string, key perUserTransferKey, op Operation, transfers *perUserTransfers) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	request := op.PerUser
	if request.offset == 0 {
		transfers.read.close()
		transfers.read = nil
		state, err := p.states.PublicReadState(ctx, user, p.storeKey, key.folder, key.private)
		if err != nil {
			return fail(storeError(err))
		}
		if len(state.CNSet) == 0 || transfers.lastRead == key && state.Revision == transfers.lastVersion {
			return append(fail(0), 1, 0, 0)
		}
		if request.limit == 0 {
			return fail(tableBufferTooSmall)
		}
		lease, err := p.memory.reserveContentsMemory(len(state.CNSet))
		if err != nil {
			return fail(storeError(err))
		}
		transfers.read = &perUserTransfer{key: key, state: state, lease: lease}
	}
	current := transfers.read
	if current == nil || current.key != key || uint32(current.next) != request.offset {
		return fail(0x80004005)
	}
	if request.limit == 0 {
		return fail(tableBufferTooSmall)
	}
	end := min(current.next+request.limit, len(current.state.CNSet))
	finished := end == len(current.state.CNSet)
	header := append(fail(0), 0)
	if finished {
		header[6] = 1
	}
	response := binary.LittleEndian.AppendUint16(header, uint16(end-current.next))
	response = append(response, current.state.CNSet[current.next:end]...)
	current.next = end
	if finished {
		transfers.lastRead, transfers.lastVersion = key, current.state.Revision
		current.close()
		transfers.read = nil
	}
	return response
}

func (p *publicStore) writePerUser(ctx context.Context, user string, key perUserTransferKey, op Operation, transfers *perUserTransfers) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	request := op.PerUser
	if request.finished > 1 || len(request.data) == 0 && request.finished == 0 {
		return fail(0x80070057)
	}
	if request.offset == 0 {
		transfers.write.close()
		transfers.write = nil
		if key.private && request.replica != p.replica {
			return fail(0x80070057)
		}
		before, err := p.states.PublicReadState(ctx, user, p.storeKey, key.folder, key.private)
		if err != nil {
			return fail(storeError(err))
		}
		lease, err := p.memory.reserveContentsMemory(maxIDSetBytes)
		if err != nil {
			return fail(storeError(err))
		}
		transfers.write = &perUserTransfer{key: key, lease: lease, state: core.PublicReadState{
			Replica: p.replica, Revision: before.Revision, CNSet: make([]byte, 0, maxIDSetBytes),
		}}
	}
	upload := transfers.write
	if upload == nil || upload.key != key || uint32(len(upload.state.CNSet)) != request.offset {
		upload.close()
		transfers.write = nil
		return fail(0x80004005)
	}
	if len(request.data) > maxIDSetBytes-len(upload.state.CNSet) {
		upload.close()
		transfers.write = nil
		return fail(0x8004011b)
	}
	upload.state.CNSet = append(upload.state.CNSet, request.data...)
	if request.finished == 0 {
		return fail(0)
	}
	defer upload.close()
	transfers.write = nil
	set, err := parseIDSet(upload.state.CNSet)
	if err != nil {
		return fail(0x8004011b)
	}
	for _, replica := range set.replicas {
		if !bytes.Equal(replica.replica[:], p.replica[:]) {
			return fail(0x8004011b)
		}
	}
	err = p.states.SavePublicReadState(ctx, user, p.storeKey, key.folder, key.private, upload.state)
	if errors.Is(err, core.ErrConflict) {
		return fail(0x80040109)
	}
	return fail(storeErrorOrSuccess(err))
}
