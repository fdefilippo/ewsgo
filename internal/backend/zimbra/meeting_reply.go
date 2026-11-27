// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"errors"
	"net/mail"
	"strconv"
	"strings"

	"ewsgo/internal/core"
)

func (m *mailbox) StageMeetingReply(ctx context.Context, object core.Object, response core.MeetingReply) (core.SendAttempt, error) {
	verb, status := "", ""
	switch response.Choice {
	case core.MeetingAccept:
		verb, status = "ACCEPT", "AC"
	case core.MeetingTentative:
		verb, status = "TENTATIVE", "TE"
	case core.MeetingDecline:
		verb, status = "DECLINE", "DE"
	default:
		return nil, core.ErrInvalid
	}

	folder := core.ID("f:" + specialFolderSpec(core.CalendarFolder).id)
	var detail *node
	var err error
	switch object.Kind {
	case core.Message:
		id, valid := strings.CutPrefix(string(object.ID), "m:")
		if !valid || !nativeID.MatchString(id) {
			return nil, core.ErrNotFound
		}
		message, err := m.native.getMessageNode(ctx, id, "0")
		if err != nil {
			return nil, semanticError(err)
		}
		current, err := messageSummary(message)
		if err != nil {
			return nil, err
		}
		if current.ID != object.ID || current.Parent != object.Parent || current.Revision != object.Revision ||
			current.Properties[core.ContentRevision] != object.Properties[core.ContentRevision] {
			return nil, core.ErrConflict
		}
		source := meetingMessageComponent(message)
		if source == nil || source.get("method") != "REQUEST" || source.get("uid") == "" {
			return nil, core.ErrUnsupported
		}
		detail, err = m.readAppointment(ctx, folder, "uid", source.get("uid"), false)
		if err != nil {
			return nil, err
		}
		series := masterComponent(*detail)
		if series == nil || series.get("seq") != source.get("seq") {
			return nil, core.ErrConflict
		}
	case core.AppointmentItem:
		id, valid := strings.CutPrefix(string(object.ID), core.AppointmentIDPrefix)
		if !valid || !nativeID.MatchString(id) || object.Parent != folder {
			return nil, core.ErrUnsupported
		}
		detail, err = m.readAppointment(ctx, folder, "id", id, false)
		if err != nil {
			return nil, err
		}
		current, err := appointmentIdentity(*detail, folder)
		if err != nil {
			return nil, err
		}
		if current.Revision != object.Revision || current.Properties[core.ContentRevision] != object.Properties[core.ContentRevision] {
			return nil, core.ErrConflict
		}
	default:
		return nil, core.ErrUnsupported
	}
	invite, comp := appointmentInvite(*detail, func(candidate node) bool { return candidate.get("ex") != "1" })
	if comp == nil || comp.get("method") != "REQUEST" || comp.get("status") == "CANC" ||
		comp.get("isOrg") == "1" || comp.child(mailNS, "recur") != nil {
		return nil, core.ErrUnsupported
	}
	org := comp.child(mailNS, "or")
	if org == nil || m.owner == "" || strings.EqualFold(org.get("a"), m.owner) {
		return nil, core.ErrAccessDenied
	}
	organizer, err := mail.ParseAddress(org.get("a"))
	if err != nil || organizer.Address != org.get("a") || org.get("sentBy") != "" {
		return nil, core.ErrUnsupported
	}
	var participant *node
	for i := range comp.Children {
		child := &comp.Children[i]
		if child.XMLName.Space == mailNS && child.XMLName.Local == "at" && strings.EqualFold(child.get("a"), m.owner) {
			if participant != nil || child.get("sentBy") != "" {
				return nil, core.ErrUnsupported
			}
			participant = child
		}
	}
	if participant == nil {
		return nil, core.ErrAccessDenied
	}
	if response.NotifyOrganizer && comp.get("rsvp") == "0" {
		return nil, core.ErrUnsupported
	}
	if participant.get("ptst") == status {
		return nil, core.ErrConflict
	}
	id := detail.get("id")
	invID, compNum, err := appointmentInviteAddress(invite)
	if err != nil {
		return nil, err
	}
	req := attr(attr(attr(attr(element(mailNS, "SendInviteReplyRequest", ""), "id", id+"-"+invID),
		"compNum", compNum), "verb", verb), "updateOrganizer", strconv.FormatBool(response.NotifyOrganizer))
	return func(ctx context.Context) (core.ID, error) {
		ack, err := m.native.request(ctx, mailNS, "SendInviteReply", "", req)
		if err != nil {
			var fault *Fault
			if errors.As(err, &fault) && fault.Code == "mail.INVITE_OUT_OF_DATE" {
				return "", core.ErrSendRejected
			}
			return "", core.ErrOutcomeUnknown
		}
		ackID := ack.get("calItemId")
		if ackID != id && (ackID != "" || response.NotifyOrganizer) {
			return "", core.ErrOutcomeUnknown
		}
		return core.ID(core.AppointmentIDPrefix + id), nil
	}, nil
}

func meetingMessageComponent(item node) *node {
	var component *node
	for i := range item.Children {
		invite := &item.Children[i]
		if invite.XMLName.Space != mailNS || invite.XMLName.Local != "inv" || invite.get("type") != "appt" {
			continue
		}
		for j := range invite.Children {
			part := &invite.Children[j]
			if part.XMLName.Space != mailNS || part.XMLName.Local != "comp" {
				continue
			}
			if component != nil {
				return nil
			}
			component = part
		}
	}
	return component
}

func projectMeetingMessage(properties core.Properties, item node) {
	comp := meetingMessageComponent(item)
	if comp == nil || comp.get("uid") == "" || len(comp.get("uid")) > 1024 {
		return
	}
	var kind core.MeetingMessageKind
	switch comp.get("method") {
	case "REQUEST":
		kind = core.MeetingRequestMessage
	case "CANCEL":
		kind = core.MeetingCancellationMessage
	case "REPLY":
		attendee := comp.child(mailNS, "at")
		if attendee == nil {
			return
		}
		switch attendee.get("ptst") {
		case "AC":
			kind = core.MeetingAcceptedMessage
		case "TE":
			kind = core.MeetingTentativeMessage
		case "DE":
			kind = core.MeetingDeclinedMessage
		default:
			return
		}
	default:
		return
	}
	properties[core.MeetingMessageType] = core.Integer(kind)
	properties[core.MeetingMessageUID] = core.Text(comp.get("uid"))
	if location := comp.get("loc"); len(location) <= 4096 {
		properties[core.MeetingMessageLocation] = core.Text(location)
	}
	allDay := comp.get("allDay") == "1"
	if start, end, err := appointmentDates(*comp, allDay); err == nil {
		properties[core.MeetingMessageStart] = core.Timestamp(start)
		properties[core.MeetingMessageEnd] = core.Timestamp(end)
		properties[core.MeetingMessageAllDay] = core.Bool(allDay)
	}
	if seq, err := strconv.ParseUint(comp.get("seq"), 10, 31); err == nil {
		properties[core.MeetingMessageSequence] = core.Integer(seq)
	}
	if kind == core.MeetingRequestMessage {
		properties[core.MeetingResponseRequested] = core.Bool(comp.get("rsvp") != "0")
		// Zimbra emits ciFolder only when it resolves the calendar item in this
		// mailbox. A remote or deleted item can lack that local folder evidence.
		if properties[core.MeetingMessageSequence] == core.Integer(0) &&
			comp.get("isOrg") != "1" && comp.get("ex") != "1" && comp.get("status") != "CANC" &&
			comp.child(mailNS, "recur") == nil && comp.get("ciFolder") == specialFolderSpec(core.CalendarFolder).id &&
			nativeID.MatchString(comp.get("calItemId")) {
			properties[core.MeetingCalendarProcessed] = core.Bool(true)
		}
	}
}
