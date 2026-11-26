// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"errors"
	"strings"

	"ewsgo/internal/core"
)

func folderName(n *node, mayDeclareClass bool) (string, error) {
	if n == nil || !isElement(*n, types, "Folder") {
		return "", core.ErrInvalid
	}
	name := n.child(types, "DisplayName")
	if name == nil || len(name.Children) != 0 || strings.TrimSpace(name.Text) == "" {
		return "", core.ErrInvalid
	}
	expected := 1
	if class := n.child(types, "FolderClass"); mayDeclareClass && class != nil {
		if class.Text != "IPF.Note" || len(class.Children) != 0 {
			return "", core.ErrUnsupported
		}
		expected++
	}
	if len(n.Children) != expected {
		return "", core.ErrUnsupported
	}
	return name.Text, nil
}

func renameFolder(ref node) (node, string, error) {
	id := ref.child(types, "FolderId")
	updates := ref.child(types, "Updates")
	if !isElement(ref, types, "FolderChange") || len(ref.Children) != 2 ||
		id == nil || updates == nil || len(updates.Children) != 1 {
		return node{}, "", core.ErrInvalid
	}
	set := updates.Children[0]
	uri := set.child(types, "FieldURI")
	if !isElement(set, types, "SetFolderField") || len(set.Children) != 2 ||
		uri == nil || uri.attr("FieldURI") != "folder:DisplayName" {
		return node{}, "", core.ErrUnsupported
	}
	name, err := folderName(set.child(types, "Folder"), false)
	return *id, name, err
}

func (h *Handler) writeFolders(ctx context.Context, op node, s core.Session) []node {
	name := op.XMLName.Local
	writer, ok := s.Mailbox().(core.MailFolderHierarchyWriter)
	if !ok {
		return syncError(name, core.ErrUnsupported)
	}
	var list *node
	var parent core.ID
	var err error
	parts := 1
	switch name {
	case "CreateFolder":
		parts++
		list = op.child(messages, "Folders")
		parent, err = h.writeTarget(ctx, s, op.child(messages, "ParentFolderId"))
	case "UpdateFolder":
		list = op.child(messages, "FolderChanges")
	case "MoveFolder":
		parts++
		list = op.child(messages, "FolderIds")
		parent, err = h.writeTarget(ctx, s, op.child(messages, "ToFolderId"))
	case "DeleteFolder":
		list = op.child(messages, "FolderIds")
		switch op.attr("DeleteType") {
		case "MoveToDeletedItems":
			parent, err = h.folderID(ctx, s, at(el(types, "DistinguishedFolderId", ""), "Id", "deleteditems"))
		case "SoftDelete", "HardDelete":
			return []node{failure(name, "ErrorCannotDeleteObject")}
		default:
			return syncError(name, core.ErrInvalid)
		}
	}
	if len(op.Children) != parts || !validBatch(list) {
		return syncError(name, core.ErrInvalid)
	}
	if err != nil {
		return []node{writeFailure(name, err, "ErrorFolderNotFound")}
	}
	scope := s.Identity().Scope
	out := make([]node, 0, len(list.Children))
	for _, input := range list.Children {
		var result core.Object
		if name == "CreateFolder" {
			var displayName string
			displayName, err = folderName(&input, true)
			if err == nil {
				result, err = writer.CreateMailFolder(ctx, core.MailFolderPlacement{Parent: parent, Name: displayName})
			}
			if err != nil && result.ID != "" {
				err = core.ErrOutcomeUnknown
			}
			if errors.Is(err, core.ErrConflict) {
				out = append(out, failure(name, "ErrorFolderExists"))
				continue
			}
		} else {
			result, err = h.changeFolder(ctx, s, writer, name, input, parent)
		}
		switch {
		case name == "DeleteFolder" && errors.Is(err, errDistinguishedFolder):
			out = append(out, failure(name, "ErrorDeleteDistinguishedFolder"))
		case err != nil:
			out = append(out, writeFailure(name, err, "ErrorFolderNotFound"))
		case name == "DeleteFolder":
			out = append(out, success(name))
		default:
			out = append(out, success(name, el(messages, "Folders", "", h.folder(scope, result))))
		}
	}
	return out
}

var errDistinguishedFolder = errors.New("distinguished folder")

func (h *Handler) changeFolder(ctx context.Context, s core.Session, writer core.MailFolderHierarchyWriter, operation string, ref node, destination core.ID) (core.Object, error) {
	var newName string
	if operation == "UpdateFolder" {
		var err error
		ref, newName, err = renameFolder(ref)
		if err != nil {
			return core.Object{}, err
		}
	}
	id, err := h.folderID(ctx, s, ref)
	if err != nil {
		return core.Object{}, err
	}
	before, err := s.Mailbox().Get(ctx, id)
	if err != nil {
		return core.Object{}, err
	}
	if before.Kind != core.Folder {
		return core.Object{}, core.ErrNotFound
	}
	if id == s.Mailbox().Root() || textProperty(before, core.WellKnown) != "" {
		if operation == "DeleteFolder" {
			return core.Object{}, errDistinguishedFolder
		}
		return core.Object{}, core.ErrAccessDenied
	}
	if key := ref.attr("ChangeKey"); key != "" && key != h.changeKey(s.Identity().Scope, before) {
		return core.Object{}, core.ErrConflict
	}
	if operation == "UpdateFolder" {
		destination = before.Parent
	} else {
		newName = string(textProperty(before, core.DisplayName))
	}
	return writer.UpdateMailFolder(ctx, before, core.MailFolderPlacement{Parent: destination, Name: newName})
}
