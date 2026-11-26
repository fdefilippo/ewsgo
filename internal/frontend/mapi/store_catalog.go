// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"

	"ewsgo/internal/core"
)

// SpecialFolderRole order is the private Logon FolderIds wire order. Roles
// identify semantics only, never FIDs, handles or backend-native folder IDs.
type SpecialFolderRole uint8

const (
	MailboxRoot SpecialFolderRole = iota
	DeferredAction
	SpoolerQueue
	IPMSubtree
	Inbox
	Outbox
	SentItems
	DeletedItems
	CommonViews
	Schedule
	Search
	Views
	Shortcuts
	specialFolderCount
)

// A blank NativeID denotes a gateway service object still to be implemented.
// NativeID is opaque; it must not be parsed by the MAPI frontend.
type specialFolder struct {
	Role     SpecialFolderRole
	NativeID core.ID
}

type privateFolderCatalog struct {
	scope   core.Scope
	folders [specialFolderCount]specialFolder
}

// privateFolders resolves the four approved native roles from the freshly
// authenticated mailbox. No state, native object or protocol ID is written.
// Truncated/ambiguous catalogs fail closed instead of picking an arbitrary ID.
func privateFolders(ctx context.Context, session core.Session) (privateFolderCatalog, error) {
	catalog, _, err := readPrivateFolderTree(ctx, session)
	return catalog, err
}

// readPrivateFolderTree returns the same complete, validated query used to
// resolve the roles. On failure neither a partial tree nor a catalog escapes.
func readPrivateFolderTree(ctx context.Context, session core.Session) (privateFolderCatalog, []core.Object, error) {
	return readPrivateFolderTreeWithCalendar(ctx, session, false)
}

func readPrivateFolderTreeWithCalendar(ctx context.Context, session core.Session, calendar bool) (privateFolderCatalog, []core.Object, error) {
	return readPrivateFolderTreeWithSpecialFolders(ctx, session, calendar, false, false)
}
func readPrivateFolderTreeWithSpecialFolders(ctx context.Context, session core.Session, calendar, contacts, tasks bool) (privateFolderCatalog, []core.Object, error) {
	var catalog privateFolderCatalog
	if session == nil {
		return privateFolderCatalog{}, nil, core.ErrInvalid
	}
	scope := session.Identity().Scope
	box := session.Mailbox()
	if scope.Backend == "" || scope.Account == "" || box == nil || box.Root() == "" {
		return privateFolderCatalog{}, nil, core.ErrInvalid
	}
	objects, err := readPrivateFolderObjects(ctx, box, [metadataFolderRoleCount]bool{calendar, contacts, tasks})
	if err != nil {
		return privateFolderCatalog{}, nil, err
	}
	if len(objects) > 1000 {
		return privateFolderCatalog{}, nil, core.ErrUnsupported
	}
	byID := make(map[core.ID]core.Object, len(objects))
	for _, object := range objects {
		if object.ID == "" || object.Kind != core.Folder {
			return privateFolderCatalog{}, nil, core.ErrInvalid
		}
		if _, duplicate := byID[object.ID]; duplicate {
			return privateFolderCatalog{}, nil, core.ErrConflict
		}
		byID[object.ID] = object
	}
	if _, exists := byID[box.Root()]; !exists {
		return privateFolderCatalog{}, nil, core.ErrNotFound
	}
	// Require a complete rooted tree: dangling parents/cycles cannot establish
	// that the selected native folders descend from the authenticated root.
	for id := range byID {
		at := id
		for steps := 0; at != box.Root(); steps++ {
			object, exists := byID[at]
			if !exists || steps >= len(byID) {
				return privateFolderCatalog{}, nil, core.ErrInvalid
			}
			at = object.Parent
		}
	}
	for i := range catalog.folders {
		catalog.folders[i].Role = SpecialFolderRole(i)
	}
	catalog.folders[IPMSubtree].NativeID = box.Root()
	for _, object := range objects {
		var role SpecialFolderRole
		switch object.Properties[core.WellKnown] {
		case core.Text("inbox"):
			role = Inbox
		case core.Text("sentitems"):
			role = SentItems
		case core.Text("deleteditems"):
			role = DeletedItems
		default:
			continue
		}
		if catalog.folders[role].NativeID != "" || object.ID == box.Root() {
			return privateFolderCatalog{}, nil, core.ErrConflict
		}
		catalog.folders[role].NativeID = object.ID
	}
	for _, role := range []SpecialFolderRole{Inbox, SentItems, DeletedItems} {
		if catalog.folders[role].NativeID == "" {
			return privateFolderCatalog{}, nil, core.ErrNotFound
		}
	}
	catalog.scope = scope
	return catalog, objects, nil
}

func readPrivateFolderObjects(ctx context.Context, box core.Mailbox, requested [metadataFolderRoleCount]bool) ([]core.Object, error) {
	if reader, ok := box.(core.OwnFolderTreeReader); ok {
		var roles []core.SpecialFolderKind
		for role, enabled := range requested {
			if enabled {
				roles = append(roles, metadataFolderRole(role).kind())
			}
		}
		if len(roles) != 0 {
			if err := hierarchyOwner(ctx, box); err != nil {
				return nil, err
			}
			if shares, ok := box.(core.SharedGroupwareFolders); ok {
				return shares.GroupwareFolderTree(ctx, roles)
			}
		}
		return reader.OwnFolderTree(ctx, roles)
	}

	page, err := box.Query(ctx, core.Query{Kind: core.Folder, Limit: 1000})
	if err != nil {
		return nil, err
	}
	if page.More || len(page.Objects) > 1000 {
		return nil, core.ErrUnsupported
	}
	objects := page.Objects
	for role, enabled := range requested {
		if !enabled {
			continue
		}
		object, err := readSpecialFolder(ctx, box, metadataFolderRole(role))
		if err == nil {
			objects = append(append([]core.Object(nil), objects...), object)
		} else if _, hasCapability := box.(core.SpecialFolderReader); !errors.Is(err, core.ErrNotFound) && (hasCapability || !errors.Is(err, core.ErrUnsupported)) {
			return nil, err
		}
	}
	return objects, nil
}
