// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"maps"
	"slices"
	"time"

	"ewsgo/internal/core"
)

var appointmentRecurrenceProperty = calendarSearchNamed(appointmentRecur, 0x0102).property

func (s *ramStore) updateAppointmentSeriesImport(ctx context.Context, box core.Mailbox, d *mailImportDraft, version messageVersion, before core.Appointment, values contentsValues) (uint64, error) {
	// Merge sparse cached edits over the last observed native revision. The
	// revision guard above this method prevents stale properties from merging.
	baseline := mapAppointmentProperties(before)
	merged := contentsValues{fixedContentsProperty(0x001a001f): core.Text("IPM.Appointment")}
	for _, named := range appointmentNamedProperties {
		if value := baseline[named.key]; value != nil {
			merged[named.property] = value
		}
	}
	merged[fixedContentsProperty(0x0037001f)] = core.Text(before.Subject)
	merged[fixedContentsProperty(appointmentBodyTag)] = core.Text(before.Body)
	maps.Copy(merged, values)
	raw, ok := merged[appointmentRecurrenceProperty].(core.Binary)
	if !ok {
		return 0, core.ErrUnsupported
	}
	attachment := d.exception
	if attachment == nil && d.pendingException != nil {
		// Outlook can save the embedded message and then save the series
		// directly. Validate that attachment before processing the recurrence.
		var err error
		attachment, err = d.pendingException.savedAttachment()
		if err != nil {
			return 0, appointmentImportRejection{importAppointmentRecurrenceAttachment, err}
		}
	}
	if attachment != nil {
		// The child object is validated against the decoded recurrence below.
		// Its presence explains the master's HasAttachments bit.
		delete(merged, fixedContentsProperty(0x0e1b000b))
		flagsKey := fixedContentsProperty(0x0e070003)
		if flags, ok := merged[flagsKey].(core.Integer); ok {
			merged[flagsKey] = flags &^ 16
		}
	}
	desired, err := appointmentSeriesImport(merged, raw)
	if err != nil {
		return 0, err
	}
	if desired.AllDay != before.AllDay || desired.Body != before.Body {
		return 0, core.ErrUnsupported
	}
	if err := d.validateCalendarException(before, desired, attachment); err != nil {
		var rejection appointmentImportRejection
		if errors.As(err, &rejection) {
			return 0, err
		}
		return 0, appointmentImportRejection{importAppointmentRecurrenceAttachment, err}
	}
	patch := core.AppointmentDetailsPatch{Subject: &desired.Subject, Location: &desired.Location}
	after, err := updateAppointmentSeries(ctx, box, before, desired, patch)
	if err != nil {
		return 0, err
	}
	return s.publishAppointmentImportEdit(ctx, d, version, before, after)
}

func appointmentImportItem(values contentsValues) (core.Appointment, error) {
	raw, recurring := values[appointmentRecurrenceProperty].(core.Binary)
	if !recurring {
		return singleAppointmentImport(values)
	}
	item, err := appointmentSeriesImport(values, raw)
	if err != nil {
		return core.Appointment{}, err
	}
	if len(item.SeriesSchedule.Exceptions)+len(item.SeriesSchedule.SkippedDates) != 0 {
		return core.Appointment{}, appointmentImportRejection{importAppointmentRecurrenceException, core.ErrUnsupported}
	}
	if item.AllDay {
		if !item.Start.Equal(item.SeriesSchedule.WallStart) || !item.End.Equal(item.SeriesSchedule.WallEnd) {
			return core.Appointment{}, appointmentImportRejection{importAppointmentRecurrenceDates, core.ErrInvalid}
		}
		return item, nil
	}
	zone, err := appointmentRecurrenceZoneForImport(values)
	if err != nil {
		return core.Appointment{}, appointmentImportRejection{importAppointmentSaveZone, err}
	}
	loc, err := recurrenceLocation(zone.Name)
	if err != nil {
		return core.Appointment{}, appointmentImportRejection{importAppointmentSaveZone, err}
	}
	for _, bound := range []struct{ utc, wall time.Time }{{item.Start, item.SeriesSchedule.WallStart}, {item.End, item.SeriesSchedule.WallEnd}} {
		_, localOffset := bound.utc.In(loc).Zone()
		if localOffset != 60*zone.OffsetAt(bound.utc) || !bound.utc.Add(time.Duration(localOffset)*time.Second).Equal(bound.wall) {
			return core.Appointment{}, appointmentImportRejection{importAppointmentRecurrenceDates, core.ErrInvalid}
		}
	}
	if loc != time.UTC {
		item.TimeZone = loc.String()
	}
	return item, nil
}

func appointmentSeriesImport(values contentsValues, raw core.Binary) (core.Appointment, error) {
	if values[fixedContentsProperty(0x0e1b000b)] == core.Bool(true) {
		return core.Appointment{}, appointmentImportRejection{importAppointmentRecurrenceAttachment, core.ErrUnsupported}
	}
	scalar := maps.Clone(values)
	delete(scalar, appointmentRecurrenceProperty)
	delete(scalar, calendarSearchNamed(core.AppointmentRecurring, 0x000b).property)
	delete(scalar, calendarSearchNamed(appointmentRecurType, 0x0003).property)
	item, err := singleAppointmentImport(scalar)
	if err != nil {
		return core.Appointment{}, err
	}
	item.Type = core.AppointmentMaster
	item.Recurrence, item.SeriesSchedule, err = decodeAppointmentRecurrence(raw, item)
	if err != nil {
		var rejection appointmentImportRejection
		if errors.As(err, &rejection) {
			return core.Appointment{}, err
		}
		return core.Appointment{}, appointmentImportRejection{importAppointmentRecurrenceDecode, err}
	}
	if kind, ok := values[calendarSearchNamed(appointmentRecurType, 0x0003).property].(core.Integer); ok && kind != core.Integer(item.Recurrence.Frequency) {
		return core.Appointment{}, appointmentImportRejection{importAppointmentRecurrenceRule, core.ErrInvalid}
	}
	return item, nil
}

func appointmentRecurrenceZoneForImport(values contentsValues) (core.AppointmentZone, error) {
	blob := func(key core.Key) core.Binary {
		raw, _ := values[calendarSearchNamed(key, 0x0102).property].(core.Binary)
		return raw
	}
	recur, legacy, display := blob(appointmentRecurZone), blob(appointmentZoneStruct), blob(appointmentStartZone)
	if len(legacy) != 0 {
		zone, err := decodeAppointmentZoneStruct(legacy)
		if err != nil {
			return core.AppointmentZone{}, err
		}
		// TimeZoneStruct supplies the rule. Use a named definition only
		// when its offsets and transitions describe the same time zone.
		for _, candidate := range []struct {
			data      core.Binary
			recurring bool
		}{{recur, true}, {recur, false}, {display, false}} {
			named, err := decodeAppointmentZoneDefinition(candidate.data, candidate.recurring)
			zone.Name = named.Name
			if err == nil && zone == named {
				return zone, nil
			}
		}
		return core.AppointmentZone{}, core.ErrUnsupported
	}
	if len(recur) != 0 {
		if zone, err := decodeAppointmentZoneDefinition(recur, true); err == nil {
			return zone, nil
		}
		return decodeAppointmentZoneDefinition(recur, false)
	}
	if len(display) != 0 {
		return decodeAppointmentDisplayZone(display)
	}
	return core.AppointmentZone{}, core.ErrUnsupported
}

func recurrenceLocation(name string) (*time.Location, error) {
	// These Windows names have equivalent modern rules in the supplied IANA
	// zones. Native TZIDs already in IANA form pass through unchanged.
	switch name {
	case "W. Europe Standard Time":
		name = "Europe/Rome"
	case "Romance Standard Time":
		name = "Europe/Paris"
	case "GMT Standard Time":
		name = "Europe/London"
	}
	if name == "" || name == "Local" {
		return nil, core.ErrUnsupported
	}
	loc, err := time.LoadLocation(name)
	if err != nil {
		return nil, core.ErrUnsupported
	}
	return loc, nil
}

func equalAppointmentRules(a, b *core.AppointmentRecurrence) bool {
	if a == nil || b == nil {
		return a == b
	}
	sameWeekStart := a.FirstDay == b.FirstDay || a.Frequency == core.RecurrenceWeekly && a.Interval == 1
	return a.Frequency == b.Frequency && a.Interval == b.Interval && a.Count == b.Count && sameWeekStart &&
		a.StartDate.Equal(b.StartDate) && a.Until.Equal(b.Until) && slices.Equal(a.Days, b.Days)
}

func updateAppointmentSeries(ctx context.Context, box core.Mailbox, before, desired core.Appointment, patch core.AppointmentDetailsPatch) (core.Appointment, error) {
	writer, writable := box.(core.AppointmentDetailsWriter)
	reader, readable := box.(core.AppointmentContentsReader)
	old, next := before.SeriesSchedule, desired.SeriesSchedule
	if !writable || !readable || old == nil || next == nil || !equalAppointmentRules(before.Recurrence, desired.Recurrence) ||
		!before.Start.Equal(desired.Start) || !before.End.Equal(desired.End) ||
		!old.WallStart.Equal(next.WallStart) || !old.WallEnd.Equal(next.WallEnd) || !slices.Equal(old.SkippedDates, next.SkippedDates) {
		return core.Appointment{}, core.ErrUnsupported
	}
	if patch.Subject != nil && *patch.Subject == before.Subject {
		patch.Subject = nil
	}
	if patch.Location != nil && *patch.Location == before.Location {
		patch.Location = nil
	}
	patch.Start, patch.End = nil, nil
	exception, err := changedAppointmentException(old.Exceptions, next.Exceptions)
	if err != nil {
		return core.Appointment{}, err
	}
	if exception != nil {
		if patch != (core.AppointmentDetailsPatch{}) {
			// The native backend commits either a master edit or one exception.
			return core.Appointment{}, core.ErrUnsupported
		}
		if err := saveAppointmentException(ctx, box, writer, before, *exception); err != nil {
			return core.Appointment{}, err
		}
	} else if patch != (core.AppointmentDetailsPatch{}) {
		if _, err := writer.UpdateAppointmentDetails(ctx, before, patch); err != nil {
			return core.Appointment{}, err
		}
	}
	after, err := reader.GetCalendarAppointment(ctx, before.Identity.Parent, before.Identity.ID)
	if err != nil || after.Identity.ID != before.Identity.ID || after.Identity.Parent != before.Identity.Parent ||
		after.Identity.Kind != before.Identity.Kind || after.Identity.Revision < before.Identity.Revision || after.UID != before.UID ||
		after.Type != core.AppointmentMaster || after.Participation != core.AppointmentPersonal || after.Cancelled ||
		after.Subject != desired.Subject || after.Location != desired.Location || after.Body != before.Body ||
		after.AllDay != before.AllDay || !after.Start.Equal(before.Start) || !after.End.Equal(before.End) ||
		!equalAppointmentRules(after.Recurrence, before.Recurrence) || after.SeriesSchedule == nil ||
		!sameAppointmentExceptions(after.SeriesSchedule.Exceptions, next.Exceptions) ||
		!slices.Equal(after.SeriesSchedule.SkippedDates, old.SkippedDates) {
		return core.Appointment{}, errors.Join(core.ErrOutcomeUnknown, err)
	}
	return after, nil
}

func sameAppointmentExceptions(a, b []core.AppointmentSeriesException) bool {
	if len(a) != len(b) {
		return false
	}
	unchanged, err := changedAppointmentException(a, b)
	return err == nil && unchanged == nil
}

func changedAppointmentException(old, next []core.AppointmentSeriesException) (*core.AppointmentSeriesException, error) {
	byAnchor := make(map[time.Time]core.AppointmentSeriesException, len(old))
	for _, e := range old {
		byAnchor[e.WallOriginalStart.UTC()] = e
	}
	var changed *core.AppointmentSeriesException
	for _, e := range next {
		key := e.WallOriginalStart.UTC()
		if old := byAnchor[key]; old != e {
			if changed != nil {
				return nil, core.ErrUnsupported
			}
			changed = &e
		}
		delete(byAnchor, key)
	}
	if len(byAnchor) != 0 {
		return nil, core.ErrUnsupported
	}
	return changed, nil
}

func saveAppointmentException(ctx context.Context, box core.Mailbox, writer core.AppointmentDetailsWriter, series core.Appointment, e core.AppointmentSeriesException) error {
	reader, ok := box.(core.AppointmentOccurrenceReader)
	if !ok || series.StartZone == nil && !series.AllDay {
		return core.ErrUnsupported
	}
	zone := core.AppointmentZone{}
	if !series.AllDay {
		zone = *series.StartZone
	}
	anchor, anchorOK := wallAppointmentInstant(e.WallOriginalStart, zone)
	start, startOK := wallAppointmentInstant(e.WallStart, zone)
	end, endOK := wallAppointmentInstant(e.WallEnd, zone)
	if !anchorOK || !startOK || !endOK || end.Before(start) {
		return core.ErrInvalid
	}
	item, err := reader.GetAppointmentOccurrence(ctx, series.Identity.Parent, series.Identity.ID, anchor, series.AllDay)
	if err != nil {
		return err
	}
	if item.SeriesID != series.Identity.ID || item.Identity.Parent != series.Identity.Parent ||
		item.Identity.Kind != core.AppointmentItem || item.Identity.Revision != series.Identity.Revision ||
		item.UID != series.UID || item.AllDay != series.AllDay || !item.OriginalStart.Equal(anchor) || item.Cancelled ||
		(item.Type != core.AppointmentOccurrence && item.Type != core.AppointmentException) {
		return core.ErrConflict
	}
	patch := core.AppointmentDetailsPatch{Subject: &e.Subject, Location: &e.Location, Start: &start, End: &end}
	_, err = writer.UpdateAppointmentDetails(ctx, item, patch)
	return err
}

// An ambiguous wall clock must not silently select a different occurrence.
func wallAppointmentInstant(wall time.Time, zone core.AppointmentZone) (time.Time, bool) {
	standard := wall.Add(-time.Duration(zone.StandardOffset) * time.Minute)
	daylight := wall.Add(-time.Duration(zone.DaylightOffset) * time.Minute)
	stdOK, dstOK := zone.OffsetAt(standard) == zone.StandardOffset, zone.OffsetAt(daylight) == zone.DaylightOffset
	if standard.Equal(daylight) || stdOK && !dstOK {
		return standard, true
	}
	return daylight, dstOK && !stdOK
}
