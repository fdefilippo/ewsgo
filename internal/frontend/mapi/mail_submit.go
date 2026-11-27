// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"time"

	"ewsgo/internal/core"
)

const (
	ropSubmitMessage      byte   = 0x32
	ropAbortSubmit        byte   = 0x34
	ropSetSpooler         byte   = 0x47
	ropSpoolerLockMessage byte   = 0x48
	ropTransportSend      byte   = 0x4a
	ropTransportNewMail   byte   = 0x51
	ropGetTransportFolder byte   = 0x6d
	unableToAbort         uint32 = 0x80040114
)

type mailDraftSender interface {
	SendDraft(context.Context, core.Session, core.Object) (core.SendObservation, error)
}

func (t *SessionTransport) EnableMailDraftSending(sender mailDraftSender) { t.mailDraftSender = sender }

func rejectLegacySubmission(objects *mailboxObjects, op Operation, handles []uint32) []byte {
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code == 0 {
		code = NotSupported
		if object.isLogon && op.ID == ropAbortSubmit {
			// A synchronous SOAP send cannot be recalled through MAPI.
			code = unableToAbort
		}
	}
	return ropError(op.ID, op.Input, code)
}

func (t *SessionTransport) executeSubmission(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	reply := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	if t.mailDraftSender == nil {
		return reply(ropNotImplemented)
	}
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return reply(code)
	}
	if op.ID == ropGetTransportFolder {
		if !object.isLogon {
			return reply(NotSupported)
		}
		return binary.LittleEndian.AppendUint64(reply(0), object.binding.fids[Outbox])
	}
	if object.message == nil {
		return reply(NotSupported)
	}
	// Preprocessing is a distinct transport path.
	if op.Flags != 0 && (op.ID != ropSubmitMessage || op.Flags != needsSpooler || object.binding.store.durable.store == nil) {
		return reply(NotSupported)
	}
	draft := object.message.mailDraft
	if draft == nil {
		store := object.binding.store
		store.mu.Lock()
		key := store.byMID[object.message.mid]
		store.mu.Unlock()
		if key.associated {
			return reply(accessDenied)
		}
		return reply(NotSupported)
	}
	store := object.binding.store
	store.spoolerMu.Lock()
	defer store.spoolerMu.Unlock()
	if response, handled := t.submitViaSpooler(ctx, session, objects, op, draft); handled {
		if rev, ok := draft.staged.Properties[core.ContentRevision].(core.Integer); ok {
			object.message.contentRevision, object.message.contentVersioned = rev, true
		}
		return response
	}
	evidence, err := draft.submit(ctx, session, t.mailDraftSender)
	object.message.contentRevision, object.message.contentVersioned = draft.staged.Properties[core.ContentRevision].(core.Integer)
	if errors.Is(err, core.ErrOutcomeUnknown) {
		op := "DraftSubmissionUnconfirmed"
		if evidence.SentCandidate != "" {
			op = "DraftSubmissionUnconfirmedWithSentCopy"
		}
		core.RecordActivity(ctx, core.Activity{Layer: "mapi", Operation: op, Result: core.ActivityOutcome(err)})
	}
	if errors.Is(err, core.ErrInvalidSendRecipients) {
		return reply(0x00000467) // ecInvalidRecips.
	}
	response := reply(faiError(err, true))
	if op.ID == ropTransportSend && err == nil {
		// SubmitDraft retained the original message properties.
		response = append(response, 1) // NoPropertiesReturned
	}
	return response
}

func (d *mailDraft) checkSendable() error {
	if d.readOnly {
		return core.ErrAccessDenied
	}
	if d.incompleteAttachments != 0 {
		return core.ErrInvalid
	}
	if len(d.rows) == 0 {
		return core.ErrInvalidSendRecipients
	}
	return nil
}

func (d *mailDraft) submit(ctx context.Context, session core.Session, sender mailDraftSender) (_ core.SendObservation, err error) {
	if d.submitted {
		return core.SendObservation{}, d.authorize(ctx, session)
	}
	if err := d.checkSendable(); err != nil {
		return core.SendObservation{}, err
	}
	reader, ok := session.Mailbox().(core.MailMessageMIMEReader)
	if !ok {
		return core.SendObservation{}, core.ErrUnsupported
	}
	if _, err := d.save(ctx, session, 2); err != nil {
		return core.SendObservation{}, err
	}
	var entry outboxDraft
	result := outboxUnconfirmed
	if d.outbox {
		entry, err = d.store.startOutboxSubmission(ctx, d)
		if err != nil {
			return core.SendObservation{}, err
		}
		defer func() {
			commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
			defer cancel()
			if finish := d.store.finishOutboxSubmission(commit, d, entry, result); finish != nil {
				err = errors.Join(core.ErrOutcomeUnknown, err, finish)
			}
		}()
	}
	if !entry.trashPending {
		native, err := reader.ReadMailMessageMIME(ctx, d.folder, d.staged.ID, false)
		if err != nil {
			return core.SendObservation{}, err
		}
		if !validSavedMailDraft(native, d.folder) || native.ID != d.staged.ID || native.Revision != d.staged.Revision {
			return core.SendObservation{}, core.ErrConflict
		}
		var evidence core.SendObservation
		if d.meeting.Class != "" {
			err = d.submitMeeting(ctx, session, native)
			core.RecordActivity(ctx, core.Activity{Layer: "mapi", Operation: "SubmitMeeting", Result: core.ActivityOutcome(err)})
		} else {
			evidence, err = sender.SendDraft(ctx, session, native)
		}
		if err != nil {
			if errors.Is(err, core.ErrOutcomeUnknown) {
				result = outboxUncertain
			}
			return evidence, err
		}
	}
	result = outboxRetained
	if entry.transportSource != "" {
		err := d.trashTransportCopy(ctx, session.Mailbox(), entry.transportSource)
		core.RecordActivity(ctx, core.Activity{Layer: "mapi", Operation: "TrashOutboxTransportCopy", Result: core.ActivityOutcome(err)})
		result = outboxTrashed
		if err != nil {
			// Submission has already succeeded. Retain the locked revision
			// so a retry can finish placement without resending.
			result = outboxTrashPending
		}
	}
	return core.SendObservation{}, nil
}

func (d *mailDraft) trashTransportCopy(ctx context.Context, box core.Mailbox, source core.ID) error {
	writer, ok := box.(core.MailMessagePlacementWriter)
	if !ok {
		return core.ErrUnsupported
	}
	id := d.staged.ID
	if source == id {
		return core.ErrConflict
	}
	current, err := box.Get(ctx, id)
	if err != nil {
		return err
	}
	if current.ID != id || current.Kind != core.Message || current.Parent == "" {
		return core.ErrConflict
	}
	if current.Parent != d.folder {
		// A lost placement reply can be reconciled by the same native ID.
		parent, err := box.Get(ctx, current.Parent)
		if err != nil {
			return err
		}
		if parent.ID == current.Parent && parent.Kind == core.Folder && parent.Properties[core.WellKnown] == core.Text("deleteditems") {
			return nil
		}
		return core.ErrConflict
	}
	if !validSavedMailDraft(current, d.folder) || current.Revision != d.staged.Revision {
		return core.ErrConflict
	}
	original, err := box.Get(ctx, source)
	if err != nil {
		return err
	}
	if original.ID != source || !validSavedMailDraft(original, d.folder) {
		return core.ErrConflict
	}
	_, err = writer.TrashMailMessage(ctx, id, d.staged.Revision)
	return err
}
