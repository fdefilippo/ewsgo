// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"crypto/rand"
	"encoding/hex"
	"sync"
	"time"

	"ewsgo/internal/core"
)

const (
	contextCookie    = "EWSGoContext"
	sequenceCookie   = "EWSGoSequence"
	sessionTTL       = 5 * time.Minute
	maxContexts      = 1024
	maxScopeContexts = 16
)

// Contexts are provisional, bounded process memory. They contain no backend
// session, credentials or durable core objects. Optional mailbox handles refer
// only to bounded gateway RAM objects. BasicTLS must
// authenticate each HTTP request before this state is consulted.
type contextState struct {
	id, sequence, endpoint         string
	scope                          core.Scope
	expires                        time.Time
	busy                           bool
	waiting                        chan struct{}
	codePage, sortLCID, stringLCID uint32
	directory                      *directoryState
	mailbox                        *mailboxObjects
}

type contexts struct {
	mu      sync.Mutex
	entries map[string]*contextState
	now     func() time.Time
}

func newContexts() *contexts {
	return &contexts{entries: make(map[string]*contextState), now: time.Now}
}

func randomCookie() string {
	var raw [32]byte
	_, _ = rand.Read(raw[:]) // crypto/rand.Read cannot return an error.
	return hex.EncodeToString(raw[:])
}

func validCookie(value string) bool {
	if len(value) != 64 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

// create atomically replaces a valid reconnect context. Sequence cookies are
// deliberately ignored on Connect/Bind, per MS-OXCMAPIHTTP 3.2.5.6.
func (s *contexts) create(scope core.Scope, endpoint, old string, connect Connect) (*contextState, uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.prune(now)
	if old != "" {
		state := s.entries[old]
		if state == nil || state.scope != scope || state.endpoint != endpoint {
			return nil, 10
		}
		if state.busy {
			return nil, 15
		}
	}
	count := 0
	for _, state := range s.entries {
		if state.scope == scope {
			count++
		}
	}
	if old == "" && (len(s.entries) >= maxContexts || count >= maxScopeContexts) {
		return nil, 1
	}
	state := &contextState{
		id: randomCookie(), sequence: randomCookie(), endpoint: endpoint,
		scope: scope, expires: now.Add(sessionTTL), busy: true,
		codePage: connect.CodePage, sortLCID: connect.SortLCID, stringLCID: connect.StringLCID,
	}
	if previous := s.entries[old]; previous != nil {
		s.remove(previous)
	}
	s.entries[state.id] = state
	return state, 0
}

func (s *contexts) acquire(scope core.Scope, endpoint, id, sequence string) (*contextState, uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.prune(now)
	state := s.entries[id]
	if state == nil || state.scope != scope || state.endpoint != endpoint {
		return nil, 10
	}
	if state.busy || state.sequence != sequence {
		return nil, 15
	}
	state.busy = true
	if state.mailbox != nil {
		state.mailbox.streamLockSession.touch()
	}
	state.sequence = randomCookie()
	return state, 0
}

func (s *contexts) finish(state *contextState, revoke bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries[state.id] != state {
		return
	}
	if revoke {
		s.remove(state)
		return
	}
	state.expires = s.now().Add(sessionTTL)
	if state.mailbox != nil {
		state.mailbox.streamLockSession.touch()
	}
	state.busy = false
}

// remove is called under s.mu with no other handle mutations in progress.
func (s *contexts) remove(state *contextState) {
	if state.waiting != nil {
		close(state.waiting)
		state.waiting = nil
	}
	if state.mailbox != nil {
		state.mailbox.close()
	}
	delete(s.entries, state.id)
}

func (s *contexts) prune(now time.Time) {
	for _, state := range s.entries {
		if !state.busy && state.waiting == nil && !now.Before(state.expires) {
			s.remove(state)
		}
	}
}

func (s *contexts) beginWait(scope core.Scope, id string) (*contextState, <-chan struct{}, uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.prune(s.now())
	state := s.entries[id]
	if state == nil || state.scope != scope || state.endpoint != "/mapi/emsmdb/" {
		return nil, nil, 10
	}
	if state.waiting != nil {
		return nil, nil, 15
	}
	if state.mailbox != nil {
		state.mailbox.streamLockSession.touch()
	}
	state.waiting = make(chan struct{})
	return state, state.waiting, 0
}

func (s *contexts) endWait(state *contextState) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries[state.id] == state {
		state.waiting = nil
		state.expires = s.now().Add(sessionTTL)
	}
}
