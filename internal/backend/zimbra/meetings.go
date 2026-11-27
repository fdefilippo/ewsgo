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

func (m *mailbox) StageMeetingInvitation(folder core.ID, meeting core.MeetingInvitation, uid string) (core.SendAttempt, error) {
	spec := specialFolderSpec(core.CalendarFolder)
	if folder != core.ID("f:"+spec.id) {
		return nil, core.ErrUnsupported
	}
	if uid == "" || len(uid) > 255 || strings.ContainsAny(uid, "\r\n\x00") || m.owner == "" ||
		len(meeting.Attendees) == 0 || len(meeting.Attendees) > core.MaxAvailabilityMailboxes ||
		meeting.Appointment.Type != core.AppointmentSingle || meeting.Appointment.AllDay {
		return nil, core.ErrUnsupported
	}
	message, err := appointmentCreateMessage(spec.id, meeting.Appointment, uid)
	if err != nil {
		return nil, err
	}
	if meeting.HTML {
		if part := message.child(mailNS, "mp"); part != nil {
			*part = attr(*part, "ct", "text/html")
		}
	}
	comp := message.child(mailNS, "inv").child(mailNS, "comp")
	*comp = attr(attr(attr(*comp, "status", "CONF"), "fb", "B"), "transp", "O")
	comp.Children = append(comp.Children, attr(element(mailNS, "or", ""), "a", m.owner))
	seen := map[string]bool{strings.ToLower(m.owner): true}
	for _, attendee := range meeting.Attendees {
		address, err := mail.ParseAddress(attendee.Address)
		key := strings.ToLower(attendee.Address)
		if err != nil || address.Address != attendee.Address || seen[key] || len(attendee.Address) > 320 ||
			len(attendee.Name) > 512 || strings.ContainsAny(attendee.Name, "\r\n\x00") {
			return nil, core.ErrInvalid
		}
		seen[key] = true
		role := "REQ"
		if attendee.Optional {
			role = "OPT"
		}
		recipient := attr(attr(element(mailNS, "e", ""), "a", attendee.Address), "t", "t")
		nativeAttendee := attr(attr(attr(element(mailNS, "at", ""), "a", attendee.Address), "role", role), "ptst", "NE")
		if meeting.RequestResponses {
			nativeAttendee = attr(nativeAttendee, "rsvp", "1")
		}
		if attendee.Name != "" {
			nativeAttendee = attr(nativeAttendee, "d", attendee.Name)
		}
		comp.Children = append(comp.Children, nativeAttendee)
		message.Children = append(message.Children, recipient)
	}
	req := element(mailNS, "CreateAppointmentRequest", "", message)
	return func(ctx context.Context) (core.ID, error) {
		// Zimbra can replace an existing appointment when given its UID.
		// Check across all folders, including the Trash.
		_, err := m.native.request(ctx, mailNS, "GetAppointment", "", getAppointmentRequest("uid", uid, false))
		if err == nil {
			return "", core.ErrSendRejected
		}
		if !errors.Is(appointmentError(err), core.ErrNotFound) {
			return "", core.ErrOutcomeUnknown
		}
		ack, err := m.native.request(ctx, mailNS, "CreateAppointment", "", req)
		if err != nil {
			mapped := semanticError(err)
			if errors.Is(mapped, core.ErrAccessDenied) || errors.Is(mapped, core.ErrAuthentication) {
				return "", core.ErrSendRejected
			}
			return "", core.ErrOutcomeUnknown
		}
		id := ack.get("calItemId")
		invite, ok := strings.CutPrefix(ack.get("invId"), id+"-")
		if !nativeID.MatchString(id) || !ok || !nativeID.MatchString(invite) {
			return "", core.ErrOutcomeUnknown
		}
		return core.ID(core.AppointmentIDPrefix + id), nil
	}, nil
}

func (m *mailbox) meetingRecipients(comp node) ([]node, error) {
	organizer := comp.child(mailNS, "or")
	if comp.get("isOrg") != "1" || comp.get("status") == "CANC" || organizer == nil ||
		m.owner == "" || !strings.EqualFold(organizer.get("a"), m.owner) {
		return nil, core.ErrAccessDenied
	}
	seen := map[string]bool{strings.ToLower(m.owner): true}
	var recipients []node
	for _, member := range comp.Children {
		if member.XMLName.Space != mailNS || member.XMLName.Local != "at" {
			continue
		}
		role, raw := member.get("role"), member.get("a")
		address, err := mail.ParseAddress(raw)
		key := strings.ToLower(raw)
		if err != nil || address.Address != raw || seen[key] || len(raw) > 320 ||
			role != "REQ" && role != "OPT" || member.get("sentBy") != "" ||
			len(recipients) == core.MaxAvailabilityMailboxes {
			return nil, core.ErrUnsupported
		}
		seen[key] = true
		recipients = append(recipients, attr(attr(element(mailNS, "e", ""), "a", raw), "t", "t"))
	}
	if len(recipients) == 0 {
		return nil, core.ErrUnsupported
	}
	return recipients, nil
}

func appointmentMeeting(comp node, owner string) *core.AppointmentMeeting {
	org := comp.child(mailNS, "or")
	if org == nil || org.get("a") == "" {
		return nil
	}
	meeting := &core.AppointmentMeeting{
		Organizer:      core.MeetingAttendee{Address: org.get("a"), Name: org.get("d")},
		OwnerOrganizer: comp.get("isOrg") == "1" && strings.EqualFold(org.get("a"), owner),
		InvitationSent: comp.get("neverSent") != "1" && (comp.get("method") == "REQUEST" || comp.get("method") == "CANCEL"),
	}
	if sequence, err := strconv.ParseUint(comp.get("seq"), 10, 31); err == nil {
		meeting.Sequence = uint32(sequence)
	}
	for _, att := range comp.Children {
		if att.XMLName.Space != mailNS || att.XMLName.Local != "at" || att.get("a") == "" ||
			att.get("role") != "REQ" && att.get("role") != "OPT" {
			continue
		}
		address := att.get("a")
		var reply core.MeetingReplyChoice
		switch att.get("ptst") {
		case "AC":
			reply = core.MeetingAccept
		case "TE":
			reply = core.MeetingTentative
		case "DE":
			reply = core.MeetingDecline
		}
		if strings.EqualFold(address, owner) {
			meeting.OwnerReply = reply
		}
		if att.get("rsvp") == "1" {
			meeting.RequestResponse = true
		}
		meeting.Participants = append(meeting.Participants, core.MeetingParticipation{
			MeetingAttendee: core.MeetingAttendee{Address: address, Name: att.get("d"), Optional: att.get("role") == "OPT"},
			Reply:           reply,
		})
	}
	if len(meeting.Participants) == 0 {
		return nil
	}
	return meeting
}

func (m *mailbox) GetMeetingByUID(ctx context.Context, folder core.ID, uid string) (core.Appointment, error) {
	if uid == "" || len(uid) > 255 {
		return core.Appointment{}, core.ErrInvalid
	}
	detail, err := m.readAppointment(ctx, folder, "uid", uid, true)
	if err != nil {
		return core.Appointment{}, err
	}
	identity, err := appointmentIdentity(*detail, folder)
	if err != nil {
		return core.Appointment{}, err
	}
	comp := masterComponent(*detail)
	if comp == nil {
		return core.Appointment{}, core.ErrUnsupported
	}
	item, err := appointmentFromComponent(*detail, *comp, identity)
	if err != nil {
		return core.Appointment{}, err
	}
	if item.UID != uid {
		return core.Appointment{}, core.ErrConflict
	}
	item.Participation = appointmentParticipation(*comp, m.owner)
	item.Meeting = appointmentMeeting(*comp, m.owner)
	if item.Meeting == nil {
		return core.Appointment{}, core.ErrUnsupported
	}
	return item, nil
}
