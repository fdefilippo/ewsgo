// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"

	"ewsgo/internal/core"
)

type contentsUploadStage byte

const (
	uploadLifecycle contentsUploadStage = iota + 1
	uploadAuthorize
	uploadSequence
	uploadParse
	uploadReplica
	uploadCounter
	uploadGivenLimit
	uploadGivenClass
	uploadGivenUnknown
	uploadCommitAccess
)

// Only the first actual Contents/FAI upload failure in a batch is retained.
// No state bytes, sizes, GUIDs, counters, handle or raw property tag survive.
type contentsUploadFailure struct {
	stage    contentsUploadStage
	property byte
	kind     byte
}

func (contentsUploadFailure) String() string   { return "contentsUploadFailure{redacted}" }
func (contentsUploadFailure) GoString() string { return "contentsUploadFailure{redacted}" }

type contentsUploadError struct {
	failure contentsUploadFailure
	cause   error
}

func (*contentsUploadError) Error() string    { return "Contents state upload failed" }
func (*contentsUploadError) String() string   { return "contentsUploadError{redacted}" }
func (*contentsUploadError) GoString() string { return "contentsUploadError{redacted}" }
func (e *contentsUploadError) Unwrap() error  { return e.cause }

func contentsUploadProperty(tag uint32) byte {
	switch tag {
	case idsetGivenTag, 0x40170102:
		return 1
	case cnsetSeenTag:
		return 2
	case cnsetSeenFAITag:
		return 3
	case cnsetReadTag:
		return 4
	default:
		return 5
	}
}

func contentsUploadErrorKind(err error) byte {
	switch {
	case errors.Is(err, ErrMalformed):
		return 1
	case errors.Is(err, core.ErrResync):
		return 2
	case errors.Is(err, core.ErrConflict):
		return 3
	case errors.Is(err, core.ErrAccessDenied):
		return 4
	case errors.Is(err, core.ErrInvalid):
		return 5
	case errors.Is(err, core.ErrUnsupported), errors.Is(err, ErrUnsupported):
		return 6
	case errors.Is(err, context.Canceled), errors.Is(err, context.DeadlineExceeded):
		return 7
	case errors.Is(err, core.ErrClosed):
		return 8
	default:
		return 9
	}
}

func inspectContentsUploadFailure(err error) contentsUploadFailure {
	var e *contentsUploadError
	if errors.As(err, &e) {
		return e.failure
	}
	return contentsUploadFailure{}
}

func (d contentsUploadFailure) labels() []string {
	if d.kind == 0 {
		return nil
	}
	stage := "other"
	stages := [...]string{"other", "lifecycle", "authorize", "sequence", "parse", "replica", "counter", "given_limit", "given_class", "given_unknown", "commit_access"}
	if int(d.stage) < len(stages) {
		stage = stages[d.stage]
	}
	property := "other"
	properties := [...]string{"other", "given", "seen", "seen_fai", "read", "other"}
	if int(d.property) < len(properties) {
		property = properties[d.property]
	}
	kind := "other"
	kinds := [...]string{"other", "malformed", "resync_required", "conflict", "access_denied", "invalid_parameter", "not_supported", "cancelled", "closed", "other"}
	if int(d.kind) < len(kinds) {
		kind = kinds[d.kind]
	}
	return []string{"state_upload_" + stage, "state_property_" + property, "state_error_" + kind}
}
