// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

func appointmentImportProperty(p contentsProperty) bool {
	if p.name == (propertyName{}) {
		switch uint32(p.id)<<16 | uint32(p.kind) {
		case 0x001a001f, 0x0037001f, 0x003d001f, 0x0e1d001f, 0x0e070003,
			0x0e1b000b, 0x30070040, 0x30080040, 0x300b0102, 0x65e00102,
			0x65e20102, 0x65e30102, appointmentStartDateTag, appointmentEndDateTag, appointmentBodyTag:
			return true
		default:
			return false
		}
	}
	for _, field := range calendarProperties {
		if p == field.property {
			return true
		}
	}
	return false
}

func singleAppointmentImport(values contentsValues) (core.Appointment, error) {
	fixed := func(tag uint32) core.Value { return values[fixedContentsProperty(tag)] }
	named := func(key core.Key, kind uint16) core.Value {
		return values[calendarSearchNamed(key, kind).property]
	}
	class, _ := fixed(0x001a001f).(core.Text)
	flags, _ := fixed(0x0e070003).(core.Integer)
	state, _ := named(appointmentStateFlags, 0x0003).(core.Integer)
	recurrence, _ := named(appointmentRecurType, 0x0003).(core.Integer)
	allDay := named(appointmentAllDay, 0x000b) == core.Bool(true)
	if !strings.EqualFold(string(class), "IPM.Appointment") || flags&(16|32|64) != 0 || state != 0 ||
		named(core.AppointmentRecurring, 0x000b) == core.Bool(true) || recurrence != 0 ||
		named(appointmentRecur, 0x0102) != nil || named(core.ReminderSet, 0x000b) == core.Bool(true) ||
		fixed(0x0e1b000b) == core.Bool(true) {
		return core.Appointment{}, appointmentImportRejection{importAppointmentSaveProfile, core.ErrUnsupported}
	}
	// MS-OXOCAL 2.2.1.30–33 defines the fixed and common dates as
	// equivalent to the corresponding Whole dates in a single appointment.
	// Accept a supplied equivalent when the partial property list omits Whole.
	start, err := appointmentImportBound(named(appointmentStartWhole, 0x0040),
		named(appointmentStart, 0x0040), fixed(appointmentStartDateTag), importAppointmentSaveStartWhole)
	if err != nil {
		return core.Appointment{}, err
	}
	end, err := appointmentImportBound(named(appointmentEndWhole, 0x0040),
		named(appointmentEnd, 0x0040), fixed(appointmentEndDateTag), importAppointmentSaveEndWhole)
	if err != nil {
		return core.Appointment{}, err
	}
	item := core.Appointment{Type: core.AppointmentSingle, Participation: core.AppointmentPersonal,
		Start: start, End: end, AllDay: allDay}
	if item.Start.Year() < 1601 || item.End.Year() > 9999 || item.End.Before(item.Start) ||
		item.Start.Nanosecond() != 0 || item.End.Nanosecond() != 0 {
		return core.Appointment{}, appointmentImportRejection{importAppointmentSaveRange, core.ErrInvalid}
	}
	elapsedMinutes := (end.Unix() - start.Unix()) / 60
	if allDay {
		raw, ok := named(appointmentStartZone, 0x0102).(core.Binary)
		if !ok {
			return core.Appointment{}, appointmentImportRejection{importAppointmentSaveZone, core.ErrUnsupported}
		}
		zone, err := decodeAppointmentDisplayZone(raw)
		if err != nil {
			return core.Appointment{}, appointmentImportRejection{importAppointmentSaveZone, err}
		}
		item.Start, err = appointmentCivilMidnight(start, zone)
		if err != nil {
			return core.Appointment{}, appointmentImportRejection{importAppointmentSaveStartWhole, err}
		}
		item.End, err = appointmentCivilMidnight(end, zone)
		if err != nil || !item.End.After(item.Start) {
			return core.Appointment{}, appointmentImportRejection{importAppointmentSaveRange, core.ErrInvalid}
		}
	}
	// Outlook may use the civil number of minutes for an all-day item that
	// crosses a DST transition. The validated date bounds stay authoritative.
	if minutes, ok := named(appointmentDuration, 0x0003).(core.Integer); ok &&
		int64(minutes) != elapsedMinutes && int64(minutes) != (item.End.Unix()-item.Start.Unix())/60 {
		return core.Appointment{}, appointmentImportRejection{importAppointmentSaveDuration, core.ErrInvalid}
	}
	subject, hasSubject := fixed(0x0037001f).(core.Text)
	if !hasSubject {
		normalized, _ := fixed(0x0e1d001f).(core.Text)
		prefix, _ := fixed(0x003d001f).(core.Text)
		subject = prefix + normalized
	}
	for _, field := range []struct {
		value    core.Value
		target   *string
		maxBytes int
	}{
		{subject, &item.Subject, maxHierarchyText},
		{named(appointmentLocation, 0x001f), &item.Location, maxHierarchyText},
		{fixed(appointmentBodyTag), &item.Body, maxContactEditBytes},
	} {
		v, _ := field.value.(core.Text)
		if len(v) > field.maxBytes || !validMailText(string(v)) {
			return core.Appointment{}, appointmentImportRejection{importAppointmentSaveText, core.ErrInvalid}
		}
		*field.target = string(v)
	}
	return item, nil
}

func appointmentImportBound(whole, common, fixed core.Value, missing importRejection) (time.Time, error) {
	var bound time.Time
	for _, candidate := range []struct {
		value  core.Value
		reason importRejection
	}{
		{whole, missing},
		{common, importAppointmentSaveCommonDates},
		{fixed, importAppointmentSaveFixedDates},
	} {
		if t, ok := candidate.value.(core.Timestamp); ok {
			value := time.Time(t).UTC()
			if !bound.IsZero() && !bound.Equal(value) {
				return time.Time{}, appointmentImportRejection{candidate.reason, core.ErrInvalid}
			}
			bound = value
		}
	}
	if bound.IsZero() {
		return time.Time{}, appointmentImportRejection{missing, core.ErrInvalid}
	}
	return bound, nil
}

func (d *mailImportDraft) saveAppointment(ctx context.Context, session core.Session) (uint64, error) {
	c := d.collector
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, appointmentImportRejection{importAppointmentSaveCollector, core.ErrClosed}
	}
	values, err := d.typedImportValues(appointmentImportProperty)
	if err != nil {
		return 0, appointmentImportRejection{importAppointmentSaveDecode, err}
	}
	if err := c.prepareNormalMutation(ctx, session); err != nil {
		return 0, appointmentImportRejection{importAppointmentSaveCollector, err}
	}
	var cn uint64
	if d.editKey.id != "" {
		cn, err = c.state.store.updateAppointmentImport(ctx, session, c.state.root, d, values)
	} else {
		item, decodeErr := singleAppointmentImport(values)
		if decodeErr != nil {
			return 0, decodeErr
		}
		cn, err = c.state.store.saveAppointmentImport(ctx, session, c.state.root, d, item)
	}
	if err != nil {
		var rejection appointmentImportRejection
		if errors.As(err, &rejection) {
			return 0, err
		}
		return 0, appointmentImportRejection{importAppointmentSaveNative, err}
	}
	c.acknowledgeNormal(d.counter, cn, d.editKey.id == "")
	d.saved = true
	d.close()
	return folderID(d.counter), nil
}

func appointmentImportUID(ref [16]byte) string {
	return "ewsgo-" + hex.EncodeToString(ref[:]) + "@ewsgo.invalid"
}

func (s *ramStore) saveAppointmentImport(ctx context.Context, session core.Session, fid uint64, d *mailImportDraft, item core.Appointment) (uint64, error) {
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	folder, err := s.scopedMessageAnchor(ctx, session, fid, false, core.CalendarFolder)
	if err != nil {
		return 0, err
	}
	if folder.native != d.folder {
		return 0, core.ErrConflict
	}
	writer, ok := session.Mailbox().(core.AppointmentImportWriter)
	reader, readable := session.Mailbox().(core.AppointmentContentsReader)
	if !ok || !readable {
		return 0, core.ErrUnsupported
	}
	payload, err := json.Marshal(item)
	if err != nil {
		return 0, core.ErrInvalid
	}
	p := mapireplica.MessageImport{Counter: d.counter, Parent: folderGlobalCount(fid), Digest: d.requestDigest(payload)}
	rand.Read(p.AppointmentReference[:])
	newImport, err := s.claimMessageImport(ctx, p)
	if err != nil {
		return 0, err
	}
	if !newImport {
		s.mu.Lock()
		p = s.pendingMessageImports[folderID(p.Counter)]
		s.mu.Unlock()
	}
	uid := appointmentImportUID(p.AppointmentReference)
	var stored core.Appointment
	if newImport {
		stored, err = writer.CreateAppointmentWithUID(ctx, folder.native, item, uid)
		if err != nil {
			return 0, s.messageImportFailure(ctx, p, err)
		}
	} else {
		stored, err = writer.FindAppointmentByUID(ctx, folder.native, uid)
		if err != nil {
			return 0, core.ErrOutcomeUnknown
		}
	}
	stored, err = reader.GetCalendarAppointment(ctx, folder.native, stored.Identity.ID)
	if err != nil {
		return 0, errors.Join(core.ErrOutcomeUnknown, err)
	}
	if stored.UID != uid || stored.Type != item.Type || stored.Participation != core.AppointmentPersonal ||
		stored.AllDay != item.AllDay || stored.Recurrence != nil || stored.Cancelled ||
		stored.Subject != item.Subject || stored.Location != item.Location || stored.Body != item.Body ||
		!stored.Start.Equal(item.Start) || !stored.End.Equal(item.End) {
		return 0, core.ErrOutcomeUnknown
	}
	obj, err := calendarAppointmentMessage(stored, folder.native)
	if err != nil || len(obj.ID) > maxHierarchyText || !validMailText(string(obj.ID)) ||
		p.NativeID != "" && p.NativeID != obj.ID {
		return 0, core.ErrOutcomeUnknown
	}
	nodes, err := scopedMessageNodes([]core.Object{obj}, folder.native, false, core.CalendarFolder)
	if err != nil {
		return 0, errors.Join(core.ErrOutcomeUnknown, err)
	}
	if p.NativeID == "" {
		p.NativeID = obj.ID
		if err := s.updateMessageImport(ctx, p, mapireplica.RecordMessageImport); err != nil {
			return 0, errors.Join(core.ErrOutcomeUnknown, err)
		}
	}
	return s.publishMessageImport(ctx, p, d, obj.Revision, nodes[0].read, nodes[0].fingerprint)
}

func (s *ramStore) updateAppointmentImport(ctx context.Context, session core.Session, fid uint64, d *mailImportDraft, values contentsValues) (uint64, error) {
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	folder, err := s.scopedMessageAnchor(ctx, session, fid, false, core.CalendarFolder)
	if err != nil {
		return 0, err
	}
	if folder.native != d.folder {
		return 0, core.ErrConflict
	}
	current, err := s.prepareMessageImportEdit(fid, d)
	if err != nil {
		return 0, err
	}
	box := session.Mailbox()
	reader, readable := box.(core.AppointmentContentsReader)
	writer, writable := box.(core.AppointmentDetailsWriter)
	native, validKey := strings.CutPrefix(string(d.editKey.id), "a:")
	if !readable || !writable || !validKey || native == "" {
		return 0, core.ErrUnsupported
	}
	id := core.ID(core.AppointmentIDPrefix + native)
	before, err := reader.GetCalendarAppointment(ctx, d.folder, id)
	if err != nil {
		return 0, err
	}
	if before.Identity.ID != id || before.Identity.Parent != d.folder || before.Identity.Kind != core.AppointmentItem ||
		before.Identity.Revision != current.nativeRevision {
		return 0, core.ErrConflict
	}
	if !editablePersonalAppointment(before) {
		return 0, appointmentImportRejection{importAppointmentSaveProfile, core.ErrUnsupported}
	}
	item, err := singleAppointmentImport(values)
	if err != nil {
		return 0, err
	}
	if item.AllDay != before.AllDay {
		return 0, appointmentImportRejection{importAppointmentSaveProfile, core.ErrUnsupported}
	}
	var patch core.AppointmentDetailsPatch
	// The Outlook direct path can omit mapped fields from its property lists.
	// An explicitly supplied empty value clears the corresponding field.
	if values[fixedContentsProperty(0x0037001f)] != nil ||
		values[fixedContentsProperty(0x0e1d001f)] != nil {
		patch.Subject = &item.Subject
	}
	if values[calendarSearchNamed(appointmentLocation, 0x001f).property] != nil {
		patch.Location = &item.Location
	}
	if values[fixedContentsProperty(appointmentBodyTag)] != nil && item.Body != before.Body {
		return 0, appointmentImportRejection{importAppointmentSaveProfile, core.ErrUnsupported}
	}
	patch.Start, patch.End = &item.Start, &item.End
	saved, err := writer.UpdateAppointmentDetails(ctx, before, patch)
	if err != nil {
		return 0, err
	}
	// Use the same projection as a normal Calendar observation, including
	// the native reminder properties and participation classification.
	after, err := reader.GetCalendarAppointment(ctx, d.folder, id)
	if err != nil {
		return 0, errors.Join(core.ErrOutcomeUnknown, err)
	}
	if after.UID != before.UID || after.Identity.Kind != before.Identity.Kind || after.Identity.Revision != saved.Identity.Revision ||
		after.Type != before.Type || after.Participation != before.Participation || after.AllDay != before.AllDay ||
		after.Cancelled || after.Body != before.Body || !after.Start.Equal(item.Start) || !after.End.Equal(item.End) ||
		(patch.Subject != nil && after.Subject != *patch.Subject) ||
		(patch.Location != nil && after.Location != *patch.Location) {
		return 0, core.ErrOutcomeUnknown
	}
	obj, err := calendarAppointmentMessage(after, d.folder)
	if err != nil || obj.ID != d.editKey.id || obj.Revision < before.Identity.Revision {
		return 0, core.ErrOutcomeUnknown
	}
	nodes, err := scopedMessageNodes([]core.Object{obj}, d.folder, false, core.CalendarFolder)
	if err != nil {
		return 0, errors.Join(core.ErrOutcomeUnknown, err)
	}
	return s.publishMessageImportEdit(ctx, d, current, obj.Revision, nodes[0].fingerprint)
}

// Called with the registry lock held before allocating an ID for an observation.
func (s *ramStore) pendingCalendarImport(fid uint64, o core.Object) bool {
	globalID, _ := o.Properties[appointmentGlobalID].(core.Binary)
	for _, p := range s.pendingMessageImports {
		if p.Special() != core.CalendarFolder || folderID(p.Parent) != fid {
			continue
		}
		if p.NativeID == o.ID || bytes.Equal(globalID, appointmentGlobalIdentity(appointmentImportUID(p.AppointmentReference))) {
			return true
		}
	}
	return false
}
