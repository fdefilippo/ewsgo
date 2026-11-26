// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import "context"

// SharedFolderMailbox queries a configured, foreign folder using the
// authenticated user's native permissions.
type SharedFolderMailbox interface {
	ReadSharedMailFolder(ctx context.Context, owner Scope, folder ID) (SharedMailFolder, error)
}

type SharedMailFolder struct {
	ID     ID
	Parent ID
}

type PublicReadState struct {
	Revision uint64
	Replica  [16]byte
	CNSet    []byte
}

type PublicReadStateStore interface {
	PublicReadState(ctx context.Context, user, store string, folder [24]byte, private bool) (PublicReadState, error)
	SavePublicReadState(ctx context.Context, user, store string, folder [24]byte, private bool, state PublicReadState) error
	PrivatePublicReadFolders(ctx context.Context, user, store string, limit int) ([][24]byte, error)
}
