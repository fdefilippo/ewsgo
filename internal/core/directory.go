// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import "context"

// DirectoryEntry identifies a native directory object, not permission to open
// its mailbox. Account is the backend's stable ID, never an address or alias.
type DirectoryEntry struct {
	Identity    Identity
	DisplayName string
}

const MaxDirectoryEntries = 1000

// DirectorySession is optional. Directory visibility is determined by the
// authenticated backend session, independently of mailbox access rights.
type DirectorySession interface {
	Directory() DirectoryReader
}

// Entries returns the complete visible set for this request, or an error.
// Truncated/unsupported catalogs must never be presented as complete results.
type DirectoryReader interface {
	Entries(context.Context) ([]DirectoryEntry, error)
}
