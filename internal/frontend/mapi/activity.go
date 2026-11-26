// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"time"

	"ewsgo/internal/core"
)

var executionOperationLabels = func() map[string]struct{} {
	labels := make(map[string]struct{}, len(executionOperations))
	for _, op := range executionOperations {
		labels[op.label] = struct{}{}
	}
	return labels
}()

// IsExecutionOperationLabel reports whether a name belongs to the fixed ROP
// vocabulary emitted by the MAPI executor.
func IsExecutionOperationLabel(name string) bool {
	_, ok := executionOperationLabels[name]
	return ok
}

func recordROPActivity(ctx context.Context, id byte, response []byte, duration time.Duration) {
	operation := "other"
	for _, op := range executionOperations {
		if op.id == id {
			operation = op.label
			break
		}
	}
	result := core.ActivitySuccess
	var code uint32
	if id != 1 {
		result = core.ActivityFailure
		if len(response) >= 6 {
			code = binary.LittleEndian.Uint32(response[2:])
			switch code {
			case 0:
				hasPartialCompletion := id == ropMoveCopyMessages || id == ropMoveFolder ||
					id == ropDeleteMessages || id == ropDeleteFolder || id == ropSetReadFlags
				if !hasPartialCompletion || len(response) == 7 && response[6] == 0 {
					result = core.ActivitySuccess
				}
			case NotSupported, ropNotImplemented:
				result = core.ActivityUnsupported
			case accessDenied:
				result = core.ActivityDenied
			case 0x8004010f:
				result = core.ActivityNotFound
			case 0x80070057, invalidObject, nullObject:
				result = core.ActivityRejected
			}
		}
	}
	if operation == "other" || len(response) < 6 {
		id, code = 0, 0
	}
	core.RecordActivity(ctx, core.Activity{Layer: "rop", Operation: operation, Result: result, Duration: duration, ROPID: id, ROPReturnValue: code})
}
