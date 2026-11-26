// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"errors"
	"strings"
	"time"
	"unicode"

	"ewsgo/internal/core"
	"ewsgo/internal/mailmime"
)

type mailUpdates struct {
	fields []node
	marks  core.MailMarks
}

func setMailMarks(marks *core.MailMarks, name string, field node) error {
	switch name {
	case "Importance":
		if len(field.Children) != 0 {
			return core.ErrInvalid
		}
		var value core.Integer
		switch field.Text {
		case "Low":
			value = 0
		case "Normal":
			value = 1
		case "High":
			value = 2
		default:
			return core.ErrInvalid
		}
		marks.Importance = &value
	case "Flag":
		flag, err := readFollowUp(field)
		if err != nil {
			return err
		}
		marks.FollowUp = &flag
	case "IsRead":
		value, err := xmlBool(field.Text)
		if err != nil || len(field.Children) != 0 {
			return core.ErrInvalid
		}
		marks.Read = &value
	case "Categories":
		if len(field.Children) > 50 {
			return core.ErrLimit
		}
		marks.Categories = make(core.Strings, 0, len(field.Children))
		seen := make(map[string]bool)
		for _, child := range field.Children {
			name := strings.ToLower(child.Text)
			if !isElement(child, types, "String") || len(child.Children) != 0 || child.Text == "" || len(child.Text) > 128 ||
				strings.TrimSpace(child.Text) != child.Text || strings.ContainsFunc(child.Text, unicode.IsControl) || seen[name] {
				return core.ErrInvalid
			}
			seen[name] = true
			marks.Categories = append(marks.Categories, child.Text)
		}
	default:
		return core.ErrUnsupported
	}
	return nil
}

func readFollowUp(field node) (core.MailFollowUp, error) {
	var flag core.MailFollowUp
	status := field.child(types, "FlagStatus")
	if status == nil || len(status.Children) != 0 {
		return flag, core.ErrInvalid
	}
	switch status.Text {
	case "NotFlagged":
	case "Flagged":
		flag.Status = core.FollowUpMarked
	case "Complete":
		flag.Status = core.FollowUpComplete
	default:
		return flag, core.ErrInvalid
	}
	seen := make(map[string]bool)
	for _, child := range field.Children {
		name := child.XMLName.Local
		if child.XMLName.Space != types || len(child.Children) != 0 || seen[name] {
			return flag, core.ErrInvalid
		}
		seen[name] = true
		if name == "FlagStatus" {
			continue
		}
		date, err := time.Parse(time.RFC3339Nano, child.Text)
		if err != nil || date.Year() < 1 || date.IsZero() {
			return flag, core.ErrInvalid
		}
		switch name {
		case "StartDate":
			flag.Start = date.UTC()
		case "DueDate":
			flag.Due = date.UTC()
		case "CompleteDate":
			flag.Complete = date.UTC()
		default:
			return flag, core.ErrInvalid
		}
	}
	if !flag.Valid() {
		return flag, core.ErrInvalid
	}
	return flag, nil
}

var mailUpdateFields = map[string]string{
	"item:Subject": "Subject", "item:Body": "Body", "item:Importance": "Importance",
	"item:Flag": "Flag", "item:Categories": "Categories", "message:IsRead": "IsRead",
	"message:ToRecipients": "ToRecipients", "message:CcRecipients": "CcRecipients",
	"message:BccRecipients": "BccRecipients", "message:ReplyTo": "ReplyTo",
	"message:IsReadReceiptRequested": "IsReadReceiptRequested",
}

func parseMailUpdates(updates node) (mailUpdates, error) {
	var result mailUpdates
	seen := make(map[string]bool)
	for _, update := range updates.Children {
		uri := update.child(types, "FieldURI")
		if uri == nil || len(uri.Children) != 0 || update.XMLName.Space != types {
			return mailUpdates{}, core.ErrInvalid
		}
		name := mailUpdateFields[uri.attr("FieldURI")]
		if name == "" || seen[name] {
			return mailUpdates{}, core.ErrUnsupported
		}
		seen[name] = true
		var field *node
		switch update.XMLName.Local {
		case "DeleteItemField":
			if len(update.Children) != 1 {
				return mailUpdates{}, core.ErrInvalid
			}
			switch name {
			case "ToRecipients", "CcRecipients", "BccRecipients", "ReplyTo", "Categories":
				empty := el(types, name, "")
				field = &empty
			case "Flag":
				clear := el(types, "Flag", "", el(types, "FlagStatus", "NotFlagged"))
				field = &clear
			default:
				return mailUpdates{}, core.ErrUnsupported
			}
		case "SetItemField":
			message := update.child(types, "Message")
			if len(update.Children) != 2 || message == nil || len(message.Children) != 1 {
				return mailUpdates{}, core.ErrInvalid
			}
			field = message.child(types, name)
		default:
			return mailUpdates{}, core.ErrUnsupported
		}
		if field == nil {
			return mailUpdates{}, core.ErrInvalid
		}
		switch name {
		case "Importance", "Flag", "Categories", "IsRead":
			if err := setMailMarks(&result.marks, name, *field); err != nil {
				return mailUpdates{}, err
			}
		default:
			result.fields = append(result.fields, *field)
		}
	}
	return result, nil
}

func (h *Handler) updateMailItems(ctx context.Context, op node, session core.Session) []node {
	const operation = "UpdateItem"
	resolution := op.attr("ConflictResolution")
	disposition := op.attr("MessageDisposition")
	if disposition != "SaveOnly" && disposition != "" {
		return syncError(operation, core.ErrUnsupported)
	}
	switch resolution {
	case "":
		resolution = "AutoResolve"
	case "AutoResolve", "AlwaysOverwrite", "NeverOverwrite":
	default:
		return syncError(operation, core.ErrInvalid)
	}
	batch := op.child(messages, "ItemChanges")
	if len(op.Children) != 1 || !validBatch(batch) {
		return syncError(operation, core.ErrInvalid)
	}
	out := make([]node, 0, len(batch.Children))
	scope := session.Identity().Scope
	for _, change := range batch.Children {
		id := change.child(types, "ItemId")
		updates := change.child(types, "Updates")
		if !isElement(change, types, "ItemChange") || len(change.Children) != 2 || id == nil || !validBatch(updates) {
			out = append(out, failure(operation, "ErrorInvalidRequest"))
			continue
		}
		overwrite := resolution == "AlwaysOverwrite"
		key := id.attr("ChangeKey")
		ref, refErr := h.decodeRef(scope, id.attr("Id"))
		if key == "" {
			if !overwrite {
				out = append(out, failure(operation, "ErrorChangeKeyRequiredForWriteOperations"))
				continue
			}
		}
		if refErr == nil && strings.HasPrefix(string(ref.ID), core.AppointmentIDPrefix) {
			switch op.attr("SendMeetingInvitationsOrCancellations") {
			case "SendToNone":
				out = append(out, h.updateAppointment(ctx, session, ref, key, *updates, overwrite))
			case "SendToAllAndSaveCopy":
				out = append(out, h.updateMeeting(ctx, session, ref, key, *updates, overwrite))
			default:
				out = append(out, writeFailure(operation, core.ErrUnsupported, "ErrorItemNotFound"))
			}
			continue
		}
		if key != "" && overwrite {
			revision, keyErr := h.decodeRef(scope, key)
			if refErr != nil || keyErr != nil || revision.ID != "revision:"+ref.ID {
				out = append(out, failure(operation, "ErrorInvalidChangeKey"))
				continue
			}
		}
		if refErr == nil && strings.HasPrefix(string(ref.ID), core.ContactIDPrefix) {
			out = append(out, h.updateContact(ctx, session, ref, key, *updates, overwrite))
			continue
		}
		if disposition == "" {
			out = append(out, writeFailure(operation, core.ErrUnsupported, "ErrorItemNotFound"))
			continue
		}
		patch, err := parseMailUpdates(*updates)
		if err != nil {
			out = append(out, writeFailure(operation, err, "ErrorItemNotFound"))
			continue
		}
		itemRef := *id
		if overwrite {
			itemRef = at(el(types, "ItemId", ""), "Id", id.attr("Id"))
		}
		var saved core.Object
		for attempt := range 2 {
			current, getErr := h.currentItem(ctx, session, itemRef)
			if getErr != nil {
				err = getErr
				break
			}
			saved, err = applyMailUpdates(ctx, session.Mailbox(), current, patch)
			if !errors.Is(err, core.ErrConflict) || !overwrite || attempt == 1 {
				break
			}
		}
		if err != nil {
			out = append(out, writeFailure(operation, err, "ErrorItemNotFound"))
			continue
		}
		out = append(out, success(operation, el(messages, "Items", "", h.message(scope, saved))))
	}
	return out
}

func applyMailUpdates(ctx context.Context, box core.Mailbox, item core.Object, patch mailUpdates) (core.Object, error) {
	draft := item.Properties[core.IsDraft] == core.Bool(true)
	if draft && patch.marks.Importance != nil {
		if item.Properties[core.MessageImportance] != *patch.marks.Importance {
			names := [...]string{"Low", "Normal", "High"}
			patch.fields = append(patch.fields, el(types, "Importance", names[*patch.marks.Importance]))
		}
		patch.marks.Importance = nil
	}
	var marks core.MailMarksWriter
	if !patch.marks.Empty() {
		var ok bool
		marks, ok = box.(core.MailMarksWriter)
		if !ok {
			return core.Object{}, core.ErrUnsupported
		}
	}
	draftChanged := len(patch.fields) != 0
	if draftChanged {
		drafts, canSave := box.(core.MailDraftWriter)
		reader, canRead := box.(core.MIMEReader)
		if !canSave || !canRead || !draft {
			return core.Object{}, core.ErrUnsupported
		}
		mime, err := reader.ReadMIME(ctx, item.ID)
		if err != nil {
			return core.Object{}, err
		}
		doc, err := mailmime.ReadDraft(mime)
		if err != nil {
			return core.Object{}, err
		}
		for _, field := range patch.fields {
			if err := setDraftField(&doc, field); err != nil {
				return core.Object{}, err
			}
		}
		mime, err = doc.Bytes()
		if err != nil {
			return core.Object{}, err
		}
		item, err = drafts.SaveMailDraft(ctx, item.ID, item.Revision, mime)
		if err != nil {
			return core.Object{}, err
		}
	}
	if marks == nil {
		return item, nil
	}
	saved, err := marks.SetMailMarks(ctx, item.ID, item.Revision, patch.marks)
	if err != nil && draftChanged {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	return saved, err
}
