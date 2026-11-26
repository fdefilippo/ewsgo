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
