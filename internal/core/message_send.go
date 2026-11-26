// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"errors"
	"regexp"
)

var sendIntentID = regexp.MustCompile(`^[a-z2-7]{26}$`)

func ValidSendIntentID(id string) bool { return sendIntentID.MatchString(id) }

// SendState describes what the gateway knows about a submission. A confirmed
// state records backend acceptance. Delivery to a recipient is outside it.
type SendState uint8

const (
	SendReady SendState = iota + 1
	SendUncertain
	SendConfirmed
	SendFailed
)

func (s SendState) String() string {
	switch s {
	case SendReady:
		return "ready"
	case SendUncertain:
		return "uncertain"
	case SendConfirmed:
		return "confirmed"
	case SendFailed:
		return "failed"
	default:
		return "unknown"
	}
}

// SendInput identifies the exact bytes and optional draft revision the caller
// prepared. The body, recipients and credentials stay outside the registry.
type SendInput struct {
	MIMEDigest [32]byte
	DraftID    ID
	Revision   Revision
}

type SendIntent struct {
	ID     string
	Input  SendInput
	State  SendState
	SentID ID // optional native receipt, available only after confirmation
}

// ErrSendRejected denotes a backend response proving that it did not accept
// the submission. Other submission errors leave the outcome uncertain.
var ErrSendRejected = errors.New("send rejected before acceptance")

var ErrInvalidSendRecipients = errors.New("invalid submission recipients")

type SendAttempt func(context.Context) (ID, error)

// MailSendBackend stages a submission without sending it. An upload may be
// temporary. The caller must durably claim the intent before calling the send
// attempt. The intent ID is stable and may serve as a backend correlation key.
type MailSendBackend interface {
	StageMailSend(context.Context, SendIntent, []byte) (SendAttempt, error)
	ObserveMailSend(context.Context, SendIntent) (SendObservation, error)
}

type SendDraftObservation uint8

const (
	SendDraftUnknown SendDraftObservation = iota
	SendDraftUnchanged
	SendDraftChanged
	SendDraftMissing
)

// SendObservation contains advisory evidence. Observations do not modify the
// intent state or authorize another dispatch.
type SendObservation struct {
	SentCandidate ID
	Draft         SendDraftObservation
}

// MailMIMESender submits a message identified by its RFC 5322 Message-ID.
// The supplied bytes include the complete MIME as received from the client.
type MailMIMESender interface {
	SendMIME(ctx context.Context, account Session, messageID string, mime []byte) (SendObservation, error)
}

// MailSendByMessageIDObserver looks for advisory evidence for a submission
// that used a caller-supplied Message-ID.
type MailSendByMessageIDObserver interface {
	ObserveMailSendByMessageID(ctx context.Context, messageID string) (SendObservation, error)
}
