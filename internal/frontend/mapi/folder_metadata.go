// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"

	"ewsgo/internal/core"
)

const maxMetadataBytes = 32 << 10
const maxStoreMetadataBytes = 256 << 10
const metadataBagOverhead = 64
const maxAdditionalEntryIDs = 32
const additionalEntryIDs = 0x36d8
const junkEntryIDIndex = 4
const additionalEntryIDsEx = 0x36d9
const remindersOnlineEntryID = 0x36d5
const folderHiddenTag = 0x10f4000b

// Closed property bag. 0x36de is deliberately unnamed and preserved as four
// opaque bytes.
type folderMetadata map[uint16][]byte

func (folderMetadata) String() string   { return "folderMetadata{redacted}" }
func (folderMetadata) GoString() string { return "folderMetadata{redacted}" }

func cloneMetadata(src folderMetadata) folderMetadata {
	out := make(folderMetadata, len(src))
	for id, v := range src {
		out[id] = bytes.Clone(v)
	}
	return out
}

func (m folderMetadata) accountedBytes() int {
	if len(m) == 0 {
		return 0
	}
	size := metadataBagOverhead
	for _, value := range m {
		size += 8 + len(value)
	}
	return size
}

func metadataID(id uint16) bool {
	return id == folderHiddenTag>>16 || id == 0x3613 || id == additionalEntryIDs || inboxMetadataID(id) || id == 0x36da || id == 0x36de
}

func inboxMetadataID(id uint16) bool {
	return id == additionalEntryIDsEx || id == remindersOnlineEntryID
}

// Canonical storage: ASCII class without terminator, raw binary/integer,
// complete ROP MV_BINARY representation for AdditionalRenEntryIds.
// This spike intentionally rejects non-ASCII classes instead of guessing a CP.
func parseFolderMetadata(r *reader) (folderMetadata, error) {
	h := r.take(4)
	if r.err != nil {
		return nil, ErrMalformed
	}
	size, count := int(binary.LittleEndian.Uint16(h)), int(binary.LittleEndian.Uint16(h[2:]))
	if size < 2 {
		return nil, ErrMalformed
	}
	data := r.take(size - 2)
	if r.err != nil {
		return nil, ErrMalformed
	}
	if count < 1 || count > 7 {
		return nil, unsupportedOperation{label: "RopSetProperties"}
	}
	list := reader{data: data}
	out := make(folderMetadata, count)
	for range count {
		tag := list.u32()
		if list.err != nil {
			return nil, ErrMalformed
		}
		id, kind := uint16(tag>>16), uint16(tag)
		if !metadataID(id) {
			return nil, unsupportedOperation{label: "RopSetProperties"}
		}
		if _, exists := out[id]; exists {
			return nil, ErrMalformed
		}
		var value []byte
		switch {
		case tag == folderHiddenTag:
			value = list.take(1)
			if list.err == nil && value[0] != 0 {
				value = []byte{1}
			}
		case id == 0x3613 && (kind == 0x1e || kind == 0x1f):
			for {
				b := list.take(1)
				if list.err != nil {
					return nil, ErrMalformed
				}
				if kind == 0x1f {
					hi := list.take(1)
					if list.err != nil {
						return nil, ErrMalformed
					}
					if hi[0] != 0 {
						return nil, unsupportedOperation{label: "RopSetProperties"}
					}
				}
				if b[0] == 0 {
					break
				}
				if b[0] < 0x20 || b[0] > 0x7e || len(value) >= 255 {
					return nil, unsupportedOperation{label: "RopSetProperties"}
				}
				value = append(value, b[0])
			}
		case (id == 0x36da || inboxMetadataID(id)) && kind == 0x102:
			h := list.take(2)
			if list.err != nil {
				return nil, ErrMalformed
			}
			value = list.take(int(binary.LittleEndian.Uint16(h)))
		case id == 0x36de && kind == 3:
			value = list.take(4)
		case id == additionalEntryIDs && kind == 0x1102:
			start := list.pos
			if err := skipAdditionalEntryIDs(&list); err != nil {
				return nil, err
			}
			// Preserve the complete ROP representation, including empty slots
			// and indexes outside the documented five. Never resolve EntryIDs.
			value = list.data[start:list.pos]
		default:
			return nil, unsupportedOperation{label: "RopSetProperties"}
		}
		if list.err != nil {
			return nil, ErrMalformed
		}
		out[id] = bytes.Clone(value)
	}
	if list.done() != nil {
		return nil, ErrMalformed
	}
	return out, nil
}

// ROP MV_BINARY: uint32 count, then uint16 byte length and bytes per item.
// No item allocations or value inspection; count is bounded before iteration.
func skipAdditionalEntryIDs(r *reader) error {
	start := r.pos
	count := r.u32()
	if r.err != nil {
		return ErrMalformed
	}
	if count > maxAdditionalEntryIDs {
		return unsupportedOperation{label: "RopSetProperties"}
	}
	for range count {
		h := r.take(2)
		if r.err != nil {
			return ErrMalformed
		}
		r.take(int(binary.LittleEndian.Uint16(h)))
		if r.err != nil {
			return ErrMalformed
		}
		if r.pos-start > maxMetadataBytes {
			return unsupportedOperation{label: "RopSetProperties"}
		}
	}
	return nil
}

func (t *SessionTransport) EnableFolderMetadataSpike() { t.folderMetadataSpike = true }

// Snapshot before preflight prevents concurrent growth from exceeding reserved
// response space. A successful write updates only this request's snapshot.
type metadataBatchView struct {
	scope   core.Scope
	values  map[uint64]folderMetadata
	maximum map[uint16]int
}

func (t *SessionTransport) metadataView(scope core.Scope, ops []Operation) *metadataBatchView {
	if !t.folderMetadataSpike {
		return nil
	}
	v := &metadataBatchView{scope: scope, values: make(map[uint64]folderMetadata), maximum: make(map[uint16]int)}
	t.stores.mu.Lock()
	s := t.stores.entries[scope]
	t.stores.mu.Unlock()
	if s != nil {
		s.mu.Lock()
		for fid, data := range s.metadata {
			v.values[fid] = cloneMetadata(data)
		}
		s.mu.Unlock()
	}
	for _, data := range v.values {
		v.include(data)
	}
	for _, op := range ops {
		v.include(op.Metadata)
		if op.SpecialFolderReference != nil {
			for _, value := range op.SpecialFolderReference.values {
				if value.tag == remindersReferenceTag {
					v.maximum[remindersOnlineEntryID] = max(v.maximum[remindersOnlineEntryID], len(value.entry))
				}
			}
		}
	}
	return v
}

func (v *metadataBatchView) include(data folderMetadata) {
	for id, value := range data {
		if len(value) <= maxMetadataBytes {
			v.maximum[id] = max(v.maximum[id], len(value))
		}
	}
}

func (v *metadataBatchView) bound(tag uint32) int {
	if v == nil || !metadataID(uint16(tag>>16)) {
		return 0
	}
	id := uint16(tag >> 16)
	switch id {
	case 0x3613:
		return max(4, (v.maximum[id]+1)*2)
	case 0x36da, additionalEntryIDsEx, remindersOnlineEntryID:
		return max(4, v.maximum[id]+2)
	case additionalEntryIDs:
		// The server can fill a missing Junk slot and its preceding slots.
		return max(4, v.maximum[id]) + (junkEntryIDIndex+1)*2 + folderEntryIDBytes
	default:
		return 4
	}
}

func (v *metadataBatchView) property(fid uint64, tag uint32, unicode bool, cp uint32) folderPropertyValue {
	fail := func(code uint32) folderPropertyValue { return folderPropertyValue{kind: 0xa, code: code} }
	id, kind := uint16(tag>>16), uint16(tag)
	value, exists := v.values[fid][id]
	if id == 0x3613 {
		if kind == 0 {
			kind = 0x1e
			if unicode {
				kind = 0x1f
			}
		}
		if kind != 0x1e && kind != 0x1f {
			return fail(NotSupported)
		}
		if !exists {
			return fail(PropertyNotFound)
		}
		if kind == 0x1e && cp != 1252 && cp != 65001 {
			return fail(0x8004011e)
		}
		out, ok := propertyString(string(value), kind, cp)
		if !ok {
			return fail(propertyBadValue)
		}
		return folderPropertyValue{kind: kind, data: out}
	}
	actual := metadataPropertyType(id)
	if kind != 0 && kind != actual {
		return fail(NotSupported)
	}
	if !exists {
		return fail(PropertyNotFound)
	}
	data := bytes.Clone(value)
	if actual == 0x102 {
		data = append(binary.LittleEndian.AppendUint16(nil, uint16(len(value))), value...)
	}
	return folderPropertyValue{kind: actual, data: data}
}

func metadataPropertyType(id uint16) uint16 {
	switch id {
	case folderHiddenTag >> 16:
		return 0xb
	case 0x36da, additionalEntryIDsEx, remindersOnlineEntryID:
		return 0x102
	case additionalEntryIDs:
		return 0x1102
	default:
		return 3
	}
}

func additionalFolderReferences(ctx context.Context, session core.Session, object serverObject, stored folderPropertyValue) folderPropertyValue {
	if stored.code != 0 && stored.code != PropertyNotFound {
		return stored
	}
	binding := object.binding
	if object.fid != binding.fids[Inbox] && object.fid != binding.fids[MailboxRoot] {
		return stored
	}
	entry, err := nativeJunkEntryID(ctx, session, binding)
	if err != nil {
		return folderPropertyValue{kind: 0xa, code: storeError(err)}
	}
	if entry == nil {
		return stored
	}
	data, err := replaceJunkReference(stored.data, entry)
	if err != nil {
		return folderPropertyValue{kind: 0xa, code: propertyBadValue}
	}
	return folderPropertyValue{kind: 0x1102, data: data}
}

func nativeJunkEntryID(ctx context.Context, session core.Session, binding storeBinding) ([]byte, error) {
	store := binding.store
	objects, err := store.metadataFolderObjects(ctx, session)
	if err != nil {
		return nil, err
	}
	junk, err := nativeJunkFolder(objects, store.root)
	if err != nil || junk == "" {
		return nil, err
	}
	ids, err := store.assign(ctx, []folderKey{{native: junk}})
	if err != nil {
		return nil, err
	}
	return marshalFolderEntryID(binding.mailboxGUID, store.replica, ids[0]), nil
}

func nativeJunkFolder(objects []core.Object, root core.ID) (core.ID, error) {
	var junk core.ID
	for _, object := range objects {
		if object.Properties[core.WellKnown] != core.Text("junkemail") {
			continue
		}
		if junk != "" || object.ID == root || object.ID == "" || object.Parent == "" || object.Parent == object.ID {
			return "", core.ErrConflict
		}
		junk = object.ID
	}
	return junk, nil
}

// Only index four is authoritative. Retain every other stored value, including
// client-defined slots beyond the documented five.
func replaceJunkReference(stored, entry []byte) ([]byte, error) {
	values, err := additionalEntryIDSlotsWithJunk(stored, entry)
	if err != nil {
		return nil, err
	}
	out := append32(nil, uint32(len(values)))
	for _, value := range values {
		out = binary.LittleEndian.AppendUint16(out, uint16(len(value)))
		out = append(out, value...)
	}
	return out, nil
}

func additionalEntryIDSlotsWithJunk(stored, entry []byte) ([][]byte, error) {
	values, err := additionalEntryIDSlots(stored)
	if err != nil {
		return nil, err
	}
	for len(values) <= junkEntryIDIndex {
		values = append(values, nil)
	}
	values[junkEntryIDIndex] = entry
	return values, nil
}

func additionalEntryIDSlots(stored []byte) ([][]byte, error) {
	if len(stored) == 0 {
		return nil, nil
	}
	r := reader{data: stored}
	if err := skipAdditionalEntryIDs(&r); err != nil || r.done() != nil {
		return nil, ErrMalformed
	}
	r.pos = 0
	count := r.u32()
	values := make([][]byte, 0, count)
	for range count {
		length := r.take(2)
		values = append(values, r.take(int(binary.LittleEndian.Uint16(length))))
	}
	return values, nil
}

// MS-OXOSFLD identifies these optional folders by their slots in the Inbox
// property. Only live virtual folders with the expected parent qualify:
// a reference written by a client cannot disable sync on a native folder.
func (b storeBinding) isSyncIssuesFolder(fid uint64) bool {
	s := b.store
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.byFID[fid]
	folder, ok := s.dynamic[key.dynamic]
	if !ok || folder.softDeleted || folder.search {
		return false
	}
	slots, err := additionalEntryIDSlots(s.metadata[b.fids[Inbox]][additionalEntryIDs])
	const syncIssues = 1
	if err != nil || len(slots) <= syncIssues {
		return false
	}
	rootFID, err := parseFolderEntryID(slots[syncIssues], b.mailboxGUID, s.replica)
	if err != nil {
		return false
	}
	rootKey := s.byFID[rootFID]
	root, ok := s.dynamic[rootKey.dynamic]
	if !ok || root.softDeleted || root.search || root.parent != (folderKey{native: s.root}) {
		return false
	}
	if fid == rootFID {
		return true
	}
	if folder.parent != rootKey {
		return false
	}
	// Conflicts, Local Failures and Server Failures. Junk is the next slot.
	for _, i := range [...]int{0, 2, 3} {
		if i < len(slots) {
			child, err := parseFolderEntryID(slots[i], b.mailboxGUID, s.replica)
			if err == nil && child == fid {
				return true
			}
		}
	}
	return false
}

// Called with the store mutex held. Published metadata bags are immutable.
func (s *ramStore) unlinkRemindersFolder(fid uint64) {
	inbox := s.byKey[folderKey{native: s.inbox}]
	old := s.metadata[inbox]
	entry := old[remindersOnlineEntryID]
	if len(entry) != folderEntryIDBytes || fidFromGlobalCounter(entry[38:44]) != fid {
		return
	}
	next := cloneMetadata(old)
	delete(next, remindersOnlineEntryID)
	s.metadataBytes += next.accountedBytes() - old.accountedBytes()
	if len(next) == 0 {
		delete(s.metadata, inbox)
	} else {
		s.metadata[inbox] = next
	}
}

func (s *ramStore) setMetadata(ctx context.Context, session core.Session, fid uint64, update folderMetadata) (map[uint64]folderMetadata, uint32) {
	return s.updateMetadata(ctx, session, fid, update, false)
}

// Serializes with hierarchy observation. All supplied properties are committed
// atomically or none on quota/cancellation failure; native data is untouched.
func (s *ramStore) updateMetadata(ctx context.Context, session core.Session, fid uint64, update folderMetadata, remove bool) (map[uint64]folderMetadata, uint32) {
	s.hierarchyMu.Lock()
	defer s.hierarchyMu.Unlock()
	if _, err := s.folder(ctx, session, fid); err != nil {
		return nil, storeError(err)
	}
	if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
		return nil, storeError(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, storeError(err)
	}
	updates := make(map[uint64]folderMetadata, 2)
	for id, data := range update {
		target := fid
		if inboxMetadataID(id) && s.byFID[fid] == (folderKey{role: MailboxRoot}) {
			target = s.byKey[folderKey{native: s.inbox}]
			if target == 0 {
				return nil, PropertyNotFound
			}
		}
		if !remove && id == remindersOnlineEntryID {
			if target != s.byKey[folderKey{native: s.inbox}] || len(data) != folderEntryIDBytes {
				return nil, 0x80070057
			}
			reference := fidFromGlobalCounter(data[38:44])
			search := s.dynamic[s.byFID[reference].dynamic]
			if !search.search || search.softDeleted {
				return nil, PropertyNotFound
			}
		}
		if updates[target] == nil {
			updates[target] = cloneMetadata(s.metadata[target])
		}
		if remove {
			delete(updates[target], id)
		} else {
			updates[target][id] = bytes.Clone(data)
		}
	}
	used := s.metadataBytes
	for target, next := range updates {
		size := next.accountedBytes()
		if size-metadataBagOverhead > maxMetadataBytes {
			return nil, 0x8007000e
		}
		used += size - s.metadata[target].accountedBytes()
	}
	if used > maxStoreMetadataBytes {
		return nil, 0x8007000e
	}
	if err := ctx.Err(); err != nil {
		return nil, storeError(err)
	}
	if err := s.persistMetadataLocked(ctx, updates); err != nil {
		return nil, storeError(err)
	}
	if s.metadata == nil {
		s.metadata = make(map[uint64]folderMetadata)
	}
	for target, next := range updates {
		if len(next) == 0 {
			delete(s.metadata, target)
		} else {
			s.metadata[target] = next
		}
	}
	s.metadataBytes = used
	return updates, 0
}

func setFolderMetadata(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32, view *metadataBatchView) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	if view == nil || view.scope != session.Identity().Scope {
		return fail(NotSupported)
	}
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if object.isLogon || object.download != nil {
		return fail(NotSupported)
	}
	if code := writeFolderMetadata(ctx, session, object, op.Metadata, view); code != 0 {
		return fail(code)
	}
	return binary.LittleEndian.AppendUint16(fail(0), 0)
}

func writeFolderMetadata(ctx context.Context, session core.Session, object serverObject, update folderMetadata, view *metadataBatchView) uint32 {
	if entryID, ok := update[remindersOnlineEntryID]; ok {
		if _, err := parseFolderEntryID(entryID, object.binding.mailboxGUID, object.binding.store.replica); err != nil {
			return storeError(err)
		}
	}
	updates, code := object.binding.store.setMetadata(ctx, session, object.fid, update)
	if code != 0 {
		return code
	}
	for fid, data := range updates {
		// Each committed bag is immutable; subsequent writes replace it.
		view.values[fid] = data
	}
	return 0
}

func metadataFingerprint(data folderMetadata, native [32]byte) [32]byte {
	h := sha256.New()
	h.Write(native[:])
	for _, id := range []uint16{folderHiddenTag >> 16, 0x3613, remindersOnlineEntryID, 0x36d8, additionalEntryIDsEx, 0x36da, 0x36de} {
		v, ok := data[id]
		if !ok {
			continue
		}
		h.Write(binary.LittleEndian.AppendUint16(nil, id))
		h.Write(binary.LittleEndian.AppendUint32(nil, uint32(len(v))))
		h.Write(v)
	}
	var out [32]byte
	copy(out[:], h.Sum(nil))
	return out
}
