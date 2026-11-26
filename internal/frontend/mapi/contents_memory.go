// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"sync"

	"ewsgo/internal/core"
)

const maxStoreContentsMemory = 16 << 20
const maxGlobalContentsMemory = 32 << 20

// Shared by the transport, not one budget per potential mailbox. All future
// FAI/staging/snapshot/stream allocations must reserve against this same ledger.
// Bytes are conservative accounting units, not a measurement of Go heap/RSS.
type contentsMemory struct {
	mu         sync.Mutex
	used       int
	stores     map[*ramStore]int
	peaks      int
	storePeaks map[*ramStore]int
	changed    chan struct{}
}
type contentsLease struct {
	budget *contentsMemory
	store  *ramStore
	bytes  int  // Protected by budget.mu. Zero after release.
	peak   bool // Temporary workspace; converted to retained bytes by shrink.
}

func (*contentsMemory) String() string   { return "contentsMemory{private}" }
func (*contentsMemory) GoString() string { return "contentsMemory{private}" }
func (*contentsLease) String() string    { return "contentsLease{private}" }
func (*contentsLease) GoString() string  { return "contentsLease{private}" }

func (s *ramStore) reserveContentsMemory(bytes int) (*contentsLease, error) {
	if s.allocator == nil {
		return nil, core.ErrUnsupported
	}
	return s.allocator.contents.reserve(s, bytes)
}
func (b *contentsMemory) reserve(store *ramStore, bytes int) (*contentsLease, error) {
	if store == nil || bytes < 0 {
		return nil, core.ErrInvalid
	}
	if bytes == 0 {
		return nil, nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.reserveLocked(store, bytes)
}

func (b *contentsMemory) reserveLocked(store *ramStore, bytes int) (*contentsLease, error) {
	if bytes > maxStoreContentsMemory-b.stores[store] || bytes > maxGlobalContentsMemory-b.used {
		return nil, contentsBudgetError{}
	}
	if b.stores == nil {
		b.stores = map[*ramStore]int{}
	}
	b.used += bytes
	b.stores[store] += bytes
	return &contentsLease{budget: b, store: store, bytes: bytes}, nil
}

// Wait only for admitted temporary workspaces that can make room. Retained
// objects never require a future client Release to complete this admission.
// Call before taking source/registry locks; shrink ends the peak phase even
// when the resulting Table or transfer remains open.
func (s *ramStore) reserveContentsPeak(ctx context.Context, bytes int) (*contentsLease, error) {
	if s.allocator == nil || bytes <= 0 {
		return s.reserveContentsMemory(bytes)
	}
	b := &s.allocator.contents
	for {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		b.mu.Lock()
		lease, err := b.reserveLocked(s, bytes)
		if err == nil {
			lease.peak = true
			b.peaks += bytes
			if b.storePeaks == nil {
				b.storePeaks = map[*ramStore]int{}
			}
			b.storePeaks[s] += bytes
			b.mu.Unlock()
			return lease, nil
		}
		// If even releasing every workspace cannot satisfy either quota, fail
		// promptly. Never turn a retained-object quota into an indefinite wait.
		if bytes > maxStoreContentsMemory-(b.stores[s]-b.storePeaks[s]) || bytes > maxGlobalContentsMemory-(b.used-b.peaks) {
			b.mu.Unlock()
			return nil, err
		}
		if b.changed == nil {
			b.changed = make(chan struct{})
		}
		changed := b.changed
		b.mu.Unlock()
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-changed:
		}
	}
}

func (l *contentsLease) endPeakLocked() {
	if l.peak {
		l.budget.peaks -= l.bytes
		l.budget.storePeaks[l.store] -= l.bytes
		if l.budget.storePeaks[l.store] == 0 {
			delete(l.budget.storePeaks, l.store)
		}
		l.peak = false
	}
}
func (b *contentsMemory) notifyLocked() {
	if b.changed != nil {
		close(b.changed)
		b.changed = nil
	}
}
func (l *contentsLease) release() {
	if l == nil {
		return
	}
	l.budget.mu.Lock()
	defer l.budget.mu.Unlock()
	l.endPeakLocked()
	l.budget.used -= l.bytes
	l.budget.stores[l.store] -= l.bytes
	l.bytes = 0
	if l.budget.stores[l.store] == 0 {
		delete(l.budget.stores, l.store)
	}
	l.budget.notifyLocked()
}
func (l *contentsLease) size() int {
	if l == nil {
		return 0
	}
	l.budget.mu.Lock()
	defer l.budget.mu.Unlock()
	return l.bytes
}

// Shrink a peak reservation after temporary copies become unreachable, without
// a release/reacquire gap that would leave retained buffers unaccounted for.
func (l *contentsLease) shrink(bytes int) error {
	if l == nil {
		if bytes == 0 {
			return nil
		}
		return core.ErrInvalid
	}
	l.budget.mu.Lock()
	defer l.budget.mu.Unlock()
	if bytes < 0 || bytes > l.bytes {
		return core.ErrInvalid
	}
	l.endPeakLocked()
	difference := l.bytes - bytes
	l.budget.used -= difference
	l.budget.stores[l.store] -= difference
	l.bytes = bytes
	if l.budget.stores[l.store] == 0 {
		delete(l.budget.stores, l.store)
	}
	l.budget.notifyLocked()
	return nil
}
