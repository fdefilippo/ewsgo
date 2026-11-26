// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

// Counts describe direct non-FAI items in a folder, not descendants. Absence
// means unknown and is distinct from a known empty folder. They do not imply
// that the backend supports downloading the corresponding item kind.
var FolderItemCount = Key{ID: 0x3602}
var FolderUnreadCount = Key{ID: 0x3603}
