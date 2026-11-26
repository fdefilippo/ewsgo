// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"encoding/binary"
	"time"
)

// The standard ROP table can return these byte sequences without truncation.
const MaxDeferredActionBytes = 510

// ValidDeferredActions accepts the standard RuleAction stream for opaque client
// actions. The gateway never interprets or executes their payload.
func ValidDeferredActions(data []byte) bool {
	if len(data) < 2 || len(data) > MaxDeferredActionBytes {
		return false
	}
	count := binary.LittleEndian.Uint16(data)
	data = data[2:]
	for range count {
		if len(data) < 11 {
			return false
		}
		size := int(binary.LittleEndian.Uint16(data))
		if size < 9 || size+2 > len(data) || data[2] != 5 {
			return false
		}
		data = data[2+size:]
	}
	return count != 0 && len(data) == 0
}

type DeferredAction struct {
	ID          ID
	Folder      ID
	Message     ID
	RuleID      uint64
	Provider    string
	Actions     []byte
	ClientEntry []byte
	Created     time.Time
	Modified    time.Time
}

type DeferredActionMailbox interface {
	// A Deferred Action Folder read may reconcile pending native deliveries.
	ReadDeferredActions(context.Context) ([]DeferredAction, error)
	// DeferredActionTargets returns only the IDs of original messages with recorded actions.
	DeferredActionTargets(ctx context.Context) (map[ID]struct{}, error)
	// The original pair is optional for a reference that was already patched.
	PatchDeferredActions(ctx context.Context, folder, message ID, server, client []byte) error
	DeleteDeferredAction(context.Context, ID) error
}

// DeferredActionsPending is the mailbox projection of PidTagHasDAMs.
var DeferredActionsPending = Key{ID: 0x3fea}
