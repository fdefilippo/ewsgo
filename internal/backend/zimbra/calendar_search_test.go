// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestCalendarSearchItemUsesActiveAlarmAndAppointmentRecurrence(t *testing.T) {
	for _, tc := range []struct {
		name           string
		role           core.SpecialFolderKind
		element        string
		active         bool
		wantRecurrence core.Value
	}{
		{"dismissed appointment", core.CalendarFolder, "appt", false, core.Bool(true)},
		{"active appointment", core.CalendarFolder, "appt", true, core.Bool(true)},
		{"active recurring task", core.TasksFolder, "task", true, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hit := element(mailNS, tc.element, "")
			for name, value := range map[string]string{
				"name": "Test", "md": "1700000000", "ms": "31", "rev": "29", "alarm": "1", "recur": "1",
			} {
				hit = attr(hit, name, value)
			}
			if tc.active {
				hit.Children = append(hit.Children, attr(element(mailNS, "alarmData", ""), "nextAlarm", "1700000300123"))
			}
			object, err := calendarSearchItem(hit, "folder", "item", tc.role)
			if err != nil {
				t.Fatal(err)
			}
			p := object.Properties
			if p[core.ReminderSet] != core.Bool(tc.active) || p[core.AppointmentRecurring] != tc.wantRecurrence {
				t.Fatal("unexpected reminder or recurrence state")
			}
			var signal core.Value
			if tc.active {
				signal = core.Timestamp(time.UnixMilli(1700000300123).UTC())
			}
			if p[core.ReminderSignalTime] != signal {
				t.Fatal("unexpected reminder signal time")
			}
			if object.ID != "item" || object.Parent != "folder" || object.Revision != 31 || len(object.MIME) != 0 {
				t.Fatal("unexpected calendar search identity")
			}
		})
	}
}
