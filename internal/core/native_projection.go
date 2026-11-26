// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import "context"

// NativeObject is the backend's last observed identity, membership and metadata revision.
// Its revision is comparable only to another observation of the same object.
type NativeObject struct {
	ID, Parent ID
	Kind       Kind
	Revision   uint64
}

// NativeCheckpoint separates the opaque backend cursor from the local position
// reached by native ingestion. Gateway-only writes leave this checkpoint alone.
type NativeCheckpoint struct {
	Cursor   Cursor
	Revision Revision
}

func (NativeCheckpoint) String() string { return "mailbox delta checkpoint [redacted]" }

// NativeBaseline is the committed state from which a full reconciliation starts.
// Legacy objects have a logical ID and parent, but no known native revision or kind.
type NativeBaseline struct {
	Checkpoint NativeCheckpoint
	Legacy     bool
	Objects    []NativeObject
}

// NativeProjection is bound to one authenticated mailbox and one backend instance.
// Apply commits observations, confirmed removals and the next cursor together.
// A zero expected checkpoint can seed a known baseline; a legacy checkpoint
// without a projection requires reconciliation.
// Reconcile commits a completed snapshot against its baseline. The backend
// supplies only removals that it verified, including during full enumeration.
type NativeProjection interface {
	Checkpoint(context.Context) (NativeCheckpoint, error)
	Members(context.Context, NativeCheckpoint, []ID) (map[ID]NativeObject, error)
	Apply(context.Context, NativeCheckpoint, Cursor, []NativeObject, []ID) error
	Baseline(context.Context) (NativeBaseline, error)
	Reconcile(context.Context, NativeBaseline, Cursor, []NativeObject, []ID) error
}

type NativeProjectionStore interface {
	OpenNativeProjection(context.Context, Scope, string) (NativeProjection, error)
}
