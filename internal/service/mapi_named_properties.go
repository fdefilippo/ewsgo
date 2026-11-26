// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"errors"

	"ewsgo/internal/core"
	"ewsgo/internal/frontend/mapi"
	"ewsgo/internal/storage/sqlite"
)

type mapiNamedProperties struct {
	store   *sqlite.Store
	mailbox string
}

func bindMAPINamedProperties(store *sqlite.Store, instance string) func(context.Context, core.Scope) (mapi.NamedPropertyStorage, error) {
	return func(ctx context.Context, scope core.Scope) (mapi.NamedPropertyStorage, error) {
		mailbox, err := store.BindMailbox(ctx, scope, instance)
		if err != nil {
			return nil, err
		}
		return mapiNamedProperties{store: store, mailbox: mailbox.MailboxID}, nil
	}
}

func (p mapiNamedProperties) Load(ctx context.Context, maximum int) (map[uint16]core.Key, error) {
	return p.store.LoadNamedPropertyBindings(ctx, p.mailbox, maximum)
}

func (p mapiNamedProperties) Register(ctx context.Context, keys []core.Key, lastID uint16) ([]uint16, error) {
	ids, err := p.store.RegisterNamedProperties(ctx, p.mailbox, keys, lastID)
	if errors.Is(err, sqlite.ErrNamedPropertyQuota) {
		return nil, core.ErrUnsupported
	}
	return ids, err
}
