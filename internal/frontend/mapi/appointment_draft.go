// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"crypto/rand"
	"errors"
	"strings"
	"time"

	"ewsgo/internal/core"
)

type appointmentDraft struct {
	base                core.Appointment
	initial             contentsValues
	uid                 string
	patch               core.AppointmentDetailsPatch
	recurrence          core.Binary
	lease               *contentsLease
	readOnly, uncertain bool
	refreshPending      bool
}

func (s *ramStore) isAppointmentMID(mid uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byMID[mid].special == core.CalendarFolder
}

func (s *ramStore) stageNewAppointment(ctx context.Context, session core.Session, fid uint64) (*appointmentDraft, error) {
	if _, ok := session.Mailbox().(core.AppointmentImportWriter); !ok {
		return nil, core.ErrUnsupported
	}
	if _, ok := session.Mailbox().(core.AppointmentContentsReader); !ok {
		return nil, core.ErrUnsupported
	}
	folder, err := s.scopedMessageAnchor(ctx, session, fid, false, core.CalendarFolder)
	if err != nil {
		return nil, err
	}
	lease, err := s.reserveContentsMemory(2 * maxContactEditBytes)
	if err != nil {
		return nil, err
	}
	var reference [16]byte
	rand.Read(reference[:])
	initial := contentsValues{}
	putContentsValue(initial, 0x001a001f, core.Text("IPM.Appointment"))
	return &appointmentDraft{
		base:    core.Appointment{Identity: core.Object{Kind: core.AppointmentItem, Parent: folder.native}},
		initial: initial, uid: appointmentImportUID(reference), lease: lease,
	}, nil
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
	return item.Participation == core.AppointmentPersonal && !item.Cancelled &&
		item.Identity.Properties[core.ReminderSet] != core.Bool(true) &&
		(item.Type == core.AppointmentSingle && item.Recurrence == nil ||
			item.Type == core.AppointmentMaster && item.Recurrence != nil && item.SeriesSchedule != nil)
}

func (d *appointmentDraft) authorize(ctx context.Context, session core.Session, object serverObject) error {
	if object.message.mid == 0 {
		folder, err := object.binding.store.scopedMessageAnchor(ctx, session, object.fid, false, core.CalendarFolder)
		if err != nil {
			return err
		}
		if folder.native != d.base.Identity.Parent {
			return core.ErrConflict
		}
		return nil
	}
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
		var code uint32
		if d.initial != nil {
			code = d.setInitialProperty(&object.binding.store.named, value)
		} else {
			code = d.setProperty(&object.binding.store.named, value)
		}
		if code != 0 {
			problems = append(problems, faiPropertyProblem{index: uint16(index), tag: value.tag, code: code})
		}
	}
	return problems, nil
}

func (d *appointmentDraft) setInitialProperty(names *namedPropertyRegistry, value faiWireProperty) uint32 {
	property, err := resolveContentsIdentity(value.tag, names)
	if err != nil || !appointmentImportProperty(property) || !contentsValueType(property.kind, value.value) {
		return NotSupported
	}
	bytes := func(value core.Value) int {
		switch v := value.(type) {
		case core.Text:
			return len(v)
		case core.Binary:
			return len(v)
		default:
			return 0
		}
	}
	size := bytes(value.value)
	for key, existing := range d.initial {
		if key != property {
			size += bytes(existing)
		}
	}
	if size > maxContactEditBytes {
		return 0x8007000e
	}
	d.initial[property] = value.value
	return 0
}

func (d *appointmentDraft) unsavedRow(ctx context.Context, session core.Session, object serverObject) (messageRow, error) {
	if err := d.authorize(ctx, session, object); err != nil {
		return messageRow{}, err
	}
	props := core.Properties{{ID: 0x001a}: core.Text("IPM.Appointment")}
	for _, tag := range appointmentFixedProperties {
		props[core.Key{ID: tag >> 16}] = d.initial[fixedContentsProperty(tag)]
	}
	props[core.Subject] = d.initial[fixedContentsProperty(0x0037001f)]
	for _, field := range calendarProperties {
		props[field.key] = d.initial[field.property]
	}
	return messageRow{parent: object.fid, special: core.CalendarFolder, object: core.Object{Properties: props}}, nil
}

func (d *appointmentDraft) setProperty(names *namedPropertyRegistry, value faiWireProperty) uint32 {
	property, err := resolveContentsIdentity(value.tag, names)
	if err != nil {
		return NotSupported
	}
	switch property {
	case appointmentRecurrenceProperty:
		raw, ok := value.value.(core.Binary)
		if !ok || d.base.Type != core.AppointmentMaster {
			return NotSupported
		}
		if _, _, err := decodeAppointmentRecurrence(raw, d.amended()); err != nil {
			return faiError(err, false)
		}
		d.recurrence = raw
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
	if d.patch == (core.AppointmentDetailsPatch{}) && d.recurrence == nil {
		return row, nil
	}
	if d.uncertain || row.object.Revision != d.base.Identity.Revision {
		return messageRow{}, core.ErrConflict
	}
	item, err := d.amendedSeries()
	if err != nil {
		return messageRow{}, err
	}
	object, err := calendarAppointmentMessage(item, d.base.Identity.Parent)
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
	if d.initial != nil {
		if d.readOnly {
			return core.ErrAccessDenied
		}
		if err := d.create(ctx, session.Mailbox()); err != nil {
			return err
		}
	}
	if d.patch != (core.AppointmentDetailsPatch{}) || d.recurrence != nil {
		if d.readOnly {
			return core.ErrAccessDenied
		}
		expected, err := d.amendedSeries()
		if err != nil {
			return err
		}
		if expected.End.Before(expected.Start) {
			return core.ErrInvalid
		}
		after, err := d.write(ctx, session.Mailbox(), expected)
		if err != nil {
			d.uncertain = errors.Is(err, core.ErrOutcomeUnknown)
			return err
		}
		d.base, d.patch, d.recurrence = after, core.AppointmentDetailsPatch{}, nil
		d.refreshPending = true
	}
	d.readOnly = d.readOnly || mode == 1
	if d.refreshPending {
		view, err := object.binding.store.observeCalendarSearchItems(ctx, session, object.fid, core.CalendarFolder)
		if err != nil {
			return errors.Join(core.ErrOutcomeUnknown, err)
		}
		for _, row := range view.messages {
			native, _ := strings.CutPrefix(string(d.base.Identity.ID), core.AppointmentIDPrefix)
			if (object.message.mid == 0 || row.mid == object.message.mid) &&
				row.object.ID == core.ID("a:"+native) && row.object.Revision >= d.base.Identity.Revision {
				object.message.mid = row.mid
				d.refreshPending = false
				return nil
			}
		}
		return core.ErrOutcomeUnknown
	}
	return nil
}

func (d *appointmentDraft) create(ctx context.Context, box core.Mailbox) error {
	item, err := appointmentImportItem(d.initial)
	if err != nil {
		return err
	}
	writer := box.(core.AppointmentImportWriter)
	reader := box.(core.AppointmentContentsReader)
	var stored core.Appointment
	if d.uncertain {
		stored, err = writer.FindAppointmentByUID(ctx, d.base.Identity.Parent, d.uid)
		if err != nil {
			return errors.Join(core.ErrOutcomeUnknown, err)
		}
	} else {
		stored, err = writer.CreateAppointmentWithUID(ctx, d.base.Identity.Parent, item, d.uid)
		if err != nil {
			d.uncertain = errors.Is(err, core.ErrOutcomeUnknown)
			return err
		}
		d.uncertain = true
	}
	stored, err = reader.GetCalendarAppointment(ctx, d.base.Identity.Parent, stored.Identity.ID)
	if err != nil || !matchesCreatedAppointment(stored, item, d.uid) {
		return errors.Join(core.ErrOutcomeUnknown, err)
	}
	d.base, d.initial, d.uncertain, d.refreshPending = stored, nil, false, true
	return nil
}

func (d *appointmentDraft) amendedSeries() (core.Appointment, error) {
	item := d.amended()
	if d.recurrence == nil {
		return item, nil
	}
	var err error
	item.Recurrence, item.SeriesSchedule, err = decodeAppointmentRecurrence(d.recurrence, item)
	return item, err
}

func (d *appointmentDraft) write(ctx context.Context, box core.Mailbox, expected core.Appointment) (core.Appointment, error) {
	if d.base.Type == core.AppointmentMaster {
		return updateAppointmentSeries(ctx, box, d.base, expected, d.patch)
	}
	writer, ok := box.(core.AppointmentDetailsWriter)
	reader, readable := box.(core.AppointmentContentsReader)
	if !ok || !readable {
		return core.Appointment{}, core.ErrUnsupported
	}
	saved, err := writer.UpdateAppointmentDetails(ctx, d.base, d.patch)
	if err != nil {
		return core.Appointment{}, err
	}
	after, err := reader.GetCalendarAppointment(ctx, d.base.Identity.Parent, d.base.Identity.ID)
	if err != nil || after.Identity.ID != d.base.Identity.ID || after.Identity.Parent != d.base.Identity.Parent ||
		after.Identity.Kind != d.base.Identity.Kind || after.Identity.Revision != saved.Identity.Revision ||
		after.Identity.Revision < d.base.Identity.Revision || !editablePersonalAppointment(after) ||
		after.UID != d.base.UID || after.AllDay != d.base.AllDay || after.Subject != expected.Subject ||
		after.Location != expected.Location || after.Body != expected.Body ||
		!after.Start.Equal(expected.Start) || !after.End.Equal(expected.End) {
		return core.Appointment{}, errors.Join(core.ErrOutcomeUnknown, err)
	}
	return after, nil
}
