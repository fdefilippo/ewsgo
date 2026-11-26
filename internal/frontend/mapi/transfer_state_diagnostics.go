// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
)

type transferStateSource byte

const (
	transferStateHierarchy transferStateSource = iota + 1
	transferStateContents
	transferStateCollector
)

type transferStateStage byte

const (
	transferStateHandleLookup transferStateStage = iota + 1
	transferStateWrongObject
	transferStateExportedSource
	transferStateAuthorization
	transferStatePriorFailure
	transferStatePending
	transferStateClosed
	transferStateCollectorState
	transferStateMemory
	transferStateEncoding
	transferStateCanceled
	transferStateHandleCreate
)

// Transfer state diagnostics describe the chosen handler and failure branch.
// No actual error text, handle or state value belongs here.
type transferStateFailure struct {
	source transferStateSource
	stage  transferStateStage
}

func (d *transferStateFailure) reject(output byte, code uint32, stage transferStateStage) []byte {
	d.stage = stage
	return ropError(0x82, output, code)
}

func (d transferStateFailure) appendLabels(labels []string) []string {
	if d.stage == 0 {
		return labels
	}
	stages := [...]string{
		"other", "handle_lookup", "wrong_object", "exported_source", "authorization",
		"prior_failure", "pending_state", "closed_state", "collector_state", "memory", "encoding",
		"canceled", "handle_create",
	}
	if int(d.stage) >= len(stages) {
		return append(labels, "transfer_state_other")
	}
	labels = append(labels, "transfer_state_"+stages[d.stage])
	sources := [...]string{"", "hierarchy", "contents", "collector"}
	if d.source != 0 && int(d.source) < len(sources) {
		labels = append(labels, "transfer_state_source_"+sources[d.source])
	}
	return labels
}

func transferStateAdmissionStage(err error) transferStateStage {
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return transferStateCanceled
	}
	return transferStateMemory
}
