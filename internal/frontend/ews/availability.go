// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"time"

	"ewsgo/internal/core"
)

const availabilityOperation = "GetUserAvailabilityRequest"

type availabilityQuery struct {
	addresses  []string
	start, end time.Time
	zone       core.AppointmentZone
	view       string
	interval   time.Duration
}

func availabilityFields(n node, names ...string) (map[string]*node, bool) {
	fields := make(map[string]*node, len(names))
	for _, name := range names {
		fields[name] = nil
	}
	for i := range n.Children {
		child := &n.Children[i]
		prev, expected := fields[child.XMLName.Local]
		if child.XMLName.Space != types || !expected || prev != nil {
			return nil, false
		}
		fields[child.XMLName.Local] = child
	}
	return fields, true
}

func readAvailabilityQuery(op node) (availabilityQuery, string) {
	query := availabilityQuery{view: "FreeBusy", interval: 30 * time.Minute}
	var timezone, mailboxes, options *node
	for i := range op.Children {
		child := &op.Children[i]
		switch {
		case isElement(*child, types, "TimeZone") && timezone == nil:
			timezone = child
		case isElement(*child, messages, "MailboxDataArray") && mailboxes == nil:
			mailboxes = child
		case isElement(*child, types, "FreeBusyViewOptions") && options == nil:
			options = child
		default:
			return query, "ErrorInvalidRequest"
		}
	}
	if mailboxes == nil || len(mailboxes.Children) == 0 || len(mailboxes.Children) > core.MaxAvailabilityMailboxes || options == nil {
		return query, "ErrorInvalidRequest"
	}
	for _, mb := range mailboxes.Children {
		fields, ok := availabilityFields(mb, "Email", "AttendeeType", "ExcludeConflicts", "ExcludeNonWorkingHours")
		if !isElement(mb, types, "MailboxData") || !ok || fields["Email"] == nil {
			return query, "ErrorInvalidRequest"
		}
		email, ok := availabilityFields(*fields["Email"], "Name", "Address", "RoutingType")
		routing, address := email["RoutingType"], availabilityText(email["Address"])
		if !ok || address == "" || routing != nil && availabilityText(routing) != "SMTP" {
			return query, "ErrorInvalidRequest"
		}
		query.addresses = append(query.addresses, address)
	}
	fields, ok := availabilityFields(*options, "TimeWindow", "MergedFreeBusyIntervalInMinutes", "RequestedView")
	if !ok || fields["TimeWindow"] == nil {
		return query, "ErrorInvalidRequest"
	}
	if interval := fields["MergedFreeBusyIntervalInMinutes"]; interval != nil {
		minutes, valid := availabilityInt(interval, 5, 1440)
		if !valid {
			return query, "ErrorInvalidRequest"
		}
		query.interval = time.Duration(minutes) * time.Minute
	}
	if requested := fields["RequestedView"]; requested != nil {
		switch view := availabilityText(requested); view {
		case "FreeBusy", "FreeBusyMerged", "MergedOnly":
			query.view = view
		case "Detailed":
			query.view = "FreeBusy"
		case "DetailedMerged":
			query.view = "FreeBusyMerged"
		default:
			return query, "ErrorInvalidRequest"
		}
	}
	query.zone, ok = readAvailabilityZone(timezone)
	if !ok {
		return query, "ErrorInvalidRequest"
	}
	window, valid := availabilityFields(*fields["TimeWindow"], "StartTime", "EndTime")
	start, sOK := availabilityTime(window["StartTime"], query.zone)
	end, eOK := availabilityTime(window["EndTime"], query.zone)
	if !valid || !sOK || !eOK || start.Year() < 1601 || end.Year() > 9999 || !end.After(start) {
		return query, "ErrorInvalidRequest"
	}
	if end.Sub(start) > core.MaxAvailabilityWindow {
		return query, "ErrorTimeIntervalTooBig"
	}
	query.start, query.end = start, end
	return query, ""
}

func serveAvailability(w http.ResponseWriter, ctx context.Context, op node, s core.Session) core.ActivityResult {
	query, code := readAvailabilityQuery(op)
	if code != "" {
		replyFault(w, code)
		return core.ActivityRejected
	}
	reader, ok := s.Mailbox().(core.AvailabilityReader)
	if !ok {
		replyFault(w, "ErrorInvalidRequest")
		return core.ActivityRejected
	}
	results, err := reader.ReadAvailability(ctx, query.addresses, query.start, query.end)
	switch {
	case errors.Is(err, core.ErrInvalid):
		replyFault(w, "ErrorInvalidRequest")
		return core.ActivityRejected
	case errors.Is(err, core.ErrAccessDenied):
		replyFault(w, "ErrorAccessDenied")
		return core.ActivityDenied
	case err != nil || len(results) != len(query.addresses):
		replyFault(w, "ErrorInternalServerError")
		return core.ActivityFailure
	}
	replies := make([]node, 0, len(results))
	remaining := 4096 // Bound the number of interval records in one response.
	for _, intervals := range results {
		remaining -= len(intervals)
		if remaining < 0 || !validAvailabilityIntervals(query, intervals) {
			replyFault(w, "ErrorInternalServerError")
			return core.ActivityFailure
		}
		replies = append(replies, el(messages, "FreeBusyResponse", "",
			at(el(messages, "ResponseMessage", "", el(messages, "ResponseCode", "NoError")), "ResponseClass", "Success"),
			availabilityView(query, intervals)))
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	_, _ = w.Write(envelope(el(messages, "GetUserAvailabilityResponse", "", el(messages, "FreeBusyResponseArray", "", replies...))))
	return core.ActivitySuccess
}

func validAvailabilityIntervals(query availabilityQuery, intervals []core.AvailabilityInterval) bool {
	cursor := query.start
	for _, row := range intervals {
		if !row.Start.Equal(cursor) || !row.End.After(cursor) || row.End.After(query.end) || row.State > core.AvailabilityNoData {
			return false
		}
		cursor = row.End
	}
	return cursor.Equal(query.end)
}

func availabilityView(query availabilityQuery, intervals []core.AvailabilityInterval) node {
	children := []node{el(types, "FreeBusyViewType", query.view)}
	if query.view != "FreeBusy" {
		width := int((query.end.Sub(query.start) + query.interval - 1) / query.interval)
		slots := bytes.Repeat([]byte{'0'}, width)
		for _, row := range intervals {
			first := int(row.Start.Sub(query.start) / query.interval)
			last := int((row.End.Sub(query.start) + query.interval - 1) / query.interval)
			for i := first; i < last; i++ {
				slots[i] = max(slots[i], '0'+byte(row.State))
			}
		}
		children = append(children, el(types, "MergedFreeBusy", string(slots)))
	}
	if query.view != "MergedOnly" {
		var events []node
		for _, row := range intervals {
			if row.State == core.AvailabilityFree {
				continue
			}
			busyType := [...]string{"Free", "Tentative", "Busy", "OOF", "NoData"}[row.State]
			events = append(events, el(types, "CalendarEvent", "",
				el(types, "StartTime", availabilityDisplayTime(row.Start, query.zone)),
				el(types, "EndTime", availabilityDisplayTime(row.End, query.zone)),
				el(types, "BusyType", busyType)))
		}
		if len(events) > 0 {
			children = append(children, el(types, "CalendarEventArray", "", events...))
		}
	}
	return el(messages, "FreeBusyView", "", children...)
}
