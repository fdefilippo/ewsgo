// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package jmap

import (
	"context"
	"crypto/sha256"
	"fmt"
	"mime"
	"strings"

	"ewsgo/internal/core"
)

type attachment struct {
	core.MailAttachment
	blob string
	size int64
}

func (s *session) ListMailAttachments(ctx context.Context, id core.ID) ([]core.MailAttachment, error) {
	parts, err := s.attachments(ctx, id)
	if err != nil {
		return nil, err
	}
	out := make([]core.MailAttachment, 0, len(parts))
	for _, part := range parts {
		out = append(out, part.MailAttachment)
	}
	return out, nil
}

func (s *session) ReadMailAttachment(ctx context.Context, id core.ID, attachmentID string) (core.Attachment, error) {
	parts, err := s.attachments(ctx, id)
	if err != nil {
		return core.Attachment{}, err
	}
	for _, part := range parts {
		if part.ID == attachmentID {
			data, err := s.downloadBlob(ctx, part.blob, "attachment", "application/octet-stream", part.size)
			if err != nil {
				return core.Attachment{}, err
			}
			return core.Attachment{ID: part.ID, MediaType: part.MediaType, Data: data}, nil
		}
	}
	return core.Attachment{}, core.ErrNotFound
}

func (s *session) attachments(ctx context.Context, id core.ID) ([]attachment, error) {
	message, mailboxID, err := s.readPlacement(ctx, id)
	if err != nil {
		return nil, err
	}
	type item struct {
		ID, BlobID  string
		MailboxIDs  map[string]bool
		Attachments []struct {
			PartID, BlobID, Name, Type, Disposition, CID string
			Size                                         int64
		}
	}
	var result getResponse[item]
	err = s.call(ctx, "Email/get", map[string]any{
		"ids": []string{message.ID}, "properties": []string{"id", "blobId", "mailboxIds", "attachments"},
		"bodyProperties": []string{"partId", "blobId", "size", "name", "type", "disposition", "cid"},
	}, &result)
	if err != nil {
		return nil, err
	}
	if result.AccountID != s.account || len(result.List)+len(result.NotFound) != 1 {
		return nil, errProtocol
	}
	if len(result.List) == 0 {
		if result.NotFound[0] != message.ID {
			return nil, errProtocol
		}
		return nil, core.ErrNotFound
	}
	current := result.List[0]
	if current.ID != message.ID || current.BlobID != message.BlobID || !current.MailboxIDs[mailboxID] {
		return nil, core.ErrConflict
	}
	if len(current.Attachments) > 128 {
		return nil, core.ErrLimit
	}
	seen := make(map[string]bool, len(current.Attachments))
	parts := make([]attachment, 0, len(current.Attachments))
	for _, p := range current.Attachments {
		mediaType, _, err := mime.ParseMediaType(p.Type)
		if err != nil || !strings.Contains(mediaType, "/") || p.PartID == "" || seen[p.PartID] ||
			!nativeID.MatchString(p.BlobID) || p.Size < 0 {
			return nil, errProtocol
		}
		seen[p.PartID] = true
		key := sha256.Sum256([]byte(string(id) + "\x00" + message.BlobID + "\x00" + p.PartID))
		parts = append(parts, attachment{core.MailAttachment{
			ID: fmt.Sprintf("%x", key), Name: p.Name, MediaType: mediaType, ContentID: p.CID,
			Inline: strings.EqualFold(p.Disposition, "inline"),
		}, p.BlobID, p.Size})
	}
	return parts, nil
}
