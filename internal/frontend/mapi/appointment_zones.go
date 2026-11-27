// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/binary"
	"time"
	"unicode/utf16"

	"ewsgo/internal/core"
)

// A floating appointment uses the effective rule in StartDisplay for both
// bounds, regardless of the current rules for the zone name (MS-OXOCAL 3.1.5.5.1).
func decodeAppointmentDisplayZone(data core.Binary) (core.AppointmentZone, error) {
	return decodeAppointmentZoneDefinition(data, false)
}

func decodeAppointmentZoneDefinition(data core.Binary, recurring bool) (core.AppointmentZone, error) {
	const ruleSize = 66
	word := binary.LittleEndian.Uint16
	if len(data) < 10 || data[0] != 2 || data[1] < 1 || data[1] > 2 || word(data[4:]) != 2 {
		return core.AppointmentZone{}, core.ErrInvalid
	}
	nameUnits := int(word(data[6:]))
	countAt := 8 + nameUnits*2
	if nameUnits == 0 || nameUnits > 260 || countAt+2 > len(data) || int(word(data[2:])) != countAt-2 {
		return core.AppointmentZone{}, core.ErrInvalid
	}
	count := int(word(data[countAt:]))
	rules := data[countAt+2:]
	if count == 0 || count > 1024 || len(rules) != count*ruleSize {
		return core.AppointmentZone{}, core.ErrInvalid
	}
	var selected []byte
	flag := uint16(2)
	if recurring {
		flag = 1
	}
	for len(rules) != 0 {
		rule := rules[:ruleSize]
		rules = rules[ruleSize:]
		flags := word(rule[4:])
		if flags&flag == 0 {
			continue
		}
		if selected != nil || rule[0] != 2 || rule[1] < 1 || rule[1] > 2 || word(rule[2:]) != 0x3e || flags&^3 != 0 {
			return core.AppointmentZone{}, core.ErrInvalid
		}
		selected = rule
	}
	if selected == nil {
		return core.AppointmentZone{}, core.ErrInvalid
	}
	units := make([]uint16, nameUnits)
	for i := range units {
		units[i] = word(data[8+2*i:])
	}
	name := string(utf16.Decode(units))
	if !validMailText(name) {
		return core.AppointmentZone{}, core.ErrInvalid
	}
	zone, err := decodeAppointmentZoneRule(selected[22:34], selected[34:50], selected[50:66])
	zone.Name = name
	return zone, err
}

func decodeAppointmentZoneStruct(data core.Binary) (core.AppointmentZone, error) {
	word := binary.LittleEndian.Uint16
	if len(data) != 48 || word(data[12:]) != 0 || word(data[30:]) != 0 {
		return core.AppointmentZone{}, core.ErrUnsupported
	}
	return decodeAppointmentZoneRule(data[:12], data[14:30], data[32:48])
}

func decodeAppointmentZoneRule(biases, standardDate, daylightDate []byte) (core.AppointmentZone, error) {
	bias := func(at int) int64 {
		return int64(int32(binary.LittleEndian.Uint32(biases[at:])))
	}
	standard, daylight := -bias(0)-bias(4), -bias(0)-bias(8)
	if standard < -14*60 || standard > 14*60 || daylight < -14*60 || daylight > 14*60 {
		return core.AppointmentZone{}, core.ErrInvalid
	}
	fromDaylight, err := decodeAppointmentTransition(standardDate)
	if err != nil {
		return core.AppointmentZone{}, err
	}
	toDaylight, err := decodeAppointmentTransition(daylightDate)
	if err != nil {
		return core.AppointmentZone{}, err
	}
	if (fromDaylight.Month == 0) != (toDaylight.Month == 0) {
		return core.AppointmentZone{}, core.ErrInvalid
	}
	return core.AppointmentZone{
		StandardOffset: int(standard), DaylightOffset: int(daylight),
		Standard: fromDaylight, Daylight: toDaylight,
	}, nil
}

func decodeAppointmentTransition(data []byte) (core.AppointmentZoneTransition, error) {
	word := binary.LittleEndian.Uint16
	month, week, weekday := word(data[2:]), word(data[6:]), word(data[4:])
	hour, min, sec := word(data[8:]), word(data[10:]), word(data[12:])
	if month == 0 {
		return core.AppointmentZoneTransition{}, nil
	}
	if word(data) != 0 || word(data[14:]) != 0 {
		return core.AppointmentZoneTransition{}, core.ErrUnsupported
	}
	if month > 12 || week < 1 || week > 5 || weekday > 6 || hour > 23 || min > 59 || sec > 59 {
		return core.AppointmentZoneTransition{}, core.ErrInvalid
	}
	return core.AppointmentZoneTransition{
		Month: time.Month(month), Week: int(week), Weekday: time.Weekday(weekday),
		Hour: int(hour), Minute: int(min), Second: int(sec),
	}, nil
}

func appointmentCivilMidnight(stamp time.Time, zone core.AppointmentZone) (time.Time, error) {
	offset := zone.OffsetAt(stamp)
	civil := stamp.Add(time.Duration(offset) * time.Minute)
	if civil.Year() < 1601 || civil.Year() > 9999 || !civil.Equal(civil.Truncate(24*time.Hour)) {
		return time.Time{}, core.ErrInvalid
	}
	return civil, nil
}

// MS-OXOCAL 2.2.1.41–43. Display properties carry the effective native
// rule without marking it as the rule for a recurrence pattern.
func encodeAppointmentZoneDefinition(zone *core.AppointmentZone, recurring bool) (core.Binary, bool) {
	if zone == nil || zone.Name == "" || len(zone.Name) > 4*260 || !validMailText(zone.Name) {
		return nil, false
	}
	key := contentsUnicode(zone.Name)
	key = key[:len(key)-2] // TZDEFINITION uses an unterminated name.
	if len(key)/2 > 260 {
		return nil, false
	}
	data := binary.LittleEndian.AppendUint16([]byte{2, 1}, uint16(6+len(key)))
	data = binary.LittleEndian.AppendUint16(data, 2) // Valid key name.
	data = binary.LittleEndian.AppendUint16(data, uint16(len(key)/2))
	data = append(data, key...)
	data = binary.LittleEndian.AppendUint16(data, 1) // One rule.
	data = append(data, 2, 1, 0x3e, 0x00)
	flags := uint16(2) // TZRULE_FLAG_EFFECTIVE_TZREG.
	if recurring {
		flags |= 1 // TZRULE_FLAG_RECUR_CURRENT_TZREG.
	}
	data = binary.LittleEndian.AppendUint16(data, flags)
	data = binary.LittleEndian.AppendUint16(data, 1601)
	data = append(data, make([]byte, 14)...)
	data = appendAppointmentBiases(data, *zone)
	data = appendAppointmentTransition(data, zone.Standard)
	data = appendAppointmentTransition(data, zone.Daylight)
	return core.Binary(data), true
}

func encodeAppointmentZoneStruct(zone core.AppointmentZone) core.Binary {
	data := appendAppointmentBiases(nil, zone)
	data = binary.LittleEndian.AppendUint16(data, 0) // Standard year is relative.
	data = appendAppointmentTransition(data, zone.Standard)
	data = binary.LittleEndian.AppendUint16(data, 0) // Daylight year is relative.
	return core.Binary(appendAppointmentTransition(data, zone.Daylight))
}

func appendAppointmentBiases(data []byte, zone core.AppointmentZone) []byte {
	// Windows biases are the minutes added to local time to obtain UTC.
	data = binary.LittleEndian.AppendUint32(data, uint32(int32(-zone.StandardOffset)))
	data = binary.LittleEndian.AppendUint32(data, 0) // Standard bias relative to base.
	return binary.LittleEndian.AppendUint32(data, uint32(int32(zone.StandardOffset-zone.DaylightOffset)))
}

func appendAppointmentTransition(out []byte, t core.AppointmentZoneTransition) []byte {
	for _, field := range [...]uint16{0, uint16(t.Month), uint16(t.Weekday), uint16(t.Week),
		uint16(t.Hour), uint16(t.Minute), uint16(t.Second), 0} {
		out = binary.LittleEndian.AppendUint16(out, field)
	}
	return out
}
