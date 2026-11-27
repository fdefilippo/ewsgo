// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
)

var appointmentFields = map[string]string{
	"calendar:UID": "UID", "calendar:Start": "Start", "calendar:End": "End",
	"calendar:OriginalStart": "OriginalStart", "calendar:IsAllDayEvent": "IsAllDayEvent",
	"calendar:Location": "Location", "calendar:IsCancelled": "IsCancelled",
	"calendar:IsRecurring": "IsRecurring", "calendar:CalendarItemType": "CalendarItemType",
	"calendar:Recurrence": "Recurrence",
	"calendar:IsMeeting":  "IsMeeting", "calendar:Organizer": "Organizer",
	"calendar:RequiredAttendees": "RequiredAttendees", "calendar:OptionalAttendees": "OptionalAttendees",
	"calendar:MyResponseType": "MyResponseType", "calendar:IsResponseRequested": "IsResponseRequested",
}

func (s responseShape) appointmentOnly() bool {
	for _, name := range appointmentFields {
		if s.extra[name] {
			return true
		}
	}
	return false
}

func (s responseShape) acceptsAppointment() bool {
	if s.mime || len(s.extended) != 0 || s.extra["Attachments"] {
		return false
	}
	for field, name := range contactFields {
		if strings.HasPrefix(field, "contacts:") && s.extra[name] {
			return false
		}
	}
	return true
}

func isCalendarFolderReference(ref node) bool {
	return isElement(ref, types, "DistinguishedFolderId") && ref.attr("Id") == "calendar"
}

func (h *Handler) calendarFolder(ctx context.Context, s core.Session, ref node) (core.Object, error) {
	box := s.Mailbox()
	folders, ok := box.(core.SpecialFolderReader)
	if _, appointments := box.(core.AppointmentReader); !ok || !appointments {
		return core.Object{}, core.ErrUnsupported
	}
	distinguished := isCalendarFolderReference(ref)
	var id core.ID
	if !distinguished {
		if !isElement(ref, types, "FolderId") {
			return core.Object{}, core.ErrInvalid
		}
		var err error
		id, err = h.decode(s.Identity().Scope, ref.attr("Id"))
		if err != nil {
			return core.Object{}, err
		}
		if strings.HasPrefix(string(id), core.SharedGroupwareFolderIDPrefix) {
			if shared, ok := box.(core.SharedGroupwareFolders); ok {
				return shared.ReadSharedGroupwareFolder(ctx, id, core.CalendarFolder)
			}
			return core.Object{}, core.ErrNotFound
		}
	}
	folder, err := folders.SpecialFolder(ctx, core.CalendarFolder)
	if err != nil {
		return core.Object{}, err
	}
	if !distinguished && id != folder.ID {
		return core.Object{}, core.ErrNotFound
	}
	return folder, nil
}

func (h *Handler) findAppointments(ctx context.Context, op node, s core.Session, shape responseShape) []node {
	const operation = "FindItem"
	if op.attr("Traversal") != "Shallow" || !shape.acceptsAppointment() || shape.extra["Body"] {
		return syncError(operation, core.ErrUnsupported)
	}
	seen := make(map[string]bool, len(op.Children))
	for _, part := range op.Children {
		name := part.XMLName.Local
		if part.XMLName.Space != messages || seen[name] {
			return syncError(operation, core.ErrInvalid)
		}
		seen[name] = true
		switch name {
		case "ItemShape", "CalendarView", "ParentFolderIds":
		default:
			return syncError(operation, core.ErrUnsupported)
		}
	}
	view := op.child(messages, "CalendarView")
	parents := op.child(messages, "ParentFolderIds")
	if view == nil || len(view.Children) != 0 || parents == nil || len(parents.Children) != 1 {
		return syncError(operation, core.ErrInvalid)
	}
	from, startErr := time.Parse(time.RFC3339Nano, view.attr("StartDate"))
	to, endErr := time.Parse(time.RFC3339Nano, view.attr("EndDate"))
	if startErr != nil || endErr != nil || from.UTC().Year() < 1 || to.UTC().Year() > 9999 {
		return syncError(operation, core.ErrInvalid)
	}
	if to.Before(from) {
		return []node{failure(operation, "ErrorCalendarEndDateIsEarlierThanStartDate")}
	}
	if to.After(from.UTC().AddDate(2, 0, 0)) {
		return []node{failure(operation, "ErrorCalendarViewRangeTooBig")}
	}
	limit := 1000
	if raw := view.attr("MaxEntriesReturned"); raw != "" {
		value, err := strconv.ParseUint(raw, 10, 32)
		if err != nil || value == 0 {
			return syncError(operation, core.ErrInvalid)
		}
		limit = int(min(value, uint64(limit)))
	}
	folder, err := h.calendarFolder(ctx, s, parents.Children[0])
	if err != nil {
		return []node{writeFailure(operation, err, "ErrorFolderNotFound")}
	}
	reader := s.Mailbox().(core.AppointmentReader)
	items, err := reader.CalendarView(ctx, folder.ID, from, to)
	if err != nil {
		return []node{writeFailure(operation, err, "ErrorFolderNotFound")}
	}
	slices.SortFunc(items, func(a, b core.Appointment) int {
		if when := a.Start.Compare(b.Start); when != 0 {
			return when
		}
		return strings.Compare(string(a.Identity.ID), string(b.Identity.ID))
	})
	count := min(limit, len(items))
	rendered := make([]node, 0, count)
	scope := s.Identity().Scope
	for _, item := range items[:count] {
		if item.Identity.Kind != core.AppointmentItem || item.Identity.Parent != folder.ID || item.Type > core.AppointmentException {
			return []node{failure(operation, "ErrorInternalServerError")}
		}
		rendered = append(rendered, shape.apply(h.appointment(scope, item)))
	}
	root := el(messages, "RootFolder", "", el(types, "Items", "", rendered...))
	root = at(at(root, "TotalItemsInView", strconv.Itoa(len(items))), "IncludesLastItemInRange", strconv.FormatBool(len(rendered) == len(items)))
	return []node{success(operation, root)}
}

func (h *Handler) getAppointment(ctx context.Context, s core.Session, ref reference, shape responseShape) node {
	reader, ok := s.Mailbox().(core.AppointmentReader)
	if !ok {
		return itemError(core.ErrNotFound)
	}
	if !shape.acceptsAppointment() {
		return itemError(core.ErrUnsupported)
	}
	item, err := reader.GetAppointment(ctx, ref.Parent, ref.ID)
	if errors.Is(err, core.ErrLimit) {
		return failure("GetItem", "ErrorDataSizeLimitExceeded")
	}
	if err != nil {
		return itemError(err)
	}
	if item.Identity.Kind != core.AppointmentItem || item.Identity.Parent != ref.Parent || item.Identity.ID != ref.ID {
		return itemError(core.ErrNotFound)
	}
	if item.Type > core.AppointmentMaster {
		return failure("GetItem", "ErrorInternalServerError")
	}
	return success("GetItem", el(messages, "Items", "", shape.apply(h.appointment(s.Identity().Scope, item))))
}

func (h *Handler) getRecurringMaster(ctx context.Context, s core.Session, selector node, shape responseShape) node {
	if !shape.acceptsAppointment() {
		return itemError(core.ErrUnsupported)
	}
	scope := s.Identity().Scope
	ref, err := h.decodeRef(scope, selector.attr("OccurrenceId"))
	if err != nil {
		return itemError(core.ErrNotFound)
	}
	reader, ok := s.Mailbox().(core.AppointmentReader)
	if !ok || !strings.HasPrefix(string(ref.ID), core.AppointmentIDPrefix) {
		return failure("GetItem", "ErrorCalendarCannotUseIdForRecurringMasterId")
	}
	instance, err := reader.GetAppointment(ctx, ref.Parent, ref.ID)
	if err != nil {
		return itemError(err)
	}
	if instance.Identity.ID != ref.ID || instance.Identity.Parent != ref.Parent ||
		(instance.Type != core.AppointmentOccurrence && instance.Type != core.AppointmentException) || instance.SeriesID == "" {
		return failure("GetItem", "ErrorCalendarCannotUseIdForRecurringMasterId")
	}
	master, err := reader.GetAppointment(ctx, ref.Parent, instance.SeriesID)
	if err != nil {
		return itemError(err)
	}
	if master.Identity.ID != instance.SeriesID || master.Identity.Parent != ref.Parent || master.Type != core.AppointmentMaster {
		return failure("GetItem", "ErrorCalendarCannotUseIdForRecurringMasterId")
	}
	return success("GetItem", el(messages, "Items", "", shape.apply(h.appointment(scope, master))))
}

func (h *Handler) appointment(scope core.Scope, appointment core.Appointment) node {
	item := appointment.Identity
	children := []node{
		at(at(el(types, "ItemId", ""), "Id", h.itemID(scope, item)), "ChangeKey", h.changeKey(scope, item)),
		at(el(types, "ParentFolderId", ""), "Id", h.encode(scope, item.Parent)),
		el(types, "ItemClass", "IPM.Appointment"),
		el(types, "Subject", appointment.Subject),
	}
	if appointment.Body != "" {
		children = append(children, at(el(types, "Body", appointment.Body), "BodyType", "Text"))
	}
	if modified, ok := item.Properties[core.LastModifiedAt].(core.Timestamp); ok {
		children = append(children, el(types, "LastModifiedTime", time.Time(modified).UTC().Format(time.RFC3339Nano)))
	}
	if appointment.UID != "" {
		children = append(children, el(types, "UID", appointment.UID))
	}
	children = append(children, el(types, "Start", appointment.Start.UTC().Format(time.RFC3339Nano)),
		el(types, "End", appointment.End.UTC().Format(time.RFC3339Nano)))
	if appointment.Type == core.AppointmentOccurrence || appointment.Type == core.AppointmentException {
		children = append(children, el(types, "OriginalStart", appointment.OriginalStart.UTC().Format(time.RFC3339Nano)))
	}
	kind := [...]string{"Single", "Occurrence", "Exception", "RecurringMaster"}[appointment.Type]
	children = append(children,
		el(types, "IsAllDayEvent", strconv.FormatBool(appointment.AllDay)),
		el(types, "Location", appointment.Location))
	if appointment.Meeting != nil {
		children = append(children, el(types, "IsMeeting", "true"))
	}
	children = append(children,
		el(types, "IsCancelled", strconv.FormatBool(appointment.Cancelled)),
		el(types, "IsRecurring", strconv.FormatBool(appointment.Type != core.AppointmentSingle)),
		el(types, "CalendarItemType", kind))
	if meeting := appointment.Meeting; meeting != nil {
		children = append(children, appointmentMeetingFields(*meeting)...)
	}
	if appointment.Recurrence != nil {
		children = append(children, appointmentRecurrence(*appointment.Recurrence))
	}
	return el(types, "CalendarItem", "", children...)
}

func appointmentMeetingFields(meeting core.AppointmentMeeting) []node {
	ownStatus := meetingReplyStatus(meeting.OwnerReply)
	if meeting.OwnerOrganizer {
		ownStatus = "Organizer"
	}
	parts := []node{el(types, "IsResponseRequested", strconv.FormatBool(meeting.RequestResponse)),
		el(types, "MyResponseType", ownStatus),
		el(types, "Organizer", "", mailboxAddress(meeting.Organizer.Name, meeting.Organizer.Address))}
	var required, optional []node
	for _, person := range meeting.Participants {
		attendee := el(types, "Attendee", "", mailboxAddress(person.Name, person.Address),
			el(types, "ResponseType", meetingReplyStatus(person.Reply)))
		if person.Optional {
			optional = append(optional, attendee)
		} else {
			required = append(required, attendee)
		}
	}
	if len(required) != 0 {
		parts = append(parts, el(types, "RequiredAttendees", "", required...))
	}
	if len(optional) != 0 {
		parts = append(parts, el(types, "OptionalAttendees", "", optional...))
	}
	return parts
}

func meetingReplyStatus(choice core.MeetingReplyChoice) string {
	switch choice {
	case core.MeetingAccept:
		return "Accept"
	case core.MeetingTentative:
		return "Tentative"
	case core.MeetingDecline:
		return "Decline"
	default:
		return "NoResponseReceived"
	}
}
