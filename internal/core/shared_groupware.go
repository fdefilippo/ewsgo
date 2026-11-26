// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import "context"

// SharedGroupwareFolders exposes folder links accepted by the authenticated
// mailbox. The link and effective native access are checked on every use.
type SharedGroupwareFolders interface {
	GroupwareFolderTree(context.Context, []SpecialFolderKind) ([]Object, error)
	ReadSharedGroupwareFolder(ctx context.Context, folder ID, role SpecialFolderKind) (Object, error)
}

var (
	GroupwareFolderClass = Key{Set: WellKnown.Set, Name: "groupwareFolderClass"}
	SharedFolderRights   = Key{Set: WellKnown.Set, Name: "sharedFolderRights"}
)

const SharedGroupwareFolderIDPrefix = "shared:"
