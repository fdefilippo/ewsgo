// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const (
	ostPropertyTag   = uint32(0x7c040102)
	maxOSTValueBytes = 32 << 10
	maxOSTFolders    = 8
	propertyBadValue = uint32(0x80040301)
)

// EnableOSTPropertySpike permits only the approved RAM experiment. Configure
// before serving, with authenticated TLS and RAM store prerequisites. This does
// not implement the immediate durable Folder/Logon persistence of MS-OXCPRPT.
func (t *SessionTransport) EnableOSTPropertySpike() { t.ostPropertySpike = true }

// A request-local view bounds reads by the data they can actually return,
// rather than by the configured 32 KiB cap. A concurrent writer cannot grow
// this snapshot after preflight. Native authorization still precedes every
// returned value; taking the snapshot conveys no access rights.
type ostBatchView struct {
	scope    core.Scope
	values   map[uint64][]byte
	maxBytes int
}

func (t *SessionTransport) ostView(scope core.Scope, ops []Operation) *ostBatchView {
	if !t.ostPropertySpike {
		return nil
	}
	needed, maximum := false, 0
	for _, op := range ops {
		if op.ID == 0x09 || op.ID == ropGetPropertiesAll {
			needed = true
		}
		if isSetPropertiesROP(op.ID) && op.Metadata == nil && op.SpecialFolderReference == nil && op.FAISet == nil {
			needed = true
			// Oversize writes always fail and must not enlarge the bound.
			if len(op.OSTValue) <= maxOSTValueBytes {
				maximum = max(maximum, len(op.OSTValue))
			}
		}
		if op.Properties != nil {
			for _, tag := range op.Properties.tags {
				needed = needed || uint16(tag>>16) == 0x7c04 && (uint16(tag) == 0 || uint16(tag) == 0x0102)
			}
		}
	}
	if !needed {
		return nil
	}
	view := &ostBatchView{scope: scope, values: make(map[uint64][]byte), maxBytes: maximum}
	t.stores.mu.Lock()
	store := t.stores.entries[scope]
	t.stores.mu.Unlock()
	if store != nil {
		store.mu.Lock()
		defer store.mu.Unlock()
		for fid, value := range store.ost {
			view.values[fid] = bytes.Clone(value)
			view.maxBytes = max(view.maxBytes, len(value))
		}
	}
	return view
}

func (v *ostBatchView) bound() int {
	if v == nil {
		return -1 // OST value reads are disabled for this batch.
	}
	return v.maxBytes
}

func (v *ostBatchView) value(store *ramStore, fid uint64) ([]byte, bool) {
	if v == nil || v.scope != store.scope {
		return nil, false
	}
	value, ok := v.values[fid]
	return value, ok
}

// Parse a complete supported SetProperties value list independently of the
// header-only diagnostic. The outer decoder still validates the whole batch.
func parseOSTValue(r *reader) ([]byte, error) {
	head := r.take(4)
	if r.err != nil {
		return nil, r.err
	}
	size, count := int(binary.LittleEndian.Uint16(head)), binary.LittleEndian.Uint16(head[2:])
	if size < 2 {
		return nil, ErrMalformed
	}
	list := r.take(size - 2) // PropertyValueSize includes PropertyValueCount.
	if r.err != nil {
		return nil, r.err
	}
	if count != 1 {
		return nil, unsupportedOperation{label: "RopSetProperties"}
	}
	if len(list) < 6 {
		return nil, ErrMalformed
	}
	if binary.LittleEndian.Uint32(list) != ostPropertyTag {
		return nil, unsupportedOperation{label: "RopSetProperties"}
	}
	length := int(binary.LittleEndian.Uint16(list[4:]))
	if length != len(list)-6 {
		return nil, ErrMalformed
	}
	// A structurally valid value above the local cap is a PropertyProblem,
	// after object authorization. Input is bounded by the single ROP buffer.
	return bytes.Clone(list[6:]), nil
}

func (s *ramStore) ostValue(fid uint64) ([]byte, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	value, exists := s.ost[fid]
	return bytes.Clone(value), exists
}

func (s *ramStore) setOSTValue(ctx context.Context, fid uint64, value []byte) uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, exists := s.byFID[fid]; !exists {
		return PropertyNotFound
	}
	if len(value) > maxOSTValueBytes {
		return propertyBadValue
	}
	if _, exists := s.ost[fid]; !exists && len(s.ost) >= maxOSTFolders {
		return NotSupported
	}
	if s.durable.store != nil {
		if err := s.advanceReplicaLocked(ctx, mapireplica.Mutation{FolderValues: []mapireplica.FolderValue{{
			Counter: folderGlobalCount(fid), Kind: mapireplica.FolderOST, Data: append([]byte{1}, value...),
		}}}); err != nil {
			return storeError(err)
		}
	}
	if s.ost == nil {
		s.ost = make(map[uint64][]byte)
	}
	s.ost[fid] = bytes.Clone(value)
	return 0
}

func setFolderOSTValue(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32, view *ostBatchView) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	if view == nil {
		return fail(NotSupported)
	}
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if object.isLogon || object.download != nil {
		return fail(NotSupported)
	}
	if _, err := object.binding.store.folder(ctx, session, object.fid); err != nil {
		return fail(storeError(err))
	}
	if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
		return fail(storeError(err))
	}
	if ctx.Err() != nil {
		return fail(0x80004005)
	}
	code = object.binding.store.setOSTValue(ctx, object.fid, op.OSTValue)
	if code == propertyBadValue {
		// MS-OXCDATA PropertyProblem: index(2), tag(4), error(4).
		out := binary.LittleEndian.AppendUint16(fail(0), 1)
		out = binary.LittleEndian.AppendUint16(out, 0)
		return append32(append32(out, ostPropertyTag), code)
	}
	if code != 0 {
		return fail(code)
	}
	// Keep this batch's read-your-write view after a real successful write.
	// Other requests retain their own independent snapshots.
	view.values[object.fid] = bytes.Clone(op.OSTValue)
	return binary.LittleEndian.AppendUint16(fail(0), 0)
}
