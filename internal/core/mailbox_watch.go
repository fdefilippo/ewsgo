// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import "context"

// MailboxWatcher is an optional capability of an authenticated session. The
// returned watch must be closed before that session is released.
type MailboxWatcher interface {
	OpenMailboxWatch(context.Context) (MailboxWatch, error)
}

// MailboxWatch offers change hints for one mailbox. The caller serializes
// Wait and Close. A hint asks the caller to reread the authoritative delta.
type MailboxWatch interface {
	Wait(context.Context) (changed bool, err error)
	Close(context.Context) error
}

// MailboxWakeups fans out coalesced hints while an authenticated request is
// active. Release must be called before the request releases its session.
// A closed channel means the caller may rejoin using its own session.
type MailboxWakeups interface {
	WatchMailbox(context.Context, Session) (hints <-chan struct{}, release func(), err error)
}
