// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestRescheduleAppointmentRejectsBothSidesOfAmbiguousLocalTime(t *testing.T) {
	original := time.Date(2027, time.March, 25, 8, 30, 0, 0, time.UTC)
	part := attr(attr(element(mailNS, "s", ""), "d", "20270325T093000"), "tz", "Europe/Rome")
	for _, hour := range []int{0, 1} {
		target := time.Date(2027, time.October, 31, hour, 30, 0, 0, time.UTC)
		if _, err := rescheduledAppointmentBound(part, original, target, false); !errors.Is(err, core.ErrUnsupported) {
			t.Fatalf("ambiguous time %v got %v", target, err)
		}
	}
	target := time.Date(2027, time.October, 31, 2, 30, 0, 0, time.UTC)
	stored, err := rescheduledAppointmentBound(part, original, target, false)
	if err != nil || stored.get("d") != "20271031T033000" || stored.get("tz") != "Europe/Rome" {
		t.Fatalf("unambiguous time produced %q in %q, error %v", stored.get("d"), stored.get("tz"), err)
	}
}

func TestAppointmentExceptionKeepsOriginalCivilDate(t *testing.T) {
	// On Terra, ridZ on an all-day exception east of UTC was the day
	// before exceptId. Before the edit, Search returned the civil date.
	comp := element(mailNS, "comp", "",
		attr(element(mailNS, "exceptId", ""), "d", "20270401"),
		attr(element(mailNS, "s", ""), "d", "20270402"),
		attr(element(mailNS, "e", ""), "d", "20270402"),
	)
	comp = attr(attr(attr(comp, "allDay", "1"), "ex", "1"), "ridZ", "20270331")
	item, err := appointmentException(node{}, comp, core.Object{ID: "appointment:42"})
	if err != nil {
		t.Fatal(err)
	}
	if item.Identity.ID != "appointment:42@20270401" ||
		item.OriginalStart.Format(time.DateOnly) != "2027-04-01" ||
		item.Start.Format(time.DateOnly) != "2027-04-02" ||
		item.End.Format(time.DateOnly) != "2027-04-03" {
		t.Fatalf("unexpected exception identity or dates: %#v", item)
	}
}

func TestExpandedExceptionWithoutRIDMatchesUniqueActualStart(t *testing.T) {
	first, err := parseXML([]byte(`<inv xmlns="urn:zimbraMail" id="42"><comp ex="1" compNum="0" ridZ="20270331">
		<exceptId d="20270401"/><s d="20270401"/>
	</comp></inv>`))
	if err != nil {
		t.Fatal(err)
	}
	second, err := parseXML([]byte(`<inv xmlns="urn:zimbraMail" id="42"><comp ex="1" compNum="0" ridZ="20270406">
		<exceptId d="20270407"/><s d="20270407"/>
	</comp></inv>`))
	if err != nil {
		t.Fatal(err)
	}
	detail := element(mailNS, "appt", "", second, first)
	row := attr(attr(element(mailNS, "appt", ""), "allDay", "1"), "tzo", "7200000")
	inst := attr(attr(element(mailNS, "inst", ""), "compNum", "0"), "s", "1806530400000")
	match, err := expandedExceptionComponent(detail, row, inst, "42")
	if err != nil || exceptionRecurrenceID(match) != "20270401" {
		t.Fatalf("got recurrence anchor %q, error %v", exceptionRecurrenceID(match), err)
	}
	detail.Children = append(detail.Children, first)
	if _, err := expandedExceptionComponent(detail, row, inst, "42"); err != ErrProtocol {
		t.Fatalf("ambiguous match returned %v", err)
	}
}

func TestCreateAppointmentRecoversByUIDWithoutRepeatingNativeWrite(t *testing.T) {
	var uid atomic.Value
	var creates, reads atomic.Int32
	fallback := fakeZimbra(t)
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		req := readMessageRequest(t, r)
		switch req.XMLName.Local {
		case "CreateAppointmentRequest":
			creates.Add(1)
			comp := req.child(mailNS, "m").child(mailNS, "inv").child(mailNS, "comp")
			generated := comp.get("uid")
			if !strings.HasSuffix(generated, "@ewsgo.invalid") {
				t.Error("create did not contain a generated UID")
			}
			uid.Store(generated)
			w.WriteHeader(http.StatusBadGateway)
		case "GetAppointmentRequest":
			reads.Add(1)
			generated, _ := uid.Load().(string)
			if generated == "" || req.get("uid") != generated || req.get("id") != "" || req.get("includeContent") != "1" {
				t.Error("reconciliation did not request the generated UID")
			}
			envelope(w, `<GetAppointmentResponse xmlns="urn:zimbraMail"><appt id="42" uid="`+generated+`" l="10" ms="6" rev="6" md="1700000000">
				<inv><comp name="Fixture" loc="Room D"><desc>Notes</desc><s u="1806573600000"/><e u="1806577200000"/></comp></inv>
			</appt></GetAppointmentResponse>`)
		default:
			fallback(w, r)
		}
	}, Config{})
	box := &mailbox{native: login(t, client)}
	item, err := box.CreateAppointment(t.Context(), "f:10", core.Appointment{
		Subject: "Fixture", Location: "Room D", Body: "Notes",
		Start: time.UnixMilli(1806573600000), End: time.UnixMilli(1806577200000),
	})
	if err != nil || item.Identity.ID != "appointment:42" || creates.Load() != 1 || reads.Load() != 1 {
		t.Fatalf("got ID %q, error %v, creates %d, reads %d", item.Identity.ID, err, creates.Load(), reads.Load())
	}
}

func TestAppointmentContentsProjectsDetachedOccurrence(t *testing.T) {
	// A Terra recipient received just the modified occurrence, without RRULE.
	detail, err := parseXML([]byte(`<appt xmlns="urn:zimbraMail" id="42" l="10" uid="test@example.invalid" ms="15" rev="13" md="1795757815">
		<inv><comp ex="1" ridZ="20261218T053600Z" class="PUB" name="Detached occurrence">
			<exceptId d="20261218T053600Z"/>
			<s d="20261218T063600Z" u="1797575760000"/><e d="20261218T073600Z" u="1797579360000"/>
		</comp></inv>
	</appt>`))
	if err != nil {
		t.Fatal(err)
	}
	row := core.Object{ID: "a:42", Parent: "f:10", Revision: 15, Properties: core.Properties{
		core.IsRead: core.Bool(true), core.ContentRevision: core.Integer(13), core.AppointmentRecurring: core.Bool(false),
	}}
	item, err := appointmentDetailForRow(row, detail)
	if err != nil {
		t.Fatal(err)
	}
	if item.Identity.ID != "appointment:42" || item.Identity.Parent != "f:10" || item.Type != core.AppointmentSingle ||
		item.Recurrence != nil || item.SeriesID != "" || item.Participation != core.AppointmentReceived ||
		!item.OriginalStart.Equal(item.Start.Add(-time.Hour)) || !item.Start.Equal(time.UnixMilli(1797575760000)) ||
		item.Identity.Properties[core.IsRead] != core.Bool(true) {
		t.Fatal("detached event lost its native identity, dates or read-only participation")
	}
	detail.Children = append(detail.Children, detail.Children[0])
	if _, err := appointmentDetailForRow(row, detail); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("ambiguous masterless item returned %v", err)
	}
}
