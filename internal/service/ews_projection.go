// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"errors"
	"time"

	"ewsgo/internal/backend/zimbra"
	"ewsgo/internal/core"
	"ewsgo/internal/frontend/ews"
	"ewsgo/internal/storage/sqlite"
)

// NewPersistentZimbraEWS connects the selected backend to the shared store.
// The backend instance must remain stable across gateway restarts.
func NewPersistentZimbraEWS(ctx context.Context, store *sqlite.Store, instance string, pageSize int) (*ews.Handler, error) {
	refresh := zimbraProjectionRefresh(store, instance)
	frontend, err := ews.NewPersistent(ctx, store, instance, refresh, pageSize)
	if err != nil {
		return nil, err
	}
	frontend.EnableMIMESending(NewMailSender(store, instance))
	return frontend, nil
}

func zimbraProjectionRefresh(store *sqlite.Store, instance string) func(context.Context, core.Session) error {
	return func(ctx context.Context, session core.Session) error {
		ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
		defer cancel()
		conflicts := 0
		for {
			more, err := zimbra.ProjectMailboxDelta(ctx, session, store, instance, 128)
			if errors.Is(err, core.ErrConflict) && conflicts < 4 {
				// Another request or an external write overtook the observation.
				// The next call starts from the committed checkpoint.
				conflicts++
				continue
			}
			if err != nil || !more {
				return err
			}
		}
	}
}
