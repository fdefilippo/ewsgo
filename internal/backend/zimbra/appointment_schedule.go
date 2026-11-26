// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"strings"
	"time"

	"ewsgo/internal/core"
)

func appointmentSeriesSchedule(detail, master node, item core.Appointment) *core.AppointmentSeriesSchedule {
	s, e := master.child(mailNS, "s"), master.child(mailNS, "e")
	if s == nil || e == nil {
		return nil
	}
	start, end := item.Start, item.End
	if !item.AllDay {
		if item.StartZone == nil || item.EndZone == nil || *item.StartZone != *item.EndZone {
			return nil
		}
		var startErr, endErr error
		start, startErr = time.Parse(zimbraLocalAppointmentTime, strings.TrimSuffix(s.get("d"), "Z"))
		end, endErr = time.Parse(zimbraLocalAppointmentTime, strings.TrimSuffix(e.get("d"), "Z"))
		if startErr != nil || endErr != nil {
			return nil
		}
	}
	if end.Before(start) || !start.Truncate(24*time.Hour).Equal(item.Recurrence.StartDate) {
		return nil
	}
	schedule := &core.AppointmentSeriesSchedule{WallStart: start, WallEnd: end}
	loc := time.UTC
	if !item.AllDay && s.get("tz") != "" {
		var err error
		loc, err = time.LoadLocation(s.get("tz"))
		if err != nil {
			return nil
		}
	}
	valid := true
	appointmentComponent(detail, func(_, comp node) bool {
		if comp.get("ex") != "1" {
			return false
		}
		rid := exceptionRecurrenceID(comp)
		anchor, err := recurrenceStart(rid)
		if err != nil || (len(rid) == len(zimbraDay)) != item.AllDay {
			valid = false
			return true
		}
		wallAnchor := appointmentWallTime(anchor, loc)
		if comp.get("status") == "CANC" {
			schedule.SkippedDates = append(schedule.SkippedDates, wallAnchor.Truncate(24*time.Hour))
			return false
		}
		// This projection has no detached message for properties outside the
		// recurrence blob. Avoid silently replacing an exception's notes,
		// meeting participants or reminder with the master's values.
		allDay := comp.get("allDay") == "1"
		if allDay != item.AllDay || item.Participation != core.AppointmentPersonal ||
			comp.get("isOrg") != "1" || comp.child(mailNS, "at") != nil ||
			comp.child(mailNS, "alarm") != nil || master.child(mailNS, "alarm") != nil ||
			appointmentBody(comp) != item.Body {
			valid = false
			return true
		}
		start, end, err := appointmentDates(comp, allDay)
		if err != nil {
			valid = false
			return true
		}
		schedule.Exceptions = append(schedule.Exceptions, core.AppointmentSeriesException{
			WallStart: appointmentWallTime(start, loc), WallEnd: appointmentWallTime(end, loc),
			WallOriginalStart: wallAnchor, Subject: comp.get("name"), Location: comp.get("loc"),
		})
		return false
	})
	if !valid {
		return nil
	}
	return schedule
}

func appointmentWallTime(instant time.Time, zone *time.Location) time.Time {
	local := instant.In(zone)
	y, m, d := local.Date()
	h, min, s := local.Clock()
	return time.Date(y, m, d, h, min, s, local.Nanosecond(), time.UTC)
}
