// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"strings"
	"time"

	"ewsgo/internal/core"
)

type appointmentDraft struct {
	base                core.Appointment
	patch               core.AppointmentDetailsPatch
	lease               *contentsLease
	readOnly, uncertain bool
	refreshPending      bool
}

func (s *ramStore) isAppointmentMID(mid uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byMID[mid].special == core.CalendarFolder
}

func (d *appointmentDraft) close() {
	if d != nil {
		d.lease.release()
	}
}

func (s *ramStore) readEditableAppointment(ctx context.Context, session core.Session, row messageRow) (*appointmentDraft, error) {
	box := session.Mailbox()
	reader, readable := box.(core.AppointmentContentsReader)
	_, writable := box.(core.AppointmentDetailsWriter)
	native, valid := strings.CutPrefix(string(row.object.ID), "a:")
	if !readable || !writable || !valid {
		return nil, core.ErrUnsupported
	}
	id := core.ID(core.AppointmentIDPrefix + native)
	base, err := reader.GetCalendarAppointment(ctx, row.object.Parent, id)
	if err != nil {
		return nil, err
	}
	if base.Identity.ID != id || base.Identity.Parent != row.object.Parent ||
		base.Identity.Kind != core.AppointmentItem || base.Identity.Revision != row.object.Revision {
		return nil, core.ErrConflict
	}
	if !editablePersonalAppointment(base) {
		return nil, core.ErrUnsupported
	}
	if len(base.UID)+len(base.Subject)+len(base.Location)+len(base.Body) > maxContactEditBytes {
		return nil, core.ErrLimit
	}
	lease, err := s.reserveContentsMemory(2 * maxContactEditBytes)
	if err != nil {
		return nil, err
	}
	return &appointmentDraft{base: base, lease: lease}, nil
}

func editablePersonalAppointment(item core.Appointment) bool {
	return item.Type == core.AppointmentSingle && item.Participation == core.AppointmentPersonal &&
		!item.Cancelled && item.Recurrence == nil && item.Identity.Properties[core.ReminderSet] != core.Bool(true)
}

func (d *appointmentDraft) authorize(ctx context.Context, session core.Session, object serverObject) error {
	key, folder, err := object.binding.store.openedMessageScope(ctx, session, object.fid, *object.message)
	if err != nil {
		return err
	}
	native, ok := strings.CutPrefix(string(d.base.Identity.ID), core.AppointmentIDPrefix)
	if !ok || key.special != core.CalendarFolder || key.id != core.ID("a:"+native) || folder.native != d.base.Identity.Parent {
		return core.ErrNotFound
	}
	if d.uncertain {
		return core.ErrOutcomeUnknown
	}
	return nil
}

func (d *appointmentDraft) set(ctx context.Context, session core.Session, object serverObject, values faiSetRequest) ([]faiPropertyProblem, error) {
	if err := d.authorize(ctx, session, object); err != nil {
		return nil, err
	}
	if d.readOnly {
		return nil, core.ErrAccessDenied
	}
	var problems []faiPropertyProblem
	for index, value := range values {
		if code := d.setProperty(&object.binding.store.named, value); code != 0 {
			problems = append(problems, faiPropertyProblem{index: uint16(index), tag: value.tag, code: code})
		}
	}
	return problems, nil
}

func (d *appointmentDraft) setProperty(names *namedPropertyRegistry, value faiWireProperty) uint32 {
	property, err := resolveContentsIdentity(value.tag, names)
	if err != nil {
		return NotSupported
	}
	switch property {
	case fixedContentsProperty(0x0037001f), calendarSearchNamed(appointmentLocation, 0x001f).property:
		text, ok := value.value.(core.Text)
		if !ok || len(text) > maxHierarchyText || !validMailText(string(text)) {
			return 0x80070057
		}
		s := string(text)
		if property.id == 0x0037 {
			d.patch.Subject = &s
		} else {
			d.patch.Location = &s
		}
	case fixedContentsProperty(appointmentStartDateTag), fixedContentsProperty(appointmentEndDateTag),
		calendarSearchNamed(appointmentStartWhole, 0x0040).property, calendarSearchNamed(appointmentEndWhole, 0x0040).property,
		calendarSearchNamed(appointmentStart, 0x0040).property, calendarSearchNamed(appointmentEnd, 0x0040).property:
		// All-day bounds need the client's effective civil time zone.
		if d.base.AllDay {
			return NotSupported
		}
		stamp, ok := value.value.(core.Timestamp)
		t := time.Time(stamp).UTC()
		if !ok || t.Year() < 1601 || t.Year() > 9999 || t.Nanosecond() != 0 {
			return 0x80070057
		}
		switch property {
		case fixedContentsProperty(appointmentStartDateTag), calendarSearchNamed(appointmentStartWhole, 0x0040).property,
			calendarSearchNamed(appointmentStart, 0x0040).property:
			d.patch.Start = &t
		default:
			d.patch.End = &t
		}
	default:
		return NotSupported
	}
	return 0
}

func (d *appointmentDraft) amended() core.Appointment {
	item := d.base
	if d.patch.Subject != nil {
		item.Subject = *d.patch.Subject
	}
	if d.patch.Location != nil {
		item.Location = *d.patch.Location
	}
	if d.patch.Start != nil {
		item.Start = *d.patch.Start
	}
	if d.patch.End != nil {
		item.End = *d.patch.End
	}
	return item
}

func (d *appointmentDraft) preview(row messageRow) (messageRow, error) {
	if d.patch == (core.AppointmentDetailsPatch{}) {
		return row, nil
	}
	if d.uncertain || row.object.Revision != d.base.Identity.Revision {
		return messageRow{}, core.ErrConflict
	}
	object, err := calendarAppointmentMessage(d.amended(), d.base.Identity.Parent)
	row.object = object
	return row, err
}

func (d *appointmentDraft) save(ctx context.Context, session core.Session, object serverObject, flags byte) error {
	mode, valid := faiSaveMode(flags)
	if !valid {
		return core.ErrInvalid
	}
	if err := d.authorize(ctx, session, object); err != nil {
		return err
	}
	if d.patch != (core.AppointmentDetailsPatch{}) {
		if d.readOnly {
			return core.ErrAccessDenied
		}
		expected := d.amended()
		if expected.End.Before(expected.Start) {
			return core.ErrInvalid
		}
		writer, ok := session.Mailbox().(core.AppointmentDetailsWriter)
		reader, readable := session.Mailbox().(core.AppointmentContentsReader)
		if !ok || !readable {
			return core.ErrUnsupported
		}
		saved, err := writer.UpdateAppointmentDetails(ctx, d.base, d.patch)
		if err != nil {
			d.uncertain = errors.Is(err, core.ErrOutcomeUnknown)
			return err
		}
		after, err := reader.GetCalendarAppointment(ctx, d.base.Identity.Parent, d.base.Identity.ID)
		if err != nil || after.Identity.ID != d.base.Identity.ID || after.Identity.Parent != d.base.Identity.Parent ||
			after.Identity.Kind != d.base.Identity.Kind || after.Identity.Revision != saved.Identity.Revision ||
			after.Identity.Revision < d.base.Identity.Revision || !editablePersonalAppointment(after) ||
			after.UID != d.base.UID || after.AllDay != d.base.AllDay || after.Subject != expected.Subject ||
			after.Location != expected.Location || after.Body != expected.Body ||
			!after.Start.Equal(expected.Start) || !after.End.Equal(expected.End) {
			d.uncertain = true
			return errors.Join(core.ErrOutcomeUnknown, err)
		}
		d.base, d.patch = after, core.AppointmentDetailsPatch{}
		d.refreshPending = true
	}
	d.readOnly = d.readOnly || mode == 1
	if d.refreshPending {
		view, err := object.binding.store.observeCalendarSearchItems(ctx, session, object.fid, core.CalendarFolder)
		if err != nil {
			return errors.Join(core.ErrOutcomeUnknown, err)
		}
		for _, row := range view.messages {
			if row.mid == object.message.mid && row.object.Revision >= d.base.Identity.Revision {
				d.refreshPending = false
				return nil
			}
		}
		return core.ErrOutcomeUnknown
	}
	return nil
}
