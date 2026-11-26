// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"math"
	"strings"
	"time"

	"ewsgo/internal/core"
)

func (m *mailbox) MarkMailRead(ctx context.Context, id core.ID, expected core.Revision, suppressReceipt bool) (out core.Object, writeErr error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: "MarkMailRead", Result: core.ActivityOutcome(writeErr), Duration: time.Since(started)})
	}()
	source, ok := parseDeltaObject(id)
	if !ok || source.kind != core.Message || expected > math.MaxInt32 {
		return core.Object{}, core.ErrInvalid
	}
	item, err := m.native.getMessageNode(ctx, source.ref, "8192", dispositionNotificationTo)
	if err != nil {
		return core.Object{}, semanticError(err)
	}
	current, err := messageSummary(item)
	if err != nil {
		return core.Object{}, err
	}
	if !hasMessageRevision(current, expected) {
		return core.Object{}, core.ErrConflict
	}
	requested, known := messageReceiptRequest(item)
	if strings.ContainsRune(item.get("f"), 'n') || known && !requested {
		return m.SetMailFlag(ctx, id, expected, core.MailFlagRead, true)
	}
	if !suppressReceipt || !known {
		return core.Object{}, core.ErrUnsupported
	}

	// Zimbra's Notified state represents both sent and declined receipts.
	// Establish the decline before marking read so the second operation can
	// be retried independently if the connection or revision changes.
	action := attr(attr(attr(element(mailNS, "action", ""), "id", source.ref), "op", "tag"), "tn", `\Notified`)
	response, err := m.native.request(ctx, mailNS, "MsgAction", "", element(mailNS, "MsgActionRequest", "", action), modifiedConstraint(expected))
	if err != nil {
		return core.Object{}, messageWriteError(err)
	}
	ack := response.child(mailNS, "action")
	if ack == nil || ack.get("id") != source.ref || ack.get("op") != "tag" {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	item, err = m.native.getMessageNode(ctx, source.ref, "8192")
	if err != nil || !strings.ContainsRune(item.get("f"), 'n') {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	declined, err := messageSummary(item)
	if err != nil || declined.Revision <= expected || declined.Parent != current.Parent {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	read, err := m.SetMailFlag(ctx, id, declined.Revision, core.MailFlagRead, true)
	if err != nil {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	return read, nil
}
