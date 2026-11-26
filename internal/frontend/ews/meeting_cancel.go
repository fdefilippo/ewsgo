// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"strings"

	"ewsgo/internal/core"
)

func (h *Handler) cancelMeetings(ctx context.Context, op node, session core.Session) []node {
	const operation = "CreateItem"
	list := op.child(messages, "Items")
	if !validBatch(list) || len(op.Children) != 1 {
		return syncError(operation, core.ErrInvalid)
	}
	if op.attr("MessageDisposition") != "SendAndSaveCopy" || op.attr("SendMeetingInvitations") != "" {
		return syncError(operation, core.ErrUnsupported)
	}
	replies := make([]node, 0, len(list.Children))
	for _, card := range list.Children {
		if !isElement(card, types, "CancelCalendarItem") {
			replies = append(replies, writeFailure(operation, core.ErrUnsupported, "ErrorItemNotFound"))
			continue
		}
		var cancellation core.MeetingCancellation
		ref := card.child(types, "ReferenceItemId")
		body := card.child(types, "NewBodyContent")
		expected := 1
		if body != nil {
			expected++
			cancellation.Body = body.Text
			cancellation.HTML = body.attr("BodyType") == "HTML"
			if len(body.Children) != 0 || !cancellation.HTML && body.attr("BodyType") != "Text" {
				replies = append(replies, failure(operation, "ErrorInvalidRequest"))
				continue
			}
		}
		if ref == nil || len(ref.Children) != 0 || len(card.Children) != expected {
			replies = append(replies, failure(operation, "ErrorInvalidRequest"))
			continue
		}
		replies = append(replies, h.cancelMeeting(ctx, session, *ref, cancellation))
	}
	return replies
}

func (h *Handler) cancelMeeting(ctx context.Context, session core.Session, item node, cancellation core.MeetingCancellation) node {
	const operation = "CreateItem"
	box := session.Mailbox()
	reader, readable := box.(core.AppointmentReader)
	backend, writable := box.(core.MeetingCancellationBackend)
	if h.journal == nil || !readable || !writable {
		return writeFailure(operation, core.ErrUnsupported, "ErrorItemNotFound")
	}
	scope := session.Identity().Scope
	ref, err := h.decodeRef(scope, item.attr("Id"))
	if err != nil || !strings.HasPrefix(string(ref.ID), core.AppointmentIDPrefix) {
		return failure(operation, "ErrorItemNotFound")
	}
	prior := item.attr("ChangeKey")
	if prior == "" {
		return failure(operation, "ErrorChangeKeyRequiredForWriteOperations")
	}
	revision, err := h.decodeRef(scope, prior)
	if err != nil || revision.ID != "revision:"+ref.ID {
		return failure(operation, "ErrorInvalidChangeKey")
	}
	raw, err := json.Marshal(cancellation)
	if err != nil {
		return writeFailure(operation, core.ErrInvalid, "ErrorItemNotFound")
	}
	mailbox, err := h.journal.store.BindMailbox(ctx, scope, h.journal.backendInstance)
	if err != nil {
		return writeFailure(operation, err, "ErrorItemNotFound")
	}
	key := sha256.Sum256([]byte("meeting cancellation\x00" + string(ref.ID) + "\x00" + prior))
	_, err = h.submitMeetingRequest(ctx, mailbox.MailboxID, key, sha256.Sum256(raw), func(ctx context.Context) (core.SendAttempt, error) {
		current, err := reader.GetAppointment(ctx, ref.Parent, ref.ID)
		if err != nil {
			return nil, err
		}
		if current.Identity.ID != ref.ID || current.Identity.Parent != ref.Parent || current.Identity.Kind != core.AppointmentItem {
			return nil, core.ErrNotFound
		}
		if h.appointmentChangeKey(scope, current) != prior {
			return nil, core.ErrConflict
		}
		return backend.StageMeetingCancellation(ctx, current, cancellation)
	})
	if errors.Is(err, core.ErrSendRejected) {
		return failure(operation, "ErrorMessageSubmissionBlocked")
	}
	if err != nil {
		return writeFailure(operation, err, "ErrorItemNotFound")
	}
	return success(operation)
}
