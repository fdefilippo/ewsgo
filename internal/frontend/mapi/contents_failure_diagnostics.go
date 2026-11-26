// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/binary"
	"errors"

	"ewsgo/internal/core"
)

// A closed stage label only. Neither the cause's text nor properties, folder
// identifiers, sizes or values are retained by the execution diagnostic.
type contentsFailureStage uint8

const (
	contentsFailureAuthorization contentsFailureStage = iota + 1
	contentsFailureState
	contentsFailureNormalMemory
	contentsFailureNormalCapture
	contentsFailureFAICapture
	contentsFailureProjectionMemory
	contentsFailureProjection
	contentsFailureDelta
	contentsFailureCheckpoint
	contentsFailureEncoding
)

var contentsFailureLabels = [...]string{
	"none", "authorization", "state", "normal_memory", "normal_capture",
	"fai_capture", "projection_memory", "projection", "delta", "checkpoint", "encoding",
}

type contentsPreparationError struct {
	stage contentsFailureStage
	cause error
}

func (contentsPreparationError) Error() string    { return "contents preparation failed" }
func (e contentsPreparationError) Unwrap() error  { return e.cause }
func (contentsPreparationError) String() string   { return "contentsPreparationError{redacted}" }
func (contentsPreparationError) GoString() string { return "contentsPreparationError{redacted}" }

// Distinguish the actual shared-ledger rejection from other unsupported
// capabilities. No quota counters are logged or embedded in the error.
type contentsBudgetError struct{}

func (contentsBudgetError) Error() string { return "contents memory budget unavailable" }
func (contentsBudgetError) Unwrap() error { return core.ErrUnsupported }

type contentsFailureDiagnostic struct {
	stage  contentsFailureStage
	budget bool
}

func (contentsFailureDiagnostic) String() string   { return "contentsFailureDiagnostic{redacted}" }
func (contentsFailureDiagnostic) GoString() string { return "contentsFailureDiagnostic{redacted}" }

func inspectContentsFailure(err error, fallback contentsFailureStage) contentsFailureDiagnostic {
	var preparation contentsPreparationError
	if errors.As(err, &preparation) {
		fallback = preparation.stage
	}
	return contentsFailureDiagnostic{fallback, errors.Is(err, contentsBudgetError{})}
}

// Capture before any subsequent ROP can release the failed transfer handle.
// The existing trace opt-in controls this field along with other buffer labels.
func (d *ExecutionDiagnostic) recordContentsFailure(objects *mailboxObjects, op Operation, handles []uint32, response []byte) {
	if !d.enabled || len(response) < 6 || response[0] != 0x4e || binary.LittleEndian.Uint32(response[2:]) == 0 {
		return
	}
	o, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 || o.download == nil || o.download.contents == nil {
		return
	}
	failure := o.download.contents.failureDetail
	if failure.stage > 0 && int(failure.stage) < len(contentsFailureLabels) {
		d.contentsFailureStages |= 1 << failure.stage
	}
	d.contentsBudgetFailure = d.contentsBudgetFailure || failure.budget
}
