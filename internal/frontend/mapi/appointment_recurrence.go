// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/binary"
	"math"
	"slices"
	"strings"
	"time"

	"ewsgo/internal/core"
)

const minutesPerDay = 24 * 60

var recurrenceEpoch = time.Date(1601, time.January, 1, 0, 0, 0, 0, time.UTC)

const recurrenceNoEndDate = 0x5ae980df

type appointmentRecurrenceEncoding struct {
	data               core.Binary
	kind               uint32
	startDate, endDate uint32
}

// MS-OXOCAL 2.2.1.44.1 and 2.2.1.44.5. The input contains local
// coordinates. Counting in UTC instants would skip or duplicate civil slots
// around a daylight transition.
func encodeAppointmentRecurrence(item core.Appointment) (appointmentRecurrenceEncoding, bool) {
	r, schedule := item.Recurrence, item.SeriesSchedule
	if r == nil || schedule == nil || r.Interval < 1 || r.Interval > 999 ||
		r.Count < 0 || r.Count > 999 || r.Count != 0 && !r.Until.IsZero() {
		return appointmentRecurrenceEncoding{}, false
	}
	startDate, ok := recurrenceMinutes(r.StartDate)
	if !ok || startDate%minutesPerDay != 0 {
		return appointmentRecurrenceEncoding{}, false
	}
	startOffset := (schedule.WallStart.Unix() - r.StartDate.Unix()) / 60
	endOffset := (schedule.WallEnd.Unix() - r.StartDate.Unix()) / 60
	if startOffset < 0 || startOffset >= minutesPerDay || endOffset < startOffset || endOffset > math.MaxUint32 ||
		schedule.WallStart.Unix()%60 != 0 || schedule.WallEnd.Unix()%60 != 0 ||
		schedule.WallStart.Nanosecond() != 0 || schedule.WallEnd.Nanosecond() != 0 {
		return appointmentRecurrenceEncoding{}, false
	}

	const daily, weekly = 0x200a, 0x200b
	frequency, period, cycle, recurrenceType := uint16(daily), uint32(r.Interval)*minutesPerDay, r.Interval, uint32(1)
	var firstDOW, dayMask uint32
	days, startDay := []int{0}, 0
	if r.Frequency == core.RecurrenceWeekly {
		if r.Interval > 99 || r.FirstDay < time.Sunday || r.FirstDay > time.Saturday {
			return appointmentRecurrenceEncoding{}, false
		}
		frequency, period, cycle, recurrenceType = weekly, uint32(r.Interval), 7*r.Interval, 2
		firstDOW = uint32(r.FirstDay)
		startDay = (int(r.StartDate.Weekday()-r.FirstDay) + 7) % 7
		days = make([]int, 0, len(r.Days))
		for _, weekday := range r.Days {
			if weekday < time.Sunday || weekday > time.Saturday || dayMask&(1<<weekday) != 0 {
				return appointmentRecurrenceEncoding{}, false
			}
			dayMask |= 1 << weekday
			days = append(days, (int(weekday-r.FirstDay)+7)%7)
		}
		slices.Sort(days)
	} else if r.Frequency != core.RecurrenceDaily || len(r.Days) != 0 {
		return appointmentRecurrenceEncoding{}, false
	}
	firstIndex := slices.Index(days, startDay)
	if firstIndex < 0 {
		return appointmentRecurrenceEncoding{}, false
	}
	const endAfterDate, endAfterCount, noEnd = 0x2021, 0x2022, 0x2023
	endType, count, endDate := uint32(noEnd), uint32(10), uint32(recurrenceNoEndDate)
	if r.Count != 0 {
		index := firstIndex + r.Count - 1
		delta := index/len(days)*cycle + days[index%len(days)] - startDay
		last, ok := recurrenceMinutes(r.StartDate.AddDate(0, 0, delta))
		if !ok {
			return appointmentRecurrenceEncoding{}, false
		}
		endType, count, endDate = endAfterCount, uint32(r.Count), last
	} else if !r.Until.IsZero() {
		limit, ok := recurrenceMinutes(r.Until)
		if !ok || limit < startDate || limit%minutesPerDay != 0 {
			return appointmentRecurrenceEncoding{}, false
		}
		window := int((limit-startDate)/minutesPerDay) + startDay
		periods, position := window/cycle, window%cycle
		lastIndex := len(days) - 1
		for lastIndex >= 0 && days[lastIndex] > position {
			lastIndex--
		}
		if lastIndex < 0 {
			periods--
			lastIndex = len(days) - 1
		}
		lastDay := periods*cycle + days[lastIndex] - startDay
		endType, count, endDate = endAfterDate, uint32(periods*len(days)+lastIndex-firstIndex+1), startDate+uint32(lastDay*minutesPerDay)
	}
	occurrenceIndex := func(day uint32) int {
		if day < startDate || endType != noEnd && day > endDate {
			return -1
		}
		return slices.Index(days, (int((day-startDate)/minutesPerDay)+startDay)%cycle)
	}
	if len(schedule.SkippedDates)+len(schedule.Exceptions) > (math.MaxUint16-80)/4 {
		return appointmentRecurrenceEncoding{}, false
	}
	skipped := make([]uint32, 0, len(schedule.SkippedDates)+len(schedule.Exceptions))
	for _, date := range schedule.SkippedDates {
		day, ok := recurrenceMinutes(date)
		if !ok || day%minutesPerDay != 0 || occurrenceIndex(day) < 0 {
			return appointmentRecurrenceEncoding{}, false
		}
		skipped = append(skipped, day)
	}
	exceptions := make([]recurrenceException, 0, len(schedule.Exceptions))
	for _, exception := range schedule.Exceptions {
		wire, ok := projectRecurrenceException(item, exception)
		originalDay := recurrenceDay(wire.original)
		index := occurrenceIndex(originalDay)
		if !ok || wire.original%minutesPerDay != uint32(startOffset) || index < 0 {
			return appointmentRecurrenceEncoding{}, false
		}
		previous := days[len(days)-1] - cycle
		if index > 0 {
			previous = days[index-1]
		}
		following := days[0] + cycle
		if index+1 < len(days) {
			following = days[index+1]
		}
		previousDay := int64(originalDay) + int64(previous-days[index])*minutesPerDay
		followingDay := int64(originalDay) + int64(following-days[index])*minutesPerDay
		newDay := int64(recurrenceDay(wire.start))
		if previousDay >= int64(startDate) && (int64(wire.start) < previousDay+endOffset || newDay <= previousDay) ||
			(endType == noEnd || followingDay <= int64(endDate)) && (int64(wire.end) > followingDay+startOffset || newDay >= followingDay) {
			return appointmentRecurrenceEncoding{}, false
		}
		if slices.Contains(skipped, originalDay) {
			return appointmentRecurrenceEncoding{}, false
		}
		skipped = append(skipped, originalDay)
		exceptions = append(exceptions, wire)
	}
	slices.SortFunc(exceptions, func(a, b recurrenceException) int {
		return int(int64(a.start) - int64(b.start))
	})
	for i := 1; i < len(exceptions); i++ {
		if exceptions[i-1].end > exceptions[i].start || recurrenceDay(exceptions[i-1].start) == recurrenceDay(exceptions[i].start) {
			return appointmentRecurrenceEncoding{}, false
		}
	}
	slices.Sort(skipped)
	skipped = slices.Compact(skipped)

	weekOrigin := int64(startDate) - int64(startDay*minutesPerDay)
	modulus := int64(cycle * minutesPerDay)
	firstDateTime := uint32((weekOrigin%modulus + modulus) % modulus)
	data := []byte{0x04, 0x30, 0x04, 0x30}
	data = binary.LittleEndian.AppendUint16(data, frequency)
	data = binary.LittleEndian.AppendUint16(data, uint16(frequency-daily))
	data = binary.LittleEndian.AppendUint16(data, 0) // Gregorian.
	data = append32(data, firstDateTime)
	data = append32(data, period)
	data = append32(data, 0) // Not a sliding task.
	if frequency == weekly {
		data = append32(data, dayMask)
	}
	for _, v := range []uint32{endType, count, firstDOW, uint32(len(skipped))} {
		data = append32(data, v)
	}
	for _, v := range skipped {
		data = append32(data, v)
	}
	data = append32(data, uint32(len(exceptions)))
	for _, exception := range exceptions {
		data = append32(data, recurrenceDay(exception.start))
	}
	for _, v := range []uint32{startDate, endDate, 0x3006, 0x3009, uint32(startOffset), uint32(endOffset)} {
		data = append32(data, v)
	}
	data = binary.LittleEndian.AppendUint16(data, uint16(len(exceptions)))
	for _, exception := range exceptions {
		data = exception.appendInfo(data)
		if len(data) > math.MaxUint16 {
			return appointmentRecurrenceEncoding{}, false
		}
	}
	data = append32(data, 0) // ReservedBlock1Size.
	for _, exception := range exceptions {
		data = exception.appendExtended(data)
		if len(data) > math.MaxUint16-4 {
			return appointmentRecurrenceEncoding{}, false
		}
	}
	data = append32(data, 0) // ReservedBlock2Size.
	return appointmentRecurrenceEncoding{data: core.Binary(data), kind: recurrenceType, startDate: startDate, endDate: endDate}, true
}

const recurrenceOverrideSubject, recurrenceOverrideLocation = 1, 0x10

type recurrenceException struct {
	start, end, original uint32
	flags                uint16
	subject, location    string
}

func projectRecurrenceException(master core.Appointment, exception core.AppointmentSeriesException) (recurrenceException, bool) {
	start, startOK := recurrenceMinutes(exception.WallStart)
	end, endOK := recurrenceMinutes(exception.WallEnd)
	original, originalOK := recurrenceMinutes(exception.WallOriginalStart)
	if !startOK || !endOK || !originalOK || end < start ||
		master.AllDay && (start != recurrenceDay(start) || end != recurrenceDay(end) || start == end) {
		return recurrenceException{}, false
	}
	wire := recurrenceException{start: start, end: end, original: original}
	if exception.Subject != master.Subject {
		wire.flags |= recurrenceOverrideSubject
		wire.subject = exception.Subject
	}
	if exception.Location != master.Location {
		wire.flags |= recurrenceOverrideLocation
		wire.location = exception.Location
	}
	// Both string representations have 16-bit lengths. Limit the source
	// size before allocating either encoding.
	if len(wire.subject)+len(wire.location) > math.MaxUint16/4 ||
		strings.ContainsRune(wire.subject, 0) || strings.ContainsRune(wire.location, 0) {
		return recurrenceException{}, false
	}
	return wire, true
}

func recurrenceDay(minutes uint32) uint32 { return minutes / minutesPerDay * minutesPerDay }

func (e recurrenceException) appendTimes(data []byte) []byte {
	data = append32(data, e.start)
	data = append32(data, e.end)
	return append32(data, e.original)
}

func (e recurrenceException) appendInfo(data []byte) []byte {
	data = binary.LittleEndian.AppendUint16(e.appendTimes(data), e.flags)
	for _, field := range []struct {
		mask  uint16
		value string
	}{{recurrenceOverrideSubject, e.subject}, {recurrenceOverrideLocation, e.location}} {
		if e.flags&field.mask != 0 {
			ansi, _ := propertyString8(field.value, 1252)
			data = binary.LittleEndian.AppendUint16(data, uint16(len(ansi)))
			data = binary.LittleEndian.AppendUint16(data, uint16(len(ansi)-1))
			data = append(data, ansi[:len(ansi)-1]...)
		}
	}
	return data
}

func (e recurrenceException) appendExtended(data []byte) []byte {
	data = append32(data, 4) // ChangeHighlightSize.
	data = append32(data, 0) // ChangeHighlightValue.
	data = append32(data, 0) // ReservedBlockEE1Size.
	if e.flags == 0 {
		return data
	}
	data = e.appendTimes(data)
	for _, field := range []struct {
		mask  uint16
		value string
	}{{recurrenceOverrideSubject, e.subject}, {recurrenceOverrideLocation, e.location}} {
		if e.flags&field.mask != 0 {
			wide := contentsUnicode(field.value)
			data = binary.LittleEndian.AppendUint16(data, uint16(len(wide)/2-1))
			data = append(data, wide[:len(wide)-2]...)
		}
	}
	return append32(data, 0) // ReservedBlockEE2Size.
}

func recurrenceMinutes(t time.Time) (uint32, bool) {
	if t.Before(recurrenceEpoch) || t.Year() > 9999 || t.Unix()%60 != 0 || t.Nanosecond() != 0 {
		return 0, false
	}
	minutes := (t.Unix() - recurrenceEpoch.Unix()) / 60
	return uint32(minutes), minutes <= math.MaxUint32
}
