// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
)

func readAvailabilityZone(n *node) (core.AppointmentZone, bool) {
	if n == nil {
		return core.AppointmentZone{}, true
	}
	fields, ok := availabilityFields(*n, "Bias", "StandardTime", "DaylightTime")
	bias, biasOK := availabilityInt(fields["Bias"], -1439, 1439)
	if !ok || !biasOK {
		return core.AppointmentZone{}, false
	}
	standard, stdBias, stdOK := readAvailabilityTransition(fields["StandardTime"])
	daylight, dstBias, dstOK := readAvailabilityTransition(fields["DaylightTime"])
	zone := core.AppointmentZone{StandardOffset: -bias - stdBias, DaylightOffset: -bias - dstBias,
		Standard: standard, Daylight: daylight}
	if !stdOK || !dstOK || (standard.Month == 0) != (daylight.Month == 0) ||
		standard.Month != 0 && standard == daylight ||
		zone.StandardOffset <= -1440 || zone.StandardOffset >= 1440 ||
		zone.DaylightOffset <= -1440 || zone.DaylightOffset >= 1440 {
		return core.AppointmentZone{}, false
	}
	return zone, true
}

func readAvailabilityTransition(n *node) (core.AppointmentZoneTransition, int, bool) {
	if n == nil {
		return core.AppointmentZoneTransition{}, 0, false
	}
	fields, ok := availabilityFields(*n, "Bias", "Time", "DayOrder", "Month", "DayOfWeek")
	bias, biasOK := availabilityInt(fields["Bias"], -1439, 1439)
	month, monthOK := availabilityInt(fields["Month"], 0, 12)
	if !ok || !biasOK || !monthOK {
		return core.AppointmentZoneTransition{}, 0, false
	}
	if month == 0 {
		return core.AppointmentZoneTransition{}, bias, true
	}
	week, weekOK := availabilityInt(fields["DayOrder"], 1, 5)
	hour, err := time.Parse("15:04:05", availabilityText(fields["Time"]))
	if !weekOK || err != nil || hour.Nanosecond() != 0 {
		return core.AppointmentZoneTransition{}, 0, false
	}
	for day := time.Sunday; day <= time.Saturday; day++ {
		if day.String() == availabilityText(fields["DayOfWeek"]) {
			return core.AppointmentZoneTransition{Month: time.Month(month), Week: week, Weekday: day,
				Hour: hour.Hour(), Minute: hour.Minute(), Second: hour.Second()}, bias, true
		}
	}
	return core.AppointmentZoneTransition{}, 0, false
}

func availabilityText(n *node) string {
	if n == nil || len(n.Children) != 0 {
		return ""
	}
	return strings.TrimSpace(n.Text)
}

func availabilityInt(n *node, min, max int) (int, bool) {
	value, err := strconv.Atoi(availabilityText(n))
	return value, err == nil && value >= min && value <= max
}

func availabilityTime(n *node, zone core.AppointmentZone) (time.Time, bool) {
	value := availabilityText(n)
	if stamp, err := time.Parse(time.RFC3339Nano, value); err == nil {
		return stamp.UTC(), true
	}
	wall, err := time.Parse("2006-01-02T15:04:05", value)
	if err != nil {
		return time.Time{}, false
	}
	standard := wall.Add(-time.Duration(zone.StandardOffset) * time.Minute)
	daylight := wall.Add(-time.Duration(zone.DaylightOffset) * time.Minute)
	stdValid := zone.OffsetAt(standard) == zone.StandardOffset
	dstValid := !standard.Equal(daylight) && zone.OffsetAt(daylight) == zone.DaylightOffset
	// A repeated or missing civil time needs an explicit offset.
	switch {
	case stdValid && !dstValid:
		return standard, true
	case dstValid && !stdValid:
		return daylight, true
	default:
		return time.Time{}, false
	}
}

func availabilityDisplayTime(stamp time.Time, zone core.AppointmentZone) string {
	offset := zone.OffsetAt(stamp)
	return stamp.In(time.FixedZone("", offset*60)).Format(time.RFC3339Nano)
}
