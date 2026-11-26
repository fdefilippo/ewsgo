// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"errors"
	"slices"
	"strings"

	"ewsgo/internal/core"
)

// Only default native Calendar, Contacts and Tasks metadata are exposed. The
// ordinary mail-only Get/Query methods deliberately remain unchanged.
func (m *mailbox) SpecialFolder(ctx context.Context, role core.SpecialFolderKind) (core.Object, error) {
	if specialFolderSpec(role).id == "" {
		return core.Object{}, core.ErrUnsupported
	}
	tree, err := m.mailFolderTree(ctx)
	if err != nil {
		return core.Object{}, folderTreeError(err)
	}
	folder, err := tree.specialFolder(role)
	if err != nil {
		return core.Object{}, err
	}
	if err := ctx.Err(); err != nil {
		return core.Object{}, err
	}
	return folder, nil
}

func (m *mailbox) OwnFolderTree(ctx context.Context, roles []core.SpecialFolderKind) ([]core.Object, error) {
	tree, err := m.mailFolderTree(ctx)
	if err != nil {
		return nil, folderTreeError(err)
	}
	return ownFolderObjects(ctx, tree, roles)
}

func ownFolderObjects(ctx context.Context, tree nativeMailFolderTree, roles []core.SpecialFolderKind) ([]core.Object, error) {
	objects := make([]core.Object, 0, len(tree))
	for _, f := range tree {
		if f.View == "" || f.View == "message" {
			if err := tree.checkRooted(f.ID); err != nil {
				return nil, err
			}
			objects = append(objects, folderObject(f))
		}
	}
	for _, role := range roles {
		if specialFolderSpec(role).id == "" {
			return nil, core.ErrUnsupported
		}
		folder, err := tree.specialFolder(role)
		if errors.Is(err, core.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		objects = append(objects, folder)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	slices.SortFunc(objects, func(a, b core.Object) int {
		return strings.Compare(string(a.ID), string(b.ID))
	})
	return objects, nil
}

func folderTreeError(err error) error {
	if errors.Is(err, ErrLimit) {
		return core.ErrUnsupported
	}
	return semanticError(err)
}

type nativeSpecialFolder struct {
	id, view, wellKnown string
}

func specialFolderSpec(role core.SpecialFolderKind) nativeSpecialFolder {
	switch role {
	case core.CalendarFolder:
		return nativeSpecialFolder{"10", "appointment", "calendar"}
	case core.ContactsFolder:
		return nativeSpecialFolder{"7", "contact", "contacts"}
	case core.TasksFolder:
		return nativeSpecialFolder{"15", "task", "tasks"}
	}
	return nativeSpecialFolder{}
}

func (tree nativeMailFolderTree) specialFolder(role core.SpecialFolderKind) (core.Object, error) {
	spec := specialFolderSpec(role)
	target, exists := tree[spec.id] // Native FolderConstants, independent of localized names.
	if !exists {
		return core.Object{}, core.ErrNotFound
	}
	if target.View != spec.view || target.Parent == "" {
		return core.Object{}, ErrProtocol
	}
	if err := tree.checkRooted(target.ID); err != nil {
		return core.Object{}, err
	}
	object := folderObject(target)
	object.Properties[core.WellKnown] = core.Text(spec.wellKnown)
	return object, nil
}
