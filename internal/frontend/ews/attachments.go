// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"

	"ewsgo/internal/core"
	"ewsgo/internal/mailmime"
)

const attachmentPrefix = "attachment:"

func (h *Handler) fileAttachment(scope core.Scope, root core.Object, file draftFile, includeRoot bool) node {
	token := h.encodeRef(reference{Scope: scope, ID: core.ID(attachmentPrefix + file.key), Parent: core.ID(h.itemID(scope, root))})
	id := at(el(types, "AttachmentId", ""), "Id", token)
	if includeRoot {
		id = at(at(id, "RootItemId", h.itemID(scope, root)), "RootItemChangeKey", h.changeKey(scope, root))
	}
	fields := []node{id, el(types, "Name", file.name)}
	if file.mediaType != "" {
		fields = append(fields, el(types, "ContentType", file.mediaType))
	}
	if file.contentID != "" {
		fields = append(fields, el(types, "ContentId", file.contentID))
	}
	if file.size >= 0 {
		fields = append(fields, el(types, "Size", strconv.FormatInt(file.size, 10)))
	}
	fields = append(fields, el(types, "IsInline", strconv.FormatBool(file.inline)))
	return el(types, "FileAttachment", "", fields...)
}

func (h *Handler) loadAttachmentDraft(ctx context.Context, s core.Session, selector node) (core.Object, draftMIME, core.MailDraftWriter, error) {
	box := s.Mailbox()
	writer, canSave := box.(core.MailDraftWriter)
	reader, canRead := box.(core.MIMEReader)
	if !canSave || !canRead {
		return core.Object{}, draftMIME{}, nil, core.ErrUnsupported
	}
	item, err := h.currentItem(ctx, s, selector)
	if err != nil {
		return core.Object{}, draftMIME{}, nil, err
	}
	if item.Properties[core.IsDraft] != core.Bool(true) {
		return core.Object{}, draftMIME{}, nil, core.ErrUnsupported
	}
	raw, err := reader.ReadMIME(ctx, item.ID)
	if err != nil {
		return core.Object{}, draftMIME{}, nil, err
	}
	doc, err := mailmime.ReadDraft(raw)
	return item, doc, writer, err
}

func (h *Handler) createAttachments(ctx context.Context, op node, s core.Session) []node {
	const operation = "CreateAttachment"
	root := op.child(messages, "ParentItemId")
	list := op.child(messages, "Attachments")
	if root == nil || len(root.Children) != 0 || !validBatch(list) || len(op.Children) != 2 {
		return syncError(operation, core.ErrInvalid)
	}
	parts, err := readFileAttachments(list)
	if err != nil {
		return []node{writeFailure(operation, err, "ErrorItemNotFound")}
	}
	selector := at(at(el(types, "ItemId", ""), "Id", root.attr("Id")), "ChangeKey", root.attr("ChangeKey"))
	item, doc, writer, err := h.loadAttachmentDraft(ctx, s, selector)
	if err != nil {
		return []node{writeFailure(operation, err, "ErrorItemNotFound")}
	}
	if err := doc.AppendFiles(parts); err != nil {
		return []node{writeFailure(operation, err, "ErrorItemNotFound")}
	}
	_, files, err := draftAttachmentTree(doc, 0)
	if err != nil {
		return []node{writeFailure(operation, err, "ErrorItemNotFound")}
	}
	if len(files) < len(parts) {
		return syncError(operation, core.ErrInvalid)
	}
	added := files[len(files)-len(parts):]
	for _, file := range added {
		if strings.Contains(file.key, ":") {
			return syncError(operation, core.ErrInvalid)
		}
	}
	saved, err := saveDraftAttachmentChange(ctx, writer, item, doc)
	if err != nil {
		return []node{writeFailure(operation, err, "ErrorItemNotFound")}
	}
	scope := s.Identity().Scope
	out := make([]node, 0, len(added))
	for _, file := range added {
		out = append(out, h.fileAttachment(scope, saved, *file, true))
	}
	return []node{success(operation, el(messages, "Attachments", "", out...))}
}

func (h *Handler) deleteAttachments(ctx context.Context, op node, s core.Session) []node {
	const operation = "DeleteAttachment"
	list := op.child(messages, "AttachmentIds")
	if !validBatch(list) || len(op.Children) != 1 {
		return syncError(operation, core.ErrInvalid)
	}
	// One native revision check and one SaveDraft for each root item.
	out := make([]node, len(list.Children))
	groupIndex := make(map[core.ID]int)
	type attachmentRequest struct {
		position int
		key      string
	}
	type rootGroup struct {
		root     core.ID
		requests []attachmentRequest
	}
	var groups []rootGroup
	scope := s.Identity().Scope
	for i, field := range list.Children {
		ref, err := h.decodeRef(scope, field.attr("Id"))
		if !isElement(field, types, "AttachmentId") || len(field.Children) != 0 || err != nil ||
			ref.Parent == "" || !strings.HasPrefix(string(ref.ID), attachmentPrefix) {
			out[i] = failure(operation, "ErrorInvalidAttachmentId")
			continue
		}
		index, seen := groupIndex[ref.Parent]
		if !seen {
			index = len(groups)
			groupIndex[ref.Parent] = index
			groups = append(groups, rootGroup{root: ref.Parent})
		}
		groups[index].requests = append(groups[index].requests, attachmentRequest{i, strings.TrimPrefix(string(ref.ID), attachmentPrefix)})
	}
	for _, group := range groups {
		item, doc, writer, err := h.loadAttachmentDraft(ctx, s, at(el(types, "ItemId", ""), "Id", string(group.root)))
		if err != nil {
			for _, req := range group.requests {
				if errors.Is(err, core.ErrUnsupported) {
					out[req.position] = failure(operation, "ErrorInvalidAttachmentId")
				} else {
					out[req.position] = writeFailure(operation, err, "ErrorInvalidAttachmentId")
				}
			}
			continue
		}
		rev, _ := item.Properties[core.ContentRevision].(core.Integer)
		tree, files, err := draftAttachmentTree(doc, rev)
		if err != nil {
			for _, req := range group.requests {
				out[req.position] = writeFailure(operation, err, "ErrorInvalidAttachmentId")
			}
			continue
		}
		existing := make(map[string]bool, len(files))
		for _, file := range files {
			existing[file.key] = true
		}
		remove := make(map[string]bool)
		var pending []int
		for _, req := range group.requests {
			if !existing[req.key] || remove[req.key] {
				out[req.position] = failure(operation, "ErrorInvalidAttachmentId")
				continue
			}
			remove[req.key] = true
			pending = append(pending, req.position)
		}
		if len(pending) == 0 {
			continue
		}
		tree.removeFiles(remove)
		doc, err = tree.rebuild()
		saved := core.Object{}
		if err == nil {
			saved, err = saveDraftAttachmentChange(ctx, writer, item, doc)
		}
		for _, i := range pending {
			if err != nil {
				out[i] = writeFailure(operation, err, "ErrorInvalidAttachmentId")
			} else {
				root := at(at(el(messages, "RootItemId", ""), "RootItemId", h.itemID(scope, saved)),
					"RootItemChangeKey", h.changeKey(scope, saved))
				out[i] = success(operation, root)
			}
		}
	}
	return out
}

func saveDraftAttachmentChange(ctx context.Context, writer core.MailDraftWriter, item core.Object, doc draftMIME) (core.Object, error) {
	raw, err := doc.Bytes()
	if err != nil {
		return core.Object{}, err
	}
	return writer.SaveMailDraft(ctx, item.ID, item.Revision, raw)
}

func insertAttachmentList(message *node, attachments node) {
	if len(attachments.Children) == 0 {
		return
	}
	at := 0
	for at < len(message.Children) {
		switch message.Children[at].XMLName.Local {
		case "MimeContent", "ItemId", "ParentFolderId", "ItemClass", "Subject", "Sensitivity", "Body":
			at++
		default:
			message.Children = slices.Insert(message.Children, at, attachments)
			return
		}
	}
	message.Children = append(message.Children, attachments)
}

func (h *Handler) draftAttachmentList(scope core.Scope, item core.Object, raw []byte) (node, error) {
	doc, err := mailmime.ReadDraft(raw)
	if err != nil {
		return node{}, err
	}
	revision, _ := item.Properties[core.ContentRevision].(core.Integer)
	_, files, err := draftAttachmentTree(doc, revision)
	if err != nil {
		return node{}, err
	}
	children := make([]node, 0, len(files))
	for _, file := range files {
		children = append(children, h.fileAttachment(scope, item, *file, false))
	}
	return el(types, "Attachments", "", children...), nil
}

func (h *Handler) nativeAttachmentList(ctx context.Context, s core.Session, item core.Object) (node, error) {
	reader, ok := s.Mailbox().(core.MailAttachmentReader)
	if !ok {
		return node{}, core.ErrUnsupported
	}
	files, err := reader.ListMailAttachments(ctx, item.ID)
	if err != nil {
		return node{}, err
	}
	children := make([]node, 0, len(files))
	for _, file := range files {
		children = append(children, h.fileAttachment(s.Identity().Scope, item, draftFile{
			key: "native:" + file.ID, name: file.Name, mediaType: file.MediaType, contentID: file.ContentID,
			size: -1, inline: file.Inline,
		}, false))
	}
	return el(types, "Attachments", "", children...), nil
}
