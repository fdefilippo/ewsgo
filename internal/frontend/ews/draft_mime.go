// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"mime"
	"strings"

	"ewsgo/internal/core"
	"ewsgo/internal/mailmime"
)

type draftMIME = mailmime.Draft

func draftRecipients(list node) (string, error) {
	if len(list.Children) > 100 {
		return "", core.ErrLimit
	}
	addresses := make([]string, 0, len(list.Children))
	for _, child := range list.Children {
		if !isElement(child, types, "Mailbox") {
			return "", core.ErrInvalid
		}
		var address, name string
		seen := make(map[string]bool)
		for _, field := range child.Children {
			kind := field.XMLName.Local
			if field.XMLName.Space != types || len(field.Children) != 0 || seen[kind] {
				return "", core.ErrInvalid
			}
			seen[kind] = true
			switch kind {
			case "Name":
				name = field.Text
			case "EmailAddress":
				address = strings.TrimSpace(field.Text)
			case "RoutingType":
				if field.Text != "SMTP" {
					return "", core.ErrUnsupported
				}
			default:
				return "", core.ErrUnsupported
			}
		}
		value, err := mailmime.SMTPAddress(name, address)
		if err != nil {
			return "", err
		}
		addresses = append(addresses, value)
	}
	return strings.Join(addresses, ", "), nil
}

func setDraftField(d *draftMIME, field node) error {
	switch field.XMLName.Local {
	case "Subject":
		if len(field.Children) != 0 || !mailmime.ValidHeaderValue(field.Text) {
			return core.ErrInvalid
		}
		d.Header.Set("Subject", mime.QEncoding.Encode("utf-8", field.Text))
	case "ToRecipients", "CcRecipients", "BccRecipients", "ReplyTo":
		value, err := draftRecipients(field)
		if err != nil {
			return err
		}
		key := strings.TrimSuffix(field.XMLName.Local, "Recipients")
		if key == "ReplyTo" {
			key = "Reply-To"
		}
		if value == "" {
			d.Header.Del(key)
		} else {
			d.Header.Set(key, value)
		}
	case "Body":
		if len(field.Children) != 0 {
			return core.ErrInvalid
		}
		kind := field.attr("BodyType")
		if kind != "Text" && kind != "HTML" {
			return core.ErrUnsupported
		}
		entity, err := mailmime.Text(field.Text, kind == "HTML")
		if err != nil {
			return err
		}
		return d.ReplaceBody(entity)
	case "Importance":
		if len(field.Children) != 0 {
			return core.ErrInvalid
		}
		d.Header.Del("Priority")
		switch field.Text {
		case "High":
			d.Header.Set("Importance", "high")
			d.Header.Set("X-Priority", "1")
		case "Low":
			d.Header.Set("Importance", "low")
			d.Header.Set("X-Priority", "5")
		case "Normal":
			d.Header.Del("Importance")
			d.Header.Del("X-Priority")
		default:
			return core.ErrInvalid
		}
	case "IsReadReceiptRequested":
		want, err := xmlBool(field.Text)
		if err != nil || len(field.Children) != 0 {
			return core.ErrInvalid
		}
		if want {
			d.Header.Set("Disposition-Notification-To", d.Header.Get("From"))
		} else {
			d.Header.Del("Disposition-Notification-To")
		}
	default:
		return core.ErrUnsupported
	}
	return nil
}
