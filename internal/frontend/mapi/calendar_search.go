// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"slices"

	"ewsgo/internal/core"
)

func searchCalendarRole(class string) core.SpecialFolderKind {
	switch class {
	case "IPF.Appointment":
		return core.CalendarFolder
	case "IPF.Task":
		return core.TasksFolder
	default:
		return 0
	}
}

func searchCalendarClass(role core.SpecialFolderKind) string {
	if role == core.CalendarFolder {
		return "IPM.Appointment"
	}
	return "IPM.Task"
}

type calendarNamedProperty struct {
	key      core.Key
	property contentsProperty
}

var calendarSearchProperties = []calendarNamedProperty{
	calendarSearchNamed(core.ReminderSet, 0x000b),
	calendarSearchNamed(core.AppointmentRecurring, 0x000b),
	calendarSearchNamed(core.ReminderSignalTime, 0x0040),
}

var calendarProperties = slices.Concat(calendarSearchProperties, appointmentNamedProperties)

func calendarSearchNamed(key core.Key, kind uint16) calendarNamedProperty {
	name, err := corePropertyName(key)
	if err != nil {
		panic("invalid standard calendar property")
	}
	return calendarNamedProperty{key, contentsProperty{name: name, kind: kind}}
}

func isCalendarSearchProperty(p contentsProperty) bool {
	return slices.ContainsFunc(calendarSearchProperties, func(f calendarNamedProperty) bool { return p == f.property })
}

func calendarSearchSchema(p contentsProperty) bool {
	if p.name != (propertyName{}) {
		return isCalendarSearchProperty(p)
	}
	switch uint32(p.id)<<16 | uint32(p.kind) {
	case parentServerIDTag, 0x001a001f, 0x0037001f, 0x0e070003,
		0x65e00102, 0x30080040, 0x65e20102, 0x65e30102, 0x67aa000b:
		return true
	default:
		return false
	}
}

// This is a conservative check. Any unsupported leaf may take either value;
// requiring a calendar property makes a row without those properties fail.
func searchMayMatchWithoutCalendarProperties(n *contentsRestriction) bool {
	if n == nil {
		return true
	}
	switch n.op {
	case 0:
		return !slices.ContainsFunc(n.children, func(child *contentsRestriction) bool {
			return !searchMayMatchWithoutCalendarProperties(child)
		})
	case 1:
		return slices.ContainsFunc(n.children, searchMayMatchWithoutCalendarProperties)
	case 2:
		return true
	default:
		return !isCalendarSearchProperty(n.property)
	}
}

func searchUsesOnlyCalendarSchema(n *contentsRestriction) bool {
	if n == nil {
		return true
	}
	if n.property != (contentsProperty{}) && !calendarSearchSchema(n.property) {
		return false
	}
	return !slices.ContainsFunc(n.children, func(child *contentsRestriction) bool { return !searchUsesOnlyCalendarSchema(child) })
}

func readCalendarSearchItems(ctx context.Context, box core.Mailbox, folder core.ID, role core.SpecialFolderKind) ([]core.Object, error) {
	if reader, ok := box.(core.AppointmentContentsReader); ok && role == core.CalendarFolder {
		appointments, err := reader.ListAppointments(ctx, folder)
		if err != nil {
			return nil, err
		}
		objects := make([]core.Object, len(appointments))
		for i, item := range appointments {
			objects[i], err = calendarAppointmentMessage(item, folder)
			if err != nil {
				return nil, err
			}
		}
		return objects, ctx.Err()
	}
	reader, ok := box.(core.CalendarSearchReader)
	if !ok {
		return nil, core.ErrUnsupported
	}
	return reader.ReadCalendarSearchItems(ctx, folder, role)
}

func (s *ramStore) observeCalendarSearchItems(ctx context.Context, session core.Session, fid uint64, role core.SpecialFolderKind) (messageSnapshot, error) {
	return s.observeScopedMessageSet(ctx, session, fid, false, role, func(ctx context.Context, box core.Mailbox, key folderKey) ([]core.Object, error) {
		return readCalendarSearchItems(ctx, box, key.native, role)
	})
}

func projectCalendarSearchItem(row messageRow) (projectedMessage, error) {
	subject, ok := row.object.Properties[core.Subject].(core.Text)
	if !ok || !validMailText(string(subject)) {
		return projectedMessage{}, core.ErrInvalid
	}
	values := contentsValues{}
	putContentsValue(values, 0x001a001f, core.Text(searchCalendarClass(row.special)))
	putContentsValue(values, 0x0037001f, subject)
	var flags core.Integer
	if row.read {
		flags = 1 // mfRead. Calendar objects have no SMTP submission state.
	}
	putContentsValue(values, 0x0e070003, flags)
	size := 1024 + 2*len(subject)
	for _, field := range calendarProperties {
		if value := row.object.Properties[field.key]; value != nil {
			if !contentsValueType(field.property.kind, value) {
				return projectedMessage{}, core.ErrInvalid
			}
			values[field.property] = value
			if text, ok := value.(core.Text); ok {
				size += 2 * len(text)
			}
			if binary, ok := value.(core.Binary); ok {
				size += len(binary)
			}
		}
	}
	if row.special == core.CalendarFolder {
		for _, tag := range appointmentFixedProperties {
			if value := row.object.Properties[core.Key{ID: tag >> 16}]; value != nil {
				putContentsValue(values, tag, value)
				if text, ok := value.(core.Text); ok {
					size += 2 * len(text)
				}
				if binary, ok := value.(core.Binary); ok {
					size += len(binary)
				}
			}
		}
	}
	result := projectedMessage{values: values, bytes: size}
	if err := projectMessageRecipients(&result, row.object.Recipients); err != nil {
		return projectedMessage{}, err
	}
	if hasCalendarRecipientRows(row) {
		organizer := receivedMeetingOrganizer(row)
		for index, recipient := range row.object.Recipients {
			for i, value := range meetingRecipientValues(recipient, organizer && index == 0) {
				putContentsValue(result.recipients[index], meetingRecipientColumns[i], value)
				switch data := value.(type) {
				case core.Binary:
					result.bytes += len(data)
				case core.Text:
					result.bytes += 2 * len(data)
				}
			}
		}
	}
	return result, nil
}

func calendarVariableProperty(tag uint32) bool {
	id, kind := uint16(tag>>16), uint16(tag)
	if id == uint16(appointmentBodyTag>>16) {
		return kind == 0 || kind == 0x001e || kind == 0x001f
	}
	return id >= 0x8001 && id <= 0xfffe && (kind == 0 || kind == 0x001e || kind == 0x001f || kind == 0x0102)
}

func openedCalendarProperty(row messageRow, tag uint32, names *namedPropertyRegistry, request specificProperties, cp uint32) (folderPropertyValue, bool) {
	missing := folderPropertyValue{kind: 0x000a, code: PropertyNotFound}
	property, err := resolveContentsIdentity(tag, names)
	if err != nil {
		return missing, uint16(tag>>16) >= 0x8001
	}
	if property.name == (propertyName{}) {
		for _, fixed := range appointmentFixedProperties {
			if row.special == core.CalendarFolder && property.id == uint16(fixed>>16) {
				value := row.object.Properties[core.Key{ID: uint32(property.id)}]
				return calendarPropertyValue(property, fixedContentsProperty(fixed), value, tag, names, request, cp), true
			}
		}
		return folderPropertyValue{}, false
	}
	for _, field := range calendarProperties {
		if field.property.name == property.name {
			return calendarPropertyValue(property, field.property, row.object.Properties[field.key], tag, names, request, cp), true
		}
	}
	return missing, true
}

func calendarPropertyValue(property, canonical contentsProperty, value core.Value, tag uint32, names *namedPropertyRegistry, request specificProperties, cp uint32) folderPropertyValue {
	if value == nil {
		return folderPropertyValue{kind: 0x000a, code: PropertyNotFound}
	}
	kind := property.kind
	if kind == 0 {
		kind = canonical.kind
		if kind == 0x001f && !request.wantUnicode {
			kind = 0x001e
		}
	}
	if text, ok := value.(core.Text); ok {
		limit := min(maxFolderStringBytes, request.variableBudget+4)
		if len(text) > limit {
			return folderPropertyValue{kind: 0x000a, code: 0x8007000e}
		}
		data, ok := propertyString(string(text), kind, cp)
		if !ok {
			return folderPropertyValue{kind: 0x000a, code: NotSupported}
		}
		if len(data) > limit {
			return folderPropertyValue{kind: 0x000a, code: 0x8007000e}
		}
		return folderPropertyValue{kind: kind, data: data}
	}
	if kind != canonical.kind {
		return folderPropertyValue{kind: 0x000a, code: NotSupported}
	}
	if value, ok := value.(core.Binary); ok {
		if len(value)+2 > request.variableBudget+4 {
			return folderPropertyValue{kind: 0x000a, code: 0x8007000e}
		}
		data := binary.LittleEndian.AppendUint16(nil, uint16(len(value)))
		return folderPropertyValue{kind: kind, data: append(data, value...)}
	}
	return tableCell(contentsValues{canonical: value}, tag&0xffff0000|uint32(kind), names, cp)
}
