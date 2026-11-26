// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"errors"
	"time"

	"ewsgo/internal/core"
)

func (h *Handler) createAppointments(ctx context.Context, op node, s core.Session) []node {
	const operation = "CreateItem"
	invites := op.attr("SendMeetingInvitations")
	sendMeeting := invites == "SendToAllAndSaveCopy"
	if invites != "" && invites != "SendToNone" && !sendMeeting {
		return syncError(operation, core.ErrUnsupported)
	}
	if disposition := op.attr("MessageDisposition"); disposition != "" && disposition != "SaveOnly" &&
		!(sendMeeting && disposition == "SendAndSaveCopy") {
		return syncError(operation, core.ErrUnsupported)
	}
	writer, ok := s.Mailbox().(core.AppointmentWriter)
	if !ok {
		return syncError(operation, core.ErrUnsupported)
	}
	list := op.child(messages, "Items")
	if !validBatch(list) {
		return syncError(operation, core.ErrInvalid)
	}
	folderRef := at(el(types, "DistinguishedFolderId", ""), "Id", "calendar")
	if saved := op.child(messages, "SavedItemFolderId"); saved != nil {
		if len(saved.Children) != 1 || len(op.Children) != 2 {
			return syncError(operation, core.ErrInvalid)
		}
		folderRef = saved.Children[0]
	} else if len(op.Children) != 1 {
		return syncError(operation, core.ErrInvalid)
	}
	folder, err := h.calendarFolder(ctx, s, folderRef)
	if err != nil {
		return []node{writeFailure(operation, err, "ErrorCannotCreateCalendarItemInNonCalendarFolder")}
	}
	out := make([]node, 0, len(list.Children))
	for _, card := range list.Children {
		if sendMeeting {
			out = append(out, h.sendMeetingInvitation(ctx, s, folder.ID, card))
			continue
		}
		appointment, err := parseAppointmentCreate(card)
		if err != nil {
			out = append(out, writeFailure(operation, err, "ErrorItemNotFound"))
			continue
		}
		saved, err := writer.CreateAppointment(ctx, folder.ID, appointment)
		if err != nil {
			out = append(out, writeFailure(operation, err, "ErrorItemNotFound"))
			continue
		}
		if saved.Type != appointment.Type || saved.Identity.ID == "" || saved.Identity.Parent != folder.ID ||
			saved.Identity.Kind != core.AppointmentItem {
			out = append(out, writeFailure(operation, core.ErrOutcomeUnknown, "ErrorItemNotFound"))
			continue
		}
		out = append(out, success(operation, el(messages, "Items", "", h.appointment(s.Identity().Scope, saved))))
	}
	return out
}

func parseAppointmentCreate(item node) (core.Appointment, error) {
	if !isElement(item, types, "CalendarItem") {
		return core.Appointment{}, core.ErrUnsupported
	}
	var appointment core.Appointment
	var endZone string
	seen := make(map[string]bool, len(item.Children))
	for _, field := range item.Children {
		name := field.XMLName.Local
		if field.XMLName.Space != types || seen[name] {
			return core.Appointment{}, core.ErrInvalid
		}
		seen[name] = true
		if len(field.Children) != 0 && name != "Recurrence" {
			return core.Appointment{}, core.ErrUnsupported
		}
		switch name {
		case "ItemClass":
			if field.Text != "IPM.Appointment" {
				return core.Appointment{}, core.ErrUnsupported
			}
		case "Subject":
			appointment.Subject = field.Text
		case "Location":
			appointment.Location = field.Text
		case "Body":
			if format := field.attr("BodyType"); format != "" && format != "Text" {
				return core.Appointment{}, core.ErrUnsupported
			}
			appointment.Body = field.Text
		case "Start", "End":
			date, err := time.Parse(time.RFC3339Nano, field.Text)
			if err != nil || date.Year() < 1 {
				return core.Appointment{}, core.ErrInvalid
			}
			if name == "Start" {
				appointment.Start = date
			} else {
				appointment.End = date
			}
		case "IsAllDayEvent":
			allDay, err := xmlBool(field.Text)
			if err != nil {
				return core.Appointment{}, err
			}
			appointment.AllDay = allDay
		case "StartTimeZone", "EndTimeZone":
			zone := field.attr("Id")
			if zone == "" || zone == "Local" || len(zone) > 128 {
				return core.Appointment{}, core.ErrInvalid
			}
			if name == "StartTimeZone" {
				appointment.TimeZone = zone
			} else {
				endZone = zone
			}
		case "Recurrence":
			recurrence, err := parseAppointmentRecurrence(field)
			if err != nil {
				return core.Appointment{}, err
			}
			appointment.Type, appointment.Recurrence = core.AppointmentMaster, recurrence
		default:
			return core.Appointment{}, core.ErrUnsupported
		}
	}
	if !seen["Start"] || !seen["End"] {
		return core.Appointment{}, core.ErrInvalid
	}
	if endZone != "" && endZone != appointment.TimeZone {
		return core.Appointment{}, core.ErrUnsupported
	}
	if appointment.Recurrence == nil && appointment.TimeZone != "" {
		return core.Appointment{}, core.ErrUnsupported
	}
	if appointment.AllDay {
		start, err := appointmentTime(appointment.Start, true)
		if err != nil {
			return core.Appointment{}, err
		}
		end, err := appointmentTime(appointment.End, true)
		if err != nil {
			return core.Appointment{}, err
		}
		appointment.Start, appointment.End = start, end
	}
	return appointment, nil
}

func appointmentTime(stamp time.Time, allDay bool) (time.Time, error) {
	if !allDay {
		return stamp.UTC(), nil
	}
	if stamp.Hour() != 0 || stamp.Minute() != 0 || stamp.Second() != 0 || stamp.Nanosecond() != 0 {
		return time.Time{}, core.ErrInvalid
	}
	year, month, day := stamp.Date()
	return time.Date(year, month, day, 0, 0, 0, 0, time.UTC), nil
}

func parseAppointmentDetailsPatch(updates node) (core.AppointmentDetailsPatch, error) {
	var patch core.AppointmentDetailsPatch
	seen := make(map[string]bool, len(updates.Children))
	for _, update := range updates.Children {
		selector := update.child(types, "FieldURI")
		if selector == nil || len(selector.Children) != 0 {
			return patch, core.ErrInvalid
		}
		var field string
		uri := selector.attr("FieldURI")
		switch uri {
		case "item:Subject":
			field = "Subject"
		case "calendar:Location":
			field = "Location"
		case "calendar:Start":
			field = "Start"
		case "calendar:End":
			field = "End"
		default:
			return patch, core.ErrUnsupported
		}
		if seen[field] {
			return patch, core.ErrInvalid
		}
		seen[field] = true
		value := ""
		switch {
		case isElement(update, types, "SetItemField"):
			item := update.child(types, "CalendarItem")
			if len(update.Children) != 2 || item == nil || len(item.Children) != 1 {
				return patch, core.ErrInvalid
			}
			part := item.child(types, field)
			if part == nil || len(part.Children) != 0 {
				return patch, core.ErrInvalid
			}
			value = part.Text
		case isElement(update, types, "DeleteItemField"):
			if len(update.Children) != 1 {
				return patch, core.ErrInvalid
			}
			if field == "Start" || field == "End" {
				return patch, core.ErrUnsupported
			}
		default:
			return patch, core.ErrUnsupported
		}
		switch field {
		case "Subject":
			patch.Subject = &value
		case "Location":
			patch.Location = &value
		case "Start", "End":
			stamp, err := time.Parse(time.RFC3339Nano, value)
			if err != nil || stamp.Year() < 1 {
				return patch, core.ErrInvalid
			}
			if field == "Start" {
				patch.Start = &stamp
			} else {
				patch.End = &stamp
			}
		}
	}
	return patch, nil
}

func (h *Handler) updateAppointment(ctx context.Context, s core.Session, ref reference, key string, updates node, overwrite bool) node {
	const operation = "UpdateItem"
	box := s.Mailbox()
	reader, canRead := box.(core.AppointmentReader)
	writer, canWrite := box.(core.AppointmentDetailsWriter)
	if !canRead || !canWrite {
		return writeFailure(operation, core.ErrUnsupported, "ErrorItemNotFound")
	}
	patch, err := parseAppointmentDetailsPatch(updates)
	if err != nil {
		return writeFailure(operation, err, "ErrorItemNotFound")
	}
	scope := s.Identity().Scope
	for attempt := range 2 {
		current, err := reader.GetAppointment(ctx, ref.Parent, ref.ID)
		if err != nil {
			return writeFailure(operation, err, "ErrorItemNotFound")
		}
		if current.Identity.ID != ref.ID || current.Identity.Parent != ref.Parent || current.Identity.Kind != core.AppointmentItem {
			return failure(operation, "ErrorItemNotFound")
		}
		if overwrite && key != "" {
			revision, err := h.decodeRef(scope, key)
			series := current.SeriesID
			if series == "" {
				series = ref.ID
			}
			if err != nil || revision.ID != "revision:"+series {
				return failure(operation, "ErrorInvalidChangeKey")
			}
		} else if !overwrite && key != h.appointmentChangeKey(scope, current) {
			return failure(operation, "ErrorIrresolvableConflict")
		}
		normalized := patch
		if patch.Start != nil {
			start, err := appointmentTime(*patch.Start, current.AllDay)
			if err != nil {
				return writeFailure(operation, err, "ErrorItemNotFound")
			}
			normalized.Start = &start
		}
		if patch.End != nil {
			end, err := appointmentTime(*patch.End, current.AllDay)
			if err != nil {
				return writeFailure(operation, err, "ErrorItemNotFound")
			}
			normalized.End = &end
		}
		saved, err := writer.UpdateAppointmentDetails(ctx, current, normalized)
		if errors.Is(err, core.ErrConflict) && overwrite && attempt == 0 {
			continue
		}
		if err != nil {
			return writeFailure(operation, err, "ErrorItemNotFound")
		}
		return success(operation, el(messages, "Items", "", h.appointment(scope, saved)))
	}
	return failure(operation, "ErrorIrresolvableConflict")
}

func (h *Handler) trashAppointment(ctx context.Context, s core.Session, ref reference, key string) node {
	const operation = "DeleteItem"
	box := s.Mailbox()
	reader, canRead := box.(core.AppointmentReader)
	writer, canWrite := box.(core.AppointmentWriter)
	if !canRead || !canWrite {
		return failure(operation, "ErrorCannotDeleteObject")
	}
	appointment, err := reader.GetAppointment(ctx, ref.Parent, ref.ID)
	if err != nil {
		return writeFailure(operation, err, "ErrorItemNotFound")
	}
	if appointment.Identity.ID != ref.ID || appointment.Identity.Parent != ref.Parent || appointment.Identity.Kind != core.AppointmentItem {
		return failure(operation, "ErrorItemNotFound")
	}
	if key != "" && key != h.appointmentChangeKey(s.Identity().Scope, appointment) {
		return failure(operation, "ErrorIrresolvableConflict")
	}
	if err := writer.TrashAppointment(ctx, appointment); err != nil {
		if errors.Is(err, core.ErrUnsupported) {
			return failure(operation, "ErrorCannotDeleteObject")
		}
		return writeFailure(operation, err, "ErrorItemNotFound")
	}
	return success(operation)
}
