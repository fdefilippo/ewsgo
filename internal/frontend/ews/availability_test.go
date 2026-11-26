// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func availabilityRequest(start, end, view string, addresses ...string) node {
	var mailboxes []node
	for _, address := range addresses {
		mailboxes = append(mailboxes, el(types, "MailboxData", "", el(types, "Email", "",
			el(types, "Address", address), el(types, "RoutingType", "SMTP")),
			el(types, "AttendeeType", "Required"), el(types, "ExcludeConflicts", "false")))
	}
	return el(messages, availabilityOperation, "", el(messages, "MailboxDataArray", "", mailboxes...),
		el(types, "FreeBusyViewOptions", "", el(types, "TimeWindow", "",
			el(types, "StartTime", start), el(types, "EndTime", end)), el(types, "RequestedView", view)))
}

type availabilityFixture struct {
	core.Mailbox
	read func([]string, time.Time, time.Time) [][]core.AvailabilityInterval
}

func (f availabilityFixture) ReadAvailability(_ context.Context, addresses []string, start, end time.Time) ([][]core.AvailabilityInterval, error) {
	return f.read(addresses, start, end), nil
}

func TestAvailabilityProjection(t *testing.T) {
	transition := func(month, hour, bias string) node {
		return el(types, "Rule", "", el(types, "Month", month), el(types, "Time", hour),
			el(types, "DayOrder", "5"), el(types, "DayOfWeek", "Sunday"), el(types, "Bias", bias))
	}
	standard, daylight := transition("10", "03:00:00", "0"), transition("3", "02:00:00", "-60")
	standard.XMLName.Local, daylight.XMLName.Local = "StandardTime", "DaylightTime"
	zone := el(types, "TimeZone", "", el(types, "Bias", "-60"), standard, daylight)
	req := availabilityRequest("2027-05-01T12:00:00", "2027-05-01T13:05:00", "DetailedMerged",
		"one@example.invalid", "two@example.invalid")
	req.Children = append([]node{zone}, req.Children...)
	s := &testSession{box: availabilityFixture{read: func(addresses []string, start, end time.Time) [][]core.AvailabilityInterval {
		if len(addresses) != 2 || addresses[0] != "one@example.invalid" || addresses[1] != "two@example.invalid" ||
			start.Format(time.RFC3339) != "2027-05-01T10:00:00Z" || end.Sub(start) != 65*time.Minute {
			t.Fatal("wrong ordered addresses or UTC query window")
		}
		tentative, busy, free := start.Add(15*time.Minute), start.Add(30*time.Minute), start.Add(35*time.Minute)
		return [][]core.AvailabilityInterval{{
			{Start: start, End: tentative, State: core.AvailabilityFree},
			{Start: tentative, End: busy, State: core.AvailabilityTentative},
			{Start: busy, End: free, State: core.AvailabilityBusy},
			{Start: free, End: end, State: core.AvailabilityFree},
		}, {{Start: start, End: end, State: core.AvailabilityNoData}}}
	}}}
	response := serveWriteTest(t, New(), s, req, "NoError")
	replies := response.find(messages, "FreeBusyResponseArray")
	if replies == nil || len(replies.Children) != 2 {
		t.Fatal("wrong response shape")
	}
	first, second := replies.Children[0], replies.Children[1]
	if availabilityText(first.find(types, "FreeBusyViewType")) != "FreeBusyMerged" ||
		availabilityText(first.find(types, "MergedFreeBusy")) != "120" ||
		availabilityText(second.find(types, "MergedFreeBusy")) != "444" {
		t.Fatal("wrong view, slot status or recipient order")
	}
	if availabilityText(first.find(types, "StartTime")) != "2027-05-01T12:15:00+02:00" ||
		availabilityText(second.find(types, "BusyType")) != "NoData" ||
		first.find(types, "CalendarEventDetails") != nil {
		t.Fatal("wrong local time, privacy or unknown status")
	}
	window := req.find(types, "TimeWindow")
	window.Children[0].Text = "2027-10-31T02:30:00"
	window.Children[1].Text = "2027-10-31T04:00:00"
	if _, code := readAvailabilityQuery(req); code != "ErrorInvalidRequest" {
		t.Fatal("ambiguous civil time accepted without offset")
	}
	window.Children[0].Text += "+01:00"
	if _, code := readAvailabilityQuery(req); code != "" {
		t.Fatal("explicit offset rejected")
	}
}
