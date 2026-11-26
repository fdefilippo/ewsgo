// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"crypto/sha256"
	"errors"
	"strconv"
	"strings"

	"ewsgo/internal/core"
)

func meetingReplyChoice(card node) core.MeetingReplyChoice {
	if card.XMLName.Space != types {
		return 0
	}
	switch card.XMLName.Local {
	case "AcceptItem":
		return core.MeetingAccept
	case "TentativelyAcceptItem":
		return core.MeetingTentative
	case "DeclineItem":
		return core.MeetingDecline
	default:
		return 0
	}
}

func hasMeetingReplies(list *node) bool {
	if list == nil {
		return false
	}
	for _, card := range list.Children {
		if meetingReplyChoice(card) != 0 {
			return true
		}
	}
	return false
}

func (h *Handler) replyToMeetings(ctx context.Context, op node, session core.Session) []node {
	const operation = "CreateItem"
	list := op.child(messages, "Items")
	if !validBatch(list) || len(op.Children) != 1 {
		return syncError(operation, core.ErrInvalid)
	}
	disposition := op.attr("MessageDisposition")
	if disposition != "SendAndSaveCopy" && disposition != "SaveOnly" || op.attr("SendMeetingInvitations") != "" {
		return syncError(operation, core.ErrUnsupported)
	}
	replies := make([]node, 0, len(list.Children))
	for _, card := range list.Children {
		choice := meetingReplyChoice(card)
		if choice == 0 {
			replies = append(replies, writeFailure(operation, core.ErrUnsupported, "ErrorItemNotFound"))
			continue
		}
		item := card.child(types, "ReferenceItemId")
		if item == nil || len(item.Children) != 0 || len(card.Children) != 1 {
			replies = append(replies, failure(operation, "ErrorInvalidRequest"))
			continue
		}
		replies = append(replies, h.replyToMeeting(ctx, session, *item, core.MeetingReply{
			Choice: choice, NotifyOrganizer: disposition == "SendAndSaveCopy",
		}))
	}
	return replies
}

func (h *Handler) replyToMeeting(ctx context.Context, session core.Session, item node, response core.MeetingReply) node {
	const operation = "CreateItem"
	box := session.Mailbox()
	backend, writable := box.(core.MeetingReplyBackend)
	if h.journal == nil || !writable {
		return writeFailure(operation, core.ErrUnsupported, "ErrorItemNotFound")
	}
	scope := session.Identity().Scope
	ref, err := h.decodeRef(scope, item.attr("Id"))
	if err != nil || ref.Parent == "" {
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
	isCalendar := strings.HasPrefix(string(ref.ID), core.AppointmentIDPrefix)
	reader, readable := box.(core.AppointmentReader)
	if isCalendar && !readable {
		return writeFailure(operation, core.ErrUnsupported, "ErrorItemNotFound")
	}
	mailbox, err := h.journal.store.BindMailbox(ctx, scope, h.journal.backendInstance)
	if err != nil {
		return writeFailure(operation, err, "ErrorItemNotFound")
	}
	text := strconv.Itoa(int(response.Choice)) + "/" + strconv.FormatBool(response.NotifyOrganizer)
	payload := sha256.Sum256([]byte(text))
	key := sha256.Sum256([]byte("meeting reply\x00" + string(ref.ID) + "\x00" + prior + "\x00" + text))
	id, err := h.submitMeetingRequest(ctx, mailbox.MailboxID, key, payload, func(ctx context.Context) (core.SendAttempt, error) {
		var object core.Object
		kind := core.Message
		var err error
		if isCalendar {
			appointment, readErr := reader.GetAppointment(ctx, ref.Parent, ref.ID)
			object, err = appointment.Identity, readErr
			kind = core.AppointmentItem
		} else {
			object, err = box.Get(ctx, ref.ID)
		}
		if err != nil {
			return nil, err
		}
		if object.ID != ref.ID || object.Parent != ref.Parent || object.Kind != kind {
			return nil, core.ErrNotFound
		}
		if h.changeKey(scope, object) != prior {
			return nil, core.ErrConflict
		}
		return backend.StageMeetingReply(ctx, object, response)
	})
	if errors.Is(err, core.ErrSendRejected) {
		return failure(operation, "ErrorMessageSubmissionBlocked")
	}
	if err != nil {
		return writeFailure(operation, err, "ErrorItemNotFound")
	}
	if readable && response.Choice != core.MeetingDecline {
		// The response is committed even if the follow-up read fails.
		if folder, folderErr := h.calendarFolder(ctx, session, at(el(types, "DistinguishedFolderId", ""), "Id", "calendar")); folderErr == nil {
			if appointment, readErr := reader.GetAppointment(ctx, folder.ID, id); readErr == nil &&
				appointment.Identity.ID == id && appointment.Identity.Parent == folder.ID && appointment.Identity.Kind == core.AppointmentItem {
				return success(operation, el(messages, "Items", "", h.appointment(scope, appointment)))
			}
		}
	}
	return success(operation)
}
