// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/xml"
	"errors"
	"slices"
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
)

func (m *mailbox) CreateAppointment(ctx context.Context, folder core.ID, input core.Appointment) (core.Appointment, error) {
	return m.createAppointment(ctx, folder, input, rand.Text()+"@ewsgo.invalid")
}

func (m *mailbox) CreateAppointmentWithUID(ctx context.Context, folder core.ID, input core.Appointment, uid string) (core.Appointment, error) {
	if !validAppointmentImportUID(uid) {
		return core.Appointment{}, core.ErrInvalid
	}
	if folder != core.ID("f:"+specialFolderSpec(core.CalendarFolder).id) {
		return core.Appointment{}, core.ErrUnsupported
	}
	return m.createAppointment(ctx, folder, input, uid)
}

func (m *mailbox) FindAppointmentByUID(ctx context.Context, folder core.ID, uid string) (core.Appointment, error) {
	if !validAppointmentImportUID(uid) {
		return core.Appointment{}, core.ErrInvalid
	}
	detail, err := m.readAppointment(ctx, folder, "uid", uid, true)
	if err != nil {
		return core.Appointment{}, err
	}
	if !personalAppointment(*detail) {
		return core.Appointment{}, core.ErrUnsupported
	}
	return readWritableAppointment(*detail, folder)
}

func validAppointmentImportUID(uid string) bool {
	const prefix, suffix = "ewsgo-", "@ewsgo.invalid"
	if len(uid) != len(prefix)+32+len(suffix) || !strings.HasPrefix(uid, prefix) || !strings.HasSuffix(uid, suffix) {
		return false
	}
	_, err := hex.DecodeString(uid[len(prefix) : len(uid)-len(suffix)])
	return err == nil
}

func (m *mailbox) createAppointment(ctx context.Context, folder core.ID, input core.Appointment, uid string) (core.Appointment, error) {
	target, err := m.groupwareFolder(ctx, folder, core.CalendarFolder)
	if err != nil {
		return core.Appointment{}, err
	}
	if target.itemPrefix != "" && input.Type != core.AppointmentSingle {
		return core.Appointment{}, core.ErrUnsupported
	}
	message, err := appointmentCreateMessage(target.nativeFolder, input, uid)
	if err != nil {
		return core.Appointment{}, err
	}
	zone := message.child(mailNS, "inv").child(mailNS, "comp").child(mailNS, "s").get("tz")
	req := element(mailNS, "CreateAppointmentRequest", "", message)
	ack, err := m.native.request(ctx, mailNS, "CreateAppointment", "", req)
	if err != nil {
		if mapped := messageWriteError(err); !errors.Is(mapped, core.ErrOutcomeUnknown) {
			return core.Appointment{}, mapped
		}
	}

	// A repeated CreateAppointment with the same UID can modify the first
	// object. Resolve a missing acknowledgement by UID without resending.
	field, value := "uid", uid
	if id := ack.get("calItemId"); err == nil && target.validItem(id) {
		field, value = "id", id
	}
	if target.itemPrefix != "" && field != "id" {
		return core.Appointment{}, core.ErrOutcomeUnknown
	}
	detail, err := m.readAppointment(ctx, folder, field, value, true)
	if err != nil || detail.get("uid") != uid {
		return core.Appointment{}, core.ErrOutcomeUnknown
	}
	item, err := readWritableAppointment(*detail, folder)
	if err != nil || item.Type != input.Type || item.AllDay != input.AllDay ||
		!item.Start.Equal(input.Start) || !item.End.Equal(input.End) || item.Subject != input.Subject ||
		item.Location != input.Location || item.Body != input.Body || item.TimeZone != zone {
		return core.Appointment{}, core.ErrOutcomeUnknown
	}
	if !sameAppointmentRecurrence(input.Recurrence, item.Recurrence) {
		// Zimbra can silently cap explicit COUNT, UNTIL and open-ended rules.
		// Revert only the exact new revision acknowledged by the native server.
		if field == "id" && ack.get("ms") == detail.get("ms") && ack.get("rev") == detail.get("rev") {
			if err := m.TrashAppointment(ctx, item); err == nil {
				return core.Appointment{}, core.ErrUnsupported
			}
		}
		return core.Appointment{}, core.ErrOutcomeUnknown
	}
	return item, nil
}

func appointmentCreateMessage(folder string, input core.Appointment, uid string) (node, error) {
	if (input.Type != core.AppointmentSingle && input.Type != core.AppointmentMaster) ||
		(input.Type == core.AppointmentMaster) != (input.Recurrence != nil) ||
		input.UID != "" || input.Start.IsZero() || input.End.Before(input.Start) {
		return node{}, core.ErrInvalid
	}
	start, end := input.Start.UTC(), input.End.UTC()
	if start.Year() < 1 || end.Year() > 9999 {
		return node{}, core.ErrInvalid
	}
	format := zimbraRecurrenceStart
	var recurrence node
	zone := ""
	if input.Recurrence != nil {
		if !input.AllDay && input.TimeZone != "" && input.TimeZone != "UTC" {
			loc, err := time.LoadLocation(input.TimeZone)
			if err != nil || input.TimeZone == "Local" {
				return node{}, core.ErrUnsupported
			}
			start, end = start.In(loc), end.In(loc)
			if ambiguousAppointmentTime(start) || ambiguousAppointmentTime(end) {
				return node{}, core.ErrUnsupported
			}
			zone, format = input.TimeZone, zimbraLocalAppointmentTime
		}
		var err error
		recurrence, err = nativeRecurrence(*input.Recurrence, start, input.AllDay)
		if err != nil {
			return node{}, err
		}
	}
	if input.AllDay {
		if !start.Before(end) || !start.Equal(start.Truncate(24*time.Hour)) || !end.Equal(end.Truncate(24*time.Hour)) {
			return node{}, core.ErrInvalid
		}
		format = zimbraDay
		end = end.AddDate(0, 0, -1) // Zimbra uses the last included civil day.
	} else if start.Nanosecond() != 0 || end.Nanosecond() != 0 {
		return node{}, core.ErrInvalid
	}
	s := attr(element(mailNS, "s", ""), "d", start.Format(format))
	e := attr(element(mailNS, "e", ""), "d", end.Format(format))
	if zone != "" {
		s, e = attr(s, "tz", zone), attr(e, "tz", zone)
	}
	comp := element(mailNS, "comp", "", s, e)
	if input.Recurrence != nil {
		comp.Children = append(comp.Children, recurrence)
	}
	comp = attr(attr(attr(comp, "name", input.Subject), "loc", input.Location), "uid", uid)
	comp = attr(attr(comp, "isOrg", "1"), "allDay", strconv.FormatBool(input.AllDay))
	children := []node{element(mailNS, "su", input.Subject), element(mailNS, "inv", "", comp)}
	if input.Body != "" {
		children = append(children, attr(element(mailNS, "mp", "",
			element(mailNS, "content", input.Body)), "ct", "text/plain"))
	}
	message := attr(element(mailNS, "m", "", children...), "l", folder)
	return message, nil
}

func readWritableAppointment(detail node, folder core.ID) (core.Appointment, error) {
	identity, err := appointmentIdentity(detail, folder)
	if err != nil {
		return core.Appointment{}, err
	}
	stored := masterComponent(detail)
	if stored == nil {
		return core.Appointment{}, core.ErrUnsupported
	}
	return appointmentFromComponent(detail, *stored, identity)
}

func (m *mailbox) TrashAppointment(ctx context.Context, before core.Appointment) error {
	o := before.Identity
	key, ok := strings.CutPrefix(string(o.ID), core.AppointmentIDPrefix)
	id, rid, instance := strings.Cut(key, "@")
	if !ok || !nativeID.MatchString(id) || o.Kind != core.AppointmentItem || o.Revision == 0 ||
		instance != (before.Type == core.AppointmentOccurrence || before.Type == core.AppointmentException) {
		return core.ErrUnsupported
	}
	current, err := m.readAppointment(ctx, o.Parent, "id", id, false)
	if err != nil {
		return err
	}
	identity, err := appointmentIdentity(*current, o.Parent)
	if err != nil {
		return err
	}
	if identity.Revision != o.Revision || identity.Properties[core.ContentRevision] != o.Properties[core.ContentRevision] {
		return core.ErrConflict
	}
	if masterComponent(*current) == nil || !personalAppointment(*current) {
		return core.ErrUnsupported
	}
	if instance {
		return m.cancelCalendarInstance(ctx, *current, before, id, rid)
	}
	action := attr(attr(element(mailNS, "action", ""), "id", id), "op", "trash")
	ack, err := m.native.request(ctx, mailNS, "ItemAction", "", element(mailNS, "ItemActionRequest", "", action), modifiedConstraint(o.Revision))
	if err != nil {
		return messageWriteError(err)
	}
	item := ack.child(mailNS, "action")
	if len(ack.Children) != 1 || item == nil || item.get("id") != id || item.get("op") != "trash" {
		return core.ErrOutcomeUnknown
	}
	if _, err := m.readAppointment(ctx, o.Parent, "id", id, false); !errors.Is(err, core.ErrNotFound) {
		return core.ErrOutcomeUnknown
	}
	return nil
}

func (m *mailbox) cancelCalendarInstance(ctx context.Context, detail node, before core.Appointment, id, rid string) error {
	format := zimbraRecurrenceStart
	if before.AllDay {
		format = zimbraDay
	}
	if rid == "" || rid != before.OriginalStart.Format(format) {
		return core.ErrInvalid
	}
	invite, master := appointmentInvite(detail, func(comp node) bool { return comp.get("ex") != "1" })
	if master == nil || master.child(mailNS, "recur") == nil {
		return core.ErrConflict
	}
	if before.Type == core.AppointmentException {
		// Use the exception's invite sequence when it has already been edited.
		// The recurrence anchor still identifies the original slot.
		var exception *node
		invite, exception = appointmentInvite(detail, func(comp node) bool {
			return comp.get("ex") == "1" && exceptionRecurrenceID(comp) == rid
		})
		if exception == nil {
			return core.ErrConflict
		}
	}
	inviteID, compNum, err := appointmentInviteAddress(invite)
	if err != nil {
		return err
	}
	req := element(mailNS, "CancelAppointmentRequest", "", attr(element(mailNS, "inst", ""), "d", rid))
	req = attr(attr(req, "id", id+"-"+inviteID), "comp", compNum)
	o := before.Identity
	// The revision lives in the SOAP change context for this native request.
	ack, err := m.native.request(ctx, mailNS, "CancelAppointment", "", req, modifiedConstraint(o.Revision))
	if err != nil {
		return messageWriteError(err)
	}
	if len(ack.Children) != 0 {
		return core.ErrOutcomeUnknown
	}
	current, err := m.readAppointment(ctx, o.Parent, "id", id, false)
	if err != nil {
		return core.ErrOutcomeUnknown
	}
	changed, err := appointmentIdentity(*current, o.Parent)
	if err != nil || changed.Revision <= o.Revision || masterComponent(*current) == nil {
		return core.ErrOutcomeUnknown
	}
	if cancelled := appointmentComponent(*current, func(_, comp node) bool {
		return comp.get("ex") == "1" && comp.get("status") == "CANC" && exceptionRecurrenceID(comp) == rid
	}); cancelled == nil {
		return core.ErrOutcomeUnknown
	}
	return nil
}

func (m *mailbox) UpdateAppointmentDetails(ctx context.Context, before core.Appointment, patch core.AppointmentDetailsPatch) (core.Appointment, error) {
	return m.updateAppointmentDetails(ctx, before, patch, false)
}

func (m *mailbox) UpdateMeetingDetails(ctx context.Context, before core.Appointment, patch core.AppointmentDetailsPatch) (core.Appointment, error) {
	if before.Type != core.AppointmentSingle || before.AllDay || before.Cancelled {
		return core.Appointment{}, core.ErrUnsupported
	}
	return m.updateAppointmentDetails(ctx, before, patch, true)
}

func (m *mailbox) updateAppointmentDetails(ctx context.Context, before core.Appointment, patch core.AppointmentDetailsPatch, sendMeeting bool) (core.Appointment, error) {
	o := before.Identity
	key, ok := strings.CutPrefix(string(o.ID), core.AppointmentIDPrefix)
	id, rid, instance := strings.Cut(key, "@")
	newException := before.Type == core.AppointmentOccurrence
	shared := strings.HasPrefix(string(o.Parent), core.SharedGroupwareFolderIDPrefix)
	if !ok || !nativeID.MatchString(id) && !shared || o.Kind != core.AppointmentItem || o.Revision == 0 ||
		instance != (before.Type == core.AppointmentException || newException) {
		return core.Appointment{}, core.ErrUnsupported
	}
	if shared && (sendMeeting || before.Type != core.AppointmentSingle) {
		return core.Appointment{}, core.ErrUnsupported
	}
	detail, err := m.readAppointment(ctx, o.Parent, "id", id, true)
	if err != nil {
		return core.Appointment{}, err
	}
	identity, err := appointmentIdentity(*detail, o.Parent)
	if err != nil {
		return core.Appointment{}, err
	}
	if identity.Revision != o.Revision || identity.Properties[core.ContentRevision] != o.Properties[core.ContentRevision] {
		return core.Appointment{}, core.ErrConflict
	}
	if strings.Contains(detail.get("f"), "a") || !sendMeeting && !personalAppointment(*detail) {
		return core.Appointment{}, core.ErrUnsupported
	}
	invite, comp := appointmentInvite(*detail, func(candidate node) bool {
		exception := candidate.get("ex") == "1"
		return exception == (instance && !newException) && (!exception || exceptionRecurrenceID(candidate) == rid)
	})
	if comp == nil || (comp.child(mailNS, "recur") != nil) != (before.Type == core.AppointmentMaster || newException) {
		return core.Appointment{}, core.ErrConflict
	}
	if invite.child(mailNS, "mp") != nil {
		return core.Appointment{}, core.ErrUnsupported
	}
	var recipients []node
	if sendMeeting {
		recipients, err = m.meetingRecipients(*comp)
		if err != nil {
			return core.Appointment{}, err
		}
	}
	subject, location := comp.get("name"), comp.get("loc")
	if patch.Subject != nil {
		subject = *patch.Subject
	}
	if patch.Location != nil {
		location = *patch.Location
	}
	start, end := before.Start, before.End
	if patch.Start != nil {
		start = patch.Start.UTC()
	}
	if patch.End != nil {
		end = patch.End.UTC()
	}
	rescheduled := !start.Equal(before.Start) || !end.Equal(before.End)
	if rescheduled {
		// Zimbra can implicitly cancel every exception when a master changes
		// its bounds. Only instance and standalone edits can use this path.
		if before.Type == core.AppointmentMaster {
			return core.Appointment{}, core.ErrUnsupported
		}
		if start.Year() < 1 || end.Year() > 9999 || end.Before(start) || sendMeeting && !start.Before(end) ||
			start.Nanosecond() != 0 || end.Nanosecond() != 0 ||
			(before.AllDay && (!start.Before(end) || !atMidnightUTC(start) || !atMidnightUTC(end))) {
			return core.Appointment{}, core.ErrInvalid
		}
	}
	if !rescheduled && subject == comp.get("name") && location == comp.get("loc") {
		if sendMeeting {
			return core.Appointment{}, core.ErrInvalid
		}
		return before, nil
	}
	inviteID, compNum, err := appointmentInviteAddress(invite)
	if err != nil {
		return core.Appointment{}, err
	}
	// Zimbra modifies an invite using a full component. Copy the native
	// component and timezone definitions so projected fields cannot erase them.
	replacement := invite
	replacement.Children = slices.Clone(invite.Children)
	operation, expected := "ModifyAppointment", before.Type
	for i := range replacement.Children {
		if &invite.Children[i] == comp {
			part := replaceAppointmentAttr(*comp, "name", subject)
			part = replaceAppointmentAttr(part, "loc", location)
			if newException {
				part, err = occurrenceComponent(part, before, rid, start, end)
				if err != nil {
					return core.Appointment{}, err
				}
				operation, expected = "CreateAppointmentException", core.AppointmentException
			} else if rescheduled {
				part, err = rescheduleAppointmentComponent(part, before, start, end)
				if err != nil {
					return core.Appointment{}, err
				}
			}
			replacement.Children[i] = part
			break
		}
	}
	children := append(recipients, element(mailNS, "su", subject), replacement)
	msg := attr(element(mailNS, "m", "", children...), "l", detail.get("l"))
	req := attr(attr(element(mailNS, operation+"Request", "", msg), "id", id+"-"+inviteID), "comp", compNum)
	req = attr(attr(req, "ms", detail.get("ms")), "rev", detail.get("rev"))
	ack, err := m.native.request(ctx, mailNS, operation, "", req)
	if err != nil {
		var fault *Fault
		if errors.As(err, &fault) && fault.Code == "mail.INVITE_OUT_OF_DATE" {
			return core.Appointment{}, core.ErrConflict
		}
		return core.Appointment{}, messageWriteError(err)
	}
	if ack.get("calItemId") != id {
		return core.Appointment{}, core.ErrOutcomeUnknown
	}
	saved, err := m.GetAppointment(ctx, o.Parent, o.ID)
	if err != nil || saved.Identity.Revision <= o.Revision || saved.Type != expected || saved.UID != before.UID ||
		saved.Subject != subject || saved.Location != location || saved.Body != before.Body ||
		!saved.Start.Equal(start) || !saved.End.Equal(end) || saved.AllDay != before.AllDay ||
		!saved.OriginalStart.Equal(before.OriginalStart) {
		return core.Appointment{}, core.ErrOutcomeUnknown
	}
	return saved, nil
}

func personalAppointment(detail node) bool {
	return appointmentComponent(detail, func(_, comp node) bool {
		tombstone := comp.get("ex") == "1" && comp.get("status") == "CANC"
		return comp.child(mailNS, "at") != nil || (!tombstone && comp.get("isOrg") != "1")
	}) == nil
}

func appointmentInvite(detail node, matches func(node) bool) (node, *node) {
	var parent node
	part := appointmentComponent(detail, func(invite, comp node) bool {
		if !matches(comp) {
			return false
		}
		parent = invite
		return true
	})
	return parent, part
}

func appointmentInviteAddress(invite node) (string, string, error) {
	id, number := invite.get("id"), invite.get("compNum")
	if _, err := strconv.ParseUint(number, 10, 31); !nativeID.MatchString(id) || err != nil {
		return "", "", ErrProtocol
	}
	return id, number, nil
}

func occurrenceComponent(comp node, before core.Appointment, rid string, start, end time.Time) (node, error) {
	format := zimbraRecurrenceStart
	if before.AllDay {
		format, end = zimbraDay, end.AddDate(0, 0, -1)
	}
	if rid != before.OriginalStart.Format(format) || start.Nanosecond() != 0 || end.Nanosecond() != 0 {
		return node{}, core.ErrUnsupported
	}
	children := make([]node, 0, len(comp.Children))
	for _, child := range comp.Children {
		if child.XMLName.Space == mailNS {
			switch child.XMLName.Local {
			case "s", "e", "dur", "recur", "exceptId":
				continue
			}
		}
		children = append(children, child)
	}
	comp.Children = append(children,
		attr(element(mailNS, "exceptId", ""), "d", rid),
		attr(element(mailNS, "s", ""), "d", start.Format(format)),
		attr(element(mailNS, "e", ""), "d", end.Format(format)),
	)
	return comp, nil
}

func atMidnightUTC(stamp time.Time) bool {
	return stamp.Equal(stamp.Truncate(24 * time.Hour))
}

func rescheduleAppointmentComponent(comp node, original core.Appointment, start, end time.Time) (node, error) {
	comp.Children = slices.Clone(comp.Children)
	for i, child := range comp.Children {
		if child.XMLName.Space != mailNS {
			continue
		}
		var before, target time.Time
		switch child.XMLName.Local {
		case "s":
			before, target = original.Start, start
		case "e":
			before, target = original.End, end
		default:
			continue
		}
		if before.Equal(target) {
			continue
		}
		if original.AllDay && child.XMLName.Local == "e" {
			target = target.AddDate(0, 0, -1) // Last included Zimbra civil day.
		}
		part, err := rescheduledAppointmentBound(child, before, target, original.AllDay)
		if err != nil {
			return node{}, err
		}
		comp.Children[i] = part
	}
	return comp, nil
}

func rescheduledAppointmentBound(bound node, original, target time.Time, allDay bool) (node, error) {
	part := element(mailNS, bound.XMLName.Local, "")
	if allDay {
		return attr(part, "d", target.Format(zimbraDay)), nil
	}
	zone := bound.get("tz")
	if zone == "" && strings.HasSuffix(bound.get("d"), "Z") {
		return attr(part, "d", target.Format(zimbraRecurrenceStart)), nil
	}
	loc, err := time.LoadLocation(zone)
	if err != nil || zone == "" || original.In(loc).Format(zimbraLocalAppointmentTime) != bound.get("d") {
		return node{}, core.ErrUnsupported
	}
	wall := target.In(loc)
	if ambiguousAppointmentTime(wall) {
		return node{}, core.ErrUnsupported
	}
	return attr(attr(part, "d", wall.Format(zimbraLocalAppointmentTime)), "tz", zone), nil
}

const zimbraLocalAppointmentTime = "20060102T150405"

func ambiguousAppointmentTime(wall time.Time) bool {
	zoneStart, zoneEnd := wall.ZoneBounds()
	_, offset := wall.Zone()
	loc := wall.Location()
	date := wall.Format(zimbraLocalAppointmentTime)
	for _, boundary := range []time.Time{zoneStart, zoneEnd} {
		if boundary.IsZero() {
			continue
		}
		if boundary.Equal(zoneStart) {
			boundary = boundary.Add(-time.Second)
		}
		_, adjacent := boundary.In(loc).Zone()
		if adjacent != offset && wall.Add(time.Duration(offset-adjacent)*time.Second).Format(zimbraLocalAppointmentTime) == date {
			// The native local DATETIME has no offset to disambiguate the fold.
			return true
		}
	}
	return false
}

func replaceAppointmentAttr(n node, key, value string) node {
	name := xml.Name{Local: key}
	n.Attrs = slices.Clone(n.Attrs)
	for i, a := range n.Attrs {
		if a.Name == name {
			n.Attrs[i].Value = value
			return n
		}
	}
	return attr(n, key, value)
}
