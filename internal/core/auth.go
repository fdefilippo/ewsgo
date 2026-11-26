// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"errors"
)

var ErrAuthentication = errors.New("authentication rejected")
var ErrAccessDenied = errors.New("mailbox access denied")

// Identity is resolved by the backend, never from a protocol's mailbox selector.
type Identity struct {
	Scope Scope
	Name  string
}
type Authenticator interface {
	Authenticate(context.Context, string, string) (Session, error)
}
type Session interface {
	Identity() Identity
	Mailbox() Mailbox
	Close(context.Context) error
}

var DisplayName = Key{ID: 0x3001}
var IsRead = Key{ID: 0x0e69}

// WellKnown is semantic metadata, not a backend-native distinguished identifier.
var WellKnown = Key{Set: "f8b556e0-5d21-4ce8-9043-05e3d3dd7b90", Name: "wellKnownFolder"}
