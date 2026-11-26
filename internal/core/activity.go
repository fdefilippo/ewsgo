// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"errors"
	"time"
)

// ActivityResult is a semantic outcome, never a backend error or wire payload.
type ActivityResult uint8

const (
	ActivitySuccess ActivityResult = iota
	ActivityRejected
	ActivityUnsupported
	ActivityAuthentication
	ActivityDenied
	ActivityNotFound
	ActivityConflict
	ActivityResync
	ActivityCanceled
	ActivityDeadline
	ActivityOutcomeUnknown
	ActivityFailure
)

func (r ActivityResult) String() string {
	names := [...]string{"success", "rejected", "not_supported", "authentication", "access_denied", "not_found", "conflict", "resync_required", "canceled", "deadline_exceeded", "outcome_unknown", "failure"}
	if int(r) >= len(names) {
		return "failure"
	}
	return names[r]
}
func ActivityOutcome(err error) ActivityResult {
	switch {
	case err == nil:
		return ActivitySuccess
	case errors.Is(err, ErrAuthentication):
		return ActivityAuthentication
	case errors.Is(err, ErrAccessDenied):
		return ActivityDenied
	case errors.Is(err, ErrUnsupported):
		return ActivityUnsupported
	case errors.Is(err, ErrNotFound):
		return ActivityNotFound
	case errors.Is(err, ErrConflict):
		return ActivityConflict
	case errors.Is(err, ErrResync):
		return ActivityResync
	case errors.Is(err, ErrOutcomeUnknown):
		return ActivityOutcomeUnknown
	case errors.Is(err, ErrInvalid), errors.Is(err, ErrSendRejected), errors.Is(err, ErrInvalidSendRecipients):
		return ActivityRejected
	case errors.Is(err, context.Canceled):
		return ActivityCanceled
	case errors.Is(err, context.DeadlineExceeded):
		return ActivityDeadline
	default:
		return ActivityFailure
	}
}

// Activity contains technical operation names and protocol outcomes. Sinks
// enforce a closed vocabulary. Callers must never pass arguments, object
// identifiers, backend errors or values.
type Activity struct {
	Layer, Operation string
	Result           ActivityResult
	Duration         time.Duration
	Operations       []string
	ROPID            byte   // Zero for events without an identified ROP.
	ROPReturnValue   uint32 // Protocol ReturnValue, without backend error text.
}
type ActivityObserver interface {
	Authenticated(string)
	Record(Activity)
}
type activityKey struct{}

func WithActivityObserver(ctx context.Context, observer ActivityObserver) context.Context {
	return context.WithValue(ctx, activityKey{}, observer)
}
func HasActivityObserver(ctx context.Context) bool { return activityObserver(ctx) != nil }
func activityObserver(ctx context.Context) ActivityObserver {
	o, _ := ctx.Value(activityKey{}).(ActivityObserver)
	return o
}
func RecordActivity(ctx context.Context, event Activity) {
	if o := activityObserver(ctx); o != nil {
		o.Record(event)
	}
}
func RecordAuthenticated(ctx context.Context, account string) {
	if o := activityObserver(ctx); o != nil {
		o.Authenticated(account)
	}
}
