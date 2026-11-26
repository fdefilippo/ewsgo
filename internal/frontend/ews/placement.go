// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"encoding/xml"
	"errors"
	"strings"

	"ewsgo/internal/core"
)

func writeFailure(op string, err error, missing string) node {
	code := "ErrorInternalServerError"
	switch {
	case errors.Is(err, core.ErrOutcomeUnknown):
	case errors.Is(err, core.ErrNotFound):
		code = missing
	case errors.Is(err, core.ErrConflict):
		code = "ErrorIrresolvableConflict"
	case errors.Is(err, core.ErrAccessDenied), errors.Is(err, core.ErrAuthentication):
		code = "ErrorAccessDenied"
	case errors.Is(err, core.ErrUnsupported):
		code = "ErrorInvalidPropertyForOperation"
	case errors.Is(err, core.ErrInvalid):
		code = "ErrorInvalidRequest"
	case errors.Is(err, core.ErrLimit):
		code = "ErrorDataSizeLimitExceeded"
	}
	return failure(op, code)
}

func isElement(n node, ns, name string) bool {
	return n.XMLName == (xml.Name{Space: ns, Local: name})
}

func validBatch(list *node) bool {
	return list != nil && len(list.Children) > 0 && len(list.Children) <= 100
}

func (h *Handler) writeTarget(ctx context.Context, s core.Session, selector *node) (core.ID, error) {
	if selector == nil || len(selector.Children) != 1 {
		return "", core.ErrInvalid
	}
	return h.folderID(ctx, s, selector.Children[0])
}

func (h *Handler) currentItem(ctx context.Context, s core.Session, ref node) (core.Object, error) {
	if !isElement(ref, types, "ItemId") || len(ref.Children) != 0 {
		return core.Object{}, core.ErrInvalid
	}
	scope := s.Identity().Scope
	id, err := h.decodeRef(scope, ref.attr("Id"))
	if err != nil {
		return core.Object{}, core.ErrNotFound
	}
	item, err := s.Mailbox().Get(ctx, id.ID)
	if err != nil {
		return core.Object{}, err
	}
	if item.Kind != core.Message || item.Parent != id.Parent {
		return core.Object{}, core.ErrNotFound
	}
	if key := ref.attr("ChangeKey"); key != "" && key != h.changeKey(scope, item) {
		return core.Object{}, core.ErrConflict
	}
	return item, nil
}

func (h *Handler) placeItems(ctx context.Context, op node, s core.Session) []node {
	name := op.XMLName.Local
	deleteMode := op.attr("DeleteType")
	soft, hasSoftDelete := s.Mailbox().(core.MailMessageSoftDeleter)
	writer, hasPlacement := s.Mailbox().(core.MailMessagePlacementWriter)
	if name == "DeleteItem" {
		switch deleteMode {
		case "MoveToDeletedItems":
		case "SoftDelete":
			if !hasSoftDelete {
				return []node{failure(name, "ErrorCannotDeleteObject")}
			}
		case "HardDelete":
			return []node{failure(name, "ErrorCannotDeleteObject")}
		default:
			return syncError(name, core.ErrInvalid)
		}
	}
	if !hasPlacement && deleteMode != "SoftDelete" {
		return syncError(name, core.ErrUnsupported)
	}
	ids := op.child(messages, "ItemIds")
	if !validBatch(ids) {
		return syncError(name, core.ErrInvalid)
	}
	returnIDs := true
	var destination core.ID
	if name != "DeleteItem" {
		explicit := op.child(messages, "ReturnNewItemIds")
		expected := 2
		if explicit != nil {
			expected++
			value, err := xmlBool(explicit.Text)
			if err != nil || len(explicit.Children) != 0 {
				return syncError(name, core.ErrInvalid)
			}
			returnIDs = value
		}
		if len(op.Children) != expected {
			return syncError(name, core.ErrInvalid)
		}
		var err error
		destination, err = h.writeTarget(ctx, s, op.child(messages, "ToFolderId"))
		if err != nil {
			return []node{writeFailure(name, err, "ErrorFolderNotFound")}
		}
	} else if len(op.Children) != 1 {
		return syncError(name, core.ErrInvalid)
	}
	scope := s.Identity().Scope
	out := make([]node, 0, len(ids.Children))
	for _, ref := range ids.Children {
		if name == "DeleteItem" && isElement(ref, types, "ItemId") && len(ref.Children) == 0 {
			id, err := h.decodeRef(scope, ref.attr("Id"))
			if err == nil && strings.HasPrefix(string(id.ID), core.AppointmentIDPrefix) {
				cancel := op.attr("SendMeetingCancellations")
				if deleteMode == "MoveToDeletedItems" && (cancel == "" || cancel == "SendToNone") {
					out = append(out, h.trashAppointment(ctx, s, id, ref.attr("ChangeKey")))
				} else {
					out = append(out, failure(name, "ErrorCannotDeleteObject"))
				}
				continue
			}
			if err == nil && strings.HasPrefix(string(id.ID), core.ContactIDPrefix) {
				if deleteMode == "MoveToDeletedItems" {
					out = append(out, h.trashContact(ctx, s, id, ref.attr("ChangeKey")))
				} else {
					out = append(out, failure(name, "ErrorCannotDeleteObject"))
				}
				continue
			}
		}
		item, err := h.currentItem(ctx, s, ref)
		if err != nil {
			out = append(out, writeFailure(name, err, "ErrorItemNotFound"))
			continue
		}
		var result core.Object
		switch name {
		case "MoveItem":
			result, err = writer.MoveMailMessage(ctx, item.ID, item.Revision, destination)
		case "CopyItem":
			result, err = writer.CopyMailMessage(ctx, item.ID, item.Revision, destination)
		case "DeleteItem":
			if deleteMode == "SoftDelete" {
				err = soft.SoftDeleteMailMessage(ctx, item.ID, item.Revision)
				if errors.Is(err, core.ErrUnsupported) {
					out = append(out, failure(name, "ErrorCannotDeleteObject"))
					continue
				}
			} else {
				_, err = writer.TrashMailMessage(ctx, item.ID, item.Revision)
			}
		}
		switch {
		case err != nil:
			out = append(out, writeFailure(name, err, "ErrorItemNotFound"))
		case name == "DeleteItem" || !returnIDs:
			out = append(out, success(name))
		default:
			id := at(at(el(types, "ItemId", ""), "Id", h.itemID(scope, result)), "ChangeKey", h.changeKey(scope, result))
			out = append(out, success(name, el(messages, "Items", "", el(types, "Message", "", id))))
		}
	}
	return out
}

func xmlBool(value string) (bool, error) {
	switch value {
	case "true", "1":
		return true, nil
	case "false", "0":
		return false, nil
	default:
		return false, core.ErrInvalid
	}
}
