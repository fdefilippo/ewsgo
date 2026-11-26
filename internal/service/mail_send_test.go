// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"errors"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/storage/sqlite"
)

type sendTestSession struct {
	core.Session
	identity core.Identity
	backend  *sendTestMailbox
}

func (s sendTestSession) Identity() core.Identity { return s.identity }
func (s sendTestSession) Mailbox() core.Mailbox   { return s.backend }

type sendTestMailbox struct {
	core.Mailbox
	stage   func(context.Context, core.SendIntent, []byte) (core.SendAttempt, error)
	observe func(context.Context, core.SendIntent) (core.SendObservation, error)
}

func (m *sendTestMailbox) StageMailSend(ctx context.Context, intent core.SendIntent, mime []byte) (core.SendAttempt, error) {
	return m.stage(ctx, intent, mime)
}

func (m *sendTestMailbox) ObserveMailSend(ctx context.Context, intent core.SendIntent) (core.SendObservation, error) {
	return m.observe(ctx, intent)
}

func TestMailSenderAcrossFailuresAndRestart(t *testing.T) {
	uploadErr := errors.New("upload interrupted")
	responseErr := errors.New("upstream response lost")
	mime := []byte("To: test@example.org\r\n\r\nFixture\r\n")
	for _, tc := range []struct {
		name      string
		sendErr   error
		failStore bool
		cancel    bool
		wantState core.SendState
		wantErr   error
	}{
		{name: "retry before dispatch", wantState: core.SendConfirmed},
		{name: "client cancels after acceptance", cancel: true, wantState: core.SendConfirmed},
		{name: "rejected before acceptance", sendErr: core.ErrSendRejected, wantState: core.SendFailed, wantErr: core.ErrSendRejected},
		{name: "accepted but response lost", sendErr: responseErr, wantState: core.SendUncertain, wantErr: core.ErrOutcomeUnknown},
		{name: "accepted but local commit lost", failStore: true, wantState: core.SendUncertain, wantErr: core.ErrOutcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ctx := t.Context()
			path := filepath.Join(t.TempDir(), "mail.sqlite")
			store, err := sqlite.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			sender := NewMailSender(store, "primary")
			var staged, dispatched, observed int
			submitCtx, cancel := context.WithCancel(ctx)
			defer cancel()
			backend := &sendTestMailbox{
				stage: func(context.Context, core.SendIntent, []byte) (core.SendAttempt, error) {
					staged++
					if staged == 1 {
						return nil, uploadErr
					}
					return func(context.Context) (core.ID, error) {
						dispatched++
						if tc.failStore {
							if err := store.Close(); err != nil {
								t.Fatal(err)
							}
						}
						if tc.cancel {
							cancel()
						}
						return "m:42", tc.sendErr
					}, nil
				},
				observe: func(_ context.Context, intent core.SendIntent) (core.SendObservation, error) {
					observed++
					if intent.Input.DraftID != "m:21" || intent.Input.Revision != 9 {
						t.Fatalf("lost draft reference: %+v", intent.Input)
					}
					return core.SendObservation{SentCandidate: "m:42", Draft: core.SendDraftUnchanged}, nil
				},
			}
			account := sendTestSession{identity: core.Identity{Scope: core.Scope{Backend: "fixture", Account: "self"}}, backend: backend}
			intent, err := sender.Prepare(ctx, account, "m:21", 9, mime)
			if err != nil {
				t.Fatal(err)
			}
			got, err := sender.Submit(ctx, account, intent.ID, mime)
			if !errors.Is(err, uploadErr) || got.State != core.SendReady || dispatched != 0 {
				t.Fatalf("upload failure: %+v, %v, dispatched %d", got, err, dispatched)
			}
			got, err = sender.Submit(submitCtx, account, intent.ID, mime)
			if !errors.Is(err, tc.wantErr) || got.State != tc.wantState || dispatched != 1 {
				t.Fatalf("submit: %+v, %v, dispatched %d", got, err, dispatched)
			}
			if !tc.failStore {
				if err := store.Close(); err != nil {
					t.Fatal(err)
				}
			}
			reopened, err := sqlite.Open(ctx, path)
			if err != nil {
				t.Fatal(err)
			}
			defer reopened.Close()
			sender = NewMailSender(reopened, "primary")
			got, observation, err := sender.Status(ctx, account, intent.ID)
			if err != nil || got.State != tc.wantState {
				t.Fatalf("status after restart: %+v, %v", got, err)
			}
			if tc.wantState == core.SendUncertain && (observed != 1 || observation.SentCandidate != "m:42" || observation.Draft != core.SendDraftUnchanged || got.SentID != "") {
				t.Fatalf("uncertain evidence: %+v, %+v, observations %d", got, observation, observed)
			}
			if tc.wantState == core.SendConfirmed && got.SentID != "m:42" {
				t.Fatalf("confirmed receipt lost: %+v", got)
			}
			got, err = sender.Submit(ctx, account, intent.ID, mime)
			if !errors.Is(err, tc.wantErr) || got.State != tc.wantState || staged != 2 || dispatched != 1 {
				t.Fatalf("retry: %+v, %v, staged %d, dispatched %d", got, err, staged, dispatched)
			}
			if _, err := sender.Submit(ctx, account, intent.ID, []byte("different MIME")); !errors.Is(err, core.ErrConflict) {
				t.Fatalf("mismatched request: %v", err)
			}
			other := account
			other.identity.Scope.Account = "other"
			if _, _, err := sender.Status(ctx, other, intent.ID); !errors.Is(err, core.ErrNotFound) {
				t.Fatalf("foreign mailbox status: %v", err)
			}
		})
	}
}

func TestMailSenderConcurrentSubmissionsDispatchOnce(t *testing.T) {
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	path := filepath.Join(t.TempDir(), "mail.sqlite")
	var senders [2]*MailSender
	for i := range senders {
		store, err := sqlite.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		senders[i] = NewMailSender(store, "primary")
	}
	var staged, dispatched atomic.Int32
	ready := make(chan struct{})
	backend := &sendTestMailbox{
		stage: func(ctx context.Context, _ core.SendIntent, _ []byte) (core.SendAttempt, error) {
			if staged.Add(1) == 2 {
				close(ready)
			}
			select {
			case <-ctx.Done():
				return nil, ctx.Err()
			case <-ready:
			}
			return func(context.Context) (core.ID, error) {
				dispatched.Add(1)
				return "m:42", nil
			}, nil
		},
	}
	account := sendTestSession{identity: core.Identity{Scope: core.Scope{Backend: "fixture", Account: "self"}}, backend: backend}
	mime := []byte("To: self@example.invalid\r\n\r\nFixture\r\n")
	intent, err := senders[0].Prepare(ctx, account, "", 0, mime)
	if err != nil {
		t.Fatal(err)
	}
	results := make(chan error, len(senders))
	for _, sender := range senders {
		go func() {
			_, err := sender.Submit(ctx, account, intent.ID, mime)
			results <- err
		}()
	}
	for range senders {
		if err := <-results; err != nil && !errors.Is(err, core.ErrOutcomeUnknown) {
			t.Errorf("concurrent submit: %v", err)
		}
	}
	got, _, err := senders[1].Status(ctx, account, intent.ID)
	if err != nil || got.State != core.SendConfirmed || got.SentID != "m:42" || dispatched.Load() != 1 {
		t.Fatalf("final outcome: %+v, %v, dispatched %d", got, err, dispatched.Load())
	}
}

func TestCorrelatedMIMESendClaimsOncePerMailbox(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "correlated.sqlite")
	var gateways [2]*MailSender
	for i := range gateways {
		store, err := sqlite.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		gateways[i] = NewMailSender(store, "primary")
	}
	var attempts atomic.Int32
	backend := &sendTestMailbox{stage: func(context.Context, core.SendIntent, []byte) (core.SendAttempt, error) {
		return func(context.Context) (core.ID, error) {
			attempts.Add(1)
			return "m:42", nil
		}, nil
	}}
	account := sendTestSession{identity: core.Identity{Scope: core.Scope{Backend: "fixture", Account: "self"}}, backend: backend}
	const id = "fixture@ewsgo.invalid"
	raw := []byte("To: self@example.invalid\r\nMessage-ID: <" + id + ">\r\n\r\nFixture.\r\n")
	results := make(chan error, len(gateways))
	for _, gateway := range gateways {
		go func() {
			_, err := gateway.SendMIME(ctx, account, id, raw)
			results <- err
		}()
	}
	for range gateways {
		if err := <-results; err != nil && !errors.Is(err, core.ErrOutcomeUnknown) {
			t.Fatalf("concurrent MIME submission: %v", err)
		}
	}
	if attempts.Load() != 1 {
		t.Fatalf("expected one dispatch, got %d", attempts.Load())
	}
	if _, err := gateways[1].SendMIME(ctx, account, id, append(raw, '\n')); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("different MIME under the same ID: %v", err)
	}
	other := account
	other.identity.Scope.Account = "other"
	if _, err := gateways[1].SendMIME(ctx, other, id, raw); err != nil || attempts.Load() != 2 {
		t.Fatalf("independent mailbox submission: %v", err)
	}
}
