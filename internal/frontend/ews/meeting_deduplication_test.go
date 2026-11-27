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

func TestMeetingWithoutUIDReplaysAndDistinctUIDsSendSeparately(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "meetings.sqlite")
	open := func() (*sqlite.Store, *Handler) {
		t.Helper()
		store, err := sqlite.Open(ctx, path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = store.Close() })
		h, err := NewPersistent(ctx, store, "meeting-fixture", func(context.Context, core.Session) error { return nil }, 10)
		if err != nil {
			t.Fatal(err)
		}
		return store, h
	}
	box := &calendarWriteFixture{}
	session := &testSession{box: box}
	store, h := open()
	create := func(uid string) (string, string) {
		t.Helper()
		fields := []node{
			el(types, "Subject", "Team meeting"),
			el(types, "Start", "2026-12-08T09:00:00Z"), el(types, "End", "2026-12-08T10:00:00Z"),
			el(types, "RequiredAttendees", "", el(types, "Attendee", "",
				el(types, "Mailbox", "", el(types, "EmailAddress", "attendee@example.invalid")))),
		}
		if uid != "" {
			fields = append(fields, el(types, "UID", uid))
		}
		result := serveWriteTest(t, h, session, at(el(messages, "CreateItem", "",
			el(messages, "Items", "", el(types, "CalendarItem", "", fields...))), "SendMeetingInvitations", "SendToAllAndSaveCopy"), "NoError")
		id, effectiveUID := result.find(types, "ItemId"), nodeText(result.find(types, "UID"))
		if id == nil || id.attr("Id") == "" || effectiveUID == "" {
			t.Fatal("created meeting is missing an item ID or UID")
		}
		return id.attr("Id"), effectiveUID
	}
	first, generated := create("")
	if got, uid := create(""); got != first || uid != generated {
		t.Fatal("identical retry changed the meeting")
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	_, h = open()
	if got, uid := create(""); got != first || uid != generated || box.writes != 1 {
		t.Fatal("restart changed the meeting or sent the invitation again")
	}
	a, uidA := create("meeting-first@example.invalid")
	b, uidB := create("meeting-second@example.invalid")
	if box.writes != 3 || a == b || a == first || b == first ||
		uidA != "meeting-first@example.invalid" || uidB != "meeting-second@example.invalid" {
		t.Fatal("intentional invitations with different UIDs were not sent separately")
	}
}
