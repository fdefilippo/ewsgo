// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import "context"

// CalendarSearchReader returns a complete, bounded metadata view of the
// requested own default Calendar or Tasks folder. Objects have their native
// identity and parent, with no message MIME or calendar write capability.
type CalendarSearchReader interface {
	ReadCalendarSearchItems(context.Context, ID, SpecialFolderKind) ([]Object, error)
}

var (
	ReminderSet          = Key{Set: "00062008-0000-0000-c000-000000000046", ID: 0x8503}
	ReminderSignalTime   = Key{Set: "00062008-0000-0000-c000-000000000046", ID: 0x8560}
	AppointmentRecurring = Key{Set: "00062002-0000-0000-c000-000000000046", ID: 0x8223}
)
