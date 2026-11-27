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
	MeetingMessageType       = Key{Set: "f8b556e0-5d21-4ce8-9043-05e3d3dd7b90", Name: "meetingMessageType"}
	MeetingMessageUID        = Key{Set: "f8b556e0-5d21-4ce8-9043-05e3d3dd7b90", Name: "meetingMessageUID"}
	MeetingMessageStart      = Key{Set: MeetingMessageType.Set, Name: "meetingMessageStart"}
	MeetingMessageEnd        = Key{Set: MeetingMessageType.Set, Name: "meetingMessageEnd"}
	MeetingMessageLocation   = Key{Set: MeetingMessageType.Set, Name: "meetingMessageLocation"}
	MeetingMessageAllDay     = Key{Set: MeetingMessageType.Set, Name: "meetingMessageAllDay"}
	MeetingMessageSequence   = Key{Set: MeetingMessageType.Set, Name: "meetingMessageSequence"}
	MeetingCalendarProcessed = Key{Set: MeetingMessageType.Set, Name: "meetingCalendarProcessed"}
	MeetingResponseRequested = Key{Set: MeetingMessageType.Set, Name: "meetingResponseRequested"}
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
	InvitationSent  bool
	RequestResponse bool
	Sequence        uint32
}

type MeetingInvitation struct {
	Appointment      Appointment
	Attendees        []MeetingAttendee
	RequestResponses bool
	HTML             bool `json:",omitempty"`
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

// MeetingAppointmentReader locates a native meeting in the authorized folder
// by its protocol-independent calendar UID, including before any MAPI sync.
type MeetingAppointmentReader interface {
	GetMeetingByUID(context.Context, ID, string) (Appointment, error)
}
