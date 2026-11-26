// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
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
	calendarSearchNamed(appointmentStartZone, 0x0102),
	calendarSearchNamed(appointmentEndZone, 0x0102),
	calendarSearchNamed(appointmentRecur, 0x0102),
	calendarSearchNamed(appointmentRecurType, 0x0003),
	calendarSearchNamed(appointmentZoneStruct, 0x0102),
	calendarSearchNamed(appointmentRecurZone, 0x0102),
	calendarSearchNamed(appointmentGlobalID, 0x0102),
	calendarSearchNamed(appointmentCleanID, 0x0102),
}

const (
	appointmentStartDateTag = 0x00600040
	appointmentEndDateTag   = 0x00610040
	appointmentBodyTag      = 0x1000001f
)

var appointmentFixedProperties = [...]uint32{appointmentStartDateTag, appointmentEndDateTag, appointmentBodyTag}

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
	return object, nil
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
	if zone, ok := encodeAppointmentZoneDefinition(item.StartZone, false); ok {
		properties[appointmentStartZone] = zone
	}
	if zone, ok := encodeAppointmentZoneDefinition(item.EndZone, false); ok {
		properties[appointmentEndZone] = zone
	}
	if zone, ok := encodeAppointmentZoneDefinition(item.StartZone, true); item.Type == core.AppointmentMaster && ok {
		if recur, kind, ok := encodeAppointmentRecurrence(item); ok {
			properties[appointmentRecur] = recur
			properties[appointmentRecurType] = core.Integer(kind)
			properties[appointmentZoneStruct] = encodeAppointmentZoneStruct(*item.StartZone)
			properties[appointmentRecurZone] = zone
		}
	}
	return properties
}
