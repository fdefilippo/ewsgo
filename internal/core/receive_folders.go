// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"time"
)

type ReceiveFolderAssociation struct {
	Class    string
	Folder   ID
	Modified time.Time
}

// ReceiveFolderMailbox persists and applies routing on the backend that
// receives mail. An empty folder ID removes an explicit association.
type ReceiveFolderMailbox interface {
	ReadReceiveFolders(context.Context) ([]ReceiveFolderAssociation, error)
	SetReceiveFolder(context.Context, string, ID) error
}
