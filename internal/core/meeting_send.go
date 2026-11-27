// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"crypto/sha256"
	"errors"
	"time"
)

func MeetingInvitationKey(uid string) [32]byte {
	return sha256.Sum256([]byte("meeting invitation UID\x00" + uid))
}

type MeetingSendJournal interface {
	PrepareCorrelatedSend(context.Context, string, SendInput, [32]byte) (SendIntent, error)
	SendIntent(context.Context, string, string) (SendIntent, error)
	ClaimSend(context.Context, string, string, SendInput) (SendIntent, bool, error)
	CompleteSend(context.Context, string, string, SendState, ID) error
}

// SubmitMeetingRequest records acceptance only after a positive native reply.
func SubmitMeetingRequest(ctx context.Context, store MeetingSendJournal, mailboxID string, key, payload [32]byte,
	stage func(context.Context) (SendAttempt, error)) (ID, error) {
	input := SendInput{MIMEDigest: payload}
	intent, err := store.PrepareCorrelatedSend(ctx, mailboxID, input, key)
	if err != nil {
		return "", err
	}
	var attempt SendAttempt
	if intent.State == SendReady {
		attempt, err = stage(ctx)
		if err != nil {
			// Another caller may have completed the same operation during
			// our read of the pre-mutation revision.
			current, readErr := store.SendIntent(ctx, mailboxID, intent.ID)
			if readErr != nil || current.State == SendReady {
				return "", err
			}
		}
	}
	intent, claimed, err := store.ClaimSend(ctx, mailboxID, intent.ID, input)
	if err != nil {
		return "", err
	}
	id := intent.SentID
	if claimed {
		id, err = attempt(ctx)
		state := SendConfirmed
		if errors.Is(err, ErrSendRejected) {
			state, id = SendFailed, ""
		} else if err != nil || id == "" {
			return "", ErrOutcomeUnknown
		}
		commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := store.CompleteSend(commitCtx, mailboxID, intent.ID, state, id); err != nil {
			return "", ErrOutcomeUnknown
		}
		intent.State = state
	}
	switch intent.State {
	case SendFailed:
		return "", ErrSendRejected
	case SendConfirmed:
		return id, nil
	default:
		return "", ErrOutcomeUnknown
	}
}
