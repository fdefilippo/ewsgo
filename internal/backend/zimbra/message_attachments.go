// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/xml"
	"io"
	"mime"
	"mime/quotedprintable"
	"net/mail"
	"regexp"
	"strconv"
	"strings"

	"ewsgo/internal/core"
)

var nativePart = regexp.MustCompile(`^[1-9][0-9]*(\.[1-9][0-9]*)*$`)
var mimePartElement = xml.Name{Space: mailNS, Local: "mp"}

type mailAttachment struct {
	core.MailAttachment
	part string
}

func (m *mailbox) ListMailAttachments(ctx context.Context, id core.ID) ([]core.MailAttachment, error) {
	_, _, attachments, err := m.messageAttachments(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]core.MailAttachment, 0, len(attachments))
	for _, attachment := range attachments {
		out = append(out, attachment.MailAttachment)
	}
	return out, nil
}

func (m *mailbox) ReadMailAttachment(ctx context.Context, id core.ID, attachmentID string) (core.Attachment, error) {
	if attachmentID == "" {
		return core.Attachment{}, core.ErrInvalid
	}
	ref, revision, attachments, err := m.messageAttachments(ctx, id)
	if err != nil {
		return core.Attachment{}, err
	}
	for _, attachment := range attachments {
		if attachment.ID == attachmentID {
			return m.readMailAttachment(ctx, ref, revision, attachment)
		}
	}
	return core.Attachment{}, core.ErrNotFound
}

func (m *mailbox) readMailAttachment(ctx context.Context, ref string, revision uint64, attachment mailAttachment) (core.Attachment, error) {
	data, err := m.native.readMessageContent(ctx, "GetAttachment", ref, attachment.part, m.native.client.mimeLimit)
	if err != nil {
		return core.Attachment{}, semanticError(err)
	}
	if attachment.part == "" {
		data, err = decodeRootAttachment(data)
		if err != nil {
			return core.Attachment{}, err
		}
	}
	item, err := m.native.getMessageNode(ctx, ref, "1")
	if err != nil {
		return core.Attachment{}, semanticError(err)
	}
	current, err := contentRevision(item)
	if err != nil {
		return core.Attachment{}, err
	}
	if current != revision {
		return core.Attachment{}, core.ErrConflict
	}
	return core.Attachment{ID: attachment.ID, MediaType: attachment.MediaType, Data: data}, nil
}

func (m *mailbox) messageAttachments(ctx context.Context, id core.ID) (string, uint64, []mailAttachment, error) {
	object, ok := parseDeltaObject(id)
	if !ok || object.kind != core.Message {
		return "", 0, nil, core.ErrInvalid
	}
	item, err := m.native.getMessageNode(ctx, object.ref, "1")
	if err != nil {
		return "", 0, nil, semanticError(err)
	}
	revision, err := contentRevision(item)
	if err != nil {
		return "", 0, nil, err
	}
	var root node
	for _, child := range item.Children {
		if child.XMLName == mimePartElement {
			if root.XMLName.Local != "" {
				return "", 0, nil, ErrProtocol
			}
			root = child
		}
	}
	if root.XMLName.Local == "" {
		return "", 0, nil, ErrProtocol
	}
	idPrefix := string(id) + ":" + strconv.FormatUint(revision, 10) + ":"
	attachments, err := attachmentParts(root, idPrefix)
	if err != nil {
		return "", 0, nil, err
	}
	return object.ref, revision, attachments, nil
}

func contentRevision(item node) (uint64, error) {
	revision, err := optionalFolderNumber(item, "rev")
	if err != nil || revision == nil || !nativeID.MatchString(item.get("l")) {
		return 0, ErrProtocol
	}
	return *revision, nil
}

func attachmentParts(root node, idPrefix string) ([]mailAttachment, error) {
	var out []mailAttachment
	seen := make(map[string]bool)
	var visit func(node, bool) error
	visit = func(item node, root bool) error {
		part := item.get("part")
		if root && part == "TEXT" {
			part = ""
		}
		if seen[part] || !(root && part == "" || nativePart.MatchString(part)) {
			return ErrProtocol
		}
		seen[part] = true
		mediaType, _, err := mime.ParseMediaType(item.get("ct"))
		if err != nil || !strings.Contains(mediaType, "/") {
			return ErrProtocol
		}
		if strings.HasPrefix(mediaType, "multipart/") {
			hasChild := false
			for _, child := range item.Children {
				if child.XMLName == mimePartElement {
					hasChild = true
					if err := visit(child, false); err != nil {
						return err
					}
				}
			}
			if !hasChild {
				return ErrProtocol
			}
			return nil
		}

		// The children of an attached .eml belong to the embedded message.
		name, disposition := item.get("filename"), item.get("cd")
		cid := strings.TrimSpace(item.get("ci"))
		if strings.HasPrefix(cid, "<") && strings.HasSuffix(cid, ">") {
			cid = cid[1 : len(cid)-1]
		}
		explicit := strings.EqualFold(disposition, "attachment")
		if explicit || name != "" || cid != "" || !strings.HasPrefix(mediaType, "text/") {
			key := part
			if part == "" {
				key = "root"
			}
			out = append(out, mailAttachment{
				MailAttachment: core.MailAttachment{
					ID: idPrefix + key, Name: name, MediaType: mediaType, ContentID: cid,
					Inline: !explicit && (cid != "" || strings.EqualFold(disposition, "inline")),
				},
				part: part,
			})
		}
		return nil
	}
	if err := visit(root, true); err != nil {
		return nil, err
	}
	return out, nil
}

func decodeRootAttachment(raw []byte) ([]byte, error) {
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil {
		return nil, ErrProtocol
	}
	body := message.Body
	switch strings.ToLower(strings.TrimSpace(message.Header.Get("Content-Transfer-Encoding"))) {
	case "", "7bit", "8bit", "binary":
	case "base64":
		body = base64.NewDecoder(base64.StdEncoding, body)
	case "quoted-printable":
		body = quotedprintable.NewReader(body)
	default:
		return nil, core.ErrUnsupported
	}
	data, err := io.ReadAll(body)
	if err != nil {
		return nil, ErrProtocol
	}
	return data, nil
}
