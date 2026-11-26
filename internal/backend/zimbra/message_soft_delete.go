// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
)

var (
	_ core.MailMessageSoftDeleter                   = (*mailbox)(nil)
	_ core.MailMessageSoftDeleterWithNonReadReceipt = (*mailbox)(nil)
)

const dispositionNotificationTo = "Disposition-Notification-To"

func (m *mailbox) SoftDeleteMailMessage(ctx context.Context, id core.ID, expected core.Revision) error {
	return m.softDeleteMailMessage(ctx, id, expected, false)
}

func (m *mailbox) SoftDeleteMailMessageWithNonReadReceipt(ctx context.Context, id core.ID, expected core.Revision) error {
	return m.softDeleteMailMessage(ctx, id, expected, true)
}

func (m *mailbox) softDeleteMailMessage(ctx context.Context, id core.ID, expected core.Revision, notifyNonRead bool) (writeErr error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: "SoftDeleteMailMessage", Result: core.ActivityOutcome(writeErr), Duration: time.Since(started)})
	}()
	source, ok := parseDeltaObject(id)
	if !ok || source.kind != core.Message || expected > math.MaxInt32 {
		return core.ErrInvalid
	}
	var headers []string
	if notifyNonRead {
		headers = []string{dispositionNotificationTo}
	}
	item, err := m.native.getMessageNode(ctx, source.ref, "8192", headers...)
	if err != nil {
		return semanticError(err)
	}
	before, err := messageSummary(item)
	if err != nil {
		return err
	}
	if !hasMessageRevision(before, expected) {
		return core.ErrConflict
	}
	if notifyNonRead && before.Properties[core.IsRead] != core.Bool(true) && !messageHasNoNonReadReceipt(item) {
		return core.ErrUnsupported
	}
	// The Zimbra delete operation bypasses the dumpster for the native
	// Drafts folder. Restrict the operation to ordinary, own mail folders.
	if before.Parent == nativeDrafts {
		return core.ErrUnsupported
	}
	parent, ok := parseDeltaObject(before.Parent)
	if !ok || parent.kind != core.Folder {
		return ErrProtocol
	}
	if err := m.mailContentFolder(ctx, parent.ref); err != nil {
		return semanticError(err)
	}
	if err := m.native.requireVisibleDumpster(ctx); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	action := attr(attr(element(mailNS, "action", ""), "id", source.ref), "op", "delete")
	result, err := m.native.request(ctx, mailNS, "MsgAction", "", element(mailNS, "MsgActionRequest", "", action), modifiedConstraint(expected))
	if err != nil {
		return messageWriteError(err)
	}
	ack := result.child(mailNS, "action")
	if len(result.Children) != 1 || ack == nil || ack.get("id") != source.ref || ack.get("op") != "delete" {
		return core.ErrOutcomeUnknown
	}
	if _, err := m.getMessageSummary(ctx, source.ref); !errors.Is(err, core.ErrNotFound) {
		return core.ErrOutcomeUnknown
	}
	if !m.native.messageInDumpster(ctx, source.ref) {
		return core.ErrOutcomeUnknown
	}
	return nil
}

func messageHasNoNonReadReceipt(item node) bool {
	requested, known := messageReceiptRequest(item)
	return known && !requested
}

func messageReceiptRequest(item node) (requested, known bool) {
	hasMIME := false
	opaque := false
	for _, child := range item.Children {
		if child.XMLName.Space != mailNS {
			continue
		}
		switch child.XMLName.Local {
		case "header":
			if strings.EqualFold(child.get("n"), dispositionNotificationTo) {
				return true, true
			}
		case "e":
			if child.get("t") == "n" {
				return true, true
			}
		case "mp":
			hasMIME = true
			if !mimePartExposesReceiptRequest(child) {
				opaque = true
			}
		}
	}
	return false, hasMIME && !opaque
}

func mimePartExposesReceiptRequest(part node) bool {
	// Encoded MAPI properties can carry a receipt request outside the
	// top-level Internet headers. An opaque part is insufficient evidence.
	switch strings.ToLower(part.get("ct")) {
	case "", "application/ms-tnef", "application/vnd.ms-tnef", "application/pkcs7-mime", "application/x-pkcs7-mime", "multipart/encrypted":
		return false
	}
	for _, child := range part.Children {
		if child.XMLName.Space == mailNS && child.XMLName.Local == "mp" && !mimePartExposesReceiptRequest(child) {
			return false
		}
	}
	return true
}

func (s *Session) requireVisibleDumpster(ctx context.Context) error {
	info, err := s.request(ctx, accountNS, "GetInfo", "", attr(element(accountNS, "GetInfoRequest", ""), "sections", "attrs"))
	if err != nil {
		return semanticError(err)
	}
	attrs := info.child(accountNS, "attrs")
	var enabled, visibleAge string
	if attrs != nil {
		for _, item := range attrs.Children {
			if item.XMLName.Space != accountNS || item.XMLName.Local != "attr" {
				continue
			}
			switch item.get("name") {
			case "zimbraDumpsterEnabled":
				if enabled != "" {
					return ErrProtocol
				}
				enabled = strings.TrimSpace(item.Text)
			case "zimbraDumpsterUserVisibleAge":
				if visibleAge != "" {
					return ErrProtocol
				}
				visibleAge = strings.TrimSpace(item.Text)
			}
		}
	}
	// Some Zimbra deployments omit the retention setting from user SOAP.
	// Enabled is required before mutation and user-visible membership is
	// required afterward. An explicitly unusable window rejects the write.
	if !strings.EqualFold(enabled, "true") || visibleAge != "" && !positiveDumpsterAge(visibleAge) {
		return core.ErrUnsupported
	}
	return nil
}

func positiveDumpsterAge(age string) bool {
	if len(age) == 0 {
		return false
	}
	if strings.ContainsRune("dhms", rune(age[len(age)-1])) {
		age = age[:len(age)-1]
	}
	n, err := strconv.ParseUint(age, 10, 32)
	return err == nil && n > 0
}

func (s *Session) messageInDumpster(ctx context.Context, id string) bool {
	item, err := s.findDumpsterMessage(ctx, id)
	return err == nil && item != nil
}

func (s *Session) findDumpsterMessage(ctx context.Context, id string) (*node, error) {
	query := element(mailNS, "SearchRequest", "", element(mailNS, "query", "item:"+id))
	response, err := s.request(ctx, mailNS, "Search", "", attr(attr(attr(query, "inDumpster", "1"), "types", "message"), "limit", "2"))
	if err != nil {
		return nil, semanticError(err)
	}
	if response.get("more") != "0" && response.get("more") != "false" || len(response.Children) > 1 {
		return nil, ErrProtocol
	}
	if len(response.Children) == 0 {
		return nil, nil
	}
	item := &response.Children[0]
	if item.XMLName.Space != mailNS || item.XMLName.Local != "m" || item.get("id") != id {
		return nil, ErrProtocol
	}
	return item, nil
}
