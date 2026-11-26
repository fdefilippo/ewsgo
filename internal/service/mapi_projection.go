// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"

	"ewsgo/internal/core"
	"ewsgo/internal/frontend/mapi"
	"ewsgo/internal/storage/sqlite"
)

type mapiNotificationJournal struct {
	store    *sqlite.Store
	instance string
	refresh  func(context.Context, core.Session) error
}

func newMAPINotificationJournal(store *sqlite.Store, instance string) mapiNotificationJournal {
	return mapiNotificationJournal{store, instance, zimbraProjectionRefresh(store, instance)}
}

func (j mapiNotificationJournal) Head(ctx context.Context, session core.Session) (mapi.NotificationPosition, error) {
	if err := j.Refresh(ctx, session); err != nil {
		return mapi.NotificationPosition{}, err
	}
	scope := session.Identity().Scope
	binding, err := j.store.BindMailbox(ctx, scope, j.instance)
	if err != nil {
		return mapi.NotificationPosition{}, err
	}
	position, err := j.store.JournalHead(ctx, binding.MailboxID)
	return mapi.NotificationPosition{Mailbox: binding.MailboxID, Generation: position.Generation, Scope: scope, Revision: position.Revision}, err
}

func (j mapiNotificationJournal) Refresh(ctx context.Context, session core.Session) error {
	return j.refresh(ctx, session)
}

func (j mapiNotificationJournal) Page(ctx context.Context, session core.Session, position mapi.NotificationPosition, limit int) ([]mapi.NotificationEvent, mapi.NotificationPosition, error) {
	if session.Identity().Scope != position.Scope {
		return nil, position, core.ErrAccessDenied
	}
	entries, next, err := j.store.JournalEntries(ctx, position.Mailbox, sqlite.JournalCursor{Generation: position.Generation, Revision: position.Revision}, limit)
	if err != nil {
		return nil, position, err
	}
	events := make([]mapi.NotificationEvent, 0, len(entries))
	for _, entry := range entries {
		events = append(events, mapi.NotificationEvent{Change: entry.Change, ObjectKind: entry.ObjectKind})
	}
	position.Revision = next.Revision
	return events, position, nil
}
