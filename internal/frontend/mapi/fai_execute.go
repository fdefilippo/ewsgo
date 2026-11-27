// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"

	"ewsgo/internal/core"
)

func (t *SessionTransport) EnableFAICollectorSpike() { t.faiCollectorSpike = true }

func faiError(err error, save bool) uint32 {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errFAIIgnore):
		return 0x80040801
	case errors.Is(err, core.ErrConflict):
		if save {
			return 0x80040109
		}
		return 0x80040802
	default:
		return hierarchyError(err)
	}
}
func (t *SessionTransport) executeFAI(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32, rejection *importRejection) []byte {
	index := op.Input
	if op.ID == 0x7e || op.ID == 0x0c {
		index = op.Output
	}
	fail := func(code uint32) []byte { return ropError(op.ID, index, code) }
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if object.message != nil && object.message.calendarException != nil && (isSetPropertiesROP(op.ID) || op.ID == 0x0c) {
		attachment := object.message.calendarException
		if err := attachment.authorize(ctx, session); err != nil {
			return fail(attachmentError(err))
		}
		if err := attachment.editable(); err != nil {
			return fail(attachmentError(err))
		}
		if isSetPropertiesROP(op.ID) {
			if op.ID == ropSetPropertiesNoReplicate || object.message.embeddedReadOnly {
				return fail(accessDenied)
			}
			problems := attachment.calendar.setMessage(&object.binding.store.named, op.FAISet)
			return propertyProblemsResponse(op.ID, index, problems)
		}
		mode, valid := faiSaveMode(op.Flags)
		if !valid || object.message.embeddedReadOnly {
			return fail(accessDenied)
		}
		if err := attachment.calendar.saveMessage(); err != nil {
			return fail(attachmentError(err))
		}
		object.message.embeddedReadOnly = mode == 1
		return binary.LittleEndian.AppendUint64(append(fail(0), op.Input), object.message.mid)
	}
	if object.message != nil && object.message.appointmentDraft != nil && (op.AppointmentSet || op.MailDraftSet || op.ID == 0x0c) {
		draft := object.message.appointmentDraft
		if op.AppointmentSet || op.MailDraftSet {
			if op.ID == ropSetPropertiesNoReplicate {
				return fail(NotSupported)
			}
			problems, err := draft.set(ctx, session, object, op.FAISet)
			if err != nil {
				return fail(faiError(err, false))
			}
			return propertyProblemsResponse(op.ID, index, problems)
		}
		if err := draft.save(ctx, session, object, op.Flags); err != nil {
			return fail(faiError(err, true))
		}
		return binary.LittleEndian.AppendUint64(append(fail(0), op.Input), object.message.mid)
	}
	if op.AppointmentSet {
		return fail(accessDenied)
	}
	if object.message != nil && object.message.contactDraft != nil && (op.ContactSet || op.ID == 0x0c) {
		draft := object.message.contactDraft
		if op.ContactSet {
			if op.ID == ropSetPropertiesNoReplicate {
				return fail(NotSupported)
			}
			problems, err := draft.stage(ctx, session, object, op.FAISet, nil)
			if err != nil {
				return fail(faiError(err, false))
			}
			return propertyProblemsResponse(op.ID, index, problems)
		}
		if err := draft.save(ctx, session, object, op.Flags); err != nil {
			return fail(faiError(err, true))
		}
		return binary.LittleEndian.AppendUint64(append(fail(0), op.Input), object.message.mid)
	}
	if op.ContactSet && !op.MailDraftSet {
		return fail(accessDenied)
	}
	if op.MailDraftSet && (object.message == nil || object.message.mailDraft == nil) &&
		(object.attachment == nil || object.attachment.draft == nil) {
		return fail(NotSupported)
	}
	d := object.download
	if op.ImportMessageSet {
		if d == nil || d.mailImport == nil {
			return fail(invalidObject)
		}
		problems, err := d.mailImport.set(ctx, session, op.FAISet)
		if err != nil {
			return fail(faiError(err, false))
		}
		return propertyProblemsResponse(op.ID, index, problems)
	}
	if op.ID == 0x0c && d != nil && d.mailImport != nil {
		mid, err := d.mailImport.save(ctx, session, op.Flags, t.mailboxAddressResolver(ctx, session))
		if err != nil {
			var rejected appointmentImportRejection
			if rejection != nil && *rejection == 0 && errors.As(err, &rejected) {
				*rejection = rejected.reason
			}
			return fail(faiError(err, true))
		}
		return binary.LittleEndian.AppendUint64(append(fail(0), op.Input), mid)
	}
	setting := isSetPropertiesROP(op.ID)
	if setting && object.attachment != nil && object.attachment.draft != nil {
		problems, err := object.attachment.draft.setProperties(ctx, session, op.FAISet)
		if err != nil {
			return fail(attachmentError(err))
		}
		return propertyProblemsResponse(op.ID, index, problems)
	}
	if setting || op.ID == 0x0c {
		if object.message != nil && object.message.mailDraft != nil {
			draft := object.message.mailDraft
			if setting {
				problems, err := draft.set(ctx, session, op.FAISet)
				if err != nil {
					return fail(faiError(err, false))
				}
				return propertyProblemsResponse(op.ID, index, problems)
			}
			mid, err := draft.save(ctx, session, op.Flags)
			if err != nil {
				return fail(faiError(err, true))
			}
			object.message.contentRevision, object.message.contentVersioned = draft.staged.Properties[core.ContentRevision].(core.Integer)
			return binary.LittleEndian.AppendUint64(append(fail(0), op.Input), mid)
		}
		var draft *faiDraft
		var collector *faiCollector
		switch {
		case object.message != nil:
			draft = object.message.draft
			if draft == nil {
				return fail(accessDenied)
			}
		case d != nil:
			draft, collector = d.draft, d.draftCollector
		}
		if draft == nil {
			return fail(NotSupported)
		}
		if setting {
			if op.FAISet == nil {
				return fail(NotSupported)
			}
			problems, err := draft.set(ctx, session, op.FAISet)
			if err != nil {
				return fail(faiError(err, false))
			}
			return propertyProblemsResponse(op.ID, index, problems)
		}
		var row messageRow
		var err error
		switch {
		case collector != nil:
			row, err = collector.save(ctx, session, draft, op.Flags)
		case object.message != nil:
			row, err = draft.save(ctx, session, op.Flags)
		default:
			return fail(NotSupported)
		}
		if err != nil {
			return fail(faiError(err, true))
		}
		return binary.LittleEndian.AppendUint64(append(fail(0), op.Input), row.mid)
	}
	if object.message != nil || object.table != nil || object.attachment != nil || object.stream != nil || object.notification != nil {
		return fail(NotSupported)
	}
	switch op.ID {
	case 0x7e:
		if object.isLogon || d != nil {
			return fail(NotSupported)
		}
		if op.Flags == 0 {
			state, err := newHierarchyState(ctx, session, object.binding.store, object.fid)
			if err != nil {
				return fail(hierarchyError(err))
			}
			handle, err := objects.addDownload(object, &hierarchyDownload{state: state, hierarchyCollector: true})
			if err != nil {
				return fail(hierarchyError(err))
			}
			handles[op.Output] = handle
			return fail(0)
		}
		// Outlook keeps synchronization error reports in its local cache.
		// Decline an upload collector before it can start importing those rows.
		if object.binding.isSyncIssuesFolder(object.fid) {
			return fail(NotSupported)
		}
		collector, err := newFAICollector(ctx, session, object.binding.store, object.fid)
		if err != nil {
			return fail(faiError(err, false))
		}
		handle, err := objects.addDownload(object, &hierarchyDownload{collector: collector})
		if err != nil {
			collector.close()
			return fail(faiError(err, false))
		}
		handles[op.Output] = handle
		return fail(0)
	case 0x74:
		if d == nil || d.collector == nil || op.FAIDeletes == nil {
			return fail(NotSupported)
		}
		return fail(faiError(d.collector.delete(ctx, session, *op.FAIDeletes), false))
	}
	return fail(NotSupported)
}

func executeMessageImport(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) ([]byte, importRejection) {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Output, code) }
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code), 0
	}
	d := object.download
	if d == nil || d.collector == nil || op.FAIImport == nil {
		return fail(NotSupported), importFAICollector
	}
	if op.FAIImport.flags&0x10 == 0 {
		draft, err := object.binding.store.newMessageImport(ctx, session, object.fid, *op.FAIImport, d.collector)
		if errors.Is(err, core.ErrUnsupported) {
			var rejected normalImportRejection
			if errors.As(err, &rejected) {
				return fail(ropNotImplemented), rejected.reason
			}
			return fail(ropNotImplemented), importNormalMessage
		}
		if err != nil {
			return fail(faiError(err, false)), 0
		}
		handle, err := objects.addDownload(object, &hierarchyDownload{mailImport: draft})
		if err != nil {
			return fail(faiError(err, false)), importFAIHandleCapacity
		}
		handles[op.Output] = handle
		return binary.LittleEndian.AppendUint64(fail(0), 0), 0
	}
	c := d.collector
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return fail(invalidObject), 0
	}
	c.state.mu.Lock()
	pending := c.state.pending != nil
	c.state.mu.Unlock()
	if pending {
		return fail(faiError(core.ErrConflict, false)), 0
	}
	draft, err := object.binding.store.newFAIDraft(ctx, session, object.fid, *op.FAIImport)
	if err != nil {
		code := faiError(err, false)
		if code == NotSupported {
			return fail(code), importFAIDraftProfile
		}
		return fail(code), 0
	}
	handle, err := objects.addDownload(object, &hierarchyDownload{draft: draft, draftCollector: c})
	if err != nil {
		draft.close()
		code := faiError(err, false)
		if code == NotSupported {
			return fail(code), importFAIHandleCapacity
		}
		return fail(code), 0
	}
	handles[op.Output] = handle
	// MS-OXCROPS: ImportMessageChange's successful MessageId MUST be zero.
	return binary.LittleEndian.AppendUint64(fail(0), 0), 0
}

// The upload collector returns Seen, SeenFAI and Read. Given belongs to the
// client's download state and MUST be absent from an upload final state.
func (f *fastTransfer) faiUploadState(state contentsSyncState) error {
	f.word(0x403a0003)
	for _, item := range []struct {
		tag uint32
		set idSet
	}{{cnsetSeenTag, state.seen}, {cnsetSeenFAITag, state.seenFAI}, {cnsetReadTag, state.read}} {
		raw, err := item.set.marshal()
		if err != nil {
			return err
		}
		f.variable(item.tag, raw)
	}
	f.word(0x403b0003)
	return f.err
}
func getFAITransferState(ctx context.Context, session core.Session, objects *mailboxObjects, object serverObject, op Operation, handles []uint32, failure *transferStateFailure) []byte {
	store := object.binding.store
	stateLease, err := store.reserveContentsMemory(contentsStateMemory)
	if err != nil {
		return failure.reject(op.Output, faiError(err, false), transferStateMemory)
	}
	lease, err := store.reserveContentsPeak(ctx, maxContentsFastTransferBytes)
	if err != nil {
		stateLease.release()
		return failure.reject(op.Output, faiError(err, false), transferStateAdmissionStage(err))
	}
	child := &contentsDownload{plan: contentsFilterPlan{associated: true}, transfer: &contentsTransfer{stream: &fastTransfer{limit: maxContentsFastTransferBytes}, lease: lease}, stateLease: stateLease, permitStore: store}
	// Reserve the handle as well as both memory peaks before freezing ownership.
	handle, err := objects.addDownload(object, &hierarchyDownload{contents: child})
	if err != nil {
		child.close()
		return failure.reject(op.Output, faiError(err, false), transferStateHandleCreate)
	}
	value, permit, stage, err := object.download.collector.transferState(ctx, session)
	child.permit = permit
	if err == nil {
		stage = transferStateEncoding
		err = child.transfer.stream.faiUploadState(value)
	}
	if err == nil {
		stage = transferStateMemory
		stream := child.transfer.stream
		err = lease.shrink(len(stream.data) + 16*len(stream.atoms))
	}
	if err == nil {
		stage = transferStateCanceled
		err = ctx.Err()
	}
	if err != nil {
		objects.release(handle)
		return failure.reject(op.Output, faiError(err, false), stage)
	}
	handles[op.Output] = handle
	return ropError(0x82, op.Output, 0)
}
