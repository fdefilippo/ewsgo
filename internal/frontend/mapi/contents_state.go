// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"sort"
	"sync"
	"time"

	"ewsgo/internal/core"
)

const maxContentsStateProofs = 256
const contentsStateMemory = 1 << 20 // Four parsed sets, pending upload, clones and temporary encodings.
const contentsProofMemory = 256

type contentsState struct {
	mu              sync.Mutex
	store           *ramStore
	root            uint64
	plan            contentsFilterPlan
	scope           [32]byte
	value           contentsSyncState
	clientGiven     []uint64  // Reserved client IDs awaiting a server binding.
	retiredGiven    []idRange // Abandoned IDs present in the client's uploaded Given.
	pending         *hierarchyPending
	lease           *contentsLease
	started, closed bool
	collector       bool
	trashContacts   bool
}
type contentsStateProof struct {
	retained   *list.Element
	references int
	lease      *contentsLease
}
type contentsProofTicket struct {
	store *ramStore
	key   [32]byte
	proof *contentsStateProof
	done  bool
}

func (*contentsState) String() string         { return "contentsState{private}" }
func (*contentsState) GoString() string       { return "contentsState{private}" }
func (*contentsStateProof) String() string    { return "contentsStateProof{private}" }
func (*contentsStateProof) GoString() string  { return "contentsStateProof{private}" }
func (*contentsProofTicket) String() string   { return "contentsProofTicket{private}" }
func (*contentsProofTicket) GoString() string { return "contentsProofTicket{private}" }

func newContentsState(ctx context.Context, session core.Session, store *ramStore, root uint64, c contentsConfigure, plan contentsFilterPlan) (*contentsState, error) {
	if store == nil {
		return nil, core.ErrInvalid
	}
	if err := validateContentsConfigure(c.frame); err != nil {
		return nil, err
	}
	if !plan.normal && !plan.associated {
		return nil, core.ErrUnsupported
	}
	if plan.special != 0 && (!plan.normal || plan.special != core.ContactsFolder && plan.special != core.CalendarFolder) {
		return nil, core.ErrInvalid
	}
	if plan.normal != (c.frame.flags&0x20 != 0) || plan.associated != (c.frame.flags&0x10 != 0) || plan.readState != (c.frame.flags&8 != 0) {
		return nil, core.ErrInvalid
	}
	scope, err := contentsScopeFingerprint(root, c.frame, plan)
	if err != nil {
		return nil, err
	}
	store.mu.Lock()
	folder := store.byFID[root]
	store.mu.Unlock()
	s := &contentsState{store: store, root: root, plan: plan, scope: scope}
	if err := s.authorize(ctx, session); err != nil {
		return nil, err
	}
	_, canReadTrashContacts := session.Mailbox().(core.TrashedContactReader)
	s.trashContacts = plan.normal && store.contactsMetadata && store.trash != "" && folder.native == store.trash && canReadTrashContacts
	lease, err := store.reserveContentsMemory(contentsStateMemory)
	if err != nil {
		return nil, err
	}
	s.lease = lease
	if err := ctx.Err(); err != nil {
		lease.release()
		return nil, err
	}
	return s, nil
}
func (s *contentsState) authorize(ctx context.Context, session core.Session) error {
	_, err := s.store.scopedMessageAnchor(ctx, session, s.root, !s.plan.normal, s.plan.special)
	return err
}
func (s *contentsState) close() {
	if s == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.closed {
		s.closed = true
		s.pending = nil
		s.value = contentsSyncState{}
		s.clientGiven, s.retiredGiven = nil, nil
		s.lease.release()
		s.lease = nil
	}
}
func (s *contentsState) upload(ctx context.Context, session core.Session, op hierarchyUpload) (err error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	diagnostic := contentsUploadFailure{stage: uploadLifecycle, property: contentsUploadProperty(op.property)}
	if op.kind != 0x75 && s.pending != nil {
		diagnostic.property = contentsUploadProperty(s.pending.property)
	}
	defer func() {
		if err != nil {
			diagnostic.kind = contentsUploadErrorKind(err)
			err = &contentsUploadError{failure: diagnostic, cause: err}
		}
	}()
	if s.closed {
		return core.ErrClosed
	}
	diagnostic.stage = uploadAuthorize
	if err := s.authorize(ctx, session); err != nil {
		return err
	}
	diagnostic.stage = uploadSequence
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
		if op.property != idsetGivenTag && op.property != cnsetSeenTag && op.property != cnsetSeenFAITag && op.property != cnsetReadTag {
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
		if s.collector && s.pending.property == idsetGivenTag {
			// Upload collectors ignore Given; it belongs to the client's download
			// state and must neither authorize imports nor be echoed by the server.
			diagnostic.stage = uploadCommitAccess
			if err := s.authorize(ctx, session); err != nil {
				return err
			}
			if err := ctx.Err(); err != nil {
				return err
			}
			s.pending = nil
			return nil
		}
		diagnostic.stage = uploadParse
		set, err := parseIDSet(s.pending.data)
		if err != nil {
			return err
		}
		stage, _, err := s.validateSetDiagnostic(set, s.pending.property)
		diagnostic.stage = stage
		if err != nil {
			return err
		}
		// Recheck current access after decoding, before replacing a committed set.
		diagnostic.stage = uploadCommitAccess
		if err := s.authorize(ctx, session); err != nil {
			return err
		}
		if err := ctx.Err(); err != nil {
			return err
		}
		switch s.pending.property {
		case idsetGivenTag:
			s.value.given = set
		case cnsetSeenTag:
			s.value.seen = set
		case cnsetSeenFAITag:
			s.value.seenFAI = set
		case cnsetReadTag:
			s.value.read = set
		}
		s.pending = nil
	default:
		return core.ErrUnsupported
	}
	return nil
}

type provisionalGiven struct {
	pending []uint64
	retired []idRange
	active  []idRange
}

func (s *contentsState) validateSetDiagnostic(set idSet, property uint32) (contentsUploadStage, provisionalGiven, error) {
	s.store.mu.Lock()
	defer s.store.mu.Unlock()
	var provisional provisionalGiven
	for _, replica := range set.replicas {
		if replica.replica != s.store.replica {
			return uploadReplica, provisional, core.ErrResync
		}
		for _, r := range replica.ranges {
			if r.low == 0 || r.high > s.store.next {
				return uploadCounter, provisional, core.ErrResync
			}
		}
	}
	if property != idsetGivenTag || len(set.replicas) == 0 {
		return uploadCommitAccess, provisional, nil
	}
	provisional.active, provisional.retired = splitRetiredGiven(contentsRanges(set), s.store.retiredMIDs[s.root])
	givenLimit := uint64(len(s.store.byMID))
	if s.store.durable.store != nil {
		givenLimit = min(givenLimit+maxFAICollectorChanges, uint64(s.store.mappingLimit()))
	}
	cardinality := uint64(0)
	for _, r := range provisional.active {
		cardinality += r.high - r.low + 1
		if cardinality > givenLimit {
			return uploadGivenLimit, provisional, core.ErrResync
		}
	}
	for _, r := range provisional.active {
		for count := r.low; count <= r.high; count++ {
			mid := folderID(count)
			if key, known := s.store.byMID[mid]; known {
				if key.associated && !s.plan.associated || !key.associated && !s.plan.normal {
					return uploadGivenClass, provisional, core.ErrResync
				}
				special := s.store.messageKeySpecialLocked(key)
				if !key.associated && special != s.plan.special && !(s.trashContacts && special == core.ContactsFolder) {
					return uploadGivenClass, provisional, core.ErrResync
				}
				continue
			}
			if _, folder := s.store.byFID[mid]; folder || s.store.durable.store == nil || !s.store.clientReservedCounterLocked(count) {
				return uploadGivenUnknown, provisional, core.ErrResync
			}
			if len(provisional.pending) == maxFAICollectorChanges {
				return uploadGivenLimit, provisional, core.ErrResync
			}
			provisional.pending = append(provisional.pending, count)
		}
	}
	return uploadCommitAccess, provisional, nil
}

// Both inputs are canonical, so even a very large retired span needs no
// per-counter allocation or iteration.
func splitRetiredGiven(given, retired []idRange) (active, removed []idRange) {
	if len(retired) == 0 {
		return given, nil
	}
	next := 0
	for _, g := range given {
		for next < len(retired) && retired[next].high < g.low {
			next++
		}
		for next < len(retired) && retired[next].low <= g.high {
			r := retired[next]
			if g.low < r.low {
				active = append(active, idRange{g.low, r.low - 1})
			}
			removed = append(removed, idRange{max(g.low, r.low), min(g.high, r.high)})
			g.low = r.high + 1
			if g.low > g.high {
				break
			}
			next++
		}
		if g.low <= g.high {
			active = append(active, g)
		}
	}
	return active, removed
}

func (s *contentsState) beginPlanning(ctx context.Context, session core.Session) (contentsSyncState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return contentsSyncState{}, core.ErrClosed
	}
	if err := s.authorize(ctx, session); err != nil {
		return contentsSyncState{}, err
	}
	if s.started || s.pending != nil {
		return contentsSyncState{}, core.ErrConflict
	}
	initial := cloneContentsState(s.value)
	_, provisional, err := s.validateSetDiagnostic(initial.given, idsetGivenTag)
	if err != nil {
		return contentsSyncState{}, err
	}
	if len(provisional.retired) != 0 {
		initial.given = singleReplicaSet(s.store.replica, provisional.active)
	}
	if len(provisional.pending) != 0 {
		// Clients can report reserved IDs before the corresponding objects
		// exist on the server. Keep them separate from the server's delta.
		given, removed := withoutCreatedGiven(initial.given, s.store.replica, provisional.pending)
		if !removed {
			return contentsSyncState{}, core.ErrResync
		}
		initial.given = given
	}
	if err := s.store.validateContentsCheckpointRoot(ctx, s.scope, initial, s.root); err != nil {
		if !errors.Is(err, core.ErrResync) || !s.canReconcileDownload(initial) {
			return contentsSyncState{}, err
		}
	}
	if err := ctx.Err(); err != nil {
		return contentsSyncState{}, err
	}
	s.started = true
	s.clientGiven = provisional.pending
	s.retiredGiven = provisional.retired
	return initial, nil
}

// Outlook can resume from a combination of its own saved ICS sets. In the
// durable store, reconcile that state against bindings from the same folder
// and epoch. The delta planner still requires both a matching MID and its
// current CN before skipping a message. Upload collectors use their own proof.
func (s *contentsState) canReconcileDownload(state contentsSyncState) bool {
	if s.collector || s.store.durable.store == nil || len(state.given.replicas) == 0 {
		return false
	}
	store := s.store
	store.mu.Lock()
	defer store.mu.Unlock()
	// Given was validated and bounded before this check, including its class.
	for _, r := range state.given.replicas[0].ranges {
		for count := r.low; count <= r.high; count++ {
			key, exists := store.byMID[folderID(count)]
			if version, known := store.messages[key]; !exists || !known || version.parent != s.root {
				return false
			}
		}
	}
	for _, set := range []idSet{state.seen, state.seenFAI, state.read} {
		for _, replica := range set.replicas {
			if replica.replica != store.replica {
				return false
			}
			for _, r := range replica.ranges {
				if r.low == 0 || r.high > store.next {
					return false
				}
				// Client-reserved object counters cannot be server-issued CNs.
				i := sort.Search(len(store.localReservations), func(i int) bool {
					return store.localReservations[i].high >= r.low
				})
				if i < len(store.localReservations) && store.localReservations[i].low <= r.high {
					return false
				}
			}
		}
	}
	return true
}

func cloneContentsState(s contentsSyncState) contentsSyncState {
	return contentsSyncState{cloneIDSet(s.given), cloneIDSet(s.seen), cloneIDSet(s.seenFAI), cloneIDSet(s.read)}
}

func contentsCheckpointKey(scope [32]byte, state contentsSyncState) ([32]byte, bool, error) {
	h := sha256.New()
	h.Write(scope[:])
	empty := true
	for _, set := range []idSet{state.given, state.seen, state.seenFAI, state.read} {
		raw, err := set.marshal()
		if err != nil {
			return [32]byte{}, false, err
		}
		if len(raw) != 0 {
			empty = false
		}
		contentsHashBytes(h, raw)
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest, empty, nil
}
func (s *ramStore) validateContentsCheckpoint(ctx context.Context, scope [32]byte, state contentsSyncState) error {
	key, empty, err := contentsCheckpointKey(scope, state)
	if err != nil {
		return err
	}
	if empty {
		return nil
	}
	s.contentsProofsMu.Lock()
	defer s.contentsProofsMu.Unlock()
	return s.validateCompletedContentsProof(ctx, key)
}

// Called with contentsProofsMu held.
func (s *ramStore) validateCompletedContentsProof(ctx context.Context, key [32]byte) error {
	if s.durable.store != nil {
		return s.durable.store.ValidateMAPIContentsProof(ctx, s.durable.mailbox, s.replica, key)
	}
	proof := s.contentsProofs[key]
	if proof == nil || proof.retained == nil {
		return core.ErrResync
	}
	return nil
}

// Reserve before the transfer begins. Pending transfers have their own slots
// within the memory ledger and do not shorten checkpoint retention.
func (s *ramStore) stageContentsCheckpoint(scope [32]byte, state contentsSyncState) (*contentsProofTicket, error) {
	key, empty, err := contentsCheckpointKey(scope, state)
	if err != nil {
		return nil, err
	}
	if empty {
		return &contentsProofTicket{done: true}, nil
	}
	s.contentsProofsMu.Lock()
	defer s.contentsProofsMu.Unlock()
	proof := s.contentsProofs[key]
	if proof == nil {
		if len(s.contentsProofs) >= maxContentsStateProofs+maxLiveTransfers {
			return nil, core.ErrUnsupported
		}
		lease, err := s.reserveContentsMemory(contentsProofMemory)
		if err != nil {
			return nil, err
		}
		proof = &contentsStateProof{lease: lease}
		if s.contentsProofs == nil {
			s.contentsProofs = map[[32]byte]*contentsStateProof{}
		}
		s.contentsProofs[key] = proof
	}
	proof.references++
	return &contentsProofTicket{store: s, key: key, proof: proof}, nil
}
func (t *contentsProofTicket) finish(ctx context.Context, publish bool) error {
	if t == nil || t.store == nil {
		return nil
	}
	s := t.store
	s.contentsProofsMu.Lock()
	defer s.contentsProofsMu.Unlock()
	if t.done {
		return nil
	}
	if publish && s.durable.store != nil {
		if err := s.durable.store.PublishMAPIContentsProof(ctx, s.durable.mailbox, s.replica, t.key); err != nil {
			return err
		}
	} else if publish {
		if t.proof.retained == nil {
			t.proof.retained = s.contentsProofOrder.PushBack(t.key)
		} else {
			s.contentsProofOrder.MoveToBack(t.proof.retained)
		}
		if s.contentsProofOrder.Len() > maxContentsStateProofs {
			oldest := s.contentsProofOrder.Front()
			key := oldest.Value.([32]byte)
			proof := s.contentsProofs[key]
			s.contentsProofOrder.Remove(oldest)
			proof.retained = nil
			s.releaseContentsProof(key, proof)
		}
	}
	t.done = true
	t.proof.references--
	s.releaseContentsProof(t.key, t.proof)
	return nil
}

// The caller holds contentsProofsMu. An expired proof may still have a
// pending transfer. That transfer can publish it again upon completion.
func (s *ramStore) releaseContentsProof(key [32]byte, proof *contentsStateProof) {
	if proof.retained == nil && proof.references == 0 {
		delete(s.contentsProofs, key)
		proof.lease.release()
	}
}

func contentsHashBytes(h hash.Hash, data []byte) {
	h.Write(binary.LittleEndian.AppendUint32(nil, uint32(len(data))))
	h.Write(data)
}
func contentsScopeFingerprint(root uint64, c synchronizationConfigure, plan contentsFilterPlan) ([32]byte, error) {
	h := sha256.New()
	h.Write([]byte("EWSGo-Contents-Scope-v1"))
	h.Write(binary.LittleEndian.AppendUint64(nil, root))
	h.Write(binary.LittleEndian.AppendUint16(nil, c.flags))
	h.Write([]byte{c.sendOptions & 0x1d})
	h.Write(binary.LittleEndian.AppendUint32(nil, c.extraFlags&15))
	for _, enabled := range []bool{plan.normal, plan.associated, plan.readState} {
		if enabled {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
	}
	tags := append([]uint32(nil), c.tags...)
	sort.Slice(tags, func(i, j int) bool { return tags[i] < tags[j] })
	var unique []uint32
	for _, tag := range tags {
		if len(unique) == 0 || tag != unique[len(unique)-1] {
			unique = append(unique, tag)
		}
	}
	h.Write(binary.LittleEndian.AppendUint32(nil, uint32(len(unique))))
	for _, tag := range unique {
		h.Write(binary.LittleEndian.AppendUint32(nil, tag))
	}
	if err := hashContentsRestriction(h, plan.restriction, 0); err != nil {
		return [32]byte{}, err
	}
	if plan.special != 0 {
		h.Write([]byte{byte(plan.special)})
	}
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest, nil
}
func hashContentsRestriction(h hash.Hash, n *contentsRestriction, depth int) error {
	if n == nil {
		h.Write([]byte{0})
		return nil
	}
	if depth > maxContentsRestrictionDepth || !n.bound {
		return core.ErrInvalid
	}
	h.Write([]byte{1, n.op, n.rel})
	h.Write(binary.LittleEndian.AppendUint32(nil, n.fuzzy))
	if n.op == 6 {
		h.Write(binary.LittleEndian.AppendUint32(nil, n.mask))
	}
	h.Write(binary.LittleEndian.AppendUint16(nil, n.property.id))
	h.Write(binary.LittleEndian.AppendUint16(nil, n.property.kind))
	h.Write(n.property.name.Set[:])
	h.Write([]byte{n.property.name.Kind})
	h.Write(binary.LittleEndian.AppendUint32(nil, n.property.name.LID))
	contentsHashBytes(h, []byte(n.property.name.Name))
	switch value := n.value.(type) {
	case nil:
		h.Write([]byte{0})
	case core.Bool:
		h.Write([]byte{1})
		if value {
			h.Write([]byte{1})
		} else {
			h.Write([]byte{0})
		}
	case core.Integer:
		h.Write([]byte{2})
		h.Write(binary.LittleEndian.AppendUint64(nil, uint64(value)))
	case core.Text:
		h.Write([]byte{3})
		contentsHashBytes(h, []byte(value))
	case core.Binary:
		h.Write([]byte{4})
		contentsHashBytes(h, value)
	case core.Timestamp:
		h.Write([]byte{5})
		h.Write(binary.LittleEndian.AppendUint64(nil, fileTime(time.Time(value))))
	default:
		return core.ErrUnsupported
	}
	h.Write(binary.LittleEndian.AppendUint32(nil, uint32(len(n.children))))
	for _, child := range n.children {
		if err := hashContentsRestriction(h, child, depth+1); err != nil {
			return err
		}
	}
	return nil
}
