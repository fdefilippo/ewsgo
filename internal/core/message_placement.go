// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"errors"
)

// ErrOutcomeUnknown means that a mutation may have applied. Callers must
// reconcile the native state before retrying, especially for a copy.
var ErrOutcomeUnknown = errors.New("write outcome unknown")

// MailMessagePlacementWriter mutates messages in the authenticated mailbox.
// Expected is the observed metadata revision of the source message. Backends
// must guard it during mutation. Move, trash and restore preserve the message
// ID. Trash moves to Deleted Items; restore moves out of Deleted Items.
// Copy returns a new ID. If readback fails after acknowledgment, a returned
// ID identifies the acknowledged object and the error is ErrOutcomeUnknown.
type MailMessagePlacementWriter interface {
	MoveMailMessage(ctx context.Context, id ID, expected Revision, destination ID) (Object, error)
	CopyMailMessage(ctx context.Context, id ID, expected Revision, destination ID) (Object, error)
	TrashMailMessage(ctx context.Context, id ID, expected Revision) (Object, error)
	RestoreMailMessage(ctx context.Context, id ID, expected Revision, destination ID) (Object, error)
}

// MailMessageSoftDeleter removes an active message into recoverable storage.
// A backend must reject the operation when it cannot establish recoverability.
type MailMessageSoftDeleter interface {
	SoftDeleteMailMessage(ctx context.Context, id ID, expected Revision) error
}

// MailMessageSoftDeleterWithNonReadReceipt honors the option to notify the
// originator when an unread message with a receipt request is deleted. A
// backend must reject without deleting if it cannot determine or fulfill the
// receipt obligation at the guarded revision.
type MailMessageSoftDeleterWithNonReadReceipt interface {
	SoftDeleteMailMessageWithNonReadReceipt(ctx context.Context, id ID, expected Revision) error
}

// RecoverableMailReader lists message envelopes in the authenticated user's
// recoverable storage. An empty folder selects the user's complete view.
// Parent is the original folder, which can itself have been removed.
type RecoverableMailReader interface {
	ReadRecoverableMail(ctx context.Context, folder ID) ([]Object, error)
}

// RecoverableMailWriter restores a selected message through the backend's
// native recovery operation. Origin is the folder reported by the dumpster
// snapshot. The restored message can have a new ID and be published by sync.
type RecoverableMailWriter interface {
	RecoverMailMessage(ctx context.Context, id, origin, destination ID) error
}

const MaxRecoverableMailMessages = 1024

// Optional display fields of a recoverable message envelope.
var SentRepresentingName = Key{ID: 0x0042}
var DisplayTo = Key{ID: 0x0e04}
var DeletedOn = Key{ID: 0x668f}
