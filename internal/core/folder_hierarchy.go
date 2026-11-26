// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"fmt"
)

var (
	ErrFolderNameConflict = fmt.Errorf("folder name already exists: %w", ErrConflict)
	ErrFolderCycle        = fmt.Errorf("folder cycle: %w", ErrInvalid)
)

type MailFolderPlacement struct {
	Parent ID
	Name   string
}

// MailFolderHierarchyWriter handles ordinary mail folders in the authenticated
// mailbox. Update checks the observed parent, name and revision before issuing
// one native placement change. Backends must check the resulting placement;
// the precondition is not necessarily atomic with concurrent native writers.
//
// If creation was acknowledged with a native ID but readback fails, the
// returned object carries that ID with the error so a retry can reconcile
// the exact folder without creating or adopting a similarly named folder.
// ErrConflict with an empty ID means creation was definitively rejected.
type MailFolderHierarchyWriter interface {
	CreateMailFolder(context.Context, MailFolderPlacement) (Object, error)
	UpdateMailFolder(context.Context, Object, MailFolderPlacement) (Object, error)
}

// MailFolderCopyReader enumerates native mail folders in parent-first order
// and rejects an unsupported subtree or a destination inside the source.
// Without recursion it returns only the source. The snapshot is an observed
// view, so callers still guard individual writes against concurrent changes.
type MailFolderCopyReader interface {
	ReadMailFolderCopyTree(ctx context.Context, source, destination ID, recursive bool) ([]Object, error)
}
