// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"math"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"ewsgo/internal/core"
)

const (
	streamLockTimeout = 30 * time.Second
	maxStreamLocks    = 256 // Per mailbox, including expired locks awaiting release.
	streamLockLost    = 0x80040115
)

var streamLockClock = time.Now()

type streamLockSession struct{ activity atomic.Int64 }

func (s *streamLockSession) touch() { s.activity.Store(int64(time.Since(streamLockClock))) }

// Native streams contain immutable encodings of one MIME revision. The key is
// independent of the context's stream handles and of the item's current FID.
type streamLockKey struct {
	mid                       uint64
	revision                  core.Integer
	tag, attachment, codePage uint32
}

type streamRegion struct {
	offset, size uint64
	flags        uint32
}

func (r streamRegion) overlaps(other streamRegion) bool {
	return r.offset < other.offset+other.size && other.offset < r.offset+r.size
}

type streamRegionLock struct {
	region  streamRegion
	stream  *propertyStream
	expired bool
}

type streamLocks struct {
	mu      sync.Mutex
	regions []streamRegionLock
}

func (s *propertyStream) adjustRegionLock(operation byte, region streamRegion) uint32 {
	if s.locks == nil {
		return ropNotImplemented
	}
	if region.size == 0 || region.size > math.MaxUint64-region.offset {
		return 0x80070057
	}
	table := s.locks
	table.mu.Lock()
	defer table.mu.Unlock()

	if operation == ropUnlockRegionStream {
		table.regions = slices.DeleteFunc(table.regions, func(lock streamRegionLock) bool {
			return lock.stream.lockKey == s.lockKey && lock.stream.lockOwner == s.lockOwner && lock.region == region
		})
		return 0
	}
	now := time.Since(streamLockClock)
	for i := range table.regions {
		lock := &table.regions[i]
		if lock.stream.lockKey != s.lockKey || !lock.region.overlaps(region) {
			continue
		}
		if lock.stream.lockOwner == s.lockOwner && lock.expired {
			return streamLockLost
		}
		if now-time.Duration(lock.stream.lockOwner.activity.Load()) > streamLockTimeout {
			lock.expired = true // Immutable streams have no writes to discard.
		}
		if !lock.expired {
			return accessDenied
		}
	}
	if len(table.regions) >= maxStreamLocks {
		return 0x8007000e
	}
	table.regions = append(table.regions, streamRegionLock{region: region, stream: s})
	return 0
}

func (s *propertyStream) checkReadLock(offset, size int64) uint32 {
	if s.locks == nil {
		return 0
	}
	region := streamRegion{offset: uint64(offset), size: uint64(size)}
	s.locks.mu.Lock()
	defer s.locks.mu.Unlock()
	var code uint32
	for _, lock := range s.locks.regions {
		if lock.stream.lockKey != s.lockKey || !lock.region.overlaps(region) {
			continue
		}
		if lock.stream.lockOwner == s.lockOwner && lock.expired {
			return streamLockLost
		}
		if lock.stream.lockOwner != s.lockOwner && !lock.expired && lock.region.flags != 1 {
			code = accessDenied
		}
	}
	return code
}

func (t *streamLocks) release(s *propertyStream) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.regions = slices.DeleteFunc(t.regions, func(lock streamRegionLock) bool { return lock.stream == s })
}
