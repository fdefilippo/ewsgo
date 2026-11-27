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
		if !ok || user == "" || password == "" || len(user) > 1024 || len(password) > 1024 {
			w.Header().Set("WWW-Authenticate", `Basic realm="EWSGo", charset="UTF-8"`)
			http.Error(w, "authentication required", http.StatusUnauthorized)
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
	failure        authenticationFailureCode
	requestContext authenticationRequestState
}

type authenticationFailureCode uint8

const (
	authFailureNone authenticationFailureCode = iota
	authFailureAuthentication
	authFailureDenied
	authFailureCanceled
	authFailureDeadline
	authFailureOther
)

type authenticationRequestState uint8

const (
	authRequestNone authenticationRequestState = iota
	authRequestActive
	authRequestCanceled
	authRequestDeadline
	authRequestOther
)

func authenticationFailure(err, requestErr error) AuthenticationFailureDiagnostic {
	d := AuthenticationFailureDiagnostic{failure: authFailureOther, requestContext: authRequestActive}
	switch {
	case errors.Is(err, ErrAuthentication):
		d.failure = authFailureAuthentication
	case errors.Is(err, ErrAccessDenied):
		d.failure = authFailureDenied
	case errors.Is(err, context.Canceled):
		d.failure = authFailureCanceled
	case errors.Is(err, context.DeadlineExceeded):
		d.failure = authFailureDeadline
	}
	switch {
	case errors.Is(requestErr, context.Canceled):
		d.requestContext = authRequestCanceled
	case errors.Is(requestErr, context.DeadlineExceeded):
		d.requestContext = authRequestDeadline
	case requestErr != nil:
		d.requestContext = authRequestOther
	}
	return d
}

func (d AuthenticationFailureDiagnostic) Fields() (failure, requestContext string) {
	failures := [...]string{
		authFailureNone: "none", authFailureAuthentication: "authentication", authFailureDenied: "access_denied",
		authFailureCanceled: "canceled", authFailureDeadline: "deadline_exceeded", authFailureOther: "other",
	}
	contexts := [...]string{
		authRequestNone: "none", authRequestActive: "active", authRequestCanceled: "canceled",
		authRequestDeadline: "deadline_exceeded", authRequestOther: "other",
	}
	if int(d.failure) >= len(failures) || int(d.requestContext) >= len(contexts) {
		return "other", "other"
	}
	return failures[d.failure], contexts[d.requestContext]
}
