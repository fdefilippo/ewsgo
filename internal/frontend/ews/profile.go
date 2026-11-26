// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import "net/http"

const ewsErrors = "http://schemas.microsoft.com/exchange/services/2006/errors"

func requestVersion(root node) (modern bool, code string) {
	modern = true // Headerless callers use the MailCraft profile.
	header := root.child(soap, "Header")
	if header == nil {
		return modern, ""
	}
	seen := false
	for _, entry := range header.Children {
		if entry.XMLName.Space != types || entry.XMLName.Local != "RequestServerVersion" {
			continue
		}
		if seen {
			return false, "ErrorInvalidRequest"
		}
		seen = true
		switch entry.attr("Version") {
		case "Exchange2010_SP2":
			modern = false
		case "Exchange2013", "Exchange2013_SP1":
			modern = true
		default:
			return false, "ErrorInvalidServerVersion"
		}
	}
	return modern, ""
}

func replyFault(w http.ResponseWriter, code string) {
	fault := at(el(soap, "Fault", "",
		at(el("", "faultcode", "s:Client"), "xmlns", ""),
		at(el("", "faultstring", code), "xmlns", ""),
		at(el("", "detail", "", el(ewsErrors, "ResponseCode", code)), "xmlns", "")), "xmlns:s", soap)
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	w.WriteHeader(http.StatusInternalServerError)
	_, _ = w.Write(envelope(fault))
}

type responseShape struct {
	idOnly, modern, mime bool
	extra                map[string]bool
	extended             []node
}

var folderFields = map[string]string{
	"folder:FolderId": "FolderId", "folder:ParentFolderId": "ParentFolderId",
	"folder:DisplayName": "DisplayName", "folder:FolderClass": "FolderClass",
	"folder:TotalCount": "TotalCount", "folder:UnreadCount": "UnreadCount",
}

var itemFields = map[string]string{
	"item:ItemId": "ItemId", "item:ParentFolderId": "ParentFolderId", "item:ItemClass": "ItemClass",
	"item:Subject": "Subject", "item:DateTimeReceived": "DateTimeReceived", "item:Size": "Size",
	"item:Categories": "Categories", "item:Importance": "Importance", "item:HasAttachments": "HasAttachments",
	"item:Attachments": "Attachments",
	"item:IsDraft":     "IsDraft", "item:ConversationId": "ConversationId", "item:Flag": "Flag", "item:Preview": "Preview",
	"message:From": "From", "message:ToRecipients": "ToRecipients", "message:CcRecipients": "CcRecipients",
	"message:BccRecipients": "BccRecipients", "message:InternetMessageId": "InternetMessageId", "message:IsRead": "IsRead",
}

func readShape(op node, name string, fields map[string]string, modern bool) (responseShape, string) {
	shape := responseShape{modern: modern}
	spec := op.child(messages, name)
	if spec == nil {
		return shape, ""
	}
	seen := make(map[string]bool, len(spec.Children))
	for _, child := range spec.Children {
		key := child.XMLName.Local
		if child.XMLName.Space != types || seen[key] {
			return shape, "ErrorInvalidRequest"
		}
		seen[key] = true
		switch key {
		case "BaseShape":
			switch child.Text {
			case "IdOnly":
				shape.idOnly = true
			case "Default", "AllProperties":
			default:
				return shape, "ErrorInvalidRequest"
			}
		case "IncludeMimeContent":
			if name != "ItemShape" || child.Text != "true" && child.Text != "false" && child.Text != "1" && child.Text != "0" {
				return shape, "ErrorInvalidRequest"
			}
			shape.mime = child.Text == "true" || child.Text == "1"
		case "AdditionalProperties":
			shape.extra = make(map[string]bool, len(child.Children))
			for _, field := range child.Children {
				if isElement(field, types, "ExtendedFieldURI") && op.XMLName.Local == "GetItem" {
					if _, err := namedStringKey(field); err != nil || len(shape.extended) == 32 {
						return shape, "ErrorInvalidPropertyRequest"
					}
					shape.extended = append(shape.extended, field)
					continue
				}
				property := fields[field.attr("FieldURI")]
				if property == "" && (op.XMLName.Local == "GetItem" || op.XMLName.Local == "FindItem") {
					property = contactFields[field.attr("FieldURI")]
					if property == "" {
						property = appointmentFields[field.attr("FieldURI")]
					}
				}
				if field.XMLName.Space != types || field.XMLName.Local != "FieldURI" || property == "" || !modern && modernProperty(property) {
					return shape, "ErrorInvalidPropertyRequest"
				}
				if property == "Attachments" && op.XMLName.Local != "GetItem" {
					return shape, "ErrorInvalidPropertyForOperation"
				}
				shape.extra[property] = true
			}
		default:
			return shape, "ErrorInvalidPropertyRequest"
		}
	}
	if shape.mime && op.XMLName.Local != "GetItem" {
		return shape, "ErrorInvalidPropertyForOperation"
	}
	return shape, ""
}

func modernProperty(name string) bool { return name == "Preview" || name == "Flag" }

func (s responseShape) apply(object node) node {
	children := object.Children[:0]
	for _, child := range object.Children {
		name := child.XMLName.Local
		if !s.modern && modernProperty(name) {
			continue
		}
		if !s.idOnly || s.extra[name] || name == "ItemId" || name == "FolderId" {
			children = append(children, child)
		}
	}
	object.Children = children
	return object
}
