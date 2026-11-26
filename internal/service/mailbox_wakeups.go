// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"sync"
	"time"

	"ewsgo/internal/core"
)

const (
	maxWatchedMailboxes = 128
	maxMailboxListeners = 32
	watchRetry          = 5 * time.Second
)

type mailboxWakeups struct {
	mu     sync.Mutex
	groups map[core.Scope]*watchGroup
	retry  time.Duration
}

type watchGroup struct {
	ctx       context.Context
	cancel    context.CancelFunc
	done      chan struct{}
	listeners map[chan struct{}]struct{}
}

var _ core.MailboxWakeups = (*mailboxWakeups)(nil)

func newMailboxWakeups() *mailboxWakeups {
	return &mailboxWakeups{groups: make(map[core.Scope]*watchGroup), retry: watchRetry}
}

func (w *mailboxWakeups) WatchMailbox(ctx context.Context, session core.Session) (<-chan struct{}, func(), error) {
	scope := session.Identity().Scope
	if scope.Account == "" || scope.Backend == "" {
		return nil, nil, core.ErrInvalid
	}
	for {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		w.mu.Lock()
		group := w.groups[scope]
		if group != nil && group.ctx.Err() != nil {
			// Let the former owner destroy its native WaitSet before starting
			// another one in the same mailbox.
			w.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-group.done:
				continue
			}
		}
		owner := group == nil
		if owner {
			if len(w.groups) >= maxWatchedMailboxes {
				w.mu.Unlock()
				return nil, nil, core.ErrLimit
			}
			lifetime, cancel := context.WithCancel(ctx)
			group = &watchGroup{ctx: lifetime, cancel: cancel, done: make(chan struct{}), listeners: make(map[chan struct{}]struct{})}
			w.groups[scope] = group
		}
		if len(group.listeners) >= maxMailboxListeners {
			w.mu.Unlock()
			return nil, nil, core.ErrLimit
		}
		hints := make(chan struct{}, 1)
		group.listeners[hints] = struct{}{}
		w.mu.Unlock()

		if owner {
			go w.run(scope, session, group)
		}
		return hints, sync.OnceFunc(func() {
			w.mu.Lock()
			if _, ok := group.listeners[hints]; ok {
				delete(group.listeners, hints)
				close(hints)
			}
			w.mu.Unlock()
			if owner {
				group.cancel()
				<-group.done
			}
		}), nil
	}
}

func (w *mailboxWakeups) run(scope core.Scope, session core.Session, group *watchGroup) {
	defer func() {
		w.mu.Lock()
		defer w.mu.Unlock()
		for listener := range group.listeners {
			close(listener)
			delete(group.listeners, listener)
		}
		delete(w.groups, scope)
		close(group.done)
	}()
	source, _ := session.(core.MailboxWatcher)
	var watch core.MailboxWatch
	closeWatch := func() {
		if watch != nil {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(group.ctx), 3*time.Second)
			_ = watch.Close(ctx)
			cancel()
			watch = nil
		}
	}
	defer closeWatch()
	for group.ctx.Err() == nil {
		if watch == nil && source != nil {
			var err error
			watch, err = source.OpenMailboxWatch(group.ctx)
			if err != nil {
				closeWatch()
			}
		}
		if watch != nil {
			started := time.Now()
			changed, err := watch.Wait(group.ctx)
			if err != nil {
				closeWatch()
			} else if changed || time.Since(started) >= w.retry {
				w.pulse(group)
				continue
			}
		}
		// With no healthy blocking feed, request a bounded reconciliation.
		// An empty early response is also paced to avoid hammering the backend.
		timer := time.NewTimer(w.retry)
		select {
		case <-group.ctx.Done():
			timer.Stop()
			return
		case <-timer.C:
			w.pulse(group)
		}
	}
}

func (w *mailboxWakeups) pulse(group *watchGroup) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if group.ctx.Err() != nil {
		return
	}
	for listener := range group.listeners {
		select {
		case listener <- struct{}{}:
		default: // A pending hint already asks this listener to check.
		}
	}
}
