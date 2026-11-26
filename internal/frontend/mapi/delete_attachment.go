// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"mime"
	"slices"

	"ewsgo/internal/core"
	"ewsgo/internal/mailmime"
)

const ropDeleteAttachment byte = 0x24

func deleteDraftAttachment(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	reply := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return reply(code)
	}
	if object.message == nil {
		return reply(nullObject)
	}
	draft := object.message.mailDraft
	if draft == nil {
		return reply(accessDenied)
	}
	if err := draft.editable(); err != nil {
		return reply(attachmentError(err))
	}
	number := op.DeleteAttachment
	pending := false
	for _, entry := range objects.objects {
		if item := entry.attachment; item != nil && item.draft != nil &&
			item.draft.parent == draft && item.number == number && item.draft.part == 0 {
			pending = true
			break
		}
	}
	part, err := draft.removeAttachment(ctx, session, number, pending)
	if err != nil {
		return reply(attachmentError(err))
	}
	for handle, entry := range objects.objects {
		switch {
		case entry.attachment != nil:
			if entry.attachment.belongsToDraft(draft, number) {
				objects.discard(handle, entry)
			} else if file := entry.attachment.draft; part != 0 && file != nil && file.parent == draft && file.part > part {
				file.part--
			}
		case entry.stream != nil && entry.stream.attachment.belongsToDraft(draft, number):
			objects.discard(handle, entry)
		case part != 0 && entry.table != nil && entry.table.attachmentMessage != nil && entry.table.attachmentMessage.mailDraft == draft:
			entry.table.removeAttachmentRow(number)
		}
	}
	return reply(0)
}

func (a *openedAttachment) belongsToDraft(draft *mailDraft, number uint32) bool {
	return a != nil && a.number == number && (a.message.mailDraft == draft || a.draft != nil && a.draft.parent == draft)
}

func (d *mailDraft) removeAttachment(ctx context.Context, session core.Session, number uint32, pending bool) (int, error) {
	peak, err := d.store.reserveContentsPeak(ctx, 2*maxMailProjectionBytes)
	if err != nil {
		return 0, err
	}
	defer peak.release()
	rows, err := d.attachmentRows(ctx, session)
	if err != nil {
		return 0, err
	}
	index := slices.Index(d.attachmentNumbers, number)
	if index < 0 {
		if pending {
			return 0, nil
		}
		return 0, core.ErrNotFound
	}
	doc, err := mailmime.ReadDraft(d.staged.MIME)
	if err != nil {
		return 0, err
	}
	kind, parameters, err := mime.ParseMediaType(doc.Header.Get("Content-Type"))
	if err != nil || kind != "multipart/mixed" {
		return 0, core.ErrUnsupported
	}
	parts, err := doc.Parts()
	if err != nil || len(parts) != len(rows)+1 {
		return 0, core.ErrUnsupported
	}

	// The attachment number can identify a direct MIME part only when the
	// first part is a complete body and contributes no attachment rows.
	projection := projectedMessage{values: contentsValues{}}
	parser := mailProjectionParser{result: &projection, budget: &mailProjectionBudget{}}
	if err := parser.part(parts[0].Header, bytes.NewReader(parts[0].Body), 1, true); err != nil ||
		len(projection.attachments) != 0 || parser.plain == nil && parser.html == nil {
		return 0, core.ErrUnsupported
	}
	part := index + 1
	parts = slices.Delete(parts, part, part+1)
	if err := doc.SetMultipart(kind, parameters, parts); err != nil {
		return 0, err
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	if err := d.installMIME(doc); err != nil {
		return 0, err
	}
	d.attachmentNumbers = slices.Delete(d.attachmentNumbers, index, index+1)
	d.staged.Properties[core.HasAttachments] = core.Bool(len(d.attachmentNumbers) > 0)
	return part, nil
}

// Attachment table rows are detached snapshots. Reflect the removal on
// tables of this draft handle while leaving other message handles isolated.
func (t *contentsTable) removeAttachmentRow(number uint32) {
	equal := func(row contentsValues) bool { return attachmentNumber(row) == number }
	if position := slices.IndexFunc(t.rows, equal); position >= 0 && position < t.cursor {
		t.cursor--
	}
	t.rows = slices.DeleteFunc(slices.Clone(t.rows), equal)
	t.baseRows = slices.DeleteFunc(slices.Clone(t.baseRows), equal)
}
