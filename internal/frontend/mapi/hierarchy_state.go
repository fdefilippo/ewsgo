// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"sync"

	"ewsgo/internal/core"
)

const (
	idsetGivenTag = uint32(0x40170003) // Special metaproperty: IDSET bytes, not int32.
	cnsetSeenTag  = uint32(0x67960102)
)

type hierarchyUpload struct {
	kind           byte
	property, size uint32
	data           []byte
}

func (hierarchyUpload) String() string   { return "hierarchyUpload{redacted}" }
func (hierarchyUpload) GoString() string { return "hierarchyUpload{redacted}" }

// The common three-byte ROP header precedes these fields. These codecs remain
// disconnected from the operational decoder until the download exists too.
func parseHierarchyUpload(r *reader, kind byte) (hierarchyUpload, error) {
	op := hierarchyUpload{kind: kind}
	switch kind {
	case 0x75:
		fields := r.take(8)
		if r.err != nil {
			return hierarchyUpload{}, ErrMalformed
		}
		op.property, op.size = binary.LittleEndian.Uint32(fields), binary.LittleEndian.Uint32(fields[4:])
		if op.size > maxIDSetBytes {
			return hierarchyUpload{}, ErrUnsupported
		}
	case 0x76:
		size := r.take(4)
		if r.err != nil {
			return hierarchyUpload{}, ErrMalformed
		}
		op.size = binary.LittleEndian.Uint32(size)
		if op.size == 0 || uint64(op.size) > uint64(len(r.data)-r.pos) {
			return hierarchyUpload{}, ErrMalformed
		}
		if op.size > maxIDSetBytes {
			return hierarchyUpload{}, ErrUnsupported
		}
		op.data = append([]byte(nil), r.take(int(op.size))...)
	case 0x77:
	default:
		return hierarchyUpload{}, ErrUnsupported
	}
	return op, nil
}

type hierarchyPending struct {
	property, size uint32
	data           []byte
}

// One hierarchy state belongs to one download context, authenticated scope,
// synchronization root, and RAM epoch. State is immutable after planning starts.
// The optional pending upload is bounded separately from two committed sets.
type hierarchyState struct {
	mu          sync.Mutex
	store       *ramStore
	root        uint64
	given, seen idSet
	pending     *hierarchyPending
	started     bool
}

func (*hierarchyState) String() string   { return "hierarchyState{redacted}" }
func (*hierarchyState) GoString() string { return "hierarchyState{redacted}" }

func newHierarchyState(ctx context.Context, session core.Session, store *ramStore, root uint64) (*hierarchyState, error) {
	if store == nil {
		return nil, core.ErrInvalid
	}
	if err := store.authorizeHierarchyAnchor(ctx, session, root); err != nil {
		return nil, err
	}
	return &hierarchyState{store: store, root: root}, nil
}

// Native read authorization conveys no right to import/create/delete anything.
// Unlike folder(), this check never creates parent mappings as a side effect.
func (s *ramStore) authorizeHierarchyAnchor(ctx context.Context, session core.Session, anchor uint64) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if session == nil || session.Identity().Scope != s.scope {
		return core.ErrAccessDenied
	}
	box := session.Mailbox()
	if box == nil || box.Root() != s.root {
		return core.ErrConflict
	}
	s.mu.Lock()
	key, ok := s.byFID[anchor]
	s.mu.Unlock()
	if !ok {
		return core.ErrNotFound
	}
	if err := hierarchyOwner(ctx, box); err != nil {
		return err
	}
	root, err := box.Get(ctx, s.root)
	if err != nil {
		return err
	}
	if root.ID != s.root || root.Kind != core.Folder {
		return core.ErrInvalid
	}
	if key.dynamic != 0 {
		if _, err := s.dynamicFolder(ctx, session, anchor, key); err != nil {
			return err
		}
	} else if key.native != "" && key.native != s.root {
		folder, err := box.Get(ctx, key.native)
		if err != nil {
			return err
		}
		if folder.ID != key.native || folder.Kind != core.Folder {
			return core.ErrInvalid
		}
	} else if key.native == "" && serviceFolderName(key.role) == "" {
		return core.ErrInvalid
	}
	if err := hierarchyOwner(ctx, box); err != nil {
		return err
	}
	current := session.Mailbox()
	if session.Identity().Scope != s.scope || current == nil || current.Root() != s.root {
		return core.ErrAccessDenied
	}
	return ctx.Err()
}

func (s *hierarchyState) upload(ctx context.Context, session core.Session, op hierarchyUpload) error {
	return s.uploadMode(ctx, session, op, false)
}

func (s *hierarchyState) uploadMode(ctx context.Context, session core.Session, op hierarchyUpload, collector bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.authorizeHierarchyAnchor(ctx, session, s.root); err != nil {
		return err
	}
	if s.started {
		return core.ErrConflict
	}
	switch op.kind {
	case 0x75:
		if op.property == 0x40170102 {
			op.property = idsetGivenTag
		}
		if s.pending != nil {
			return core.ErrConflict
		}
		if op.property != idsetGivenTag && op.property != cnsetSeenTag {
			return core.ErrUnsupported
		}
		if op.size > maxIDSetBytes {
			return core.ErrUnsupported
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		s.pending = &hierarchyPending{property: op.property, size: op.size}
	case 0x76:
		if s.pending == nil {
			return core.ErrConflict
		}
		if len(op.data) == 0 || uint32(len(op.data)) != op.size || op.size > s.pending.size-uint32(len(s.pending.data)) {
			return core.ErrInvalid
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		s.pending.data = append(s.pending.data, op.data...)
	case 0x77:
		if s.pending == nil || uint32(len(s.pending.data)) != s.pending.size {
			return core.ErrConflict
		}
		if collector && s.pending.property == idsetGivenTag {
			// Given belongs to the client's download state, not to this uploader.
			if err := ctx.Err(); err != nil {
				return err
			}
			s.pending = nil
			return nil
		}
		set, err := parseIDSet(s.pending.data)
		if err != nil {
			return err
		}
		if err := s.validateStateSet(set, s.pending.property); err != nil {
			return err
		}
		if collector {
			// Validation restricts both operands to the current single replica.
			var ranges []idRange
			for _, operand := range []idSet{s.seen, set} {
				for _, replica := range operand.replicas {
					for _, r := range replica.ranges {
						if r.low == 0 {
							return core.ErrResync
						}
					}
					ranges = append(ranges, replica.ranges...)
				}
			}
			set = idSet{replicas: []replicaSet{{replica: s.store.replica, ranges: mergeIDRanges(ranges)}}}
			if _, err := set.marshal(); err != nil {
				return err
			}
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		if s.pending.property == idsetGivenTag {
			s.given = set
		} else {
			s.seen = set
		}
		s.pending = nil
	default:
		return core.ErrUnsupported
	}
	return nil
}

func (s *hierarchyState) validateStateSet(set idSet, property uint32) error {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	cardinality := uint64(0)
	for _, replica := range set.replicas {
		if replica.replica != s.store.replica {
			return core.ErrResync
		}
		for _, r := range replica.ranges {
			if r.high > s.store.next {
				return core.ErrResync
			}
			if property == idsetGivenTag {
				if r.low == 0 {
					return core.ErrResync
				}
				cardinality += r.high - r.low + 1
				if cardinality > uint64(len(s.store.byFID)) {
					return core.ErrResync
				}
			}
		}
	}
	if property == idsetGivenTag {
		matched := uint64(0)
		for fid := range s.store.byFID {
			if set.contains(s.store.replica, folderGlobalCount(fid)) {
				matched++
			}
		}
		if matched != cardinality {
			return core.ErrResync
		}
	}
	return nil
}

func folderGlobalCount(fid uint64) uint64 {
	wire := binary.LittleEndian.AppendUint64(nil, fid)
	var n uint64
	for _, b := range wire[2:] {
		n = n<<8 | uint64(b)
	}
	return n
}

type hierarchyDelta struct {
	changes                   []hierarchyFolder
	removed                   []uint64
	initialGiven, initialSeen idSet
	finalGiven, finalSeen     idSet
}

func (hierarchyDelta) String() string   { return "hierarchyDelta{redacted}" }
func (hierarchyDelta) GoString() string { return "hierarchyDelta{redacted}" }

// planHierarchy does not emit or commit final state to a client. The future
// transfer must deliver these changes/deletions before returning final state.
func (s *hierarchyState) planHierarchy(ctx context.Context, session core.Session, snapshot hierarchySnapshot) (hierarchyDelta, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.store.authorizeHierarchyAnchor(ctx, session, s.root); err != nil {
		return hierarchyDelta{}, err
	}
	if s.started || s.pending != nil {
		return hierarchyDelta{}, core.ErrConflict
	}
	if snapshot.replica != s.store.replica || snapshot.root != s.root {
		return hierarchyDelta{}, core.ErrResync
	}
	// A snapshot older than the uploaded state cannot silently drop newly
	// mapped IDs. No final state may acknowledge a deletion absent from it.
	for _, set := range []idSet{s.given, s.seen} {
		for _, replica := range set.replicas {
			for _, r := range replica.ranges {
				if r.high > snapshot.highWater {
					return hierarchyDelta{}, core.ErrResync
				}
			}
		}
	}
	known := make(map[uint64]bool, len(snapshot.known))
	for _, fid := range snapshot.known {
		known[folderGlobalCount(fid)] = true
	}
	for _, replica := range s.given.replicas {
		for _, r := range replica.ranges {
			matched := uint64(0)
			for n := range known {
				if n >= r.low && n <= r.high {
					matched++
				}
			}
			if matched != r.high-r.low+1 {
				return hierarchyDelta{}, core.ErrResync
			}
		}
	}
	delta := hierarchyDelta{initialGiven: s.given, initialSeen: s.seen}
	current := make(map[uint64]bool, len(snapshot.folders))
	var given, seen []idRange
	for _, replica := range s.seen.replicas {
		seen = append(seen, replica.ranges...)
	}
	for _, folder := range snapshot.folders {
		current[folder.fid] = true
		id := folderGlobalCount(folder.fid)
		given = append(given, idRange{id, id})
		seen = append(seen, idRange{folder.cn, folder.cn})
		if !s.given.contains(snapshot.replica, id) || !s.seen.contains(snapshot.replica, folder.cn) {
			delta.changes = append(delta.changes, folder)
		}
	}
	for _, fid := range snapshot.known {
		if !current[fid] && s.given.contains(snapshot.replica, folderGlobalCount(fid)) {
			delta.removed = append(delta.removed, fid)
		}
	}
	delta.finalGiven = singleReplicaSet(snapshot.replica, given)
	delta.finalSeen = singleReplicaSet(snapshot.replica, seen)
	for _, set := range []idSet{delta.finalGiven, delta.finalSeen} {
		if _, err := set.marshal(); err != nil {
			return hierarchyDelta{}, err
		}
	}
	// Return detached state: no future caller can edit committed uploaded sets.
	delta.initialGiven = cloneIDSet(delta.initialGiven)
	delta.initialSeen = cloneIDSet(delta.initialSeen)
	if err := ctx.Err(); err != nil {
		return hierarchyDelta{}, err
	}
	s.started = true
	return delta, nil
}

func cloneIDSet(set idSet) idSet {
	var out idSet
	for _, s := range set.replicas {
		out.replicas = append(out.replicas, replicaSet{replica: s.replica, ranges: append([]idRange(nil), s.ranges...)})
	}
	return out
}
