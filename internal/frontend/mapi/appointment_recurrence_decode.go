// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/binary"
	"math"
	"slices"
	"time"
	"unicode/utf16"

	"ewsgo/internal/core"
)

// Read the daily and weekly recurrence shapes representable by the calendar
// backend. Exception text comes from the ExtendedException Unicode fields.
func decodeAppointmentRecurrence(data core.Binary, master core.Appointment) (*core.AppointmentRecurrence, *core.AppointmentSeriesSchedule, error) {
	if len(data) > math.MaxUint16 {
		return nil, nil, core.ErrLimit
	}
	r := reader{data: data}
	if recurrenceUint16(&r) != 0x3004 || recurrenceUint16(&r) != 0x3004 {
		return nil, nil, core.ErrInvalid
	}
	frequency, pattern, calendar := recurrenceUint16(&r), recurrenceUint16(&r), recurrenceUint16(&r)
	first, period, sliding := r.u32(), r.u32(), r.u32()
	if calendar > 1 || sliding != 0 {
		return nil, nil, core.ErrUnsupported
	}
	recurrence := new(core.AppointmentRecurrence)
	var mask uint32
	switch {
	case frequency == 0x200a && pattern == 0 && period != 0 && period%minutesPerDay == 0 && period/minutesPerDay <= 999:
		recurrence.Frequency, recurrence.Interval = core.RecurrenceDaily, int(period/minutesPerDay)
	case frequency == 0x200b && pattern == 1 && period > 0 && period <= 99:
		recurrence.Frequency, recurrence.Interval = core.RecurrenceWeekly, int(period)
		mask = r.u32()
		if mask == 0 || mask > 0x7f {
			return nil, nil, core.ErrInvalid
		}
		for day := time.Sunday; day <= time.Saturday; day++ {
			if mask&(1<<day) != 0 {
				recurrence.Days = append(recurrence.Days, day)
			}
		}
	default:
		return nil, nil, core.ErrUnsupported
	}
	endType, count, firstDOW := r.u32(), r.u32(), r.u32()
	if firstDOW > 6 {
		return nil, nil, core.ErrInvalid
	}
	if recurrence.Frequency == core.RecurrenceWeekly {
		recurrence.FirstDay = time.Weekday(firstDOW)
	}
	deleted, modified := recurrenceDateList(&r), recurrenceDateList(&r)
	start, end := r.u32(), r.u32()
	if start%minutesPerDay != 0 || end < start {
		return nil, nil, core.ErrInvalid
	}
	recurrence.StartDate = recurrenceWall(start)
	switch endType {
	case 0x2021:
		recurrence.Until = recurrenceWall(end)
	case 0x2022:
		if count == 0 || count > 999 {
			return nil, nil, core.ErrUnsupported
		}
		recurrence.Count = int(count)
	case 0x2023, math.MaxUint32:
		if end != recurrenceNoEndDate {
			return nil, nil, core.ErrInvalid
		}
	default:
		return nil, nil, core.ErrInvalid
	}
	readVersion, writeVersion := r.u32(), r.u32()
	startOffset, endOffset, exceptionCount := r.u32(), r.u32(), int(recurrenceUint16(&r))
	if readVersion != 0x3006 || writeVersion < 0x3006 || writeVersion > 0x3009 ||
		startOffset >= minutesPerDay || endOffset < startOffset || endOffset > math.MaxUint32-start ||
		exceptionCount != len(modified) || exceptionCount > len(r.data[r.pos:])/14 {
		return nil, nil, core.ErrInvalid
	}
	entries := make([]recurrenceException, exceptionCount)
	for i := range entries {
		e := &entries[i]
		e.start, e.end, e.original, e.flags = r.u32(), r.u32(), r.u32(), recurrenceUint16(&r)
		if e.flags&^(recurrenceOverrideSubject|recurrenceOverrideLocation) != 0 {
			return nil, nil, appointmentImportRejection{importAppointmentRecurrenceException, core.ErrUnsupported}
		}
		for _, flag := range []uint16{recurrenceOverrideSubject, recurrenceOverrideLocation} {
			if e.flags&flag != 0 {
				withNUL, length := recurrenceUint16(&r), recurrenceUint16(&r)
				if int(withNUL) != int(length)+1 {
					return nil, nil, core.ErrInvalid
				}
				r.take(int(length))
			}
		}
	}
	r.blob(math.MaxUint16) // ReservedBlock1.
	schedule := &core.AppointmentSeriesSchedule{WallStart: recurrenceWall(start + startOffset), WallEnd: recurrenceWall(start + endOffset)}
	for i, e := range entries {
		if writeVersion >= 0x3009 {
			if len(r.blob(math.MaxUint16)) < 4 { // ChangeHighlight.
				return nil, nil, core.ErrInvalid
			}
		}
		r.blob(math.MaxUint16) // ReservedBlockEE1.
		if e.flags != 0 {
			if r.u32() != e.start || r.u32() != e.end || r.u32() != e.original {
				return nil, nil, core.ErrInvalid
			}
			if e.flags&recurrenceOverrideSubject != 0 {
				e.subject = recurrenceText(&r)
			}
			if e.flags&recurrenceOverrideLocation != 0 {
				e.location = recurrenceText(&r)
			}
			r.blob(math.MaxUint16) // ReservedBlockEE2.
		}
		anchor := recurrenceDay(e.original)
		index, exists := slices.BinarySearch(deleted, anchor)
		if !exists || recurrenceDay(e.start) != modified[i] || !validMailText(e.subject) || !validMailText(e.location) {
			return nil, nil, core.ErrInvalid
		}
		deleted = slices.Delete(deleted, index, index+1)
		subject, location := master.Subject, master.Location
		if e.flags&recurrenceOverrideSubject != 0 {
			subject = e.subject
		}
		if e.flags&recurrenceOverrideLocation != 0 {
			location = e.location
		}
		schedule.Exceptions = append(schedule.Exceptions, core.AppointmentSeriesException{
			WallStart: recurrenceWall(e.start), WallEnd: recurrenceWall(e.end), WallOriginalStart: recurrenceWall(e.original),
			Subject: subject, Location: location,
		})
	}
	r.blob(math.MaxUint16) // ReservedBlock2.
	if r.done() != nil {
		return nil, nil, core.ErrInvalid
	}
	for _, day := range deleted {
		schedule.SkippedDates = append(schedule.SkippedDates, recurrenceWall(day))
	}
	master.Recurrence, master.SeriesSchedule = recurrence, schedule
	canonical, ok := encodeAppointmentRecurrence(master)
	// The encoder also checks that anchors belong to the series, adjacent
	// instances do not overlap, and that the range has the declared end.
	if !ok || first != binary.LittleEndian.Uint32(canonical.data[10:]) || end != canonical.endDate {
		return nil, nil, appointmentImportRejection{importAppointmentRecurrenceRule, core.ErrInvalid}
	}
	return recurrence, schedule, nil
}

func recurrenceUint16(r *reader) uint16 {
	if b := r.take(2); b != nil {
		return binary.LittleEndian.Uint16(b)
	}
	return 0
}

func recurrenceDateList(r *reader) []uint32 {
	n := int(r.u32())
	if n < 0 || n > (len(r.data)-r.pos)/4 {
		r.err = ErrMalformed
		return nil
	}
	dates := make([]uint32, n)
	for i := range dates {
		dates[i] = r.u32()
		if dates[i]%minutesPerDay != 0 || i > 0 && dates[i] <= dates[i-1] {
			r.err = ErrMalformed
		}
	}
	return dates
}

func recurrenceText(r *reader) string {
	n := int(recurrenceUint16(r))
	data := r.take(n * 2)
	if data == nil || n > maxHierarchyText {
		r.err = ErrMalformed
		return ""
	}
	units := make([]uint16, n)
	for i := range units {
		units[i] = binary.LittleEndian.Uint16(data[i*2:])
	}
	return string(utf16.Decode(units))
}

func recurrenceWall(minutes uint32) time.Time {
	return time.Unix(recurrenceEpoch.Unix()+int64(minutes)*60, 0).UTC()
}
