// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/xml"
	"errors"
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
)

const (
	maxAppointmentInstances = 1000
	zimbraDay               = "20060102"
	zimbraRecurrenceStart   = "20060102T150405Z"
)

func (m *mailbox) CalendarView(ctx context.Context, folder core.ID, from, to time.Time) ([]core.Appointment, error) {
	return m.calendarView(ctx, folder, "", from, to)
}

func (m *mailbox) calendarView(ctx context.Context, folder core.ID, id string, from, to time.Time) ([]core.Appointment, error) {
	spec := specialFolderSpec(core.CalendarFolder)
	target, err := m.groupwareFolder(ctx, folder, core.CalendarFolder)
	if err != nil {
		return nil, err
	}
	query := "inid:" + target.searchFolder
	if id != "" && target.itemPrefix == "" {
		query += " item:" + id
	}
	// DATE values are timezone independent in EWS. In Search they are
	// anchored to a Zimbra zone, so fetch a margin and filter the projected
	// intervals below. Preserve exact timed boundaries in the final result.
	req := element(mailNS, "SearchRequest", "", element(mailNS, "query", query))
	req = attr(attr(attr(req, "types", spec.view), "offset", "0"), "limit", strconv.Itoa(maxAppointmentInstances))
	req = attr(attr(req, "calExpandInstStart", strconv.FormatInt(from.Add(-24*time.Hour).UnixMilli(), 10)),
		"calExpandInstEnd", strconv.FormatInt(to.Add(24*time.Hour).UnixMilli(), 10))
	response, err := m.native.request(ctx, mailNS, "Search", "", req)
	if err != nil {
		return nil, appointmentError(err)
	}
	switch response.get("more") {
	case "0", "false":
	case "1", "true":
		return nil, core.ErrLimit
	default:
		return nil, ErrProtocol
	}
	if response.get("offset") != "0" || len(response.Children) > maxAppointmentInstances {
		return nil, ErrProtocol
	}

	seen := make(map[core.ID]bool)
	var items []core.Appointment
	count := 0
	for _, row := range response.Children {
		key := row.get("id")
		if row.XMLName != (xml.Name{Space: mailNS, Local: "appt"}) || !target.validItem(key) ||
			row.get("l") != target.nativeFolder {
			return nil, ErrProtocol
		}
		if !target.canReadAppointment(row.get("class")) {
			continue
		}
		if id != "" && id != key {
			if target.itemPrefix == "" {
				return nil, ErrProtocol
			}
			continue
		}
		identity, err := appointmentIdentity(row, folder)
		if err != nil {
			return nil, err
		}
		var detail *node
		for _, inst := range row.Children {
			if inst.XMLName != (xml.Name{Space: mailNS, Local: "inst"}) {
				continue
			}
			count++
			if count > maxAppointmentInstances {
				return nil, core.ErrLimit
			}
			item, err := appointmentInstance(row, inst, identity)
			if err != nil {
				return nil, err
			}
			if item.AllDay || item.Type == core.AppointmentException {
				if detail == nil {
					detail, err = m.readAppointment(ctx, folder, "id", key, false)
					if err != nil {
						return nil, err
					}
					if detail.get("ms") != row.get("ms") || detail.get("rev") != row.get("rev") {
						return nil, core.ErrConflict
					}
				}
				if item.Type == core.AppointmentException {
					appointmentID, inviteID, compound := strings.Cut(inst.get("invId"), "-")
					if !compound || appointmentID != key || !nativeID.MatchString(inviteID) {
						return nil, ErrProtocol
					}
					comp, matchErr := expandedExceptionComponent(*detail, row, inst, inviteID)
					if matchErr != nil {
						return nil, matchErr
					}
					item, err = appointmentException(*detail, comp, identity)
				} else {
					err = setInstanceDates(&item, *detail)
				}
				if err != nil {
					return nil, err
				}
			}
			if seen[item.Identity.ID] {
				return nil, ErrProtocol
			}
			seen[item.Identity.ID] = true
			if !item.End.Before(from) && item.Start.Before(to) {
				item.Body = ""
				items = append(items, item)
			}
		}
	}
	return items, ctx.Err()
}

func (m *mailbox) GetAppointment(ctx context.Context, folder, id core.ID) (core.Appointment, error) {
	key, ok := strings.CutPrefix(string(id), core.AppointmentIDPrefix)
	native, rid, instance := strings.Cut(key, "@")
	if !ok {
		return core.Appointment{}, core.ErrNotFound
	}
	detail, err := m.readAppointment(ctx, folder, "id", native, true)
	if err != nil {
		return core.Appointment{}, err
	}
	identity, err := appointmentIdentity(*detail, folder)
	if err != nil {
		return core.Appointment{}, err
	}
	if instance {
		anchor, err := recurrenceStart(rid)
		if err != nil {
			return core.Appointment{}, core.ErrNotFound
		}
		if comp := appointmentComponent(*detail, func(_, comp node) bool {
			return comp.get("ex") == "1" && exceptionRecurrenceID(comp) == rid
		}); comp != nil {
			if comp.get("status") == "CANC" {
				return core.Appointment{}, core.ErrNotFound
			}
			return appointmentException(*detail, *comp, identity)
		}
		// Verify that this anchor still exists in the native series. A removed
		// occurrence must not be reconstructed from the master's dates.
		items, err := m.calendarView(ctx, folder, native, anchor.Add(-24*time.Hour), anchor.Add(24*time.Hour))
		if err != nil {
			return core.Appointment{}, err
		}
		for _, item := range items {
			if item.Identity.ID == id {
				if item.Identity.Revision != identity.Revision {
					return core.Appointment{}, core.ErrConflict
				}
				if comp := masterComponent(*detail); comp != nil {
					item.Body = appointmentBody(*comp)
				}
				return item, nil
			}
		}
		return core.Appointment{}, core.ErrNotFound
	}
	comp := masterComponent(*detail)
	if comp == nil {
		return core.Appointment{}, core.ErrUnsupported
	}
	item, err := appointmentFromComponent(*detail, *comp, identity)
	if err == nil {
		item.Meeting = appointmentMeeting(*comp, m.owner)
	}
	return item, err
}

func (m *mailbox) readAppointment(ctx context.Context, folder core.ID, field, value string, includeBody bool) (*node, error) {
	target, err := m.groupwareFolder(ctx, folder, core.CalendarFolder)
	if err != nil {
		return nil, err
	}
	if field == "id" && !target.validItem(value) {
		return nil, core.ErrNotFound
	}
	// Native UID lookups default to the logged-in mailbox. A link is not a
	// mailbox selector and must never change the authentication context.
	if field == "uid" && target.itemPrefix != "" {
		return nil, core.ErrUnsupported
	}
	response, err := m.native.request(ctx, mailNS, "GetAppointment", "", getAppointmentRequest(field, value, includeBody))
	if err != nil {
		return nil, appointmentError(err)
	}
	return parseGetAppointmentForTarget(response, target, field, value)
}

func getAppointmentRequest(field, value string, includeBody bool) node {
	req := attr(attr(element(mailNS, "GetAppointmentRequest", ""), field, value), "sync", "1")
	if includeBody {
		req = attr(req, "includeContent", "1")
	}
	return req
}

func parseGetAppointmentForTarget(response node, target groupwareTarget, field, value string) (*node, error) {
	item := response.child(mailNS, "appt")
	if response.XMLName != (xml.Name{Space: mailNS, Local: "GetAppointmentResponse"}) ||
		item == nil || len(response.Children) != 1 || item.get(field) != value || !target.validItem(item.get("id")) {
		return nil, ErrProtocol
	}
	if item.get("l") != target.nativeFolder {
		return nil, core.ErrNotFound
	}
	if target.itemPrefix != "" && !target.viewPrivate {
		comp := masterComponent(*item)
		if comp == nil || !target.canReadAppointment(comp.get("class")) {
			return nil, core.ErrAccessDenied
		}
	}
	return item, nil
}

func appointmentError(err error) error {
	if errors.Is(err, ErrLimit) {
		return core.ErrLimit
	}
	var fault *Fault
	if errors.As(err, &fault) && fault.Code == "mail.NO_SUCH_CALITEM" {
		return core.ErrNotFound
	}
	return semanticError(err)
}

func appointmentIdentity(n node, parent core.ID) (core.Object, error) {
	ms, err := optionalFolderNumber(n, "ms")
	if err != nil || ms == nil || *ms == 0 {
		return core.Object{}, ErrProtocol
	}
	rev, err := optionalFolderNumber(n, "rev")
	if err != nil || rev == nil || *rev == 0 {
		return core.Object{}, ErrProtocol
	}
	md, err := optionalFolderNumber(n, "md")
	if err != nil || md == nil || *md > 253402300799 {
		return core.Object{}, ErrProtocol
	}
	return core.Object{
		ID: core.ID(core.AppointmentIDPrefix + n.get("id")), Parent: parent, Kind: core.AppointmentItem,
		Revision: core.Revision(*ms), Properties: core.Properties{
			core.ContentRevision: core.Integer(*rev),
			core.LastModifiedAt:  core.Timestamp(time.Unix(int64(*md), 0).UTC()),
		},
	}, nil
}

func appointmentInstance(row, inst node, identity core.Object) (core.Appointment, error) {
	exception := inst.get("ex") == "1"
	item := core.Appointment{Identity: identity, UID: row.get("uid"),
		Subject: row.get("name"), Location: row.get("loc"),
		AllDay: row.get("allDay") == "1", Cancelled: row.get("status") == "CANC"}
	if exception {
		item.Type = core.AppointmentException
		return item, nil // Read the matching native component for actual dates.
	}
	if row.get("recur") == "1" {
		rid := inst.get("ridZ")
		anchor, err := recurrenceStart(rid)
		if err != nil {
			return core.Appointment{}, err
		}
		item.Type, item.OriginalStart = core.AppointmentOccurrence, anchor
		item.SeriesID = identity.ID
		item.Identity.ID += core.ID("@" + rid)
	}
	if item.AllDay {
		return item, nil
	}
	start, err := appointmentMillis(inst.get("s"))
	if err != nil {
		return core.Appointment{}, err
	}
	duration := inst.get("dur")
	if duration == "" {
		duration = row.get("dur")
	}
	ms, err := strconv.ParseInt(duration, 10, 64)
	if err != nil || ms < 0 || ms > 253402300799999-start.UnixMilli() {
		return core.Appointment{}, ErrProtocol
	}
	item.Start, item.End = start, time.UnixMilli(start.UnixMilli()+ms).UTC()
	return item, nil
}

func recurrenceStart(value string) (time.Time, error) {
	format := zimbraRecurrenceStart
	if len(value) == len(zimbraDay) {
		format = zimbraDay
	}
	stamp, err := time.Parse(format, value)
	if err != nil || stamp.Year() < 1 {
		return time.Time{}, ErrProtocol
	}
	return stamp, nil
}

func appointmentMillis(value string) (time.Time, error) {
	ms, err := strconv.ParseInt(value, 10, 64)
	if err != nil || ms < -62135596800000 || ms > 253402300799999 {
		return time.Time{}, ErrProtocol
	}
	return time.UnixMilli(ms).UTC(), nil
}

func appointmentComponent(detail node, matches func(invite, comp node) bool) *node {
	for _, invite := range detail.Children {
		if invite.XMLName != (xml.Name{Space: mailNS, Local: "inv"}) {
			continue
		}
		for i := range invite.Children {
			comp := &invite.Children[i]
			if comp.XMLName == (xml.Name{Space: mailNS, Local: "comp"}) && matches(invite, *comp) {
				return comp
			}
		}
	}
	return nil
}

func masterComponent(detail node) *node {
	return appointmentComponent(detail, func(_, comp node) bool {
		return comp.get("ex") != "1"
	})
}

func expandedExceptionComponent(detail, row, inst node, inviteID string) (node, error) {
	rid := inst.get("ridZ")
	startMillis := inst.get("s")
	var civil string
	allDay := inst.get("allDay")
	if allDay == "" {
		allDay = row.get("allDay")
	}
	if rid == "" && allDay == "1" {
		start, err := appointmentMillis(startMillis)
		if err != nil {
			return node{}, err
		}
		offset := inst.get("tzo")
		if offset == "" {
			offset = row.get("tzo")
		}
		if offset != "" {
			ms, err := strconv.ParseInt(offset, 10, 64)
			if err != nil || ms <= -86400000 || ms >= 86400000 {
				return node{}, ErrProtocol
			}
			start = start.Add(time.Duration(ms) * time.Millisecond)
		}
		if !start.Equal(start.Truncate(24 * time.Hour)) {
			return node{}, ErrProtocol
		}
		civil = start.Format(zimbraDay)
	}

	// An exception at the original time can omit ridZ in Search. Invitation
	// ID and component number can also be reused by distinct exceptions.
	// Require one matching component and take its canonical anchor.
	var found node
	matches := 0
	appointmentComponent(detail, func(invite, comp node) bool {
		if comp.get("ex") != "1" || invite.get("id") != inviteID || comp.get("compNum") != inst.get("compNum") {
			return false
		}
		if rid != "" {
			if comp.get("ridZ") != rid && exceptionRecurrenceID(comp) != rid {
				return false
			}
		} else {
			start := comp.child(mailNS, "s")
			if start == nil || (civil != "" && start.get("d") != civil) ||
				(civil == "" && (startMillis == "" || start.get("u") != startMillis)) {
				return false
			}
		}
		found = comp
		matches++
		return matches > 1
	})
	if matches != 1 {
		return node{}, ErrProtocol
	}
	return found, nil
}

func setInstanceDates(item *core.Appointment, detail node) error {
	comp := masterComponent(detail)
	if comp == nil || comp.get("allDay") != "1" {
		return ErrProtocol
	}
	start, end, err := appointmentDates(*comp, true)
	if err != nil {
		return err
	}
	if item.Type == core.AppointmentOccurrence {
		days := end.Unix()/86400 - start.Unix()/86400
		start, end = item.OriginalStart, item.OriginalStart.AddDate(0, 0, int(days))
	}
	item.Start, item.End = start, end
	return nil
}

func appointmentException(detail, comp node, identity core.Object) (core.Appointment, error) {
	rid := exceptionRecurrenceID(comp)
	anchor, err := recurrenceStart(rid)
	if err != nil {
		return core.Appointment{}, err
	}
	series := identity.ID
	identity.ID += core.ID("@" + rid)
	item, err := appointmentFromComponent(detail, comp, identity)
	item.Type, item.OriginalStart = core.AppointmentException, anchor
	item.SeriesID = series
	return item, err
}

func exceptionRecurrenceID(comp node) string {
	// For an all-day exception east of UTC, Zimbra can report the
	// previous date in ridZ. exceptId retains the original civil date.
	if except := comp.child(mailNS, "exceptId"); except != nil {
		if date := except.get("d"); len(date) == len(zimbraDay) {
			return date
		}
	}
	return comp.get("ridZ")
}

func appointmentFromComponent(detail, comp node, identity core.Object) (core.Appointment, error) {
	item := core.Appointment{Identity: identity, UID: detail.get("uid"),
		Subject: comp.get("name"), Location: comp.get("loc"), Body: appointmentBody(comp),
		AllDay: comp.get("allDay") == "1", Cancelled: comp.get("status") == "CANC"}
	if comp.child(mailNS, "recur") != nil {
		item.Type = core.AppointmentMaster
		item.SeriesID = identity.ID
	}
	var err error
	item.Start, item.End, err = appointmentDates(comp, item.AllDay)
	if err == nil && item.Type == core.AppointmentMaster {
		item.TimeZone, item.Recurrence = readAppointmentRecurrence(comp, item.Start, item.AllDay)
	}
	return item, err
}

func appointmentBody(comp node) string {
	if desc := comp.child(mailNS, "desc"); desc != nil {
		return desc.Text
	}
	return ""
}

func appointmentDates(comp node, allDay bool) (time.Time, time.Time, error) {
	s, e := comp.child(mailNS, "s"), comp.child(mailNS, "e")
	if s == nil || e == nil {
		return time.Time{}, time.Time{}, ErrProtocol
	}
	if allDay {
		start, startErr := time.Parse(zimbraDay, s.get("d"))
		end, endErr := time.Parse(zimbraDay, e.get("d"))
		end = end.AddDate(0, 0, 1) // Zimbra stores the last included day.
		if startErr != nil || endErr != nil || start.Year() < 1 || end.Year() > 9999 || !start.Before(end) {
			return time.Time{}, time.Time{}, ErrProtocol
		}
		return start, end, nil
	}
	start, startErr := appointmentMillis(s.get("u"))
	end, endErr := appointmentMillis(e.get("u"))
	if startErr != nil || endErr != nil || end.Before(start) {
		return time.Time{}, time.Time{}, ErrProtocol
	}
	return start, end, nil
}
