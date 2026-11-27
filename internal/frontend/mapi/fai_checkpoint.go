// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const maxFAIStatePermits = 64
const faiStatePermitMemory = 512 << 10 // SeenFAI, Seen and Read plus temporary encodings.
const maxFAICollectorChanges = 128

type faiStatePermit struct {
	root                                   uint64
	before, readBefore, normalBefore       idSet
	afterKey, readAfterKey, normalAfterKey [32]byte
	readBound                              bool
	created                                []uint64 // Only successfully created MIDs, at most 128.
	closed                                 bool
	lease                                  *contentsLease
}

func (*faiStatePermit) String() string   { return "faiStatePermit{private}" }
func (*faiStatePermit) GoString() string { return "faiStatePermit{private}" }
func (s *ramStore) reserveFAIStatePermit(root uint64, before idSet) (*faiStatePermit, error) {
	s.contentsProofsMu.Lock()
	defer s.contentsProofsMu.Unlock()
	if len(s.faiStatePermits) >= maxFAIStatePermits {
		return nil, core.ErrUnsupported
	}
	lease, err := s.reserveContentsMemory(faiStatePermitMemory)
	if err != nil {
		return nil, err
	}
	p := &faiStatePermit{root: root, before: cloneIDSet(before), lease: lease}
	if s.faiStatePermits == nil {
		s.faiStatePermits = map[*faiStatePermit]bool{}
	}
	s.faiStatePermits[p] = true
	return p, nil
}
func (s *ramStore) finishFAIStatePermit(p *faiStatePermit, publish bool) {
	if p == nil {
		return
	}
	s.contentsProofsMu.Lock()
	defer s.contentsProofsMu.Unlock()
	if p.closed {
		return
	}
	p.closed = true
	delete(s.faiStatePermits, p)
	if publish {
		retainedBytes := 512 + 8*cap(p.created)
		for _, set := range []idSet{p.before, p.readBefore, p.normalBefore} {
			retainedBytes += 64 * cap(set.replicas)
			for _, replica := range set.replicas {
				retainedBytes += 16 * cap(replica.ranges)
			}
		}
		_ = p.lease.shrink(min(retainedBytes, faiStatePermitMemory))
		if len(s.faiUploadProofs) == maxFAIStatePermits {
			s.faiUploadProofs[0].release()
			copy(s.faiUploadProofs, s.faiUploadProofs[1:])
			s.faiUploadProofs = s.faiUploadProofs[:maxFAIStatePermits-1]
		}
		s.faiUploadProofs = append(s.faiUploadProofs, p)
		return
	}
	p.release()
}

func (p *faiStatePermit) release() {
	p.before, p.readBefore, p.normalBefore = idSet{}, idSet{}, idSet{}
	p.created = nil
	p.lease.release()
	p.lease = nil
}
func (s *ramStore) freezeUploadStatePermit(p *faiStatePermit, state contentsSyncState) error {
	if p == nil {
		return core.ErrInvalid
	}
	raw, err := state.seenFAI.marshal()
	if err != nil {
		return err
	}
	s.contentsProofsMu.Lock()
	defer s.contentsProofsMu.Unlock()
	if p.closed || p.afterKey != ([32]byte{}) {
		return core.ErrConflict
	}
	if !state.seenFAI.containsSet(p.before) || !state.seen.containsSet(p.normalBefore) ||
		p.readBound && !state.read.containsSet(p.readBefore) {
		return core.ErrInvalid
	}
	var readAfterKey, normalAfterKey [32]byte
	if p.readBound {
		encoded, err := state.read.marshal()
		if err != nil {
			return err
		}
		readAfterKey = sha256.Sum256(encoded)
	}
	if !p.normalBefore.containsSet(state.seen) {
		encoded, err := state.seen.marshal()
		if err != nil {
			return err
		}
		normalAfterKey = sha256.Sum256(encoded)
	}
	p.readAfterKey, p.normalAfterKey = readAfterKey, normalAfterKey
	p.afterKey = sha256.Sum256(raw)
	return nil
}
func (s *ramStore) publishUploadProof(ctx context.Context, p *faiStatePermit) error {
	if p == nil || s.durable.store == nil {
		return nil
	}
	before, err := p.before.marshal()
	if err != nil {
		return err
	}
	proof := mapireplica.UploadProof{
		Root: folderGlobalCount(p.root), Before: before, After: p.afterKey, Created: p.created,
	}
	if p.readBound {
		proof.ReadBefore, err = p.readBefore.marshal()
		if err != nil {
			return err
		}
		proof.ReadAfter = p.readAfterKey
	}
	if p.normalAfterKey != ([32]byte{}) {
		proof.NormalBefore, err = p.normalBefore.marshal()
		if err != nil {
			return err
		}
		proof.NormalAfter = p.normalAfterKey
	}
	return s.durable.store.PublishMAPIUploadProof(ctx, s.durable.mailbox, s.replica, proof)
}

func (s *ramStore) loadUploadProofs(ctx context.Context, root uint64) ([]*faiStatePermit, *contentsLease, error) {
	proofs, err := s.durable.store.LoadMAPIUploadProofs(ctx, s.durable.mailbox, s.replica, folderGlobalCount(root))
	if err != nil || len(proofs) == 0 {
		return nil, nil, err
	}
	if len(proofs) > maxFAIStatePermits {
		return nil, nil, core.ErrInvalid
	}
	budget := 0
	for _, proof := range proofs {
		budget += 512 + 6*(len(proof.Before)+len(proof.ReadBefore)+len(proof.NormalBefore)) + 8*len(proof.Created)
	}
	lease, err := s.reserveContentsMemory(budget)
	if err != nil {
		return nil, nil, err
	}
	accepted := make([]*faiStatePermit, 0, len(proofs))
	for _, proof := range proofs {
		p := &faiStatePermit{root: root, afterKey: proof.After, readAfterKey: proof.ReadAfter, created: proof.Created}
		p.before, err = parseIDSet(proof.Before)
		if err == nil && proof.ReadAfter != ([32]byte{}) {
			p.readBound = true
			p.readBefore, err = parseIDSet(proof.ReadBefore)
		}
		if err == nil && proof.NormalAfter != ([32]byte{}) {
			p.normalAfterKey = proof.NormalAfter
			p.normalBefore, err = parseIDSet(proof.NormalBefore)
		}
		if err != nil {
			lease.release()
			return nil, nil, err
		}
		accepted = append(accepted, p)
	}
	return accepted, lease, nil
}

func (s *ramStore) validateContentsCheckpointRoot(ctx context.Context, scope [32]byte, state contentsSyncState, root uint64) error {
	s.contentsProofsMu.Lock()
	defer s.contentsProofsMu.Unlock()
	visited := map[[32]byte]bool{}
	var lookupErr error
	var permits []*faiStatePermit
	var proofLease *contentsLease
	defer func() { proofLease.release() }()
	loaded := false
	var valid func(contentsSyncState) bool
	valid = func(candidate contentsSyncState) bool {
		if lookupErr != nil {
			return false
		}
		key, empty, err := contentsCheckpointKey(scope, candidate)
		if err != nil {
			return false
		}
		if empty {
			return true
		}
		// Each permit can require both the unchanged-Given and the
		// newly-created-Given predecessor candidate. Bound both branches.
		if visited[key] || len(visited) > 2*maxFAIStatePermits {
			return false
		}
		visited[key] = true
		if err := s.validateCompletedContentsProof(ctx, key); err == nil {
			return true
		} else if !errors.Is(err, core.ErrResync) {
			lookupErr = err
			return false
		}
		raw, err := candidate.seenFAI.marshal()
		if err != nil {
			return false
		}
		afterKey := sha256.Sum256(raw)
		if !loaded {
			loaded = true
			if s.durable.store != nil {
				permits, proofLease, lookupErr = s.loadUploadProofs(ctx, root)
				if lookupErr != nil {
					return false
				}
			}
			for _, p := range s.faiUploadProofs {
				if p.root == root {
					permits = append(permits, p)
				}
			}
		}
		for _, p := range permits {
			if p.afterKey != afterKey {
				continue
			}
			earlier := candidate
			earlier.seenFAI = p.before
			if p.readBound {
				read, err := candidate.read.marshal()
				if err != nil || sha256.Sum256(read) != p.readAfterKey {
					continue
				}
				earlier.read = p.readBefore
			}
			if p.normalAfterKey != ([32]byte{}) {
				seen, err := candidate.seen.marshal()
				if err != nil || sha256.Sum256(seen) != p.normalAfterKey {
					continue
				}
				earlier.seen = p.normalBefore
			}
			// MS-OXCFXICS 3.3.5.8.7: a client can add new imported MIDs to
			// Given. Strip only IDs actually created by this published grant,
			// then require the same earlier checkpoint, scope and root.
			given, changed := withoutCreatedGiven(candidate.given, s.replica, p.created)
			if changed {
				earlier.given = given
				if valid(earlier) {
					return true
				}
				earlier.given = candidate.given
			}
			if valid(earlier) {
				return true
			}
		}
		return false
	}
	if valid(state) {
		return nil
	}
	if lookupErr != nil {
		return lookupErr
	}
	return core.ErrResync
}

// Owns one upload state and reserves a future published SeenFAI transformation
// before the first mutation. Native read-state writes require the separate transport capability.
type faiCollector struct {
	mu              sync.Mutex
	state           *contentsState
	baseline        idSet
	baselineRead    idSet
	baselineNormal  idSet
	readImported    bool
	grant           *faiStatePermit
	created         []uint64
	changes         int
	started, closed bool
}

func (*faiCollector) String() string   { return "faiCollector{private}" }
func (*faiCollector) GoString() string { return "faiCollector{private}" }
func newFAICollector(ctx context.Context, session core.Session, store *ramStore, root uint64) (*faiCollector, error) {
	c := contentsConfigure{frame: synchronizationConfigure{kind: 1, sendOptions: 1, flags: 0x19}}
	plan, err := compileContentsFilter(c, &store.named, contentsProjectionSchema(false, true))
	if err != nil {
		return nil, err
	}
	state, err := newContentsState(ctx, session, store, root, c, plan)
	if err != nil {
		return nil, err
	}
	state.collector = true
	return &faiCollector{state: state}, nil
}
func (c *faiCollector) close() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return
	}
	c.closed = true
	c.state.store.finishFAIStatePermit(c.grant, false)
	c.grant = nil
	c.baseline = idSet{}
	c.baselineRead = idSet{}
	c.baselineNormal = idSet{}
	c.created = nil
	c.state.close()
}

// Caller holds collector.mu across its actual Save/Delete and acknowledge.
func (c *faiCollector) prepareMutation(ctx context.Context, session core.Session, count int) error {
	if c.closed {
		return core.ErrClosed
	}
	if count < 1 || count > maxFAICollectorChanges-c.changes {
		return core.ErrUnsupported
	}
	if err := c.state.authorize(ctx, session); err != nil {
		return err
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if c.state.closed || c.state.pending != nil {
		return core.ErrConflict
	}
	if !c.started {
		ranges := 0
		for _, r := range c.state.value.seenFAI.replicas {
			ranges += len(r.ranges)
		}
		if ranges > maxIDSetRanges-maxFAICollectorChanges {
			return core.ErrUnsupported
		}
		c.baseline = cloneIDSet(c.state.value.seenFAI)
		c.baselineRead = cloneIDSet(c.state.value.read)
		c.baselineNormal = cloneIDSet(c.state.value.seen)
	}
	if c.grant == nil {
		if err := c.reserveGrant(); err != nil {
			return err
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	c.started = true
	c.state.started = true
	return nil
}

func (c *faiCollector) reserveGrant() error {
	grant, err := c.state.store.reserveFAIStatePermit(c.state.root, c.baseline)
	if err != nil {
		return err
	}
	if c.readImported {
		grant.readBound, grant.readBefore = true, cloneIDSet(c.baselineRead)
	}
	grant.normalBefore = cloneIDSet(c.baselineNormal)
	c.grant = grant
	return nil
}
func (c *faiCollector) acknowledge(counters []uint64) {
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	ranges := contentsRanges(c.state.value.seenFAI)
	for _, cn := range counters {
		ranges = append(ranges, idRange{cn, cn})
	}
	c.state.value.seenFAI = singleReplicaSet(c.state.store.replica, ranges)
	c.changes += len(counters)
}
func (c *faiCollector) save(ctx context.Context, session core.Session, draft *faiDraft, flags byte) (messageRow, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if draft.store != c.state.store || draft.root != c.state.root {
		return messageRow{}, core.ErrAccessDenied
	}
	if err := c.prepareMutation(ctx, session, 1); err != nil {
		return messageRow{}, err
	}
	row, created, downloadRequired, err := draft.saveWithCreation(ctx, session, flags)
	if err != nil {
		return messageRow{}, err
	}
	if downloadRequired {
		// The client uploaded the losing version and still needs the winner.
		c.changes++
	} else {
		c.acknowledge([]uint64{row.cn})
	}
	if created {
		c.created = append(c.created, folderGlobalCount(row.mid))
	}
	return row, nil
}
func (c *faiCollector) delete(ctx context.Context, session core.Session, request faiDeleteRequest) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.prepareMutation(ctx, session, len(request.sources)); err != nil {
		return err
	}
	var native core.MailMessageSoftDeleter
	var contacts core.ContactSoftDeleter
	if request.flags == 0 {
		native, _ = session.Mailbox().(core.MailMessageSoftDeleter)
		contacts, _ = session.Mailbox().(core.ContactSoftDeleter)
	}
	changes, err := c.state.store.deleteContentsChanges(ctx, session, c.state.root, request, native, contacts)
	if native != nil || contacts != nil {
		// A successful prefix can survive a later native or SQLite failure.
		// Charge the entire request while leaving uncommitted ICS state alone.
		c.changes += len(request.sources) - len(changes)
	}
	if err != nil {
		return err
	}
	c.acknowledge(changes)
	return nil
}

// Transfers ownership of a frozen grant to a state-only source. The caller must
// publish it only with that source's final successful GetBuffer response.
func (c *faiCollector) transferState(ctx context.Context, session core.Session) (contentsSyncState, *faiStatePermit, transferStateStage, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return contentsSyncState{}, nil, transferStateClosed, core.ErrClosed
	}
	if err := c.state.authorize(ctx, session); err != nil {
		return contentsSyncState{}, nil, transferStateAuthorization, err
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if c.state.pending != nil {
		return contentsSyncState{}, nil, transferStatePending, core.ErrConflict
	}
	if c.started && c.grant == nil {
		if err := c.reserveGrant(); err != nil {
			return contentsSyncState{}, nil, transferStateCollectorState, err
		}
	}
	if c.grant != nil {
		if err := c.state.store.freezeUploadStatePermit(c.grant, c.state.value); err != nil {
			return contentsSyncState{}, nil, transferStateCollectorState, err
		}
		c.grant.created = append([]uint64(nil), c.created...)
	}
	// Given is ignored on upload and is absent from the upload final state.
	value := cloneContentsState(c.state.value)
	value.given = idSet{}
	grant := c.grant
	c.grant = nil
	return value, grant, 0, nil
}

// Bounded point subtraction for verified client-reserved or committed IDs.
func withoutCreatedGiven(given idSet, replica [16]byte, created []uint64) (idSet, bool) {
	if len(given.replicas) != 1 || given.replicas[0].replica != replica || len(created) == 0 || len(created) > maxFAICollectorChanges {
		return idSet{}, false
	}
	ranges := append([]idRange(nil), given.replicas[0].ranges...)
	changed := false
	for _, point := range created {
		var next []idRange
		for _, r := range ranges {
			if point < r.low || point > r.high {
				next = append(next, r)
			} else {
				changed = true
				if r.low < point {
					next = append(next, idRange{r.low, point - 1})
				}
				if point < r.high {
					next = append(next, idRange{point + 1, r.high})
				}
			}
			if len(next) > maxIDSetRanges {
				return idSet{}, false
			}
		}
		ranges = next
	}
	return singleReplicaSet(replica, ranges), changed
}
