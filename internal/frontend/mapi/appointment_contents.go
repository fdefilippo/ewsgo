// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"net/mail"
	"strings"
	"time"

	"ewsgo/internal/core"
)

const appointmentPropertySet = "00062002-0000-0000-c000-000000000046"
const commonPropertySet = "00062008-0000-0000-c000-000000000046"
const meetingPropertySet = "6ed8da90-450b-101b-98da-00aa003f1305"

var (
	appointmentStartWhole = core.Key{Set: appointmentPropertySet, ID: 0x820d}
	appointmentEndWhole   = core.Key{Set: appointmentPropertySet, ID: 0x820e}
	appointmentLocation   = core.Key{Set: appointmentPropertySet, ID: 0x8208}
	appointmentDuration   = core.Key{Set: appointmentPropertySet, ID: 0x8213}
	appointmentAllDay     = core.Key{Set: appointmentPropertySet, ID: 0x8215}
	appointmentStart      = core.Key{Set: commonPropertySet, ID: 0x8516}
	appointmentEnd        = core.Key{Set: commonPropertySet, ID: 0x8517}
	appointmentStateFlags = core.Key{Set: appointmentPropertySet, ID: 0x8217}
	appointmentStartZone  = core.Key{Set: appointmentPropertySet, ID: 0x825e}
	appointmentEndZone    = core.Key{Set: appointmentPropertySet, ID: 0x825f}
	appointmentRecur      = core.Key{Set: appointmentPropertySet, ID: 0x8216}
	appointmentRecurType  = core.Key{Set: appointmentPropertySet, ID: 0x8231}
	appointmentZoneStruct = core.Key{Set: appointmentPropertySet, ID: 0x8233}
	appointmentClipStart  = core.Key{Set: appointmentPropertySet, ID: 0x8235}
	appointmentClipEnd    = core.Key{Set: appointmentPropertySet, ID: 0x8236}
	appointmentRecurZone  = core.Key{Set: appointmentPropertySet, ID: 0x8260}
	appointmentGlobalID   = core.Key{Set: meetingPropertySet, ID: 0x0003}
	appointmentCleanID    = core.Key{Set: meetingPropertySet, ID: 0x0023}
)

var appointmentNamedProperties = []calendarNamedProperty{
	calendarSearchNamed(appointmentLocation, 0x001f),
	calendarSearchNamed(appointmentStartWhole, 0x0040),
	calendarSearchNamed(appointmentEndWhole, 0x0040),
	calendarSearchNamed(appointmentDuration, 0x0003),
	calendarSearchNamed(appointmentAllDay, 0x000b),
	calendarSearchNamed(appointmentStart, 0x0040),
	calendarSearchNamed(appointmentEnd, 0x0040),
	calendarSearchNamed(appointmentStateFlags, 0x0003),
	calendarSearchNamed(appointmentResponse, 0x0003),
	calendarSearchNamed(appointmentSequence, 0x0003),
	calendarSearchNamed(appointmentLastSent, 0x0003),
	calendarSearchNamed(appointmentInvited, 0x000b),
	calendarSearchNamed(appointmentStartZone, 0x0102),
	calendarSearchNamed(appointmentEndZone, 0x0102),
	calendarSearchNamed(appointmentRecur, 0x0102),
	calendarSearchNamed(appointmentRecurType, 0x0003),
	calendarSearchNamed(appointmentZoneStruct, 0x0102),
	calendarSearchNamed(appointmentClipStart, 0x0040),
	calendarSearchNamed(appointmentClipEnd, 0x0040),
	calendarSearchNamed(appointmentRecurZone, 0x0102),
	calendarSearchNamed(appointmentGlobalID, 0x0102),
	calendarSearchNamed(appointmentCleanID, 0x0102),
}

const (
	appointmentStartDateTag = 0x00600040
	appointmentEndDateTag   = 0x00610040
	appointmentBodyTag      = 0x1000001f
)

var appointmentFixedProperties = []uint32{
	appointmentStartDateTag, appointmentEndDateTag, appointmentBodyTag, meetingResponseRequestedTag, meetingReplyRequestedTag,
	representingEntryTag, senderEntryTag,
	0x0042001f, 0x0064001f, 0x0065001f, 0x5d02001f,
	0x0c1a001f, 0x0c1e001f, 0x0c1f001f, 0x5d01001f,
}

func calendarAppointmentMessage(item core.Appointment, parent core.ID) (core.Object, error) {
	object := item.Identity
	native, ok := strings.CutPrefix(string(object.ID), core.AppointmentIDPrefix)
	if !ok || native == "" || strings.ContainsRune(native, '@') || object.Kind != core.AppointmentItem ||
		object.Parent != parent || (item.Type != core.AppointmentSingle && item.Type != core.AppointmentMaster) ||
		item.Start.Year() < 1601 || item.End.Year() > 9999 || item.End.Before(item.Start) ||
		!validMailText(item.Subject) || !validMailText(item.Location) || !validMailText(item.Body) {
		return core.Object{}, core.ErrInvalid
	}
	// Keep the identity already assigned to this native item by Reminders.
	object.ID, object.Kind = core.ID("a:"+native), core.Message
	object.Properties = mapAppointmentProperties(item)
	if item.Meeting != nil {
		if len(item.Meeting.Participants) > core.MaxMailContentMessages {
			return core.Object{}, core.ErrLimit
		}
		if item.Participation == core.AppointmentReceived {
			address, _ := object.Properties[core.Key{ID: 0x5d02}].(core.Text)
			name, _ := object.Properties[core.Key{ID: 0x0042}].(core.Text)
			if address != "" {
				object.Recipients = append(object.Recipients, core.Recipient{
					Kind: core.RecipientTo, Address: string(address), DisplayName: string(name),
				})
			}
		}
		for _, attendee := range item.Meeting.Participants {
			if len(object.Recipients) != 0 && item.Participation == core.AppointmentReceived &&
				strings.EqualFold(attendee.Address, item.Meeting.Organizer.Address) {
				continue
			}
			if parsed, err := mail.ParseAddress(attendee.Address); err != nil || parsed.Address != attendee.Address ||
				!validMailText(attendee.Name) || len(attendee.Name) > maxHierarchyText {
				continue // A native calendar attendee can lack a routable mailbox.
			}
			kind := core.RecipientTo
			if attendee.Optional {
				kind = core.RecipientCc
			}
			object.Recipients = append(object.Recipients, core.Recipient{Kind: kind, Address: attendee.Address, DisplayName: attendee.Name})
		}
	}
	return object, nil
}

// For a received meeting, the organizer is the first row when Zimbra
// supplied a routable identity. MS-OXCICAL maps ORGANIZER to a recipient
// with the recipOrganizer flag and a single address book identity.
func receivedMeetingOrganizer(row messageRow) bool {
	flags, _ := row.object.Properties[appointmentStateFlags].(core.Integer)
	address, _ := row.object.Properties[core.Key{ID: 0x5d02}].(core.Text)
	return row.special == core.CalendarFolder && flags&3 == 3 && address != "" &&
		len(row.object.Recipients) != 0 && row.object.Recipients[0].Address == string(address)
}

func mapAppointmentProperties(item core.Appointment) core.Properties {
	start, end := core.Timestamp(item.Start), core.Timestamp(item.End)
	properties := core.Properties{
		core.Subject:                        core.Text(item.Subject),
		core.AppointmentRecurring:           core.Bool(item.Type == core.AppointmentMaster),
		appointmentAllDay:                   core.Bool(item.AllDay),
		appointmentStartWhole:               start,
		appointmentEndWhole:                 end,
		appointmentStart:                    start,
		appointmentEnd:                      end,
		appointmentDuration:                 core.Integer((item.End.Unix() - item.Start.Unix()) / int64(time.Minute/time.Second)),
		{ID: appointmentStartDateTag >> 16}: start,
		{ID: appointmentEndDateTag >> 16}:   end,
	}
	if item.Location != "" {
		properties[appointmentLocation] = core.Text(item.Location)
	}
	if item.Body != "" {
		properties[core.Key{ID: messageBodyID}] = core.Text(item.Body)
	}
	if uid := appointmentGlobalIdentity(item.UID); uid != nil {
		properties[appointmentGlobalID] = uid
		properties[appointmentCleanID] = uid
	}
	for _, key := range []core.Key{core.MetadataRevision, core.ContentRevision, core.LastModifiedAt, core.IsRead, core.ReminderSet, core.ReminderSignalTime} {
		if value, ok := item.Identity.Properties[key]; ok {
			properties[key] = value
		}
	}
	switch item.Participation {
	case core.AppointmentPersonal:
		properties[appointmentStateFlags] = core.Integer(0)
	case core.AppointmentOrganized, core.AppointmentReceived:
		const meeting, received, canceled = 1, 2, 4
		state := core.Integer(meeting)
		if item.Participation == core.AppointmentReceived {
			state |= received
		}
		if item.Cancelled {
			state |= canceled
		}
		properties[appointmentStateFlags] = state
	}
	if item.Meeting != nil {
		response := core.Integer(5)
		switch {
		case item.Meeting.OwnerOrganizer:
			response = 1
		case item.Meeting.OwnerReply == core.MeetingTentative:
			response = 2
		case item.Meeting.OwnerReply == core.MeetingAccept:
			response = 3
		case item.Meeting.OwnerReply == core.MeetingDecline:
			response = 4
		}
		properties[appointmentResponse] = response
		properties[appointmentSequence] = core.Integer(item.Meeting.Sequence)
		if item.Meeting.InvitationSent {
			properties[appointmentLastSent] = core.Integer(item.Meeting.Sequence)
		}
		properties[appointmentInvited] = core.Bool(item.Meeting.InvitationSent)
		properties[core.Key{ID: meetingResponseRequestedTag >> 16}] = core.Bool(item.Meeting.RequestResponse)
		properties[core.Key{ID: meetingReplyRequestedTag >> 16}] = core.Bool(item.Meeting.RequestResponse)
		organizer := item.Meeting.Organizer
		if addr, err := mail.ParseAddress(organizer.Address); err == nil && addr.Address == organizer.Address &&
			len(organizer.Name) <= maxHierarchyText && validMailText(organizer.Name) {
			name := organizer.Name
			if name == "" {
				name = organizer.Address
			}
			for i, value := range []string{name, "SMTP", organizer.Address, organizer.Address} {
				properties[core.Key{ID: representingNameTags[i] >> 16}] = core.Text(value)
				properties[core.Key{ID: senderNameTags[i] >> 16}] = core.Text(value)
			}
			entry := smtpOneOffEntry(name, organizer.Address)
			properties[core.Key{ID: representingEntryTag >> 16}] = entry
			properties[core.Key{ID: senderEntryTag >> 16}] = entry
		}
	}
	if zone, ok := encodeAppointmentZoneDefinition(item.StartZone, false); ok {
		properties[appointmentStartZone] = zone
	}
	if zone, ok := encodeAppointmentZoneDefinition(item.EndZone, false); ok {
		properties[appointmentEndZone] = zone
	}
	if item.Type == core.AppointmentSingle {
		properties[appointmentClipStart], properties[appointmentClipEnd] = start, end
	}
	if zone, ok := encodeAppointmentZoneDefinition(item.StartZone, true); item.Type == core.AppointmentMaster && ok {
		if recur, ok := encodeAppointmentRecurrence(item); ok {
			properties[appointmentRecur] = recur.data
			properties[appointmentRecurType] = core.Integer(recur.kind)
			properties[appointmentZoneStruct] = encodeAppointmentZoneStruct(*item.StartZone)
			properties[appointmentRecurZone] = zone
			// MS-OXOCAL 2.2.1.14–15. Use the series' civil dates
			// and its time zone when projecting native appointments.
			first, startOK := wallAppointmentInstant(recurrenceWall(recur.startDate), *item.StartZone)
			last, endOK := time.Date(4500, time.August, 31, 23, 59, 0, 0, time.UTC), true
			if recur.endDate != recurrenceNoEndDate {
				last, endOK = wallAppointmentInstant(recurrenceWall(recur.endDate), *item.StartZone)
			}
			if startOK && endOK && first.Year() >= 1601 && last.Year() <= 9999 {
				properties[appointmentClipStart], properties[appointmentClipEnd] = core.Timestamp(first), core.Timestamp(last)
			}
		}
	}
	return properties
}
