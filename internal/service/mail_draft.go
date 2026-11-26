// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"bytes"
	"context"
	"crypto/sha256"
	"errors"
	"net/mail"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/storage/sqlite"
)

// MailDraftCreator coordinates the first native save with a durable claim.
type MailDraftCreator struct {
	store           *sqlite.Store
	backendInstance string
}

func (c MailDraftCreator) CreateMailDraft(ctx context.Context, account core.Session, folder core.ID, reference string, raw []byte) (core.Object, error) {
	if !core.ValidDraftReference(reference) || folder == "" {
		return core.Object{}, core.ErrInvalid
	}
	if _, err := mail.ReadMessage(bytes.NewReader(raw)); err != nil ||
		len(core.StripDraftReferenceHeader(raw)) != len(raw) {
		return core.Object{}, core.ErrInvalid
	}
	box := account.Mailbox()
	writer, canWrite := box.(core.MailDraftWriter)
	finder, canFind := box.(core.MailDraftReferenceFinder)
	if !canWrite || !canFind {
		return core.Object{}, core.ErrUnsupported
	}
	parent, err := box.Get(ctx, folder)
	if err != nil {
		return core.Object{}, err
	}
	if parent.ID != folder || parent.Kind != core.Folder || parent.Properties[core.WellKnown] != core.Text("drafts") {
		return core.Object{}, core.ErrUnsupported
	}
	mailbox, err := c.store.BindMailbox(ctx, account.Identity().Scope, c.backendInstance)
	if err != nil {
		return core.Object{}, err
	}
	dispatch, id, err := c.store.ClaimMailDraftCreation(ctx, mailbox.MailboxID, folder, reference, sha256.Sum256(raw))
	if err != nil {
		return core.Object{}, err
	}
	persist := func(id core.ID) error {
		writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
		defer cancel()
		return c.store.AcknowledgeMailDraftCreation(writeCtx, mailbox.MailboxID, reference, id)
	}
	if dispatch {
		mime := append([]byte(core.DraftReferenceHeader+": "+reference+"\r\n"), raw...)
		saved, err := writer.SaveMailDraft(ctx, "", 0, mime)
		id = saved.ID
		if id != "" && persist(id) != nil {
			return core.Object{}, core.ErrOutcomeUnknown
		}
		if err == nil && matchesCreatedDraft(saved, folder, id) {
			return saved, nil
		}
		if err != nil && id == "" && !errors.Is(err, core.ErrOutcomeUnknown) {
			writeCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if c.store.ForgetRejectedMailDraftCreation(writeCtx, mailbox.MailboxID, reference) != nil {
				return core.Object{}, core.ErrOutcomeUnknown
			}
			return core.Object{}, err
		}
	}
	matches, err := finder.FindMailDraftsByReference(ctx, folder, reference)
	if err != nil || len(matches) != 1 || !matchesCreatedDraft(matches[0], folder, id) {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	if err := persist(matches[0].ID); err != nil {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	return matches[0], nil
}

func matchesCreatedDraft(o core.Object, folder, id core.ID) bool {
	return o.ID != "" && (id == "" || o.ID == id) && o.Kind == core.Message &&
		o.Parent == folder && o.Revision > 0 && o.Properties[core.IsDraft] == core.Bool(true)
}
