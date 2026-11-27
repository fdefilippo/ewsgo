// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"path/filepath"
	"strconv"
	"testing"

	"ewsgo/internal/core"
	"ewsgo/internal/storage/sqlite"
)

type calendarWriteFixture struct {
	fixtureMailbox
	core.AppointmentReader
	core.AppointmentWriter
	saved  core.Appointment
	writes int
}

func (*calendarWriteFixture) SpecialFolder(_ context.Context, kind core.SpecialFolderKind) (core.Object, error) {
	if kind != core.CalendarFolder {
		return core.Object{}, core.ErrNotFound
	}
	return core.Object{ID: "fixture-calendar", Kind: core.Folder}, nil
}

func (f *calendarWriteFixture) CreateAppointment(_ context.Context, folder core.ID, item core.Appointment) (core.Appointment, error) {
	if item.TimeZone != "" {
		return core.Appointment{}, core.ErrUnsupported
	}
	item.Identity = core.Object{ID: core.ID(core.AppointmentIDPrefix + strconv.Itoa(f.writes+1)), Parent: folder, Kind: core.AppointmentItem, Revision: 1}
	f.saved = item
	f.writes++
	return item, nil
}

func (f *calendarWriteFixture) GetAppointment(_ context.Context, folder, id core.ID) (core.Appointment, error) {
	if f.saved.Identity.ID != id || f.saved.Identity.Parent != folder {
		return core.Appointment{}, core.ErrNotFound
	}
	return f.saved, nil
}

func (f *calendarWriteFixture) StageMeetingInvitation(folder core.ID, invitation core.MeetingInvitation, uid string) (core.SendAttempt, error) {
	item := invitation.Appointment
	item.UID = uid
	return func(ctx context.Context) (core.ID, error) {
		saved, err := f.CreateAppointment(ctx, folder, item)
		return saved.Identity.ID, err
	}, nil
}

func TestCreateItemCalendarUTCAndReadBack(t *testing.T) {
	ctx := t.Context()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "calendar.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h, err := NewPersistent(ctx, store, "utc-fixture", func(context.Context, core.Session) error { return nil }, 10)
	if err != nil {
		t.Fatal(err)
	}
	box := &calendarWriteFixture{}
	session := &testSession{box: box}
	for _, invites := range []string{"SendToNone", "SendToAllAndSaveCopy"} {
		t.Run(invites, func(t *testing.T) {
			request := func(zone string) node {
				fields := []node{
					el(types, "Start", "2026-12-08T10:15:00+01:00"),
					el(types, "End", "2026-12-08T11:00:00+01:00"),
					at(el(types, "StartTimeZone", ""), "Id", zone),
					at(el(types, "EndTimeZone", ""), "Id", "UTC"),
				}
				if invites == "SendToAllAndSaveCopy" {
					fields = append(fields, el(types, "RequiredAttendees", "", el(types, "Attendee", "",
						el(types, "Mailbox", "", el(types, "EmailAddress", "attendee@example.invalid")))))
				}
				return at(el(messages, "CreateItem", "", el(messages, "Items", "", el(types, "CalendarItem", "", fields...))),
					"SendMeetingInvitations", invites)
			}
			before := box.writes
			serveWriteTest(t, h, session, request("Example/Unsupported"), "ErrorInvalidPropertyForOperation")
			created := serveWriteTest(t, h, session, request("UTC"), "NoError")
			if box.writes != before+1 || created.find(types, "ItemId") == nil {
				t.Fatal("expected one saved calendar item with an identity")
			}
			get := el(messages, "GetItem", "", el(messages, "ItemShape", "", el(types, "BaseShape", "Default")),
				el(messages, "ItemIds", "", *created.find(types, "ItemId")))
			got := serveWriteTest(t, h, session, get, "NoError")
			if nodeText(got.find(types, "Start")) != "2026-12-08T09:15:00Z" || nodeText(got.find(types, "End")) != "2026-12-08T10:00:00Z" {
				t.Fatal("calendar bounds changed when rereading the saved item")
			}
		})
	}
}
