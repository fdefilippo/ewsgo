// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"maps"
	"strings"
	"time"

	"ewsgo/internal/core"
)

const (
	calendarExceptionClass = "IPM.OLE.CLASS.{00061055-0000-0000-C000-000000000046}"
	exceptionReplaceTag    = 0x7ff90040
)

var exceptionReplaceProperty = calendarSearchNamed(core.Key{Set: appointmentPropertySet, ID: 0x8228}, 0x0040).property

// These drafts exist only under an ICS import of an existing calendar item.
// Saving the message or attachment does not issue a native mutation.
type calendarExceptionDraft struct {
	mid        uint64
	attachment map[uint32]core.Value
	message    contentsValues
	saved      contentsValues
}

type savedCalendarException struct {
	message contentsValues
}

func calendarExceptionAttachmentTag(tag uint32) bool {
	switch tag {
	case 0x3001001f, 0x37020102, 0x37050003, 0x37090102, 0x370b0003, 0x37140003,
		exceptionReplaceTag, 0x7ffa0003, 0x7ffb0040, 0x7ffc0040, 0x7ffd0003, 0x7ffe000b, 0x7fff000b:
		return true
	}
	return false
}

func (d *calendarExceptionDraft) setAttachment(request faiSetRequest) []faiPropertyProblem {
	if d.attachment == nil {
		d.attachment = make(map[uint32]core.Value)
	}
	var problems []faiPropertyProblem
	for i, value := range request {
		valid := calendarExceptionAttachmentTag(value.tag) && value.value != nil
		switch v := value.value.(type) {
		case core.Text:
			valid = valid && len(v) <= maxHierarchyText && validMailText(string(v))
		case core.Binary:
			// Encoding and rendering metadata do not carry a file payload.
			valid = valid && len(v) <= 256
		}
		if !valid {
			problems = append(problems, faiPropertyProblem{uint16(i), value.tag, NotSupported})
			continue
		}
		d.attachment[value.tag] = value.value
	}
	return problems
}

func openCalendarException(ctx context.Context, session core.Session, objects *mailboxObjects, input serverObject, op Operation, handles []uint32, cp uint32) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Output, code) }
	a := input.attachment.draft
	d := a.calendar
	if err := a.authorize(ctx, session); err != nil {
		return fail(attachmentError(err))
	}
	mode := op.OpenMessage.mode
	if mode != 0 {
		if err := a.editable(); err != nil {
			return fail(attachmentError(err))
		}
	}
	if d.mid == 0 && mode != 2 {
		return fail(PropertyNotFound)
	}
	if len(objects.objects) >= maxLiveStoreHandles || objects.allocated >= maxContextHandleAllocations {
		return fail(0x8007000e)
	}
	row := messageRow{object: core.Object{Properties: core.Properties{core.Subject: d.saved[fixedContentsProperty(0x0037001f)]}}}
	reply, code := openMessageResponse(op.ID, op.Output, row, op.OpenMessage.limit)
	if code != 0 {
		return fail(code)
	}
	handle, err := objects.allocator.handle()
	if err != nil {
		return fail(0x8007000e)
	}
	if d.mid == 0 {
		store := input.binding.store
		store.mu.Lock()
		d.mid, err = store.stageIDLocked(ctx, 0)
		store.mu.Unlock()
		if err != nil {
			return fail(storeError(err))
		}
	}
	objects.objects[handle] = serverObject{logon: op.Logon, binding: input.binding, fid: input.fid,
		message: &openedMessage{mid: d.mid, codePage: cp, calendarException: a, embeddedReadOnly: mode == 0}}
	objects.allocated++
	handles[op.Output] = handle
	response := binary.LittleEndian.AppendUint64(append(reply[:6:6], 0), d.mid)
	return append(response, reply[6:]...)
}

func (d *calendarExceptionDraft) setMessage(names *namedPropertyRegistry, request faiSetRequest) []faiPropertyProblem {
	if d.message == nil {
		d.message = make(contentsValues)
	}
	var problems []faiPropertyProblem
	for i, value := range request {
		p, err := resolveContentsIdentity(value.tag, names)
		valid := err == nil && value.value != nil && (appointmentImportProperty(p) || p == exceptionReplaceProperty)
		switch v := value.value.(type) {
		case core.Text:
			limit := maxHierarchyText
			if value.tag == appointmentBodyTag {
				limit = maxContactEditBytes
			}
			valid = valid && len(v) <= limit && validMailText(string(v))
		case core.Binary:
			valid = valid && len(v) <= 1024
		}
		if !valid {
			problems = append(problems, faiPropertyProblem{uint16(i), value.tag, NotSupported})
			continue
		}
		d.message[p] = value.value
		d.saved = nil
	}
	return problems
}

func (d *calendarExceptionDraft) saveMessage() error {
	class, _ := d.message[fixedContentsProperty(0x001a001f)].(core.Text)
	if !strings.EqualFold(string(class), calendarExceptionClass) && !strings.EqualFold(string(class), "IPM.Appointment") {
		return core.ErrUnsupported
	}
	if _, ok := d.message[exceptionReplaceProperty].(core.Timestamp); !ok {
		return core.ErrInvalid
	}
	d.saved = maps.Clone(d.message)
	return nil
}

func readCalendarExceptionProperties(ctx context.Context, session core.Session, object serverObject, op Operation, diagnostic *StorePropertyDiagnostic) []byte {
	a := object.message.calendarException
	if err := a.authorize(ctx, session); err != nil {
		return ropError(op.ID, op.Input, attachmentError(err))
	}
	if op.ID != 0x07 { // RopGetPropertiesSpecific.
		return ropError(op.ID, op.Input, NotSupported)
	}
	request := *op.Properties
	values := make([]folderPropertyValue, len(request.tags))
	for i, tag := range request.tags {
		v, ok := openedMeetingProperty(a.calendar.message, tag, &object.binding.store.named, request, object.message.codePage)
		if !ok {
			v = folderPropertyValue{kind: 0x000a, code: PropertyNotFound}
		}
		if v.code != 0 {
			diagnostic.Errors++
		} else {
			diagnostic.Values++
			if v.kind == 0x001e || v.kind == 0x001f || v.kind == 0x0102 {
				request.variableBudget -= max(0, len(v.data)-4)
			}
		}
		values[i] = v
	}
	return propertyReadResponse(op, values)
}

func (a *draftAttachment) saveCalendarException() error {
	saved, err := a.calendar.savedAttachment()
	if err != nil {
		return err
	}
	a.importParent.exception = saved
	return nil
}

func (d *calendarExceptionDraft) savedAttachment() (*savedCalendarException, error) {
	flags, _ := d.attachment[0x7ffd0003].(core.Integer)
	if d.saved == nil || d.attachment[0x37050003] != core.Integer(5) ||
		flags&2 == 0 || d.attachment[0x7ffe000b] != core.Bool(true) || d.attachment[0x7fff000b] == core.Bool(true) {
		return nil, core.ErrUnsupported
	}
	return &savedCalendarException{message: d.saved}, nil
}

func (d *mailImportDraft) validateCalendarException(before, desired core.Appointment, attachment *savedCalendarException) error {
	if len(d.files) == 0 {
		return nil
	}
	if attachment == nil {
		return core.ErrUnsupported
	}
	changed, err := changedAppointmentException(before.SeriesSchedule.Exceptions, desired.SeriesSchedule.Exceptions)
	if err != nil || changed == nil || before.StartZone == nil && !before.AllDay {
		return core.ErrUnsupported
	}
	zone := core.AppointmentZone{}
	if !before.AllDay {
		zone = *before.StartZone
	}
	values := maps.Clone(attachment.message)
	values[fixedContentsProperty(0x001a001f)] = core.Text("IPM.Appointment")
	item, err := singleAppointmentImport(values)
	if err != nil || item.AllDay != before.AllDay {
		return core.ErrUnsupported
	}
	anchor, anchorOK := wallAppointmentInstant(changed.WallOriginalStart, zone)
	start, startOK := wallAppointmentInstant(changed.WallStart, zone)
	end, endOK := wallAppointmentInstant(changed.WallEnd, zone)
	replace, replaceOK := values[exceptionReplaceProperty].(core.Timestamp)
	if !anchorOK || !startOK || !endOK {
		return appointmentImportRejection{importAppointmentExceptionWallTime, core.ErrInvalid}
	}
	if !start.Equal(item.Start) || !end.Equal(item.End) {
		return appointmentImportRejection{importAppointmentExceptionBounds, core.ErrInvalid}
	}
	if !replaceOK || !anchor.Equal(time.Time(replace)) {
		return appointmentImportRejection{importAppointmentExceptionEmbeddedAnchor, core.ErrInvalid}
	}
	// The recurrence and the embedded message agree on the original UTC
	// anchor. The outer ReplaceTime is a lookup hint for reading a stored
	// attachment and can differ in an Outlook import. It never selects the
	// native occurrence to modify.
	if (values[fixedContentsProperty(0x0037001f)] != nil || values[fixedContentsProperty(0x0e1d001f)] != nil) && item.Subject != changed.Subject ||
		values[calendarSearchNamed(appointmentLocation, 0x001f).property] != nil && item.Location != changed.Location ||
		values[fixedContentsProperty(appointmentBodyTag)] != nil && item.Body != before.Body {
		return core.ErrUnsupported
	}
	return nil
}
