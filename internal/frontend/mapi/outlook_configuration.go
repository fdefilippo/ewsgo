// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const (
	freeBusyEntryIDsTag   uint32 = 0x36e41102
	delegatorWantsCopyTag uint32 = 0x6842000b
	delegatorWantsInfoTag uint32 = 0x684b000b
	localFreeBusyClass           = core.Text("IPM.Microsoft.ScheduleData.FreeBusy")
	localFreeBusySubject         = core.Text("LocalFreebusy")
	localFreeBusyBytes           = 4096
)

// EnablePrivateOutlookConfiguration exposes the private configuration folder
// in the Outlook lab. The stored message confers no mailbox or calendar rights.
func (t *SessionTransport) EnablePrivateOutlookConfiguration() {
	t.stores.privateConfig = true
}

func (s *ramStore) isFreeBusyFolder(fid uint64) bool {
	if !s.privateConfig {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byFID[fid] == (folderKey{role: freeBusyFolderRole})
}

func (s *ramStore) authorizeLocalMessage(ctx context.Context, session core.Session, fid uint64, privateConfig bool) error {
	if !privateConfig {
		_, err := s.messageAnchor(ctx, session, fid, true)
		return err
	}
	if session == nil || session.Identity().Scope != s.scope {
		return core.ErrAccessDenied
	}
	box := session.Mailbox()
	if !s.isFreeBusyFolder(fid) || box == nil || box.Root() != s.root {
		return core.ErrNotFound
	}
	if err := hierarchyOwner(ctx, box); err != nil {
		return err
	}
	return ctx.Err()
}

func freeBusyEntryIDs(ctx context.Context, session core.Session, object serverObject) folderPropertyValue {
	binding := object.binding
	s := binding.store
	fail := func(code uint32) folderPropertyValue {
		return folderPropertyValue{kind: 0x000a, code: code}
	}
	if !s.privateConfig || object.fid != binding.fids[MailboxRoot] && object.fid != binding.fids[Inbox] {
		return fail(PropertyNotFound)
	}
	if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
		return fail(storeError(err))
	}
	if err := ctx.Err(); err != nil {
		return fail(storeError(err))
	}
	ids, err := s.assign(ctx, []folderKey{{role: freeBusyFolderRole}})
	if err != nil {
		return fail(storeError(err))
	}
	return folderPropertyValue{kind: 0x1102, data: marshalFreeBusyEntryIDs(binding, ids[0], s.freeBusyMessage().mid)}
}

func marshalFreeBusyEntryIDs(binding storeBinding, fid, mid uint64) []byte {
	replica := binding.store.replica
	var local []byte
	if mid != 0 {
		local = marshalMessageEntryID(binding.mailboxGUID, replica, fid, mid)
	}
	folder := marshalFolderEntryID(binding.mailboxGUID, replica, fid)
	data := append32(nil, 4)
	for _, entry := range [][]byte{nil, local, nil, folder} {
		data = binary.LittleEndian.AppendUint16(data, uint16(len(entry)))
		data = append(data, entry...)
	}
	return data
}

type freeBusyReferenceSet struct{ wire []byte }

func (freeBusyReferenceSet) String() string   { return "freeBusyReferenceSet{redacted}" }
func (freeBusyReferenceSet) GoString() string { return "freeBusyReferenceSet{redacted}" }

func parseFreeBusyReferenceSet(r *reader) (*freeBusyReferenceSet, error) {
	header := r.take(4)
	if r.err != nil {
		return nil, ErrMalformed
	}
	size := int(binary.LittleEndian.Uint16(header))
	if size < 2 {
		return nil, ErrMalformed
	}
	v := reader{data: r.take(size - 2)}
	if r.err != nil || v.u32() != freeBusyEntryIDsTag {
		return nil, ErrMalformed
	}
	if binary.LittleEndian.Uint16(header[2:]) != 1 {
		return nil, unsupportedOperation{label: "RopSetProperties"}
	}
	if v.u32() != 4 {
		return nil, ErrMalformed
	}
	for range 4 {
		length := v.take(2)
		if v.err != nil {
			return nil, ErrMalformed
		}
		n := int(binary.LittleEndian.Uint16(length))
		if n > messageEntryIDBytes {
			return nil, ErrMalformed
		}
		v.take(n)
	}
	if v.done() != nil {
		return nil, ErrMalformed
	}
	return &freeBusyReferenceSet{wire: bytes.Clone(v.data[4:])}, nil
}

// Clients can reaffirm the current computed references from either parent.
// Arbitrary linkage would allow Outlook configuration to affect other objects.
func setFreeBusyReference(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return ropError(op.ID, op.Input, code)
	}
	s := object.binding.store
	if !s.privateConfig || object.isLogon || object.download != nil {
		return ropError(op.ID, op.Input, NotSupported)
	}
	deny := func(code uint32) []byte {
		return propertyProblemsResponse(op.ID, op.Input, []faiPropertyProblem{{tag: freeBusyEntryIDsTag, code: code}})
	}
	if object.fid != object.binding.fids[MailboxRoot] && object.fid != object.binding.fids[Inbox] {
		return deny(accessDenied)
	}
	if _, err := s.folder(ctx, session, object.fid); err != nil {
		return ropError(op.ID, op.Input, storeError(err))
	}
	if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
		return ropError(op.ID, op.Input, storeError(err))
	}
	s.mu.Lock()
	fid := s.byKey[folderKey{role: freeBusyFolderRole}]
	mid := s.localFreeBusy.mid
	s.mu.Unlock()
	if fid == 0 {
		return deny(PropertyNotFound)
	}
	if !bytes.Equal(op.FreeBusyReference.wire, marshalFreeBusyEntryIDs(object.binding, fid, mid)) {
		return deny(accessDenied)
	}
	return propertyProblemsResponse(op.ID, op.Input, nil)
}

func (s *ramStore) freeBusyMessage() messageRow {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.localFreeBusy
}

func (s *ramStore) observeFreeBusyMessage(ctx context.Context, session core.Session, fid uint64) (messageSnapshot, error) {
	if err := s.authorizeLocalMessage(ctx, session, fid, true); err != nil {
		return messageSnapshot{}, err
	}
	s.mu.Lock()
	highWater := s.next
	row := s.localFreeBusy
	s.mu.Unlock()
	result := messageSnapshot{root: fid, replica: s.replica, highWater: highWater}
	if row.mid != 0 {
		result.messages = []messageRow{row}
		result.known = []messageKnown{{mid: row.mid, parent: fid, cn: row.cn, present: true}}
		result.retainedBytes = localFreeBusyBytes
	}
	return result, nil
}

func projectFreeBusyMessage(row messageRow) projectedMessage {
	p := projectedMessage{values: contentsValues{}, bytes: localFreeBusyBytes}
	for _, tag := range []uint32{0x001a001f, 0x0037001f, 0x003d001f, 0x0e1d001f, 0x30070040, delegatorWantsCopyTag, delegatorWantsInfoTag} {
		if value := row.object.Properties[core.Key{ID: tag >> 16}]; value != nil {
			putContentsValue(p.values, tag, value)
		}
	}
	putContentsValue(p.values, 0x0e070003, core.Integer(0))
	return p
}

// Only the Outlook configuration class, subject and two local preferences
// are writable. A client cannot set delegate identities or grant permissions.
func (d *faiDraft) setFreeBusyProperties(ctx context.Context, session core.Session, values faiSetRequest) ([]faiPropertyProblem, error) {
	if err := d.store.authorizeLocalMessage(ctx, session, d.root, true); err != nil {
		return nil, err
	}
	next, err := core.CloneProperties(d.properties)
	if err != nil {
		return nil, err
	}
	var accepted faiSetRequest
	var problems []faiPropertyProblem
	for index, v := range values {
		code := uint32(0)
		switch v.tag {
		case 0x001a001f:
			if v.value != localFreeBusyClass {
				code = NotSupported
			}
		case 0x0037001f, 0x0e1d001f:
			if v.value != localFreeBusySubject {
				code = NotSupported
			}
		case 0x003d001f:
			if v.value != core.Text("") {
				code = NotSupported
			}
		case delegatorWantsCopyTag, delegatorWantsInfoTag:
			if !contentsValueType(0x000b, v.value) {
				code = NotSupported
			}
		default:
			code = NotSupported
		}
		if code != 0 {
			problems = append(problems, faiPropertyProblem{uint16(index), v.tag, code})
			continue
		}
		accepted = append(accepted, v)
		next[core.Key{ID: v.tag >> 16}] = v.value
	}
	if err = applyFAISubject(next, accepted); err != nil {
		return nil, err
	}
	if err = ctx.Err(); err != nil {
		return nil, err
	}
	if len(accepted) != 0 {
		d.properties, d.dirty = next, true
	}
	return problems, nil
}

func (d *faiDraft) saveFreeBusyMessage(ctx context.Context, session core.Session, flags byte) (messageRow, bool, error) {
	s := d.store
	if err := s.authorizeLocalMessage(ctx, session, d.root, true); err != nil {
		return messageRow{}, false, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	old := s.localFreeBusy
	if old.object.Revision != d.base || old.mid != 0 && old.object.ID != d.id {
		return messageRow{}, false, core.ErrConflict
	}
	if d.saved && !d.dirty {
		d.finishSave(flags)
		return old, false, nil
	}
	if d.readOnly {
		return messageRow{}, false, core.ErrAccessDenied
	}
	if d.properties[core.Key{ID: 0x001a}] != localFreeBusyClass || d.properties[core.Subject] != localFreeBusySubject {
		return messageRow{}, false, core.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return messageRow{}, false, err
	}
	if s.next >= maxGlobalCount {
		return messageRow{}, false, core.ErrUnsupported
	}
	created := old.mid == 0
	var newLease *contentsLease
	if created {
		lease, err := s.reserveContentsMemory(localFreeBusyBytes)
		if err != nil {
			return messageRow{}, false, err
		}
		newLease = lease
		old = messageRow{
			mid: d.newMID, parent: d.root, sourceKey: d.request.source, privateConfig: true,
			object: core.Object{ID: d.id, Kind: core.Message},
		}
	}
	now := time.Now().UTC()
	old.cn = s.next + 1
	if created {
		old.readCN = old.cn
	}
	old.changeKey = gidChangeXID(s.hierarchyXID(folderID(old.cn)))
	old.predecessors = append([]byte{22}, []byte(old.changeKey)...)
	props, err := core.CloneProperties(d.properties)
	if err != nil {
		newLease.release()
		return messageRow{}, false, err
	}
	props[core.LastModifiedAt] = core.Timestamp(now)
	old.object.Properties = props
	old.object.Revision++
	old.modified = now
	if err := s.commitFreeBusyReplica(ctx, old, created); err != nil {
		newLease.release()
		return messageRow{}, false, err
	}
	if created {
		s.localFreeBusyLease = newLease
	}
	s.next = old.cn
	s.localFreeBusy = old
	d.properties = props // Future edits replace this map.
	d.base, d.saved, d.dirty = old.object.Revision, true, false
	d.finishSave(flags)
	return old, created, nil
}

const privateConfigurationKey byte = 0xff

func (s *ramStore) commitFreeBusyReplica(ctx context.Context, row messageRow, created bool) error {
	if s.durable.store == nil {
		return nil
	}
	counter := folderGlobalCount(row.mid)
	mutation := mapireplica.Mutation{
		HighWater: row.cn,
		Projection: []mapireplica.Projection{{
			Counter: counter, Parent: folderGlobalCount(row.parent), ChangeNumber: row.cn,
			ReadNumber: row.readCN, NativeRevision: row.object.Revision, Modified: row.modified,
			ChangeKey: []byte(row.changeKey), Predecessors: row.predecessors, Present: true,
		}},
		FAI: &mapireplica.FAIChanges{Objects: []mapireplica.FAIObject{{
			Counter: counter, Source: row.sourceKey, Properties: row.object.Properties,
			CacheBytes: localFreeBusyBytes, PrivateConfig: true,
		}}},
	}
	if created {
		mutation.StagedBindings = []mapireplica.Binding{{
			Namespace: mapireplica.Message, Key: []byte{privateConfigurationKey}, Counter: counter,
		}}
	}
	return s.commitReplica(ctx, mutation)
}

func (s *ramStore) loadFreeBusyReplica(epoch [16]byte, object mapireplica.FAIObject, version mapireplica.Projection) error {
	fid, mid := folderID(version.Parent), folderID(version.Counter)
	if version.Counter != object.Counter || !version.Present || version.Read || version.ReadNumber == 0 ||
		s.byFID[fid] != (folderKey{role: freeBusyFolderRole}) || object.Source != hierarchyXID(epoch, object.Counter) ||
		object.CacheBytes != localFreeBusyBytes || object.Properties[core.Key{ID: 0x001a}] != localFreeBusyClass ||
		object.Properties[core.Subject] != localFreeBusySubject {
		return core.ErrInvalid
	}
	lease, err := s.reserveContentsMemory(localFreeBusyBytes)
	if err != nil {
		return err
	}
	s.localFreeBusyLease = lease
	s.localFreeBusy = messageRow{
		mid: mid, parent: fid, cn: version.ChangeNumber, readCN: version.ReadNumber, sourceKey: object.Source,
		changeKey: changeXID(version.ChangeKey), predecessors: version.Predecessors, modified: version.Modified,
		privateConfig: true, object: core.Object{
			ID: faiIDForSource(object.Source), Kind: core.Message, Revision: version.NativeRevision, Properties: object.Properties,
		},
	}
	return nil
}

func (s *ramStore) editFreeBusyMessage(row messageRow) (*faiDraft, error) {
	lease, err := s.reserveContentsMemory(maxFAIObjectBytes)
	if err != nil {
		return nil, err
	}
	return &faiDraft{
		store: s, root: row.parent, id: row.object.ID, base: row.object.Revision,
		properties: row.object.Properties, lease: lease, saved: true, privateConfig: true,
	}, nil
}
