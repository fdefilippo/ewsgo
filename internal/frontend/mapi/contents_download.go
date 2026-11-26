// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"

	"ewsgo/internal/core"
)

// Wrapped by the existing transfer handle kind so a transfer can never be used
// as a Folder, Logon or named-property registration target.
type contentsDownload struct {
	permit         *faiStatePermit
	permitStore    *ramStore
	state          *contentsState // Nil for immutable GetTransferState source.
	config         synchronizationConfigure
	plan           contentsFilterPlan
	transfer       *contentsTransfer
	stateLease     *contentsLease // Copies for immutable state-only source.
	cursor         int
	completed      bool
	completedSteps int
	final          contentsSyncState
	ticket         *contentsProofTicket
	failure        uint32
	failureDetail  contentsFailureDiagnostic
	sharedRights   core.AccessRights
}

func (*contentsDownload) String() string   { return "contentsDownload{private}" }
func (*contentsDownload) GoString() string { return "contentsDownload{private}" }
func (d *contentsDownload) close() {
	if d == nil {
		return
	}
	if d.permitStore != nil {
		d.permitStore.finishFAIStatePermit(d.permit, false)
		d.permit = nil
	}
	d.state.close()
	d.state = nil
	d.transfer.close()
	d.transfer = nil
	_ = d.ticket.finish(context.Background(), false)
	d.ticket = nil
	d.stateLease.release()
	d.stateLease = nil
	d.final = contentsSyncState{}
}
func (t *SessionTransport) EnableContentsDownloadSpike() { t.contentsDownloadSpike = true }

func appointmentContentsSchema(associated bool) func(contentsProperty) bool {
	fai := contentsProjectionSchema(false, associated)
	return func(p contentsProperty) bool {
		if p.name != (propertyName{}) {
			for _, field := range calendarProperties {
				if p == field.property {
					return true
				}
			}
			return fai(p)
		}
		switch uint32(p.id)<<16 | uint32(p.kind) {
		case 0x001a001f, 0x0037001f, 0x0e070003, appointmentStartDateTag, appointmentEndDateTag, appointmentBodyTag:
			return true
		default:
			return fai(p)
		}
	}
}

func registerCalendarNames(ctx context.Context, names *namedPropertyRegistry) error {
	properties := make([]propertyName, 0, len(calendarProperties))
	for _, field := range calendarProperties {
		properties = append(properties, field.property.name)
	}
	if _, code := names.ids(ctx, properties, true); code != 0 {
		return core.ErrUnsupported
	}
	return nil
}

func configureContents(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) ([]byte, contentsConfigureFailure) {
	diagnostic := contentsConfigureFailure{stage: contentsInputStage}
	fail := func(code uint32) ([]byte, contentsConfigureFailure) {
		diagnostic.code = code
		return ropError(0x70, op.Output, code), diagnostic
	}
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	diagnostic.stage = contentsObjectStage
	if object.isLogon || object.download != nil || op.Contents == nil || object.binding.store.isDeferredActionFolder(object.fid) {
		return fail(NotSupported)
	}
	c := *op.Contents
	diagnostic.stage = contentsOptionsStage
	if err := validateContentsConfigure(c.frame); err != nil {
		return fail(hierarchyError(err))
	}
	store := object.binding.store
	normal, associated := c.frame.flags&0x20 != 0, c.frame.flags&0x10 != 0
	var special core.SpecialFolderKind
	schema := contentsProjectionSchema(normal, associated)
	if _, available := session.Mailbox().(core.ContactReader); available && store.contactsMetadata && normal {
		if _, err := store.scopedMessageAnchor(ctx, session, object.fid, false, core.ContactsFolder); err == nil {
			special = core.ContactsFolder
			schema = contactContentsSchema(associated)
		}
	}
	if _, available := session.Mailbox().(core.AppointmentContentsReader); available && store.calendarMetadata && normal && special == 0 {
		if _, err := store.scopedMessageAnchor(ctx, session, object.fid, false, core.CalendarFolder); err == nil {
			special = core.CalendarFolder
			schema = appointmentContentsSchema(associated)
		}
	}
	plan, bindingDiagnostic, err := compileContentsFilterDiagnostic(c, &store.named, schema)
	if err != nil {
		diagnostic = bindingDiagnostic
		return fail(hierarchyError(err))
	}
	plan.special = special
	diagnostic.stage = contentsStateStage
	state, err := newContentsState(ctx, session, store, object.fid, c, plan)
	if err != nil {
		diagnostic.metadataOnly = errors.Is(err, normalMetadataOnlyError{})
		return fail(hierarchyError(err))
	}
	d := &contentsDownload{state: state, config: c.frame, plan: plan}
	diagnostic.stage = contentsHandleStage
	handle, err := objects.addDownload(object, &hierarchyDownload{contents: d})
	if err != nil {
		d.close()
		return fail(hierarchyError(err))
	}
	handles[op.Output] = handle
	return ropError(0x70, op.Output, 0), contentsConfigureFailure{}
}
func uploadContents(ctx context.Context, session core.Session, object serverObject, op Operation) ([]byte, contentsUploadFailure) {
	d := object.download.contents
	if d.state == nil {
		return ropError(op.ID, op.Input, NotSupported), contentsUploadFailure{}
	}
	err := d.state.upload(ctx, session, *op.Upload)
	if err != nil {
		return ropError(op.ID, op.Input, hierarchyError(err)), inspectContentsUploadFailure(err)
	}
	return ropError(op.ID, op.Input, 0), contentsUploadFailure{}
}
func (d *contentsDownload) authorize(ctx context.Context, session core.Session, object serverObject) error {
	store := object.binding.store
	if _, err := store.scopedMessageAnchor(ctx, session, object.fid, !d.plan.normal, d.plan.special); err != nil {
		return err
	}
	return store.checkSharedContentRights(ctx, session, object.fid, d.sharedRights)
}
func (d *contentsDownload) prepare(ctx context.Context, session core.Session, object serverObject) error {
	if d.transfer != nil {
		return nil
	}
	if d.state == nil {
		return core.ErrInvalid
	}
	rights, err := object.binding.store.sharedContentRights(ctx, session, object.fid)
	if err != nil {
		return contentsPreparationError{contentsFailureAuthorization, err}
	}
	d.sharedRights = rights
	prepared, err := d.state.prepareContents(ctx, session, d.config)
	if err != nil {
		return err
	}
	defer func() { prepared.close() }()
	// Trash can contain both mail and native contacts. Resolve the contact
	// property IDs before either form of the stream starts sending changes.
	if d.plan.special == core.ContactsFolder || slices.ContainsFunc(prepared.delta.changes, func(change contentsChange) bool {
		return change.row.special == core.ContactsFolder
	}) {
		if err := registerContactNames(ctx, &object.binding.store.named); err != nil {
			return err
		}
	}
	if d.plan.special == core.CalendarFolder {
		if err := registerCalendarNames(ctx, &object.binding.store.named); err != nil {
			return err
		}
	}
	var transfer *contentsTransfer
	if hasIndexedChanges(prepared.delta.changes) {
		transfer, err = newContentsSequence(d.config, prepared)
	} else {
		transfer, err = object.binding.store.contentsFastTransfer(ctx, d.config, d.plan.selection, prepared.delta)
	}
	if err != nil {
		return err
	}
	if err = ctx.Err(); err != nil {
		transfer.close()
		return err
	}
	// Encoding admission may have waited for another request's workspace.
	// Revalidate access before publishing a stream or its final checkpoint.
	if err = d.authorize(ctx, session, object); err != nil {
		transfer.close()
		return contentsPreparationError{contentsFailureAuthorization, err}
	}
	d.transfer = transfer
	d.final = cloneContentsState(prepared.delta.final)
	d.ticket = prepared.ticket
	prepared.ticket = nil
	if transfer.sequence != nil {
		prepared = nil // The sequence owns its retained metadata and projection leases.
	}
	return nil
}
func contentsBufferResponse(input byte, code uint32, status uint16, data []byte, done, total int) []byte {
	out := ropError(0x4e, input, code)
	out = binary.LittleEndian.AppendUint16(out, status)
	out = binary.LittleEndian.AppendUint16(out, uint16(min(done, 65535)))
	out = binary.LittleEndian.AppendUint16(out, uint16(min(total, 65535)))
	out = append(out, 0)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(data)))
	return append(out, data...)
}
func getContentsBuffer(ctx context.Context, session core.Session, object serverObject, op Operation) []byte {
	d := object.download.contents
	fail := func(code uint32) []byte { return contentsBufferResponse(op.Input, code, 0, nil, 0, 0) }
	if err := d.authorize(ctx, session, object); err != nil {
		d.failTransfer(err, contentsFailureAuthorization)
	}
	if d.failure != 0 {
		return fail(d.failure)
	}
	if d.completed {
		return contentsBufferResponse(op.Input, 0, 3, nil, d.completedSteps, d.completedSteps)
	}
	if err := d.prepare(ctx, session, object); err != nil {
		d.failTransfer(err, contentsFailureEncoding)
		return fail(d.failure)
	}
	var data []byte
	var status uint16
	var done, total int
	end := d.cursor
	if d.transfer.sequence != nil {
		var err error
		data, status, done, total, err = d.progressiveBuffer(ctx, session, object.binding.store, int(op.TransferLimit))
		if err != nil {
			d.failTransfer(err, contentsFailureEncoding)
			return fail(d.failure)
		}
	} else {
		stream := d.transfer.stream
		end = stream.chunkEnd(d.cursor, int(op.TransferLimit))
		status = 1
		if end == len(stream.data) {
			status = 3
		} else if end == d.cursor {
			status = 2
		}
		total = max(1, (len(stream.data)+1023)/1024)
		done = end / 1024
		data = stream.data[d.cursor:end]
	}
	if d.transfer.sequence != nil && d.sharedRights != 0 {
		if err := object.binding.store.checkSharedContentRights(ctx, session, object.fid, d.sharedRights); err != nil {
			d.failTransfer(err, contentsFailureAuthorization)
			return fail(d.failure)
		}
	}
	if err := ctx.Err(); err != nil {
		if d.transfer.sequence != nil {
			d.failTransfer(err, contentsFailureAuthorization)
		}
		return fail(hierarchyError(err))
	}
	if status == 3 {
		if err := d.ticket.finish(ctx, true); err != nil {
			if d.transfer.sequence != nil {
				d.failTransfer(err, contentsFailureCheckpoint)
			}
			return fail(hierarchyError(err))
		}
		if d.permitStore != nil {
			if err := d.permitStore.publishUploadProof(ctx, d.permit); err != nil {
				return fail(hierarchyError(err))
			}
		}
		done = total
	}
	out := contentsBufferResponse(op.Input, 0, status, data, done, total)
	// Execute has reserved and validated the entire response before any ROP.
	// This commits response bytes, not an acknowledgement from the network peer.
	d.cursor = end
	if status == 3 {
		if d.permitStore != nil {
			d.permitStore.finishFAIStatePermit(d.permit, d.permitStore.durable.store == nil)
			d.permit = nil
		}
		d.ticket = nil
		d.completed, d.completedSteps = true, total
		d.transfer.close()
	}
	return out
}

func (d *contentsDownload) failTransfer(err error, stage contentsFailureStage) {
	d.failure = hierarchyError(err)
	d.failureDetail = inspectContentsFailure(err, stage)
	_ = d.ticket.finish(context.Background(), false)
	d.ticket = nil
	d.transfer.close()
}
func getContentsTransferState(ctx context.Context, session core.Session, objects *mailboxObjects, object serverObject, op Operation, handles []uint32, failure *transferStateFailure) []byte {
	d := object.download.contents
	if d.state == nil {
		return failure.reject(op.Output, NotSupported, transferStateExportedSource)
	}
	if err := d.authorize(ctx, session, object); err != nil {
		return failure.reject(op.Output, hierarchyError(err), transferStateAuthorization)
	}
	if d.failure != 0 {
		return failure.reject(op.Output, d.failure, transferStatePriorFailure)
	}
	d.state.mu.Lock()
	defer d.state.mu.Unlock()
	if d.state.closed {
		return failure.reject(op.Output, hierarchyError(core.ErrConflict), transferStateClosed)
	}
	if d.state.pending != nil {
		return failure.reject(op.Output, hierarchyError(core.ErrConflict), transferStatePending)
	}
	value := d.state.value
	if d.completed {
		value = d.final
	}
	stateLease, err := object.binding.store.reserveContentsMemory(contentsStateMemory)
	if err != nil {
		return failure.reject(op.Output, hierarchyError(err), transferStateMemory)
	}
	lease, err := object.binding.store.reserveContentsPeak(ctx, maxContentsFastTransferBytes)
	if err != nil {
		stateLease.release()
		return failure.reject(op.Output, hierarchyError(err), transferStateAdmissionStage(err))
	}
	stream := &fastTransfer{limit: maxContentsFastTransferBytes}
	if err = stream.contentsState(value); err != nil {
		lease.release()
		stateLease.release()
		return failure.reject(op.Output, hierarchyError(err), transferStateEncoding)
	}
	if err = lease.shrink(len(stream.data) + 16*len(stream.atoms)); err != nil {
		lease.release()
		stateLease.release()
		return failure.reject(op.Output, hierarchyError(err), transferStateMemory)
	}
	child := &contentsDownload{config: d.config, plan: d.plan, transfer: &contentsTransfer{stream: stream, lease: lease}, stateLease: stateLease}
	if err = d.authorize(ctx, session, object); err != nil {
		child.close()
		return failure.reject(op.Output, hierarchyError(err), transferStateAuthorization)
	}
	if err = ctx.Err(); err != nil {
		child.close()
		return failure.reject(op.Output, hierarchyError(err), transferStateCanceled)
	}
	handle, err := objects.addDownload(object, &hierarchyDownload{contents: child})
	if err != nil {
		child.close()
		return failure.reject(op.Output, hierarchyError(err), transferStateHandleCreate)
	}
	handles[op.Output] = handle
	return ropError(0x82, op.Output, 0)
}
