// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"bytes"
	"context"
	"encoding/base64"
	"errors"
	"net/mail"
	"strings"

	"ewsgo/internal/core"
)

func (h *Handler) sendMIMEItems(ctx context.Context, op node, session core.Session) []node {
	const operation = "CreateItem"
	if h.sender == nil {
		return syncError(operation, core.ErrUnsupported)
	}
	list := op.child(messages, "Items")
	target := op.child(messages, "SavedItemFolderId")
	if !validBatch(list) || target == nil && len(op.Children) != 1 || target != nil && len(op.Children) != 2 {
		return syncError(operation, core.ErrInvalid)
	}
	if target != nil {
		sent, err := h.folderID(ctx, session, at(el(types, "DistinguishedFolderId", ""), "Id", "sentitems"))
		if err != nil {
			return []node{writeFailure(operation, err, "ErrorSavedItemFolderNotFound")}
		}
		folder, err := h.writeTarget(ctx, session, target)
		if err != nil {
			return []node{writeFailure(operation, err, "ErrorSavedItemFolderNotFound")}
		}
		if folder != sent {
			return []node{failure(operation, "ErrorInvalidPropertyForOperation")}
		}
	}
	out := make([]node, 0, len(list.Children))
	for _, message := range list.Children {
		raw, messageID, code := readSendMIME(message)
		if code != "" {
			out = append(out, failure(operation, code))
			continue
		}
		evidence, err := h.sender.SendMIME(ctx, session, messageID, raw)
		var reply node
		switch {
		case err == nil:
			reply = success(operation)
		case errors.Is(err, core.ErrSendRejected):
			reply = failure(operation, "ErrorMessageSubmissionBlocked")
		case errors.Is(err, core.ErrLimit):
			reply = failure(operation, "ErrorMessageSizeExceeded")
		case errors.Is(err, core.ErrOutcomeUnknown):
			reply = failure(operation, "ErrorInternalServerError")
			text := "The gateway cannot confirm this submission."
			if evidence.SentCandidate != "" {
				text += " A matching copy was observed in Sent Items."
			}
			reply.Children = append([]node{el(messages, "MessageText", text)}, reply.Children...)
		default:
			reply = writeFailure(operation, err, "ErrorItemNotFound")
		}
		out = append(out, reply)
	}
	return out
}

func readSendMIME(message node) ([]byte, string, string) {
	content := message.child(types, "MimeContent")
	if !isElement(message, types, "Message") || len(message.Children) != 1 || content == nil || len(content.Children) != 0 {
		return nil, "", "ErrorInvalidRequest"
	}
	raw, err := base64.StdEncoding.DecodeString(content.Text)
	if err != nil {
		return nil, "", "ErrorMimeContentInvalidBase64String"
	}
	parsed, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, "", "ErrorMimeContentInvalid"
	}
	ids := parsed.Header["Message-Id"]
	if len(ids) != 1 || len(ids[0]) < 3 {
		return nil, "", "ErrorMimeContentInvalid"
	}
	id := strings.TrimSpace(ids[0])
	if len(id) < 3 || id[0] != '<' || id[len(id)-1] != '>' || !core.ValidInternetMessageID(id[1:len(id)-1]) {
		return nil, "", "ErrorMimeContentInvalid"
	}
	count := 0
	for _, field := range []string{"To", "Cc", "Bcc"} {
		for _, value := range parsed.Header[field] {
			if strings.TrimSpace(value) == "" {
				continue
			}
			addresses, err := mail.ParseAddressList(value)
			if err != nil {
				return nil, "", "ErrorInvalidRecipients"
			}
			count += len(addresses)
		}
	}
	if count == 0 {
		return nil, "", "ErrorInvalidRecipients"
	}
	return raw, id[1 : len(id)-1], ""
}
