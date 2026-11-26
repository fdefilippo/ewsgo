// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"encoding/xml"
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
)

func appointmentZone(invite node, bound *node) *core.AppointmentZone {
	if bound == nil {
		return nil
	}
	name := bound.get("tz")
	if name == "" {
		if strings.HasSuffix(bound.get("d"), "Z") {
			return &core.AppointmentZone{Name: "UTC"}
		}
		return nil
	}
	var match *node
	for i := range invite.Children {
		child := &invite.Children[i]
		if child.XMLName == (xml.Name{Space: mailNS, Local: "tz"}) && child.get("id") == name {
			if match != nil {
				return nil
			}
			match = child
		}
	}
	if match == nil {
		return nil
	}
	standard, err := strconv.Atoi(match.get("stdoff"))
	if err != nil || standard <= -24*60 || standard >= 24*60 {
		return nil
	}
	zone := &core.AppointmentZone{Name: name, StandardOffset: standard, DaylightOffset: standard}
	std, dst := match.child(mailNS, "standard"), match.child(mailNS, "daylight")
	if std == nil && dst == nil && match.get("dayoff") == "" {
		return zone
	}
	daylight, err := strconv.Atoi(match.get("dayoff"))
	if err != nil || daylight <= -24*60 || daylight >= 24*60 {
		return nil
	}
	zone.DaylightOffset = daylight
	if std == nil && dst == nil && daylight == standard {
		return zone
	}
	seasonStart, okStd := appointmentTransition(std)
	daylightStart, okDst := appointmentTransition(dst)
	if !okStd || !okDst || seasonStart == daylightStart || daylight == standard {
		return nil
	}
	zone.Standard, zone.Daylight = seasonStart, daylightStart
	return zone
}

func appointmentTransition(n *node) (core.AppointmentZoneTransition, bool) {
	if n == nil || n.get("mday") != "" {
		// A recurring SYSTEMTIME can express a weekday ordinal.
		return core.AppointmentZoneTransition{}, false
	}
	fields := [...]struct {
		name     string
		min, max int
	}{{"mon", 1, 12}, {"week", -1, 4}, {"wkday", 1, 7}, {"hour", 0, 23}, {"min", 0, 59}, {"sec", 0, 59}}
	var values [len(fields)]int
	for i, field := range fields {
		value, err := strconv.Atoi(n.get(field.name))
		if err != nil || value < field.min || value > field.max {
			return core.AppointmentZoneTransition{}, false
		}
		values[i] = value
	}
	month, week, day := values[0], values[1], values[2]
	if week == 0 {
		return core.AppointmentZoneTransition{}, false
	}
	if week == -1 {
		week = 5
	}
	return core.AppointmentZoneTransition{Month: time.Month(month), Week: week, Weekday: time.Weekday(day - 1),
		Hour: values[3], Minute: values[4], Second: values[5]}, true
}
