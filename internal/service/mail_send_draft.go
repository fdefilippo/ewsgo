// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/mail"
	"strings"
	"time"

	"ewsgo/internal/core"
)

// SendDraft submits a saved native revision and retains the original draft.
// A retry of that revision can observe the registry without dispatching again.
func (s *MailSender) SendDraft(ctx context.Context, account core.Session, draft core.Object) (_ core.SendObservation, err error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "core", Operation: "SendDraft", Result: core.ActivityOutcome(err), Duration: time.Since(started)})
	}()
	if draft.ID == "" || draft.Revision == 0 || draft.Kind != core.Message || draft.Properties[core.IsDraft] != core.Bool(true) {
		return core.SendObservation{}, core.ErrInvalid
	}
	if _, ok := account.Mailbox().(core.MailSendBackend); !ok {
		return core.SendObservation{}, core.ErrUnsupported
	}
	if len(draft.MIME) > core.MaxMailContentMIMEBytes {
		return core.SendObservation{}, core.ErrLimit
	}
	raw := core.StripDraftReferenceHeader(draft.MIME)
	messageID, err := validateDraftSubmission(raw, account.Identity().Name)
	if err != nil {
		return core.SendObservation{}, err
	}
	mailbox, err := s.store.BindMailbox(ctx, account.Identity().Scope, s.backendInstance)
	if err != nil {
		return core.SendObservation{}, err
	}
	input := core.SendInput{DraftID: draft.ID, Revision: draft.Revision, MIMEDigest: sha256.Sum256(raw)}
	key := core.DraftSendKey(draft.ID, draft.Revision)
	intent, err := s.store.PrepareCorrelatedSend(ctx, mailbox.MailboxID, input, key)
	if err != nil {
		return core.SendObservation{}, err
	}
	_, err = s.submit(ctx, account, intent.ID, input.MIMEDigest, raw)
	if !errors.Is(err, core.ErrOutcomeUnknown) {
		return core.SendObservation{}, err
	}
	_, evidence, _ := s.Status(ctx, account, intent.ID)
	if observer, ok := account.Mailbox().(core.MailSendByMessageIDObserver); ok && messageID != "" && evidence.SentCandidate == "" {
		byID, _ := observer.ObserveMailSendByMessageID(ctx, messageID)
		evidence.SentCandidate = byID.SentCandidate
	}
	return evidence, err
}

func (s *MailSender) RecoverDraftSend(ctx context.Context, account core.Session, draft core.ID, revision core.Revision) (core.SendState, error) {
	mailbox, err := s.store.BindMailbox(ctx, account.Identity().Scope, s.backendInstance)
	if err != nil {
		return 0, err
	}
	return s.store.RecoverCorrelatedDraftSend(ctx, mailbox.MailboxID, draft, revision, core.DraftSendKey(draft, revision))
}

func validateDraftSubmission(raw []byte, actor string) (string, error) {
	msg, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return "", core.ErrInvalid
	}
	for field := range msg.Header {
		// net/mail retains noncanonical names it accepts. Reject names another
		// mail parser could interpret differently before checking the sender.
		for i := range len(field) {
			if c := field[i]; c < 33 || c > 126 || c == ':' {
				return "", core.ErrInvalid
			}
		}
		if strings.HasPrefix(field, "Resent-") {
			return "", core.ErrUnsupported
		}
	}
	for _, field := range []string{"From", "Sender"} {
		values := msg.Header[field]
		if field == "Sender" && len(values) == 0 {
			continue
		}
		if len(values) != 1 {
			return "", core.ErrAccessDenied
		}
		addr, err := mail.ParseAddress(values[0])
		if err != nil || !strings.EqualFold(addr.Address, actor) {
			return "", core.ErrAccessDenied
		}
	}
	total := 0
	for _, field := range []string{"To", "Cc", "Bcc"} {
		for _, value := range msg.Header[field] {
			if strings.TrimSpace(value) == "" {
				continue
			}
			addresses, err := mail.ParseAddressList(value)
			if err != nil {
				return "", core.ErrInvalidSendRecipients
			}
			total += len(addresses)
		}
	}
	if total == 0 {
		return "", core.ErrInvalidSendRecipients
	}
	ids := msg.Header["Message-Id"]
	if len(ids) == 0 {
		return "", nil
	}
	if len(ids) != 1 {
		return "", core.ErrInvalid
	}
	id, ok := strings.CutPrefix(strings.TrimSpace(ids[0]), "<")
	id, end := strings.CutSuffix(id, ">")
	if !ok || !end || !core.ValidInternetMessageID(id) {
		return "", core.ErrInvalid
	}
	return id, nil
}
