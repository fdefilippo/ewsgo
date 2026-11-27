// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"errors"
	"math"
	"strings"
	"unicode/utf8"

	"ewsgo/internal/core"
)

func placementParent(value core.MailFolderPlacement) (string, error) {
	parent, ok := strings.CutPrefix(string(value.Parent), "f:")
	if !ok || !nativeID.MatchString(parent) || value.Name == "" || len(value.Name) > 1024 ||
		!utf8.ValidString(value.Name) || strings.ContainsAny(value.Name, "\x00\r\n/") {
		return "", core.ErrInvalid
	}
	return parent, nil
}

func (m *mailbox) CreateMailFolder(ctx context.Context, placement core.MailFolderPlacement) (core.Object, error) {
	parent, err := placementParent(placement)
	if err != nil {
		return core.Object{}, err
	}
	if err = m.mailContentFolder(ctx, parent); err != nil {
		return core.Object{}, errors.Join(core.ErrWriteNotDispatched, semanticError(err))
	}
	folder := attr(attr(attr(attr(element(mailNS, "folder", ""), "name", placement.Name), "l", parent), "view", "message"), "fie", "0")
	result, err := m.native.requestMutation(ctx, mailNS, "CreateFolder", "", element(mailNS, "CreateFolderRequest", "", folder))
	if err != nil {
		return core.Object{}, folderPlacementError(err)
	}
	item := result.child(mailNS, "folder")
	if len(result.Children) != 1 || item == nil || !nativeID.MatchString(item.get("id")) || item.get("id") == parent {
		return core.Object{}, errors.Join(core.ErrOutcomeUnknown, ErrProtocol)
	}
	id := item.get("id")
	verified, err := m.verifyMailFolderPlacement(ctx, id, parent, placement.Name)
	if err != nil {
		return core.Object{ID: core.ID("f:" + id), Kind: core.Folder}, err
	}
	return verified, nil
}

func (m *mailbox) UpdateMailFolder(ctx context.Context, before core.Object, placement core.MailFolderPlacement) (core.Object, error) {
	parent, err := placementParent(placement)
	if err != nil {
		return core.Object{}, err
	}
	id, ok := strings.CutPrefix(string(before.ID), "f:")
	name, known := before.Properties[core.DisplayName].(core.Text)
	revision, hasRevision := before.Properties[core.MetadataRevision].(core.Integer)
	if !ok || !nativeID.MatchString(id) || id == "1" || before.Kind != core.Folder || !known || name == "" {
		return core.Object{}, core.ErrInvalid
	}
	if !hasRevision || revision < 0 || revision > math.MaxInt32 || core.Revision(revision) != before.Revision {
		return core.Object{}, core.ErrUnsupported
	}
	tree, err := m.mailFolderTree(ctx)
	if err != nil {
		return core.Object{}, semanticError(err)
	}
	current, err := tree.mailFolder(id)
	if err != nil {
		return core.Object{}, err
	}
	if _, err := tree.mailFolder(parent); err != nil {
		return core.Object{}, err
	}
	destinationInTrash := false
	for at := parent; at != "1"; at = tree[at].Parent {
		if at == id {
			return core.Object{}, core.ErrFolderCycle
		}
		destinationInTrash = destinationInTrash || at == "3"
	}
	if current.MetadataRevision == nil || *current.MetadataRevision != uint64(before.Revision) ||
		core.ID("f:"+current.Parent) != before.Parent || current.Name != string(name) {
		return core.Object{}, core.ErrConflict
	}
	if current.Name == placement.Name && current.Parent == parent {
		return folderObject(current), nil
	}
	action := attr(attr(attr(attr(element(mailNS, "action", ""), "id", id), "op", "update"), "name", placement.Name), "l", parent)
	var constraint []node
	// Zimbra marks descendant messages read on a move into Trash. A folder's
	// sequence can precede theirs even though the folder placement is current.
	if !destinationInTrash || current.Parent == parent {
		constraint = append(constraint, modifiedConstraint(before.Revision))
	}
	result, err := m.native.request(ctx, mailNS, "FolderAction", "", element(mailNS, "FolderActionRequest", "", action), constraint...)
	if err != nil {
		var fault *Fault
		if errors.As(err, &fault) {
			switch fault.Code {
			case "mail.ALREADY_EXISTS":
				return core.Object{}, core.ErrFolderNameConflict
			case "mail.INVALID_NAME":
				return core.Object{}, core.ErrInvalid
			case "mail.CANNOT_CONTAIN":
				return core.Object{}, core.ErrUnsupported
			}
		}
		return core.Object{}, messageWriteError(err)
	}
	applied := result.child(mailNS, "action")
	if len(result.Children) != 1 || applied == nil || applied.get("id") != id || applied.get("op") != "update" {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	verified, err := m.verifyMailFolderPlacement(ctx, id, parent, placement.Name)
	if err != nil {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	return verified, nil
}

func (m *mailbox) verifyMailFolderPlacement(ctx context.Context, id, parent, name string) (core.Object, error) {
	tree, err := m.mailFolderTree(ctx)
	if err != nil {
		return core.Object{}, semanticError(err)
	}
	folder, err := tree.mailFolder(id)
	if err != nil {
		return core.Object{}, err
	}
	if folder.Parent != parent || folder.Name != name {
		return core.Object{}, core.ErrConflict
	}
	return folderObject(folder), nil
}

func folderPlacementError(err error) error {
	var fault *Fault
	if errors.As(err, &fault) {
		switch fault.Code {
		case "mail.ALREADY_EXISTS":
			return core.ErrConflict
		case "mail.INVALID_NAME":
			return core.ErrInvalid
		case "mail.CANNOT_CONTAIN":
			return core.ErrUnsupported
		}
	}
	return messageWriteError(err)
}
