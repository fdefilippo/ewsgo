// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"slices"
	"sort"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

// MS-OXCFXICS 3.2.5.9.4.7 permits a batch limit, which must include
// the recommended upper size (3.3.5.8.12). This allocates no per-ID memory.
const maxLocalReplicaIDs = uint32(0x10000)
const maxGlobalCount = uint64(1<<48 - 1)
const ropSetLocalReplicaMidsetDeleted = 0x93

type localReplicaRange struct {
	replica [16]byte
	ids     idRange
}

func decodeLocalReplicaDeletedRanges(r *reader) ([]localReplicaRange, error) {
	size := r.take(2)
	if r.err != nil {
		return nil, r.err
	}
	payload := r.take(int(binary.LittleEndian.Uint16(size)))
	if r.err != nil || len(payload) < 4 {
		return nil, ErrMalformed
	}
	const rangeBytes = 48
	count := binary.LittleEndian.Uint32(payload)
	ranges := payload[4:]
	if count == 0 || uint64(count)*rangeBytes != uint64(len(ranges)) {
		return nil, ErrMalformed
	}
	decoded := make([]localReplicaRange, 0, count)
	for len(ranges) != 0 {
		low, high := ranges[:24], ranges[24:48]
		if !bytes.Equal(low[:16], high[:16]) || bytes.Compare(low[16:22], high[16:22]) > 0 {
			return nil, ErrMalformed
		}
		decoded = append(decoded, localReplicaRange{
			replica: [16]byte(low[:16]),
			ids:     idRange{readGlobalCount(low[16:22]), readGlobalCount(high[16:22])},
		})
		ranges = ranges[rangeBytes:]
	}
	return decoded, nil
}

func readGlobalCount(raw []byte) uint64 {
	var count uint64
	for _, b := range raw {
		count = count<<8 | uint64(b)
	}
	return count
}

// GLOBCNT is a six-byte counter with its most significant byte first.
func globalCount(n uint64) (out [6]byte) {
	for i := range out {
		out[i] = byte(n >> uint(8*(5-i)))
	}
	return out
}

func (s *ramStore) clientReservedCounterLocked(counter uint64) bool {
	return rangeCovered(s.localReservations, idRange{counter, counter})
}

func rangeCovered(ranges []idRange, target idRange) bool {
	index := sort.Search(len(ranges), func(i int) bool { return ranges[i].high >= target.high })
	return index < len(ranges) && ranges[index].low <= target.low
}

func (s *ramStore) retiredCounterLocked(counter uint64) bool {
	return rangeCovered(s.retiredMIDUnion, idRange{counter, counter})
}

// Retired counters are global claims on the allocation. Their ICS tombstones
// are visible only to clients syncing the folder in which they were reported.
func (s *ramStore) retireLocalMIDs(ctx context.Context, fid uint64, requested []localReplicaRange) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	prior := s.retiredMIDs[fid]
	next := slices.Clone(prior)
	for _, r := range requested {
		if r.replica != s.replica || r.ids.low == 0 || !rangeCovered(s.localReservations, r.ids) {
			return core.ErrInvalid
		}
		next = append(next, r.ids)
	}
	next = mergeIDRanges(next)
	if slices.Equal(next, prior) {
		return ctx.Err()
	}
	if s.retiredRangeCount-len(prior)+len(next) > maxIDSetRanges {
		return core.ErrLimit
	}
	if s.retirementConflictsLocked(next) {
		return core.ErrConflict
	}
	var data []byte
	for _, r := range next {
		low, high := globalCount(r.low), globalCount(r.high)
		data = append(data, low[:]...)
		data = append(data, high[:]...)
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.durable.store != nil {
		value := mapireplica.FolderValue{Counter: folderGlobalCount(fid), Kind: mapireplica.FolderRetiredMIDs, Data: data}
		if err := s.advanceReplicaLocked(ctx, mapireplica.Mutation{FolderValues: []mapireplica.FolderValue{value}}); err != nil {
			return err
		}
	}
	s.addRetiredMIDsLocked(fid, next)
	s.retiredMIDUnion = mergeIDRanges(slices.Concat(s.retiredMIDUnion, next))
	return nil
}

func (s *ramStore) retirementConflictsLocked(ranges []idRange) bool {
	contains := func(id uint64) bool {
		counter := folderGlobalCount(id)
		return rangeCovered(ranges, idRange{counter, counter})
	}
	for id := range s.byMID {
		if contains(id) {
			return true
		}
	}
	for id := range s.byFID {
		if contains(id) {
			return true
		}
	}
	for id := range s.pendingCreations {
		if contains(id) {
			return true
		}
	}
	for id := range s.pendingMessageImports {
		if contains(id) {
			return true
		}
	}
	for _, move := range s.messageMoves {
		if rangeCovered(ranges, idRange{move.Destination, move.Destination}) {
			return true
		}
	}
	return false
}

func (s *ramStore) addRetiredMIDsLocked(fid uint64, ranges []idRange) {
	if s.retiredMIDs == nil {
		s.retiredMIDs = make(map[uint64][]idRange)
	}
	s.retiredRangeCount += len(ranges) - len(s.retiredMIDs[fid])
	s.retiredMIDs[fid] = ranges
}

func (s *ramStore) loadRetiredMIDs(fid uint64, data []byte) error {
	if len(data) == 0 || len(data)%12 != 0 || s.retiredRangeCount+len(data)/12 > maxIDSetRanges {
		return core.ErrInvalid
	}
	ranges := make([]idRange, 0, len(data)/12)
	for i := 0; i < len(data); i += 12 {
		r := idRange{readGlobalCount(data[i : i+6]), readGlobalCount(data[i+6 : i+12])}
		if r.low == 0 || r.high < r.low || !rangeCovered(s.localReservations, r) ||
			len(ranges) != 0 && r.low <= ranges[len(ranges)-1].high+1 {
			return core.ErrInvalid
		}
		ranges = append(ranges, r)
	}
	s.addRetiredMIDsLocked(fid, ranges)
	return nil
}

func setLocalReplicaMidsetDeleted(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if object.fid == 0 {
		return fail(NotSupported)
	}
	store := object.binding.store
	store.hierarchyMu.Lock()
	defer store.hierarchyMu.Unlock()
	if _, err := store.messageAnchor(ctx, session, object.fid, true); err != nil {
		return fail(storeError(err))
	}
	if err := store.retireLocalMIDs(ctx, object.fid, op.LocalReplicaDeleted); err != nil {
		return fail(hierarchyError(err))
	}
	return fail(0)
}

// Reservations and folder mappings share one counter under the same lock.
// A reserved ID does not create an object or become an OpenFolder target.
func (s *ramStore) reserveLocalIDs(ctx context.Context, count uint32) ([6]byte, uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if ctx.Err() != nil {
		return [6]byte{}, 0x80004005
	}
	if count == 0 || count > maxLocalReplicaIDs {
		return [6]byte{}, 0x80070057
	}
	if s.next > maxGlobalCount || uint64(count) > maxGlobalCount-s.next {
		return [6]byte{}, 0x8007000e
	}
	first := s.next + 1
	merge := len(s.localReservations) > 0 && s.localReservations[len(s.localReservations)-1].high+1 == first
	if !merge && len(s.localReservations) >= maxIDSetRanges {
		return [6]byte{}, 0x8007000e
	}
	budget := s.messageRegistryBytesLocked()
	if !merge {
		budget += 16
	}
	if budget > maxMessageRegistryBytes {
		return [6]byte{}, 0x8007000e
	}
	if s.durable.store != nil {
		if _, err := s.durable.store.ReserveMAPIReplicaIDs(ctx, s.durable.mailbox, s.replicaPosition(), count); err != nil {
			return [6]byte{}, hierarchyError(err)
		}
	}
	s.next += uint64(count)
	if merge {
		s.localReservations[len(s.localReservations)-1].high = s.next
	} else {
		s.localReservations = append(s.localReservations, idRange{first, s.next})
	}
	return globalCount(first), 0
}

func getLocalReplicaIDs(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(0x7f, op.Input, code) }
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if !object.isLogon {
		return fail(NotSupported)
	}
	if op.ReplicaIDCount == 0 || op.ReplicaIDCount > maxLocalReplicaIDs {
		return fail(0x80070057)
	}
	store := object.binding.store
	if _, err := store.folder(ctx, session, object.binding.fids[MailboxRoot]); err != nil {
		return fail(storeError(err))
	}
	if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
		return fail(storeError(err))
	}
	first, code := store.reserveLocalIDs(ctx, op.ReplicaIDCount)
	if code != 0 {
		return fail(code)
	}
	out := append(fail(0), store.replica[:]...)
	return append(out, first[:]...)
}
