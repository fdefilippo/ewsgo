// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"net/http"

	"ewsgo/internal/backend/jmap"
	"ewsgo/internal/core"
	"ewsgo/internal/frontend/ews"
	"ewsgo/internal/storage/sqlite"
)

func jmapEWS(ctx context.Context, health http.Handler, backend *jmap.Client, store *sqlite.Store, instance string) (http.Handler, error) {
	frontend, err := ews.NewPersistent(ctx, store, instance, func(ctx context.Context, session core.Session) error {
		return jmap.Refresh(ctx, session, store, instance)
	}, 200)
	if err != nil {
		return nil, err
	}
	frontend.EnableMIMESending(NewMailSender(store, instance))
	mux := http.NewServeMux()
	mux.Handle("/EWS/Exchange.asmx", core.BasicTLS(backend, frontend.Serve))
	mux.Handle("/", health)
	return mux, nil
}
