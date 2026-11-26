// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"slices"
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
)

func parseAppointmentRecurrence(n node) (*core.AppointmentRecurrence, error) {
	if len(n.Children) != 2 {
		return nil, core.ErrInvalid
	}
	pattern, duration := n.Children[0], n.Children[1]
	recurrence := &core.AppointmentRecurrence{FirstDay: time.Sunday}
	switch {
	case isElement(pattern, types, "DailyRecurrence"):
		recurrence.Frequency = core.RecurrenceDaily
	case isElement(pattern, types, "WeeklyRecurrence"):
		recurrence.Frequency = core.RecurrenceWeekly
	default:
		return nil, core.ErrUnsupported
	}
	fields, err := recurrenceFields(pattern, "Interval", "DaysOfWeek", "FirstDayOfWeek")
	if err != nil {
		return nil, err
	}
	recurrence.Interval, err = strconv.Atoi(fields["Interval"])
	if err != nil || recurrence.Interval < 1 {
		return nil, core.ErrInvalid
	}
	switch recurrence.Frequency {
	case core.RecurrenceDaily:
		if len(fields) != 1 || recurrence.Interval > 999 {
			return nil, core.ErrInvalid
		}
	case core.RecurrenceWeekly:
		if recurrence.Interval > 99 {
			return nil, core.ErrInvalid
		}
		for token := range strings.FieldsSeq(fields["DaysOfWeek"]) {
			day, ok := appointmentWeekday(token)
			if !ok || slices.Contains(recurrence.Days, day) {
				return nil, core.ErrInvalid
			}
			recurrence.Days = append(recurrence.Days, day)
		}
		if len(recurrence.Days) == 0 {
			return nil, core.ErrInvalid
		}
		slices.Sort(recurrence.Days)
		if raw, exists := fields["FirstDayOfWeek"]; exists {
			day, ok := appointmentWeekday(raw)
			if !ok {
				return nil, core.ErrInvalid
			}
			recurrence.FirstDay = day
		}
	}
	fields, err = recurrenceFields(duration, "StartDate", "EndDate", "NumberOfOccurrences")
	if err != nil {
		return nil, err
	}
	recurrence.StartDate, err = recurrenceCivilDate(fields["StartDate"])
	if err != nil {
		return nil, err
	}
	switch {
	case isElement(duration, types, "NoEndRecurrence") && len(fields) == 1:
	case isElement(duration, types, "EndDateRecurrence") && len(fields) == 2:
		recurrence.Until, err = recurrenceCivilDate(fields["EndDate"])
		if err != nil || recurrence.Until.Before(recurrence.StartDate) {
			return nil, core.ErrInvalid
		}
	case isElement(duration, types, "NumberedRecurrence") && len(fields) == 2:
		recurrence.Count, err = strconv.Atoi(fields["NumberOfOccurrences"])
		if err != nil || recurrence.Count < 1 || recurrence.Count > 999 {
			return nil, core.ErrInvalid
		}
	default:
		return nil, core.ErrInvalid
	}
	return recurrence, nil
}

func appointmentWeekday(s string) (time.Weekday, bool) {
	for day := time.Sunday; day <= time.Saturday; day++ {
		if s == day.String() {
			return day, true
		}
	}
	return 0, false
}

func recurrenceFields(n node, allowed ...string) (map[string]string, error) {
	values := make(map[string]string, len(n.Children))
	for _, field := range n.Children {
		name := field.XMLName.Local
		if field.XMLName.Space != types || len(field.Children) != 0 || !slices.Contains(allowed, name) {
			return nil, core.ErrInvalid
		}
		if _, exists := values[name]; exists {
			return nil, core.ErrInvalid
		}
		values[name] = strings.TrimSpace(field.Text)
	}
	return values, nil
}

func recurrenceCivilDate(value string) (time.Time, error) {
	t, err := time.Parse(time.DateOnly, value)
	if err != nil {
		t, err = time.Parse("2006-01-02Z07:00", value)
	}
	if err != nil || t.Before(time.Date(1601, time.April, 1, 0, 0, 0, 0, t.Location())) {
		return time.Time{}, core.ErrInvalid
	}
	return appointmentTime(t, true)
}

func appointmentRecurrence(r core.AppointmentRecurrence) node {
	interval := el(types, "Interval", strconv.Itoa(r.Interval))
	pattern := el(types, "DailyRecurrence", "", interval)
	if r.Frequency == core.RecurrenceWeekly {
		days := make([]string, 0, len(r.Days))
		for _, day := range r.Days {
			days = append(days, day.String())
		}
		pattern = el(types, "WeeklyRecurrence", "", interval,
			el(types, "DaysOfWeek", strings.Join(days, " ")),
			el(types, "FirstDayOfWeek", r.FirstDay.String()))
	}
	begin := el(types, "StartDate", r.StartDate.Format(time.DateOnly))
	rng := el(types, "NoEndRecurrence", "", begin)
	if r.Count != 0 {
		rng = el(types, "NumberedRecurrence", "", begin, el(types, "NumberOfOccurrences", strconv.Itoa(r.Count)))
	} else if !r.Until.IsZero() {
		rng = el(types, "EndDateRecurrence", "", begin, el(types, "EndDate", r.Until.Format(time.DateOnly)))
	}
	return el(types, "Recurrence", "", pattern, rng)
}
