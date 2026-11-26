// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"errors"
	"net/http"
	"time"
)

// BasicTLS authenticates each request directly against the selected backend.
// Trusted reverse-proxy headers are deliberately not accepted as proof of TLS.
func BasicTLS(auth Authenticator, next func(http.ResponseWriter, *http.Request, Session)) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-store")
		if r.TLS == nil {
			http.Error(w, "TLS required", http.StatusUpgradeRequired)
			return
		}
		user, password, ok := r.BasicAuth()
		if !ok || user == "" || len(user) > 1024 || len(password) > 1024 {
			w.Header().Set("WWW-Authenticate", `Basic realm="EWSGo", charset="UTF-8"`)
			http.Error(w, "authentication required", 401)
			return
		}
		started := time.Now()
		session, err := auth.Authenticate(r.Context(), user, password)
		RecordActivity(r.Context(), Activity{Layer: "authentication", Operation: "Authenticate", Result: ActivityOutcome(err), Duration: time.Since(started)})
		if err != nil {
			if recorder, ok := w.(interface {
				RecordAuthenticationFailure(AuthenticationFailureDiagnostic)
			}); ok {
				recorder.RecordAuthenticationFailure(authenticationFailure(err, r.Context().Err()))
			}
			if errors.Is(err, ErrAuthentication) {
				w.Header().Set("WWW-Authenticate", `Basic realm="EWSGo", charset="UTF-8"`)
				http.Error(w, "authentication rejected", 401)
			} else if errors.Is(err, ErrAccessDenied) {
				http.Error(w, "access denied", 403)
			} else {
				http.Error(w, "backend unavailable", 502)
			}
			return
		}
		RecordAuthenticated(r.Context(), session.Identity().Name)
		defer func() {
			ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 3*time.Second)
			defer cancel()
			_ = session.Close(ctx)
		}()
		next(w, r, session)
	})
}

// AuthenticationFailureDiagnostic contains only enum values, never errors,
// credentials or backend details. A receiver is installed only by lab tracing.
type AuthenticationFailureDiagnostic struct {
	failure, requestContext byte
}

func authenticationFailure(err, requestErr error) AuthenticationFailureDiagnostic {
	d := AuthenticationFailureDiagnostic{failure: 5, requestContext: 1}
	switch {
	case errors.Is(err, ErrAuthentication):
		d.failure = 1
	case errors.Is(err, ErrAccessDenied):
		d.failure = 2
	case errors.Is(err, context.Canceled):
		d.failure = 3
	case errors.Is(err, context.DeadlineExceeded):
		d.failure = 4
	}
	switch {
	case errors.Is(requestErr, context.Canceled):
		d.requestContext = 2
	case errors.Is(requestErr, context.DeadlineExceeded):
		d.requestContext = 3
	case requestErr != nil:
		d.requestContext = 4
	}
	return d
}

func (d AuthenticationFailureDiagnostic) Fields() (failure, requestContext string) {
	failures := [...]string{"none", "authentication", "access_denied", "canceled", "deadline_exceeded", "other"}
	contexts := [...]string{"none", "active", "canceled", "deadline_exceeded", "other"}
	if int(d.failure) >= len(failures) || int(d.requestContext) >= len(contexts) {
		return "other", "other"
	}
	return failures[d.failure], contexts[d.requestContext]
}
