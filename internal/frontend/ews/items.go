// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
)

func textProperty(o core.Object, key core.Key) core.Text {
	value, _ := o.Properties[key].(core.Text)
	return value
}
func (h *Handler) itemID(scope core.Scope, o core.Object) string {
	return h.encodeRef(reference{Scope: scope, ID: o.ID, Parent: o.Parent})
}
func (h *Handler) changeKey(scope core.Scope, o core.Object) string {
	revision := strconv.FormatUint(uint64(o.Revision), 10)
	if content, ok := o.Properties[core.ContentRevision].(core.Integer); ok {
		revision += "/" + strconv.FormatInt(int64(content), 10)
	}
	return h.encodeRef(reference{Scope: scope, ID: "revision:" + o.ID, Parent: core.ID(revision)})
}

func mailboxAddress(name, address string) node {
	var children []node
	if name != "" {
		children = append(children, el(types, "Name", name))
	}
	return el(types, "Mailbox", "", append(children, el(types, "EmailAddress", address), el(types, "RoutingType", "SMTP"))...)
}

func (h *Handler) message(scope core.Scope, o core.Object) node {
	children := []node{at(at(el(types, "ItemId", ""), "Id", h.itemID(scope, o)), "ChangeKey", h.changeKey(scope, o))}
	if o.Parent != "" {
		children = append(children, at(el(types, "ParentFolderId", ""), "Id", h.encode(scope, o.Parent)))
	}
	elementName, itemClass, response := meetingMessageShape(o)
	children = append(children, el(types, "ItemClass", itemClass))
	if subject, ok := o.Properties[core.Subject].(core.Text); ok {
		children = append(children, el(types, "Subject", string(subject)))
	}
	if received, ok := o.Properties[core.ReceivedAt].(core.Timestamp); ok && !time.Time(received).IsZero() {
		children = append(children, el(types, "DateTimeReceived", time.Time(received).UTC().Format(time.RFC3339Nano)))
	}
	if size, ok := o.Properties[core.MessageSize].(core.Integer); ok {
		children = append(children, el(types, "Size", strconv.FormatInt(int64(size), 10)))
	}
	if categories, ok := o.Properties[core.MailCategories].(core.Strings); ok {
		values := make([]node, 0, len(categories))
		for _, category := range categories {
			values = append(values, el(types, "String", category))
		}
		children = append(children, el(types, "Categories", "", values...))
	}
	if importance, ok := o.Properties[core.MessageImportance].(core.Integer); ok && importance >= 0 && importance <= 2 {
		children = append(children, el(types, "Importance", [3]string{"Low", "Normal", "High"}[importance]))
	}
	if draft, ok := o.Properties[core.IsDraft].(core.Bool); ok {
		children = append(children, el(types, "IsDraft", strconv.FormatBool(bool(draft))))
	}
	if attached, ok := o.Properties[core.HasAttachments].(core.Bool); ok {
		children = append(children, el(types, "HasAttachments", strconv.FormatBool(bool(attached))))
	}
	if conversation := textProperty(o, core.ConversationID); conversation != "" {
		children = append(children, at(el(types, "ConversationId", ""), "Id", h.encode(scope, core.ID("conversation:"+string(conversation)))))
	}
	if _, ok := o.Properties[core.IsFlagged].(core.Bool); ok {
		flag := core.MailFollowUpOf(o.Properties)
		status := [...]string{"NotFlagged", "Flagged", "Complete"}[flag.Status]
		fields := []node{el(types, "FlagStatus", status)}
		for _, date := range []struct {
			name  string
			value time.Time
		}{{"StartDate", flag.Start}, {"DueDate", flag.Due}, {"CompleteDate", flag.Complete}} {
			if !date.value.IsZero() {
				fields = append(fields, el(types, date.name, date.value.UTC().Format(time.RFC3339Nano)))
			}
		}
		children = append(children, el(types, "Flag", "", fields...))
	}
	if preview, ok := o.Properties[core.Preview].(core.Text); ok {
		children = append(children, el(types, "Preview", string(preview)))
	}
	for _, role := range []struct {
		kind core.RecipientKind
		name string
	}{
		{core.RecipientTo, "ToRecipients"}, {core.RecipientCc, "CcRecipients"}, {core.RecipientBcc, "BccRecipients"},
	} {
		var recipients []node
		for _, recipient := range o.Recipients {
			if recipient.Kind == role.kind {
				recipients = append(recipients, mailboxAddress(recipient.DisplayName, recipient.Address))
			}
		}
		if len(recipients) != 0 {
			children = append(children, el(types, role.name, "", recipients...))
		}
	}
	if address := textProperty(o, core.FromAddress); address != "" {
		children = append(children, el(types, "From", "", mailboxAddress("", string(address))))
	}
	if internetID := textProperty(o, core.MessageID); internetID != "" {
		children = append(children, el(types, "InternetMessageId", string(internetID)))
	}
	if read, ok := o.Properties[core.IsRead].(core.Bool); ok {
		children = append(children, el(types, "IsRead", strconv.FormatBool(bool(read))))
	}
	if response != "" {
		children = append(children, el(types, "ResponseType", response))
	}
	if uid := textProperty(o, core.MeetingMessageUID); uid != "" && elementName != "Message" {
		children = append(children, el(types, "UID", string(uid)))
	}
	return el(types, elementName, "", children...)
}

func meetingMessageShape(o core.Object) (elementName, itemClass, response string) {
	kind, _ := o.Properties[core.MeetingMessageType].(core.Integer)
	switch core.MeetingMessageKind(kind) {
	case core.MeetingRequestMessage:
		return "MeetingRequest", "IPM.Schedule.Meeting.Request", ""
	case core.MeetingCancellationMessage:
		return "MeetingCancellation", "IPM.Schedule.Meeting.Canceled", ""
	case core.MeetingAcceptedMessage:
		return "MeetingResponse", "IPM.Schedule.Meeting.Resp.Pos", "Accept"
	case core.MeetingTentativeMessage:
		return "MeetingResponse", "IPM.Schedule.Meeting.Resp.Tent", "Tentative"
	case core.MeetingDeclinedMessage:
		return "MeetingResponse", "IPM.Schedule.Meeting.Resp.Neg", "Decline"
	default:
		return "Message", "IPM.Note", ""
	}
}

func itemError(err error) node {
	switch {
	case errors.Is(err, core.ErrNotFound):
		return failure("GetItem", "ErrorItemNotFound")
	case errors.Is(err, core.ErrAccessDenied):
		return failure("GetItem", "ErrorAccessDenied")
	case errors.Is(err, core.ErrUnsupported):
		return failure("GetItem", "ErrorInvalidPropertyForOperation")
	default:
		return failure("GetItem", "ErrorInternalServerError")
	}
}

func (h *Handler) getItems(r *http.Request, op node, s core.Session, shape responseShape) []node {
	ids := op.child(messages, "ItemIds")
	if ids == nil || len(ids.Children) < 1 || len(ids.Children) > 100 {
		return syncError("GetItem", core.ErrInvalid)
	}
	box := s.Mailbox()
	reader, canReadMIME := box.(core.MIMEReader)
	remaining := 32 << 20 // Raw bytes, before base64.
	var out []node
	for _, id := range ids.Children {
		if isElement(id, types, "RecurringMasterItemId") {
			out = append(out, h.getRecurringMaster(r.Context(), s, id, shape))
			continue
		}
		if id.XMLName.Space != types || id.XMLName.Local != "ItemId" {
			out = append(out, failure("GetItem", "ErrorInvalidRequest"))
			continue
		}
		ref, err := h.decodeRef(s.Identity().Scope, id.attr("Id"))
		if err != nil {
			out = append(out, failure("GetItem", "ErrorItemNotFound"))
			continue
		}
		if strings.HasPrefix(string(ref.ID), core.ContactIDPrefix) {
			if shape.appointmentOnly() {
				out = append(out, itemError(core.ErrUnsupported))
				continue
			}
			out = append(out, h.getContact(r.Context(), s, ref, shape))
			continue
		}
		if strings.HasPrefix(string(ref.ID), core.AppointmentIDPrefix) {
			out = append(out, h.getAppointment(r.Context(), s, ref, shape))
			continue
		}
		if shape.contactOnly() || shape.appointmentOnly() {
			out = append(out, itemError(core.ErrUnsupported))
			continue
		}
		object, err := box.Get(r.Context(), ref.ID)
		if err != nil {
			out = append(out, itemError(err))
			continue
		}
		if object.Kind != core.Message || ref.Parent != object.Parent {
			out = append(out, failure("GetItem", "ErrorItemNotFound"))
			continue
		}
		message := shape.apply(h.message(s.Identity().Scope, object))
		isDraft := object.Properties[core.IsDraft] == core.Bool(true)
		includeFiles := shape.extra["Attachments"]
		if includeFiles && !isDraft && object.Properties[core.HasAttachments] != core.Bool(false) {
			attachments, err := h.nativeAttachmentList(r.Context(), s, object)
			if err != nil {
				out = append(out, itemError(err))
				continue
			}
			insertAttachmentList(&message, attachments)
		}
		if shape.mime || isDraft && (len(shape.extended) != 0 || includeFiles) {
			if !canReadMIME {
				out = append(out, itemError(core.ErrUnsupported))
				continue
			}
			if size, ok := object.Properties[core.MessageSize].(core.Integer); remaining == 0 || ok && int64(size) > int64(remaining) {
				out = append(out, failure("GetItem", "ErrorDataSizeLimitExceeded"))
				continue
			}
			raw, err := reader.ReadMIME(r.Context(), ref.ID)
			if err != nil {
				out = append(out, itemError(err))
				continue
			}
			if len(raw) > remaining {
				out = append(out, failure("GetItem", "ErrorDataSizeLimitExceeded"))
				continue
			}
			remaining -= len(raw)
			var extended []node
			if isDraft {
				if includeFiles {
					attachments, err := h.draftAttachmentList(s.Identity().Scope, object, raw)
					if err != nil {
						out = append(out, itemError(err))
						continue
					}
					insertAttachmentList(&message, attachments)
				}
				raw, extended, err = h.readDraftProperties(r.Context(), s.Identity().Scope, raw, shape)
				if err != nil {
					out = append(out, itemError(err))
					continue
				}
			}
			message.Children = append(message.Children, extended...)
			if shape.mime {
				mime := el(types, "MimeContent", base64.StdEncoding.EncodeToString(raw))
				message.Children = append([]node{mime}, message.Children...)
			}
		}
		out = append(out, success("GetItem", el(messages, "Items", "", message)))
	}
	return out
}
func (h *Handler) updateItems(r *http.Request, op node, s core.Session) []node {
	_, canWriteDrafts := s.Mailbox().(core.MailDraftWriter)
	_, canWriteMarks := s.Mailbox().(core.MailMarksWriter)
	if canWriteDrafts || canWriteMarks {
		return h.updateMailItems(r.Context(), op, s)
	}
	if op.attr("MessageDisposition") != "SaveOnly" {
		return syncError("UpdateItem", core.ErrUnsupported)
	}
	writer, ok := s.Mailbox().(core.ReadStateWriter)
	if !ok {
		return syncError("UpdateItem", core.ErrUnsupported)
	}
	if resolution := op.attr("ConflictResolution"); resolution != "AutoResolve" && resolution != "AlwaysOverwrite" {
		return syncError("UpdateItem", core.ErrUnsupported)
	}
	changes := op.child(messages, "ItemChanges")
	if changes == nil || len(changes.Children) == 0 || len(changes.Children) > 100 {
		return syncError("UpdateItem", core.ErrInvalid)
	}
	var out []node
	for _, change := range changes.Children {
		id := change.child(types, "ItemId")
		updates := change.child(types, "Updates")
		if id == nil || updates == nil || len(updates.Children) != 1 {
			out = append(out, failure("UpdateItem", "ErrorInvalidRequest"))
			continue
		}
		field := updates.Children[0]
		uri := field.child(types, "FieldURI")
		message := field.child(types, "Message")
		var value *node
		if message != nil {
			value = message.child(types, "IsRead")
		}
		if change.XMLName.Space != types || change.XMLName.Local != "ItemChange" || len(change.Children) != 2 || field.XMLName.Space != types || field.XMLName.Local != "SetItemField" || len(field.Children) != 2 || message == nil || len(message.Children) != 1 || uri == nil || uri.attr("FieldURI") != "message:IsRead" || value == nil || (value.Text != "true" && value.Text != "false") {
			out = append(out, failure("UpdateItem", "ErrorInvalidPropertyForOperation"))
			continue
		}
		ref, err := h.decodeRef(s.Identity().Scope, id.attr("Id"))
		if err != nil {
			out = append(out, failure("UpdateItem", "ErrorItemNotFound"))
			continue
		}
		current, err := s.Mailbox().Get(r.Context(), ref.ID)
		if err != nil || current.Kind != core.Message || current.Parent != ref.Parent {
			out = append(out, failure("UpdateItem", "ErrorItemNotFound"))
			continue
		}
		saved, err := writer.SetRead(r.Context(), ref.ID, value.Text == "true")
		if err != nil {
			out = append(out, failure("UpdateItem", "ErrorInternalServerError"))
			continue
		}
		out = append(out, success("UpdateItem", el(messages, "Items", "", h.message(s.Identity().Scope, saved))))
	}
	return out
}
