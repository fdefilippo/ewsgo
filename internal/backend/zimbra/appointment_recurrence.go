// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"slices"
	"strconv"
	"time"

	"ewsgo/internal/core"
)

var nativeWeekdays = [...]string{"SU", "MO", "TU", "WE", "TH", "FR", "SA"}

func nativeRecurrence(r core.AppointmentRecurrence, start time.Time, allDay bool) (node, error) {
	first := time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	if !r.StartDate.Equal(first) || r.Interval < 1 || r.Count < 0 || r.Count > 999 ||
		(!r.Until.IsZero() && (r.Count != 0 || r.Until.Before(first) || !atMidnightUTC(r.Until))) {
		return node{}, core.ErrInvalid
	}
	frequency := "DAI"
	parts := []node{attr(element(mailNS, "interval", ""), "ival", strconv.Itoa(r.Interval))}
	switch r.Frequency {
	case core.RecurrenceDaily:
		if r.Interval > 999 || len(r.Days) != 0 {
			return node{}, core.ErrInvalid
		}
	case core.RecurrenceWeekly:
		if r.Interval > 99 || r.FirstDay < time.Sunday || r.FirstDay > time.Saturday || !slices.Contains(r.Days, start.Weekday()) {
			return node{}, core.ErrInvalid
		}
		frequency = "WEE"
		days := element(mailNS, "byday", "")
		seen := [7]bool{}
		for _, d := range r.Days {
			if d < time.Sunday || d > time.Saturday || seen[d] {
				return node{}, core.ErrInvalid
			}
			seen[d] = true
			days.Children = append(days.Children, attr(element(mailNS, "wkday", ""), "day", nativeWeekdays[d]))
		}
		parts = append(parts, days, attr(element(mailNS, "wkst", ""), "day", nativeWeekdays[r.FirstDay]))
	default:
		return node{}, core.ErrUnsupported
	}
	if r.Count != 0 {
		parts = append(parts, attr(element(mailNS, "count", ""), "num", strconv.Itoa(r.Count)))
	}
	if !r.Until.IsZero() {
		date := r.Until.Format(zimbraDay)
		if !allDay {
			endOfDay := time.Date(r.Until.Year(), r.Until.Month(), r.Until.Day(), 23, 59, 59, 0, start.Location()).UTC()
			if endOfDay.Year() > 9999 {
				return node{}, core.ErrInvalid
			}
			date = endOfDay.Format(zimbraRecurrenceStart)
		}
		parts = append(parts, attr(element(mailNS, "until", ""), "d", date))
	}
	rule := attr(element(mailNS, "rule", "", parts...), "freq", frequency)
	return element(mailNS, "recur", "", element(mailNS, "add", "", rule)), nil
}

func readAppointmentRecurrence(comp node, start time.Time, allDay bool) (string, *core.AppointmentRecurrence) {
	recur, startBound := comp.child(mailNS, "recur"), comp.child(mailNS, "s")
	if recur == nil || len(recur.Children) != 1 || startBound == nil {
		return "", nil
	}
	add := recur.child(mailNS, "add")
	if add == nil || len(add.Children) != 1 {
		return "", nil
	}
	rule := add.child(mailNS, "rule")
	if rule == nil {
		return "", nil
	}
	zone := startBound.get("tz")
	if zone != "" && !allDay {
		location, err := time.LoadLocation(zone)
		if err != nil || zone == "Local" {
			return "", nil
		}
		start = start.In(location)
	}
	r := &core.AppointmentRecurrence{Interval: 1, FirstDay: time.Monday}
	r.StartDate = time.Date(start.Year(), start.Month(), start.Day(), 0, 0, 0, 0, time.UTC)
	switch rule.get("freq") {
	case "DAI":
		r.Frequency = core.RecurrenceDaily
		r.FirstDay = time.Sunday
	case "WEE":
		r.Frequency = core.RecurrenceWeekly
	default:
		return zone, nil
	}
	seen := make(map[string]bool, len(rule.Children))
	for _, part := range rule.Children {
		name := part.XMLName.Local
		if part.XMLName.Space != mailNS || seen[name] {
			return zone, nil
		}
		seen[name] = true
		switch name {
		case "interval":
			r.Interval, _ = strconv.Atoi(part.get("ival"))
		case "count":
			r.Count, _ = strconv.Atoi(part.get("num"))
			if r.Count < 1 {
				return zone, nil
			}
		case "until":
			until, err := recurrenceStart(part.get("d"))
			if err != nil {
				return zone, nil
			}
			if !allDay && len(part.get("d")) != len(zimbraDay) {
				until = until.In(start.Location())
				secondsInDay := func(value time.Time) int { return 3600*value.Hour() + 60*value.Minute() + value.Second() }
				if secondsInDay(until) < secondsInDay(start) {
					until = until.AddDate(0, 0, -1)
				}
			}
			r.Until = time.Date(until.Year(), until.Month(), until.Day(), 0, 0, 0, 0, time.UTC)
		case "wkst":
			if r.Frequency == core.RecurrenceDaily { // Native default can be explicit.
				continue
			}
			day := slices.Index(nativeWeekdays[:], part.get("day"))
			if day < 0 {
				return zone, nil
			}
			r.FirstDay = time.Weekday(day)
		case "byday":
			for _, field := range part.Children {
				day := slices.Index(nativeWeekdays[:], field.get("day"))
				if field.XMLName.Space != mailNS || field.XMLName.Local != "wkday" || day < 0 ||
					field.get("ordwk") != "" || slices.Contains(r.Days, time.Weekday(day)) {
					return zone, nil
				}
				r.Days = append(r.Days, time.Weekday(day))
			}
		default:
			return zone, nil
		}
	}
	slices.Sort(r.Days)
	if _, err := nativeRecurrence(*r, start, allDay); err != nil {
		return zone, nil
	}
	return zone, r
}

func sameAppointmentRecurrence(want, got *core.AppointmentRecurrence) bool {
	if want == nil || got == nil {
		return want == got
	}
	return want.Frequency == got.Frequency && want.Interval == got.Interval && want.Count == got.Count &&
		want.StartDate.Equal(got.StartDate) && want.Until.Equal(got.Until) && want.FirstDay == got.FirstDay &&
		slices.Equal(want.Days, got.Days)
}
