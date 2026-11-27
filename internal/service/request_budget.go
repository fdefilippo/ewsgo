// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"errors"
	"net/http"
	"time"
)

type requestBudget struct {
	upload, total, response time.Duration
}

func protocolRequestBudget(r *http.Request) requestBudget {
	switch r.URL.Path {
	case "/EWS/Exchange.asmx":
		return requestBudget{45 * time.Second, 105 * time.Second, 15 * time.Second}
	case "/mapi/emsmdb/", "/mapi/nspi/":
		if r.URL.Path == "/mapi/emsmdb/" && r.Header.Get("X-RequestType") == "NotificationWait" {
			return requestBudget{45 * time.Second, 6 * time.Minute, 15 * time.Second}
		}
		return requestBudget{45 * time.Second, 135 * time.Second, 15 * time.Second}
	default:
		return requestBudget{15 * time.Second, 25 * time.Second, 5 * time.Second}
	}
}

// The upload deadline caps socket reads. The overall context also bounds
// authentication and backend operations, with time for their error replies
// and cleanup before the write deadline. Mutation outcomes belong to the
// protocol handler, including operations interrupted after native dispatch.
func withRequestBudget(next http.Handler, selectBudget func(*http.Request) requestBudget) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		budget := selectBudget(r)
		start := time.Now()
		controller := http.NewResponseController(w)
		if err := controller.SetReadDeadline(start.Add(budget.upload)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return
		}
		if err := controller.SetWriteDeadline(start.Add(budget.total + budget.response)); err != nil && !errors.Is(err, http.ErrNotSupported) {
			return
		}
		ctx, cancel := context.WithDeadline(r.Context(), start.Add(budget.total))
		defer cancel()
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}
