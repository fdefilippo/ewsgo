// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/storage/sqlite"
)

// MailSender coordinates submissions for an authenticated mailbox. The caller
// retains the receipt and uses it for any retry or status request.
type MailSender struct {
	store           *sqlite.Store
	backendInstance string
}

func NewMailSender(store *sqlite.Store, backendInstance string) *MailSender {
	return &MailSender{store: store, backendInstance: backendInstance}
}

func (s *MailSender) Prepare(ctx context.Context, account core.Session, draft core.ID, revision core.Revision, mime []byte) (_ core.SendIntent, err error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "core", Operation: "PrepareMailSend", Result: core.ActivityOutcome(err), Duration: time.Since(started)})
	}()
	if len(mime) == 0 {
		return core.SendIntent{}, core.ErrInvalid
	}
	if _, ok := account.Mailbox().(core.MailSendBackend); !ok {
		return core.SendIntent{}, core.ErrUnsupported
	}
	mailbox, err := s.store.BindMailbox(ctx, account.Identity().Scope, s.backendInstance)
	if err != nil {
		return core.SendIntent{}, err
	}
	return s.store.PrepareSend(ctx, mailbox.MailboxID, core.SendInput{MIMEDigest: sha256.Sum256(mime), DraftID: draft, Revision: revision})
}

func (s *MailSender) SendMIME(ctx context.Context, account core.Session, messageID string, mime []byte) (_ core.SendObservation, err error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "core", Operation: "SendMIME", Result: core.ActivityOutcome(err), Duration: time.Since(started)})
	}()
	if len(mime) == 0 || !core.ValidInternetMessageID(messageID) {
		return core.SendObservation{}, core.ErrInvalid
	}
	if _, ok := account.Mailbox().(core.MailSendBackend); !ok {
		return core.SendObservation{}, core.ErrUnsupported
	}
	mailbox, err := s.store.BindMailbox(ctx, account.Identity().Scope, s.backendInstance)
	if err != nil {
		return core.SendObservation{}, err
	}
	input := core.SendInput{MIMEDigest: sha256.Sum256(mime)}
	key := sha256.Sum256([]byte(messageID))
	intent, err := s.store.PrepareCorrelatedSend(ctx, mailbox.MailboxID, input, key)
	if err != nil {
		return core.SendObservation{}, err
	}
	_, err = s.submit(ctx, account, intent.ID, input.MIMEDigest, core.StripDraftReferenceHeader(mime))
	var evidence core.SendObservation
	if observer, ok := account.Mailbox().(core.MailSendByMessageIDObserver); ok && errors.Is(err, core.ErrOutcomeUnknown) {
		var observeErr error
		started := time.Now()
		evidence, observeErr = observer.ObserveMailSendByMessageID(ctx, messageID)
		core.RecordActivity(ctx, core.Activity{Layer: "core", Operation: "ObserveMailSend", Result: core.ActivityOutcome(observeErr), Duration: time.Since(started)})
	}
	return evidence, err
}

func (s *MailSender) Submit(ctx context.Context, account core.Session, id string, mime []byte) (core.SendIntent, error) {
	return s.submit(ctx, account, id, sha256.Sum256(mime), mime)
}

func (s *MailSender) submit(ctx context.Context, account core.Session, id string, digest [32]byte, mime []byte) (_ core.SendIntent, err error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "core", Operation: "SubmitMailSend", Result: core.ActivityOutcome(err), Duration: time.Since(started)})
	}()
	mailbox, err := s.store.BindMailbox(ctx, account.Identity().Scope, s.backendInstance)
	if err != nil {
		return core.SendIntent{}, err
	}
	intent, err := s.store.SendIntent(ctx, mailbox.MailboxID, id)
	if err != nil {
		return core.SendIntent{}, err
	}
	if intent.Input.MIMEDigest != digest {
		return core.SendIntent{}, core.ErrConflict
	}
	if intent.State != core.SendReady {
		return intent, sendStateError(intent.State)
	}
	backend, ok := account.Mailbox().(core.MailSendBackend)
	if !ok {
		return intent, core.ErrUnsupported
	}
	attempt, err := backend.StageMailSend(ctx, intent, mime)
	if err != nil {
		return intent, err
	}
	if attempt == nil {
		return intent, core.ErrInvalid
	}
	intent, claimed, err := s.store.ClaimSend(ctx, mailbox.MailboxID, id, intent.Input)
	if err != nil {
		return intent, err
	}
	if !claimed {
		return intent, sendStateError(intent.State)
	}

	var sentID core.ID
	if err = ctx.Err(); err != nil {
		err = errors.Join(core.ErrSendRetryable, err)
	} else {
		sentID, err = attempt(ctx)
	}
	state := core.SendConfirmed
	switch {
	case errors.Is(err, core.ErrOutcomeUnknown) || sentID != "" && err != nil:
		return intent, core.ErrOutcomeUnknown
	case errors.Is(err, core.ErrSendRetryable):
		state, sentID = core.SendReady, ""
	case errors.Is(err, core.ErrSendRejected):
		state, sentID = core.SendFailed, ""
	case err != nil:
		return intent, core.ErrOutcomeUnknown
	}
	// Persist an acknowledgment even if the caller disconnected while the
	// upstream request was in flight.
	commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.store.CompleteSend(commitCtx, mailbox.MailboxID, id, state, sentID); err != nil {
		return intent, core.ErrOutcomeUnknown
	}
	intent.State, intent.SentID = state, sentID
	if state == core.SendReady {
		return intent, err
	}
	return intent, sendStateError(state)
}

func (s *MailSender) Status(ctx context.Context, account core.Session, id string) (_ core.SendIntent, _ core.SendObservation, err error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "core", Operation: "ObserveMailSend", Result: core.ActivityOutcome(err), Duration: time.Since(started)})
	}()
	mailbox, err := s.store.BindMailbox(ctx, account.Identity().Scope, s.backendInstance)
	if err != nil {
		return core.SendIntent{}, core.SendObservation{}, err
	}
	intent, err := s.store.SendIntent(ctx, mailbox.MailboxID, id)
	if err != nil || intent.State != core.SendUncertain {
		return intent, core.SendObservation{}, err
	}
	backend, ok := account.Mailbox().(core.MailSendBackend)
	if !ok {
		return intent, core.SendObservation{}, core.ErrUnsupported
	}
	observed, err := backend.ObserveMailSend(ctx, intent)
	return intent, observed, err
}

func sendStateError(state core.SendState) error {
	switch state {
	case core.SendConfirmed:
		return nil
	case core.SendFailed:
		return core.ErrSendRejected
	default:
		return core.ErrOutcomeUnknown
	}
}
