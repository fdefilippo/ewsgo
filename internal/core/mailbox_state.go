// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import "context"

// MailboxOwnerReader reports whether the active backend session owns this
// mailbox. Mailbox preferences are independent of this authorization check.
type MailboxOwnerReader interface {
	MailboxOwner(context.Context) (bool, error)
}

// MailboxStateReader is an optional, provisional semantic read capability.
// Rights describe access through this provider, not unsupported native APIs.
type MailboxStateReader interface {
	MailboxState(context.Context) (MailboxState, error)
}

type MailboxState struct {
	Owner, CanSend, OutOfOffice bool
}
