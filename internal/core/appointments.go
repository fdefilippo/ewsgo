// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"time"
)

const AppointmentIDPrefix = "appointment:"

type AppointmentType uint8

const (
	AppointmentSingle AppointmentType = iota
	AppointmentOccurrence
	AppointmentException
	AppointmentMaster
)

// Timed values are UTC instants. All-day bounds represent civil dates at
// UTC midnight, with an exclusive end. OriginalStart retains the recurrence
// anchor independently of any change to the instance's dates.
type Appointment struct {
	Identity                  Object
	SeriesID                  ID
	Type                      AppointmentType
	UID, Subject, Location    string
	Body                      string
	Start, End, OriginalStart time.Time
	AllDay, Cancelled         bool
	TimeZone                  string // Named IANA zone for a timed recurring series.
	Recurrence                *AppointmentRecurrence
	StartZone, EndZone        *AppointmentZone
	Participation             AppointmentParticipation
	Meeting                   *AppointmentMeeting `json:",omitempty"`
	SeriesSchedule            *AppointmentSeriesSchedule
}

// WallStart and WallEnd hold the local civil fields in UTC, independent of
// each event's offset. SkippedDates are local midnights with no occurrence.
type AppointmentSeriesSchedule struct {
	WallStart, WallEnd time.Time
	SkippedDates       []time.Time
	Exceptions         []AppointmentSeriesException
}

// AppointmentSeriesException uses the series' local civil clock for all
// three bounds, including the original recurrence anchor.
type AppointmentSeriesException struct {
	WallStart, WallEnd, WallOriginalStart time.Time
	Subject, Location                     string
}

type AppointmentParticipation uint8

const (
	AppointmentParticipationUnknown AppointmentParticipation = iota
	AppointmentPersonal
	AppointmentOrganized
	AppointmentReceived
)

// AppointmentZone carries the stored rules for converting an appointment
// between UTC and its display zone. Offsets in minutes are east of UTC.
type AppointmentZone struct {
	Name                           string
	StandardOffset, DaylightOffset int
	Standard, Daylight             AppointmentZoneTransition
}

// OffsetAt returns the active offset in minutes east of UTC.
func (z AppointmentZone) OffsetAt(stamp time.Time) int {
	stamp = stamp.UTC()
	offset := z.StandardOffset
	if z.Standard.Month != 0 && z.StandardOffset != z.DaylightOffset {
		var latest time.Time
		for year := stamp.Year() - 1; year <= stamp.Year()+1; year++ {
			for _, transition := range [...]struct {
				rule          AppointmentZoneTransition
				before, after int
			}{
				{z.Standard, z.DaylightOffset, z.StandardOffset},
				{z.Daylight, z.StandardOffset, z.DaylightOffset},
			} {
				rule := transition.rule
				first := time.Date(year, rule.Month, 1, rule.Hour, rule.Minute, rule.Second, 0, time.UTC)
				days := (int(rule.Weekday)-int(first.Weekday())+7)%7 + (rule.Week-1)*7
				wall := first.AddDate(0, 0, days)
				if wall.Month() != rule.Month {
					wall = wall.AddDate(0, 0, -7)
				}
				at := wall.Add(-time.Duration(transition.before) * time.Minute)
				if !at.After(stamp) && at.After(latest) {
					latest, offset = at, transition.after
				}
			}
		}
	}
	return offset
}

// A zero Month means the zone does not change offset. Week is 1–5 with
// week 5 denoting the last occurrence of Weekday in the month.
type AppointmentZoneTransition struct {
	Month                time.Month
	Week                 int
	Weekday              time.Weekday
	Hour, Minute, Second int
}

type RecurrenceFrequency uint8

const (
	RecurrenceDaily RecurrenceFrequency = iota + 1
	RecurrenceWeekly
)

// Dates represent civil midnights in UTC. Until is inclusive. A zero Until
// and Count indicate an open-ended series.
type AppointmentRecurrence struct {
	Frequency        RecurrenceFrequency
	Interval         int
	StartDate, Until time.Time
	Count            int
	Days             []time.Weekday
	FirstDay         time.Weekday
}

type AppointmentReader interface {
	CalendarView(context.Context, ID, time.Time, time.Time) ([]Appointment, error)
	GetAppointment(context.Context, ID, ID) (Appointment, error)
}

// AppointmentContentsReader exposes unexpanded calendar items with their
// folder revision, read state and any active reminder. A series appears
// once, with the revision of its master.
type AppointmentContentsReader interface {
	ListAppointments(context.Context, ID) ([]Appointment, error)
	GetCalendarAppointment(context.Context, ID, ID) (Appointment, error)
}

// Personal appointments have no external attendees. Invitations and
// cancellations that send mail are separate operations.
type AppointmentWriter interface {
	CreateAppointment(context.Context, ID, Appointment) (Appointment, error)
	TrashAppointment(context.Context, Appointment) error
}

// AppointmentImportWriter creates a personal appointment with a gateway-issued
// UID that can be looked up after a lost reply, without resending.
type AppointmentImportWriter interface {
	CreateAppointmentWithUID(context.Context, ID, Appointment, string) (Appointment, error)
	FindAppointmentByUID(context.Context, ID, string) (Appointment, error)
}

type AppointmentDetailsPatch struct {
	Subject, Location *string
	Start, End        *time.Time // Same representation as the appointment bounds.
}

type AppointmentDetailsWriter interface {
	UpdateAppointmentDetails(context.Context, Appointment, AppointmentDetailsPatch) (Appointment, error)
}
