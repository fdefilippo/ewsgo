// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"slices"
	"strconv"
	"strings"

	"ewsgo/internal/core"
)

func (h *Handler) findFolders(ctx context.Context, op node, session core.Session, shape responseShape) []node {
	const operation = "FindFolder"
	if op.attr("Traversal") != "Shallow" {
		return syncError(operation, core.ErrUnsupported)
	}
	seen := make(map[string]bool, len(op.Children))
	for _, child := range op.Children {
		name := child.XMLName.Local
		if child.XMLName.Space != messages || seen[name] {
			return syncError(operation, core.ErrInvalid)
		}
		seen[name] = true
		switch name {
		case "FolderShape", "IndexedPageFolderView", "ParentFolderIds":
		default:
			return syncError(operation, core.ErrUnsupported)
		}
	}
	parents := op.child(messages, "ParentFolderIds")
	if parents == nil || len(parents.Children) != 1 {
		return syncError(operation, core.ErrInvalid)
	}
	offset, limit := 0, 100
	if view := op.child(messages, "IndexedPageFolderView"); view != nil {
		var offsetErr, limitErr error
		offset, offsetErr = strconv.Atoi(view.attr("Offset"))
		limit, limitErr = strconv.Atoi(view.attr("MaxEntriesReturned"))
		if offsetErr != nil || limitErr != nil || offset < 0 || limit < 1 || limit > 1000 ||
			view.attr("BasePoint") != "Beginning" || len(view.Children) != 0 {
			return syncError(operation, core.ErrInvalid)
		}
	}
	parent, err := h.readFolder(ctx, session, parents.Children[0])
	if err != nil {
		return syncError(operation, err)
	}
	if parent.Kind != core.Folder || strings.HasPrefix(string(parent.ID), core.SharedGroupwareFolderIDPrefix) {
		return syncError(operation, core.ErrUnsupported)
	}
	box := session.Mailbox()
	var folders []core.Object
	var readTree func(context.Context, []core.SpecialFolderKind) ([]core.Object, error)
	if shares, ok := box.(core.SharedGroupwareFolders); ok {
		readTree = shares.GroupwareFolderTree
	} else if tree, ok := box.(core.OwnFolderTreeReader); ok {
		readTree = tree.OwnFolderTree
	}
	if readTree != nil {
		all, err := readTree(ctx, []core.SpecialFolderKind{core.CalendarFolder, core.ContactsFolder})
		if err != nil {
			return syncError(operation, err)
		}
		for _, folder := range all {
			if folder.Parent == parent.ID {
				folders = append(folders, folder)
			}
		}
	} else {
		page, err := box.Query(ctx, core.Query{Parent: parent.ID, Kind: core.Folder, Limit: 1000})
		if err != nil {
			return syncError(operation, err)
		}
		if page.More {
			return syncError(operation, core.ErrLimit)
		}
		folders = page.Objects
	}
	if len(folders) > 1000 {
		return syncError(operation, core.ErrLimit)
	}
	slices.SortFunc(folders, func(a, b core.Object) int { return strings.Compare(string(a.ID), string(b.ID)) })
	start := min(offset, len(folders))
	end := min(start+limit, len(folders))
	list := make([]node, 0, end-start)
	for _, folder := range folders[start:end] {
		list = append(list, shape.apply(h.folder(session.Identity().Scope, folder)))
	}
	root := el(messages, "RootFolder", "", el(types, "Folders", "", list...))
	root = at(at(at(root, "IndexedPagingOffset", strconv.Itoa(end)), "TotalItemsInView", strconv.Itoa(len(folders))),
		"IncludesLastItemInRange", strconv.FormatBool(end == len(folders)))
	return []node{success(operation, root)}
}
