// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/xml"
	"errors"
	"strconv"
	"strings"

	"ewsgo/internal/core"
)

func (m *mailbox) ListAppointments(ctx context.Context, folder core.ID) ([]core.Appointment, error) {
	rows, err := m.ReadCalendarSearchItems(ctx, folder, core.CalendarFolder)
	if err != nil {
		return nil, err
	}
	items := make([]core.Appointment, len(rows))
	used := 0
	for start := 0; start < len(rows); {
		end := min(start+16, len(rows))
		batch, err := m.appointmentDetailsBatch(ctx, folder, rows[start:end])
		if errors.Is(err, core.ErrLimit) && end > start+1 {
			end = start + 1
			batch, err = m.appointmentDetailsBatch(ctx, folder, rows[start:end])
		}
		if err != nil {
			return nil, err
		}
		for i, item := range batch {
			used += 1024 + len(item.Subject) + len(item.UID) + len(item.Location) + len(item.Body)
			if item.Meeting != nil {
				used += len(item.Meeting.Organizer.Address) + len(item.Meeting.Organizer.Name)
				for _, attendee := range item.Meeting.Participants {
					used += 128 + len(attendee.Address) + len(attendee.Name)
				}
			}
			if used > core.MaxMailContentViewBytes {
				return nil, core.ErrLimit
			}
			items[start+i] = item
		}
		start = end
	}
	return items, ctx.Err()
}

func (m *mailbox) GetCalendarAppointment(ctx context.Context, folder, id core.ID) (core.Appointment, error) {
	native, ok := strings.CutPrefix(string(id), core.AppointmentIDPrefix)
	if !ok || native == "" {
		return core.Appointment{}, core.ErrNotFound
	}
	rows, err := m.readCalendarSearchItems(ctx, folder, core.CalendarFolder, native)
	if err != nil {
		return core.Appointment{}, err
	}
	if len(rows) != 1 {
		return core.Appointment{}, core.ErrNotFound
	}
	detail, err := m.readAppointment(ctx, folder, "id", native, true)
	if err != nil {
		return core.Appointment{}, err
	}
	return appointmentDetailForRow(rows[0], *detail, m.owner)
}

func (m *mailbox) appointmentDetailsBatch(ctx context.Context, folder core.ID, rows []core.Object) ([]core.Appointment, error) {
	target, err := m.groupwareFolder(ctx, folder, core.CalendarFolder)
	if err != nil {
		return nil, err
	}
	q := attr(element(contextNS, "BatchRequest", ""), "onerror", "stop")
	for i, row := range rows {
		native := strings.TrimPrefix(string(row.ID), "a:")
		if row.Parent != folder || !target.validItem(native) {
			return nil, core.ErrConflict
		}
		q.Children = append(q.Children, attr(getAppointmentRequest("id", native, true), "requestId", strconv.Itoa(i+1)))
	}
	response, err := m.native.request(ctx, contextNS, "Batch", "", q)
	if err != nil {
		return nil, appointmentError(err)
	}
	if len(response.Children) != len(rows) {
		return nil, core.ErrConflict
	}
	items := make([]core.Appointment, len(rows))
	for i, child := range response.Children {
		if child.get("requestId") != strconv.Itoa(i+1) {
			return nil, ErrProtocol
		}
		if child.XMLName == (xml.Name{Space: soapNS, Local: "Fault"}) {
			return nil, core.ErrConflict
		}
		entry := rows[i]
		detail, err := parseGetAppointmentForTarget(child, target, "id", strings.TrimPrefix(string(entry.ID), "a:"))
		if err != nil {
			if errors.Is(err, core.ErrNotFound) {
				return nil, core.ErrConflict
			}
			return nil, err
		}
		items[i], err = appointmentDetailForRow(entry, *detail, m.owner)
		if err != nil {
			return nil, err
		}
	}
	return items, nil
}

func appointmentDetailForRow(row core.Object, detail node, owner string) (core.Appointment, error) {
	identity, err := appointmentIdentity(detail, row.Parent)
	if err != nil {
		return core.Appointment{}, err
	}
	if identity.Revision != row.Revision || identity.Properties[core.ContentRevision] != row.Properties[core.ContentRevision] {
		return core.Appointment{}, core.ErrConflict
	}
	for _, key := range []core.Key{core.MetadataRevision, core.IsRead, core.ReminderSet, core.ReminderSignalTime} {
		if value, ok := row.Properties[key]; ok {
			identity.Properties[key] = value
		}
	}
	invite, comp := appointmentInvite(detail, func(comp node) bool { return comp.get("ex") != "1" })
	detached := comp == nil
	if detached {
		invite, comp = detachedAppointmentComponent(detail)
		if comp == nil {
			return core.Appointment{}, core.ErrUnsupported
		}
	}
	item, err := appointmentFromComponent(detail, *comp, identity)
	if err != nil {
		return core.Appointment{}, err
	}
	if core.Bool(item.Type == core.AppointmentMaster) != row.Properties[core.AppointmentRecurring] {
		return core.Appointment{}, core.ErrConflict
	}
	if detached {
		// A recipient can have one exception without the series. Preserve its
		// native identity and actual dates as a standalone read-only event.
		item.OriginalStart, err = recurrenceStart(exceptionRecurrenceID(*comp))
		if err != nil {
			return core.Appointment{}, err
		}
		item.Participation = core.AppointmentReceived
	} else {
		item.Participation = appointmentParticipation(*comp, owner)
	}
	if item.Participation == core.AppointmentReceived || item.Participation == core.AppointmentOrganized {
		item.Meeting = appointmentMeeting(*comp, owner)
	}
	if item.AllDay {
		item.StartZone = &core.AppointmentZone{Name: "UTC"}
		item.EndZone = item.StartZone
	} else {
		item.StartZone = appointmentZone(invite, comp.child(mailNS, "s"))
		item.EndZone = appointmentZone(invite, comp.child(mailNS, "e"))
	}
	if item.Type == core.AppointmentMaster && item.Recurrence != nil {
		item.SeriesSchedule = appointmentSeriesSchedule(detail, *comp, item)
	}
	return item, nil
}

func appointmentParticipation(comp node, owner string) core.AppointmentParticipation {
	switch comp.get("isOrg") {
	case "1":
		if comp.child(mailNS, "at") != nil {
			return core.AppointmentOrganized
		}
		return core.AppointmentPersonal
	case "0":
		return core.AppointmentReceived
	case "":
		// GetAppointment on Zimbra 10 can omit isOrg on the recipient's
		// copy. Require a native incoming scheduling component.
		method := comp.get("method")
		org := comp.child(mailNS, "or")
		if (method == "REQUEST" || method == "CANCEL") && org != nil &&
			org.get("a") != "" && !strings.EqualFold(org.get("a"), owner) {
			return core.AppointmentReceived
		}
	}
	return core.AppointmentParticipationUnknown
}

// Only the single-exception form makes a standalone object unambiguous.
func detachedAppointmentComponent(detail node) (node, *node) {
	var invite node
	var component *node
	for _, child := range detail.Children {
		if child.XMLName != (xml.Name{Space: mailNS, Local: "inv"}) {
			continue
		}
		for i := range child.Children {
			comp := &child.Children[i]
			if comp.XMLName != (xml.Name{Space: mailNS, Local: "comp"}) {
				continue
			}
			if component != nil || comp.get("ex") != "1" || comp.child(mailNS, "recur") != nil {
				return node{}, nil
			}
			invite, component = child, comp
		}
	}
	return invite, component
}
