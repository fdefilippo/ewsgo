// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"strings"

	"ewsgo/internal/core"
)

func (m *mailbox) ReadMailFolderCopyTree(ctx context.Context, source, destination core.ID, recursive bool) ([]core.Object, error) {
	from, fromOK := strings.CutPrefix(string(source), "f:")
	to, toOK := strings.CutPrefix(string(destination), "f:")
	if !fromOK || !toOK || !nativeID.MatchString(from) || !nativeID.MatchString(to) || from == "1" {
		return nil, core.ErrInvalid
	}
	tree, err := m.mailFolderTree(ctx)
	if err != nil {
		return nil, folderTreeError(err)
	}
	original, err := tree.mailFolder(from)
	if err != nil {
		return nil, err
	}
	if _, err := tree.mailFolder(to); err != nil {
		return nil, err
	}
	for id := to; id != "1"; id = tree[id].Parent {
		if id == from {
			return nil, core.ErrFolderCycle
		}
	}
	if !recursive {
		return []core.Object{folderObject(original)}, nil
	}

	// Keep the raw subtree here so a linked or saved-search child cannot
	// disappear during projection to ordinary mail folders.
	selector := attr(element(mailNS, "folder", ""), "l", from)
	request := attr(attr(element(mailNS, "GetFolderRequest", "", selector), "depth", "-1"), "tr", "0")
	result, err := m.native.request(ctx, mailNS, "GetFolder", "", request)
	if err != nil {
		return nil, semanticError(err)
	}
	if len(result.Children) != 1 || result.Children[0].get("id") != from {
		return nil, ErrProtocol
	}
	var out []core.Object
	seen := make(map[string]bool)
	var visit func(node, string) error
	visit = func(entry node, parent string) error {
		if entry.XMLName.Space != mailNS || entry.XMLName.Local != "folder" {
			return core.ErrUnsupported
		}
		folder, err := folderFromNode(entry)
		if err != nil {
			return err
		}
		current, err := tree.mailFolder(folder.ID)
		if err != nil {
			return err
		}
		if seen[folder.ID] || folder.Parent != parent || current.Parent != parent || current.Name != folder.Name {
			return core.ErrConflict
		}
		if folder.ID != from {
			if _, err := placementParent(core.MailFolderPlacement{Parent: core.ID("f:" + parent), Name: folder.Name}); err != nil {
				return err
			}
			if clip(folder.Name, 256) != folder.Name {
				return core.ErrUnsupported
			}
		}
		seen[folder.ID] = true
		out = append(out, folderObject(folder))
		for _, child := range entry.Children {
			switch child.XMLName.Local {
			case "folder", "link", "search":
				if err := visit(child, folder.ID); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := visit(result.Children[0], original.Parent); err != nil {
		return nil, err
	}
	for id, folder := range tree {
		if seen[folder.Parent] && !seen[id] {
			return nil, core.ErrConflict
		}
	}
	return out, nil
}
