// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"path/filepath"
	"testing"

	"ewsgo/internal/core"
	"ewsgo/internal/storage/sqlite"
)

type occurrenceWriteFixture struct {
	calendarWriteFixture
	core.MailMarksWriter
}

func (f *occurrenceWriteFixture) UpdateAppointmentDetails(_ context.Context, item core.Appointment, patch core.AppointmentDetailsPatch) (core.Appointment, error) {
	item.Identity.Revision++
	item.Subject = *patch.Subject
	f.saved = item
	f.writes++
	return item, nil
}

func TestOccurrenceChangeKeyMatchesReadAndWriteIdentity(t *testing.T) {
	ctx := t.Context()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "ews.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	h, err := NewPersistent(ctx, store, "occurrence-fixture", func(context.Context, core.Session) error { return nil }, 10)
	if err != nil {
		t.Fatal(err)
	}
	box := &occurrenceWriteFixture{}
	box.saved = core.Appointment{
		Identity: core.Object{ID: core.AppointmentIDPrefix + "fixture/2", Parent: "fixture-calendar", Kind: core.AppointmentItem, Revision: 9},
		SeriesID: core.AppointmentIDPrefix + "fixture", Type: core.AppointmentOccurrence,
	}
	session := &testSession{box: box}
	scope := session.Identity().Scope
	get := el(messages, "GetItem", "",
		el(messages, "ItemShape", "", el(types, "BaseShape", "Default")),
		el(messages, "ItemIds", "", at(el(types, "ItemId", ""), "Id", h.itemID(scope, box.saved.Identity))))
	result := serveWriteTest(t, h, session, get, "NoError")
	item := result.find(types, "ItemId")
	if item == nil || item.attr("ChangeKey") == "" {
		t.Fatal("GetItem did not emit a ChangeKey")
	}
	update := func(key, resolution string) node {
		change := el(types, "ItemChange", "",
			at(at(el(types, "ItemId", ""), "Id", item.attr("Id")), "ChangeKey", key),
			el(types, "Updates", "", el(types, "SetItemField", "",
				at(el(types, "FieldURI", ""), "FieldURI", "item:Subject"),
				el(types, "CalendarItem", "", el(types, "Subject", "Edited")))))
		return at(at(el(messages, "UpdateItem", "", el(messages, "ItemChanges", "", change)),
			"ConflictResolution", resolution), "SendMeetingInvitationsOrCancellations", "SendToNone")
	}
	key := item.attr("ChangeKey")
	other := box.saved.Identity
	other.ID = core.AppointmentIDPrefix + "fixture/3"
	serveWriteTest(t, h, session, update(h.changeKey(scope, other), "AlwaysOverwrite"), "ErrorInvalidChangeKey")
	serveWriteTest(t, h, session, update(key, "NeverOverwrite"), "NoError")
	serveWriteTest(t, h, session, update(key, "NeverOverwrite"), "ErrorIrresolvableConflict")
	if box.writes != 1 || box.saved.Subject != "Edited" {
		t.Fatalf("writes=%d, subject=%q", box.writes, box.saved.Subject)
	}
}
