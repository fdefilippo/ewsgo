// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"

	"ewsgo/internal/core"
)

func (h *Handler) updateMeeting(ctx context.Context, session core.Session, ref reference, priorKey string, updates node, overwrite bool) node {
	const operation = "UpdateItem"
	box := session.Mailbox()
	reader, readable := box.(core.AppointmentReader)
	writer, writable := box.(core.MeetingDetailsWriter)
	if h.journal == nil || !readable || !writable || overwrite {
		return writeFailure(operation, core.ErrUnsupported, "ErrorItemNotFound")
	}
	patch, err := parseAppointmentDetailsPatch(updates)
	if err != nil {
		return writeFailure(operation, err, "ErrorItemNotFound")
	}
	// Equivalent UTC instants must produce the same durable request digest.
	if patch.Start != nil {
		start := patch.Start.UTC()
		patch.Start = &start
	}
	if patch.End != nil {
		end := patch.End.UTC()
		patch.End = &end
	}
	raw, err := json.Marshal(patch)
	if err != nil {
		return writeFailure(operation, core.ErrInvalid, "ErrorItemNotFound")
	}
	scope := session.Identity().Scope
	rev, err := h.decodeRef(scope, priorKey)
	if err != nil || rev.ID != "revision:"+ref.ID {
		return failure(operation, "ErrorInvalidChangeKey")
	}
	mailbox, err := h.journal.store.BindMailbox(ctx, scope, h.journal.backendInstance)
	if err != nil {
		return writeFailure(operation, err, "ErrorItemNotFound")
	}
	payload := sha256.Sum256(raw)
	key := sha256.Sum256(append([]byte("meeting update\x00"+string(ref.ID)+"\x00"+priorKey+"\x00"), payload[:]...))
	id, err := h.submitMeetingRequest(ctx, mailbox.MailboxID, key, payload, func(ctx context.Context) (core.SendAttempt, error) {
		current, err := reader.GetAppointment(ctx, ref.Parent, ref.ID)
		if err != nil {
			return nil, err
		}
		if current.Identity.ID != ref.ID || current.Identity.Parent != ref.Parent || current.Identity.Kind != core.AppointmentItem {
			return nil, core.ErrNotFound
		}
		if h.appointmentChangeKey(scope, current) != priorKey {
			return nil, core.ErrConflict
		}
		if current.Type != core.AppointmentSingle || current.AllDay || current.Cancelled {
			return nil, core.ErrUnsupported
		}
		return func(ctx context.Context) (core.ID, error) {
			saved, err := writer.UpdateMeetingDetails(ctx, current, patch)
			if err != nil {
				switch {
				case errors.Is(err, core.ErrConflict), errors.Is(err, core.ErrNotFound),
					errors.Is(err, core.ErrUnsupported), errors.Is(err, core.ErrAccessDenied), errors.Is(err, core.ErrInvalid):
					return "", core.ErrSendRejected
				default:
					return "", err
				}
			}
			if saved.Identity.ID != current.Identity.ID || saved.Identity.Parent != current.Identity.Parent ||
				saved.UID != current.UID || saved.Identity.Revision <= current.Identity.Revision {
				return "", core.ErrOutcomeUnknown
			}
			return saved.Identity.ID, nil
		}, nil
	})
	if errors.Is(err, core.ErrSendRejected) {
		return failure(operation, "ErrorMessageSubmissionBlocked")
	}
	if err != nil {
		return writeFailure(operation, err, "ErrorItemNotFound")
	}
	item, err := reader.GetAppointment(ctx, ref.Parent, id)
	if err != nil || item.Identity.ID != ref.ID || item.Identity.Parent != ref.Parent || item.Identity.Kind != core.AppointmentItem {
		return writeFailure(operation, core.ErrOutcomeUnknown, "ErrorItemNotFound")
	}
	return success(operation, el(messages, "Items", "", h.appointment(scope, item)))
}
