// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/mail"
	"slices"
	"strings"
	"time"

	"ewsgo/internal/core"
)

func parseMeetingInvitation(item node) (core.MeetingInvitation, string, error) {
	meeting := core.MeetingInvitation{RequestResponses: true}
	basic := item
	basic.Children = nil
	seen := make(map[string]bool, len(item.Children))
	var uid string
	for _, field := range item.Children {
		name := field.XMLName.Local
		if field.XMLName.Space != types || seen[name] {
			return meeting, "", core.ErrInvalid
		}
		seen[name] = true
		switch name {
		case "UID":
			uid = field.Text
			if len(field.Children) != 0 || uid == "" || strings.TrimSpace(uid) != uid || len(uid) > 255 ||
				strings.ContainsAny(uid, "\r\n\x00") {
				return meeting, "", core.ErrInvalid
			}
		case "IsResponseRequested":
			value, err := xmlBool(field.Text)
			if err != nil || len(field.Children) != 0 {
				return meeting, "", core.ErrInvalid
			}
			meeting.RequestResponses = value
		case "RequiredAttendees", "OptionalAttendees":
			if len(field.Children) == 0 || len(meeting.Attendees)+len(field.Children) > core.MaxAvailabilityMailboxes {
				return meeting, "", core.ErrInvalid
			}
			for _, child := range field.Children {
				p, ok := parseMeetingAttendee(child)
				if !ok {
					return meeting, "", core.ErrInvalid
				}
				p.Optional = name == "OptionalAttendees"
				meeting.Attendees = append(meeting.Attendees, p)
			}
		default:
			basic.Children = append(basic.Children, field)
		}
	}
	itemFields, err := parseAppointmentCreate(basic)
	if err != nil {
		return meeting, "", err
	}
	if len(meeting.Attendees) == 0 || itemFields.Type != core.AppointmentSingle || itemFields.AllDay ||
		!itemFields.Start.Before(itemFields.End) || itemFields.TimeZone != "" && itemFields.TimeZone != "UTC" {
		return meeting, "", core.ErrUnsupported
	}
	itemFields.Start, itemFields.End, itemFields.TimeZone = itemFields.Start.UTC(), itemFields.End.UTC(), ""
	meeting.Appointment = itemFields
	slices.SortFunc(meeting.Attendees, func(a, b core.MeetingAttendee) int {
		return strings.Compare(strings.ToLower(a.Address), strings.ToLower(b.Address))
	})
	for i := 1; i < len(meeting.Attendees); i++ {
		if strings.EqualFold(meeting.Attendees[i-1].Address, meeting.Attendees[i].Address) {
			return meeting, "", core.ErrInvalid
		}
	}
	return meeting, uid, nil
}

func parseMeetingAttendee(attendee node) (core.MeetingAttendee, bool) {
	if !isElement(attendee, types, "Attendee") || len(attendee.Children) != 1 {
		return core.MeetingAttendee{}, false
	}
	mb := attendee.Children[0]
	if !isElement(mb, types, "Mailbox") {
		return core.MeetingAttendee{}, false
	}
	fields, ok := availabilityFields(mb, "Name", "EmailAddress", "RoutingType", "MailboxType")
	addr := availabilityText(fields["EmailAddress"])
	name := availabilityText(fields["Name"])
	routing := availabilityText(fields["RoutingType"])
	kind := availabilityText(fields["MailboxType"])
	if !ok || addr == "" || len(addr) > 320 || len(name) > 512 || strings.ContainsAny(name, "\r\n\x00") ||
		routing != "" && routing != "SMTP" || kind != "" && kind != "Mailbox" && kind != "Contact" {
		return core.MeetingAttendee{}, false
	}
	a, err := mail.ParseAddress(addr)
	if err != nil || a.Address != addr {
		return core.MeetingAttendee{}, false
	}
	return core.MeetingAttendee{Address: addr, Name: name}, true
}

func (h *Handler) sendMeetingInvitation(ctx context.Context, account core.Session, folder core.ID, card node) node {
	const op = "CreateItem"
	box := account.Mailbox()
	backend, ok := box.(core.MeetingInvitationBackend)
	reader, readable := box.(core.AppointmentReader)
	if h.journal == nil || !ok || !readable {
		return writeFailure(op, core.ErrUnsupported, "ErrorItemNotFound")
	}
	meeting, uid, err := parseMeetingInvitation(card)
	if err != nil {
		return writeFailure(op, err, "ErrorItemNotFound")
	}
	item, err := h.submitMeetingInvitation(ctx, account.Identity().Scope, backend, reader, folder, meeting, uid)
	switch {
	case errors.Is(err, core.ErrOutcomeUnknown):
		reply := failure(op, "ErrorInternalServerError")
		reply.Children = append([]node{el(messages, "MessageText", "The gateway cannot confirm this meeting invitation.")}, reply.Children...)
		return reply
	case errors.Is(err, core.ErrSendRejected):
		return failure(op, "ErrorMessageSubmissionBlocked")
	case err != nil:
		return writeFailure(op, err, "ErrorItemNotFound")
	}
	return success(op, el(messages, "Items", "", h.appointment(account.Identity().Scope, item)))
}

func (h *Handler) submitMeetingInvitation(ctx context.Context, scope core.Scope, backend core.MeetingInvitationBackend,
	reader core.AppointmentReader, folder core.ID, meeting core.MeetingInvitation, uid string) (core.Appointment, error) {
	raw, err := json.Marshal(meeting)
	if err != nil {
		return core.Appointment{}, core.ErrInvalid
	}
	payload := sha256.Sum256(raw)
	key := sha256.Sum256(append([]byte("meeting invitation content\x00"), payload[:]...))
	if uid != "" {
		key = sha256.Sum256([]byte("meeting invitation UID\x00" + uid))
	}
	store := h.journal.store
	mailbox, err := store.BindMailbox(ctx, scope, h.journal.backendInstance)
	if err != nil {
		return core.Appointment{}, err
	}
	if uid == "" {
		mac := hmac.New(sha256.New, h.key[:])
		mac.Write([]byte("meeting native UID\x00" + mailbox.MailboxID + "\x00"))
		mac.Write(key[:])
		uid = "ewsgo-" + hex.EncodeToString(mac.Sum(nil)[:16]) + "@ewsgo.invalid"
	}
	attempt, err := backend.StageMeetingInvitation(folder, meeting, uid)
	if err != nil {
		return core.Appointment{}, err
	}
	id, err := h.submitMeetingRequest(ctx, mailbox.MailboxID, key, payload, func(context.Context) (core.SendAttempt, error) {
		return attempt, nil
	})
	if err != nil {
		return core.Appointment{}, err
	}
	// Native acceptance is already durable. A read failure cannot trigger
	// another invitation.
	item, err := reader.GetAppointment(ctx, folder, id)
	if err != nil || item.Identity.ID != id || item.Identity.Parent != folder ||
		item.Identity.Kind != core.AppointmentItem || item.Type != core.AppointmentSingle || item.UID != uid {
		return core.Appointment{}, core.ErrOutcomeUnknown
	}
	return item, nil
}

func (h *Handler) submitMeetingRequest(ctx context.Context, mailboxID string, key, payload [32]byte,
	stage func(context.Context) (core.SendAttempt, error)) (core.ID, error) {
	store := h.journal.store
	input := core.SendInput{MIMEDigest: payload}
	intent, err := store.PrepareCorrelatedSend(ctx, mailboxID, input, key)
	if err != nil {
		return "", err
	}
	var attempt core.SendAttempt
	if intent.State == core.SendReady {
		attempt, err = stage(ctx)
		if err != nil {
			// Another caller may have completed the same operation during
			// our read of the pre-mutation revision.
			current, readErr := store.SendIntent(ctx, mailboxID, intent.ID)
			if readErr != nil || current.State == core.SendReady {
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
		state := core.SendConfirmed
		if errors.Is(err, core.ErrSendRejected) {
			state, id = core.SendFailed, ""
		} else if err != nil || id == "" {
			return "", core.ErrOutcomeUnknown
		}
		commitCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		if err := store.CompleteSend(commitCtx, mailboxID, intent.ID, state, id); err != nil {
			return "", core.ErrOutcomeUnknown
		}
		intent.State = state
	}
	switch intent.State {
	case core.SendFailed:
		return "", core.ErrSendRejected
	case core.SendConfirmed:
		return id, nil
	default:
		return "", core.ErrOutcomeUnknown
	}
}
