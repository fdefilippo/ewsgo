// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import "context"

// SpecialFolderReader is an optional metadata-only capability. It does not
// promise contents, editing, or a broader Get/Query catalog.
type SpecialFolderReader interface {
	SpecialFolder(context.Context, SpecialFolderKind) (Object, error)
}

// OwnFolderTreeReader returns the complete mail folder tree of the authenticated
// mailbox and any requested default special folders present in the same read.
// Shared mounts are excluded. Special folders expose metadata only.
type OwnFolderTreeReader interface {
	OwnFolderTree(context.Context, []SpecialFolderKind) ([]Object, error)
}

type SpecialFolderKind uint8

const (
	CalendarFolder SpecialFolderKind = iota + 1
	ContactsFolder
	TasksFolder
)
