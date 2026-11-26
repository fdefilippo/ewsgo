// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import "context"

type MeetingMessageKind uint8

const (
	MeetingRequestMessage MeetingMessageKind = iota + 1
	MeetingCancellationMessage
	MeetingAcceptedMessage
	MeetingTentativeMessage
	MeetingDeclinedMessage
)

var (
	MeetingMessageType = Key{Set: "f8b556e0-5d21-4ce8-9043-05e3d3dd7b90", Name: "meetingMessageType"}
	MeetingMessageUID  = Key{Set: "f8b556e0-5d21-4ce8-9043-05e3d3dd7b90", Name: "meetingMessageUID"}
)

type MeetingAttendee struct {
	Address  string
	Name     string
	Optional bool
}

type MeetingParticipation struct {
	MeetingAttendee
	Reply MeetingReplyChoice
}

type AppointmentMeeting struct {
	Organizer       MeetingAttendee
	Participants    []MeetingParticipation
	OwnerOrganizer  bool
	OwnerReply      MeetingReplyChoice
	RequestResponse bool
}

type MeetingInvitation struct {
	Appointment      Appointment
	Attendees        []MeetingAttendee
	RequestResponses bool
}

// MeetingInvitationBackend prepares a single organizer submission. The
// caller durably claims the intent before invoking the native operation.
type MeetingInvitationBackend interface {
	StageMeetingInvitation(folder ID, meeting MeetingInvitation, uid string) (SendAttempt, error)
}

type MeetingDetailsWriter interface {
	UpdateMeetingDetails(context.Context, Appointment, AppointmentDetailsPatch) (Appointment, error)
}

type MeetingCancellation struct {
	Body string
	HTML bool
}

type MeetingCancellationBackend interface {
	StageMeetingCancellation(context.Context, Appointment, MeetingCancellation) (SendAttempt, error)
}

type MeetingReplyChoice uint8

const (
	MeetingAccept MeetingReplyChoice = iota + 1
	MeetingTentative
	MeetingDecline
)

type MeetingReply struct {
	Choice          MeetingReplyChoice
	NotifyOrganizer bool
}

type MeetingReplyBackend interface {
	StageMeetingReply(context.Context, Object, MeetingReply) (SendAttempt, error)
}
