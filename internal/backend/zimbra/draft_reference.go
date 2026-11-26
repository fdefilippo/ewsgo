// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"bytes"
	"context"
	"errors"
	"net/mail"

	"ewsgo/internal/core"
)

var _ core.MailDraftReferenceFinder = (*mailbox)(nil)

func (m *mailbox) FindMailDraftsByReference(ctx context.Context, folder core.ID, reference string) ([]core.Object, error) {
	parent, ok := parseDeltaObject(folder)
	if !ok || parent.kind != core.Folder || !core.ValidDraftReference(reference) {
		return nil, core.ErrInvalid
	}
	if err := m.mailContentFolder(ctx, parent.ref); err != nil {
		return nil, semanticError(err)
	}
	query := "inid:" + parent.ref + " #" + core.DraftReferenceHeader + ":" + reference
	hits, more, err := m.native.searchMessagePage(ctx, query, 0, 0, 100)
	if err != nil {
		return nil, err
	}
	if more {
		return nil, core.ErrLimit
	}
	var drafts []core.Object
	for _, hit := range hits {
		object, err := m.getMessageSummary(ctx, hit.id)
		if errors.Is(err, core.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if object.Parent != folder || object.Properties[core.IsDraft] != core.Bool(true) {
			continue
		}
		raw, err := m.ReadMIME(ctx, object.ID)
		if errors.Is(err, core.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		envelope, err := mail.ReadMessage(bytes.NewReader(raw))
		if err != nil {
			return nil, ErrProtocol
		}
		values := envelope.Header[core.DraftReferenceHeader]
		if len(values) == 1 && values[0] == reference {
			drafts = append(drafts, object)
		}
	}
	return drafts, nil
}
