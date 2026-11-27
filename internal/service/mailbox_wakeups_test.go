// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"ewsgo/internal/core"
)

type watchSession struct {
	core.Session
	scope  core.Scope
	source *testWatchSource
}

func (s watchSession) Identity() core.Identity { return core.Identity{Scope: s.scope} }
func (s watchSession) OpenMailboxWatch(context.Context) (core.MailboxWatch, error) {
	s.source.opens.Add(1)
	if s.source.unavailable.Load() {
		return nil, core.ErrUnsupported
	}
	s.source.active.Add(1)
	return s.source, nil
}

type testWatchSource struct {
	opens, active, calls atomic.Int32
	unavailable          atomic.Bool
	hints                chan error
}

func (w *testWatchSource) Wait(ctx context.Context) (bool, error) {
	w.calls.Add(1)
	select {
	case <-ctx.Done():
		return false, ctx.Err()
	case err := <-w.hints:
		return err == nil, err
	}
}

func (w *testWatchSource) Close(context.Context) error {
	w.active.Add(-1)
	return nil
}

func receiveHint(t *testing.T, ctx context.Context, hints <-chan struct{}, wantOpen bool) {
	t.Helper()
	select {
	case <-ctx.Done():
		t.Fatal("missing wakeup")
	case _, open := <-hints:
		if open != wantOpen {
			t.Fatalf("unexpected watch channel state: %t", open)
		}
	}
}

func TestMailboxWakeupsShareWaitAndRejoinAfterOwnerLeaves(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	hub := newMailboxWakeups()
	source := &testWatchSource{hints: make(chan error, 1)}
	session := watchSession{scope: core.Scope{Backend: "fixture", Account: "one"}, source: source}
	first, releaseFirst, err := hub.WatchMailbox(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseFirst()
	second, releaseSecond, err := hub.WatchMailbox(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseSecond()

	for range 2 {
		source.hints <- nil
		receiveHint(t, ctx, first, true)
		receiveHint(t, ctx, second, true)
	}
	if source.opens.Load() != 1 || source.active.Load() != 1 || source.calls.Load() < 2 {
		t.Fatal("clients did not share repeated waits on a single native watch")
	}
	releaseFirst()
	receiveHint(t, ctx, second, false)
	if source.active.Load() != 0 {
		t.Fatal("native watch outlived the owner")
	}
	second, releaseRejoined, err := hub.WatchMailbox(ctx, session)
	if err != nil {
		t.Fatal(err)
	}
	defer releaseRejoined()
	source.hints <- nil
	receiveHint(t, ctx, second, true)
	if source.opens.Load() != 2 || source.active.Load() != 1 {
		t.Fatal("rejoin did not establish one new native watch")
	}
}

func TestMailboxWakeupsFallbackAndBoundedFanout(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx, cancel := context.WithTimeout(t.Context(), 4*watchRetry)
		defer cancel()
		hub := newMailboxWakeups()
		source := &testWatchSource{hints: make(chan error, 1)}
		source.unavailable.Store(true)
		session := watchSession{scope: core.Scope{Backend: "fixture", Account: "one"}, source: source}
		first, release, err := hub.WatchMailbox(ctx, session)
		if err != nil {
			t.Fatal(err)
		}
		defer release()
		for range maxMailboxListeners - 1 {
			_, stop, err := hub.WatchMailbox(ctx, session)
			if err != nil {
				t.Fatal(err)
			}
			defer stop()
		}
		if _, _, err := hub.WatchMailbox(ctx, session); !errors.Is(err, core.ErrLimit) {
			t.Fatalf("excess listener: %v", err)
		}

		receiveHint(t, ctx, first, true) // Fallback while the native feed is unavailable.
		synctest.Wait()
		if source.active.Load() != 0 {
			t.Fatal("unexpected native watch during fallback")
		}

		// Each fallback hint comes before the next open attempt. Wait for
		// the worker to settle in the native wait before injecting its loss.
		source.unavailable.Store(false)
		receiveHint(t, ctx, first, true)
		synctest.Wait()
		if source.active.Load() != 1 || source.calls.Load() != 1 {
			t.Fatal("did not open the native watch")
		}
		source.hints <- core.ErrResync
		synctest.Wait()
		if source.active.Load() != 0 {
			t.Fatal("lost native watch was not closed")
		}

		receiveHint(t, ctx, first, true) // Reconciliation after losing the native feed.
		synctest.Wait()
		if source.active.Load() != 1 || source.calls.Load() != 2 {
			t.Fatal("did not recover the native watch")
		}
		source.hints <- nil
		receiveHint(t, ctx, first, true) // Wakeup from the recovered native feed.
	})
}

func TestMailboxWakeupsOwnerCancellationIsIsolated(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	hub := newMailboxWakeups()
	ownerContext, disconnect := context.WithCancel(ctx)
	defer disconnect()
	sourceA := &testWatchSource{hints: make(chan error, 1)}
	replacementA := &testWatchSource{hints: make(chan error, 1)}
	sourceB := &testWatchSource{hints: make(chan error, 1)}
	scopeA := core.Scope{Backend: "fixture", Account: "a"}
	scopeB := core.Scope{Backend: "fixture", Account: "b"}
	watch := func(ctx context.Context, scope core.Scope, source *testWatchSource) (<-chan struct{}, func()) {
		t.Helper()
		hints, stop, err := hub.WatchMailbox(ctx, watchSession{scope: scope, source: source})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(stop)
		return hints, stop
	}
	firstA, stopFirst := watch(ownerContext, scopeA, sourceA)
	secondA, _ := watch(ctx, scopeA, replacementA)
	firstB, stopB := watch(ctx, scopeB, sourceB)
	secondB, _ := watch(ctx, scopeB, sourceB)

	start := time.Now()
	sourceB.hints <- nil
	receiveHint(t, ctx, firstB, true)
	receiveHint(t, ctx, secondB, true)
	sourceA.hints <- nil
	receiveHint(t, ctx, firstA, true)
	receiveHint(t, ctx, secondA, true)
	fanout := time.Since(start)
	if sourceA.opens.Load() != 1 || sourceB.opens.Load() != 1 || replacementA.opens.Load() != 0 {
		t.Fatal("two mailbox scopes did not use one native registration each")
	}

	start = time.Now()
	disconnect()
	receiveHint(t, ctx, secondA, false)
	stopFirst()
	if sourceA.active.Load() != 0 || replacementA.active.Load() != 0 {
		t.Fatal("old native watch survived owner request")
	}
	sourceB.hints <- nil
	receiveHint(t, ctx, firstB, true)
	receiveHint(t, ctx, secondB, true)
	rejoined, stopA := watch(ctx, scopeA, replacementA)
	replacementA.hints <- nil
	receiveHint(t, ctx, rejoined, true)
	t.Logf("two-scope fanout %v, owner cancel and peer rejoin %v", fanout, time.Since(start))
	stopA()
	stopB()
	receiveHint(t, ctx, rejoined, false)
	receiveHint(t, ctx, secondB, false)
	if sourceA.opens.Load() != 1 || sourceB.opens.Load() != 1 || replacementA.opens.Load() != 1 ||
		sourceA.active.Load()+sourceB.active.Load()+replacementA.active.Load() != 0 {
		t.Fatal("unexpected native reopen or retained watch")
	}
}
