// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const maxReadStateChanges = 128

type readStateChange struct {
	source [22]byte
	read   bool
}

func (readStateChange) String() string   { return "readStateChange{private}" }
func (readStateChange) GoString() string { return "readStateChange{private}" }

func parseReadStateChanges(r *reader) ([]readStateChange, error) {
	header := r.take(2)
	if r.err != nil {
		return nil, r.err
	}
	size := int(binary.LittleEndian.Uint16(header))
	if size == 0 {
		return nil, ErrMalformed
	}
	data := reader{data: r.take(size)}
	if r.err != nil {
		return nil, r.err
	}
	var out []readStateChange
	seen := map[[22]byte]bool{}
	records := 0
	for data.pos < len(data.data) {
		records++
		if records > maxReadStateChanges {
			return nil, ErrUnsupported
		}
		n := data.take(2)
		if data.err != nil {
			return nil, data.err
		}
		width := int(binary.LittleEndian.Uint16(n))
		if width == 0 {
			return nil, ErrMalformed
		}
		key, mark := data.take(width), data.take(1)
		if data.err != nil {
			return nil, data.err
		}
		if width != 22 { // SourceKeys issued by the current RAM namespace.
			return nil, ErrUnsupported
		}
		if mark[0] > 1 {
			return nil, ErrMalformed
		}
		change := readStateChange{read: mark[0] == 1}
		copy(change.source[:], key)
		if before, ok := seen[change.source]; ok {
			if before != change.read {
				return nil, ErrUnsupported
			}
			continue
		}
		if len(out) >= maxReadStateChanges {
			return nil, ErrUnsupported
		}
		seen[change.source] = change.read
		out = append(out, change)
	}
	return out, nil
}

// Configure before serving; the service requires the authenticated TLS lab.
func (t *SessionTransport) EnableReadStateImport() { t.readStateImport = true }

func executeReadState(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if object.download == nil || object.download.collector == nil {
		return fail(NotSupported)
	}
	return fail(faiError(object.download.collector.importReadStates(ctx, session, op.ReadStates), false))
}

func (c *faiCollector) importReadStates(ctx context.Context, session core.Session, changes []readStateChange) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.prepareMutation(ctx, session, len(changes)); err != nil {
		return err
	}
	c.state.mu.Lock()
	ranges := len(contentsRanges(c.state.value.read))
	c.state.mu.Unlock()
	if ranges > maxIDSetRanges-maxReadStateChanges {
		return core.ErrUnsupported
	}
	c.grant.readBound = true
	c.grant.readBefore = cloneIDSet(c.baselineRead)
	c.readImported = true
	counters, err := c.state.store.importReadStates(ctx, session, c.state.root, changes)
	c.changes += len(changes)
	if err != nil {
		// Native operations are not transactional. Keep ICS unacknowledged
		// on failure; the actual verified prefix remains in the registry.
		return err
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	next := contentsRanges(c.state.value.read)
	for _, cn := range counters {
		next = append(next, idRange{cn, cn})
	}
	c.state.value.read = singleReplicaSet(c.state.store.replica, next)
	return nil
}

type readStateTarget struct {
	key    messageKey
	read   bool
	nextCN uint64
}

// Checks all targets before writing. Backends with MailFlagWriter also guard
// the interval between the last read and the write with a native revision.
// A later native failure can still leave a partial batch.
func (s *ramStore) importReadStates(ctx context.Context, session core.Session, root uint64, changes []readStateChange) ([]uint64, error) {
	if len(changes) == 0 || len(changes) > maxReadStateChanges {
		return nil, core.ErrInvalid
	}
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	if _, err := s.messageAnchor(ctx, session, root, true); err != nil {
		return nil, err
	}
	var targets []readStateTarget
	for _, change := range changes {
		ignored := false
		for _, record := range s.fai.records {
			if record.sync != nil && record.sync.source == change.source {
				ignored = true // MS-OXCFXICS: FAI read-state requests MUST be ignored.
				break
			}
		}
		if ignored {
			continue
		}
		wire := [8]byte{1, 0}
		copy(wire[2:], change.source[16:])
		mid := binary.LittleEndian.Uint64(wire[:])
		s.mu.Lock()
		key, known := s.byMID[mid]
		v := s.messages[key]
		associated := known && key.associated && s.hierarchyXID(mid) == change.source
		valid := known && !key.associated && key.special == 0 && v.present && v.parent == root && s.hierarchyXID(mid) == change.source
		s.mu.Unlock()
		if associated {
			continue
		}
		if !valid {
			return nil, core.ErrNotFound
		}
		targets = append(targets, readStateTarget{key: key, read: change.read})
	}
	if len(targets) == 0 {
		return nil, nil
	}
	anchor, err := s.messageAnchor(ctx, session, root, false)
	if err != nil {
		return nil, err
	}
	box := session.Mailbox()
	flagWriter, conditional := box.(core.MailFlagWriter)
	writer, legacy := box.(core.ReadStateWriter)
	if !conditional && !legacy {
		return nil, core.ErrUnsupported
	}
	readCurrent := func(target readStateTarget) (core.Object, error) {
		if err := ctx.Err(); err != nil {
			return core.Object{}, err
		}
		object, err := box.Get(ctx, target.key.id)
		if err != nil {
			return core.Object{}, err
		}
		if object.ID != target.key.id || object.Kind != core.Message || object.Parent != anchor.native {
			return core.Object{}, core.ErrConflict
		}
		if _, ok := object.Properties[core.IsRead].(core.Bool); !ok {
			return core.Object{}, core.ErrInvalid
		}
		return object, nil
	}
	for _, target := range targets {
		if _, err := readCurrent(target); err != nil {
			return nil, err
		}
	}
	s.mu.Lock()
	if s.next > maxGlobalCount || uint64(len(targets)) > maxGlobalCount-s.next {
		s.mu.Unlock()
		return nil, core.ErrUnsupported
	}
	if s.durable.store == nil {
		// The RAM path reserves the counters before native effects. The durable
		// path advances the counter and the resulting read state in one commit.
		for i := range targets {
			s.next++
			targets[i].nextCN = s.next
		}
	}
	s.mu.Unlock()
	var counters []uint64
	for _, target := range targets {
		currentAnchor, err := s.messageAnchor(ctx, session, root, false)
		if err != nil {
			return nil, err
		}
		if currentAnchor != anchor {
			return nil, core.ErrConflict
		}
		object, err := readCurrent(target)
		if err != nil {
			return nil, err
		}
		if object.Properties[core.IsRead] != core.Bool(target.read) {
			if conditional {
				object, err = flagWriter.SetMailFlag(ctx, object.ID, object.Revision, core.MailFlagRead, target.read)
			} else {
				object, err = writer.SetRead(ctx, object.ID, target.read)
			}
			if err != nil {
				return nil, err
			}
			if object.ID != target.key.id || object.Kind != core.Message || object.Parent != anchor.native || object.Properties[core.IsRead] != core.Bool(target.read) {
				return nil, core.ErrConflict
			}
		}
		counter, err := s.commitReadState(ctx, target)
		if err != nil {
			return nil, err
		}
		counters = append(counters, counter)
	}
	if _, err := s.messageAnchor(ctx, session, root, false); err != nil {
		return nil, err
	}
	return counters, nil
}

func (s *ramStore) commitReadState(ctx context.Context, target readStateTarget) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	version := s.messages[target.key]
	if version.read == target.read {
		return version.readCN, nil
	}
	version.read, version.readCN = target.read, target.nextCN
	if s.durable.store != nil {
		if s.next >= maxGlobalCount {
			return 0, core.ErrLimit
		}
		version.readCN = s.next + 1
		if err := s.commitReplica(ctx, mapireplica.Mutation{
			HighWater:  version.readCN,
			Projection: []mapireplica.Projection{s.persistedMessageProjection(s.byMessage[target.key], version)},
		}); err != nil {
			// A retry reads the already-applied native flag before reissuing any write.
			return 0, err
		}
		s.next = version.readCN
	}
	s.messages[target.key] = version
	return version.readCN, nil
}
