// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

// EnableRAMStore opts into the approved laboratory object model. Configure
// before serving requests. Restart loses replicas, FIDs and server handles.
func (t *SessionTransport) EnableRAMStore() { t.stores = newRAMStores() }

// EnableNamedPropertyPersistence is configured after EnableRAMStore, before
// accepting client requests. It preserves named-property IDs across restarts.
func (t *SessionTransport) EnableNamedPropertyPersistence(bind func(context.Context, core.Scope) (NamedPropertyStorage, error)) {
	t.stores.namedStorage = bind
}

// EnableReplicaPersistence selects the durable namespace for new private
// Logons. Call during transport construction, after enabling the store.
func (t *SessionTransport) EnableReplicaPersistence(store mapireplica.Store, bind func(context.Context, core.Scope) (string, error)) {
	t.stores.replicaStorage = func(ctx context.Context, scope core.Scope) (boundReplica, error) {
		mailbox, err := bind(ctx, scope)
		return boundReplica{store: store, mailbox: mailbox}, err
	}
}

type storeResults struct {
	logons, folders, errors int
	unsupported             string
	properties              StorePropertyDiagnostic
	synchronization         SynchronizationDiagnostic
	synchronizationOptions  SynchronizationOptionsDiagnostic
	createFolder            CreateFolderDiagnostic
	setList                 SetListDiagnostic
	calendarSet             CalendarReferenceSetDiagnostic
	bindingProperties       bool
	tableProfileEnabled     bool
	folderOpenEnabled       bool
	execution               ExecutionDiagnostic
}

func executeError(code, maximum uint32) []byte {
	body, _ := ExecuteResponse(code, nil, nil, maximum)
	return body
}

func (t *SessionTransport) executeStore(ctx context.Context, session core.Session, state *contextState, identity MailboxIdentity, request Execute) ([]byte, storeResults) {
	var counts storeResults
	var hierarchyRefusal hierarchyImportRefusal
	var messageImportRefusal importRejection
	var messageSaveRejection *importRejection
	if t.synchronizationOptionsTrace {
		messageSaveRejection = &messageImportRefusal
	}
	var transferStateRefusal transferStateFailure
	counts.execution.enabled = t.synchronizationOptionsTrace || core.HasActivityObserver(ctx)
	counts.execution.at(1)
	counts.properties.unknown.enabled = t.unknownPropertyTrace
	counts.tableProfileEnabled = t.unknownPropertyTrace && t.synchronizationOptionsTrace
	counts.folderOpenEnabled = counts.tableProfileEnabled
	if state.mailbox == nil {
		return executeError(NotSupported, request.MaxOutput), counts
	}
	if request.Flags & ^uint32(7) != 0 {
		return executeError(NotSupported, request.MaxOutput), counts
	}
	if request.MaxOutput > 0x40000 {
		counts.execution.at(4)
		return Failure(0x000006f7), counts // No ultra-large packed capability advertised.
	}
	counts.execution.at(2)
	var buffer ROPBuffer
	var err error
	// After NotificationWait the client can issue Execute without a ROP
	// buffer if its only purpose is to collect pending notifications.
	if len(request.ROP) != 0 || t.wakeups == nil {
		buffer, err = ParseROP(request.ROP)
	}
	if err != nil {
		code := uint32(0x80070057)
		if errors.Is(err, ErrUnsupported) {
			code = NotSupported
		}
		return executeError(code, request.MaxOutput), counts
	}
	counts.execution.at(3)
	var draftObjects *mailboxObjects
	if t.mailDraftCreator != nil {
		draftObjects = state.mailbox
	}
	ops, err := buffer.decodeReadStateOperations(t.ostPropertySpike, t.hierarchyDownloadSpike, t.createFolderSpike, t.folderMetadataSpike, t.setListTrace, t.calendarMetadataSpike, t.contactsMetadataSpike, t.tasksMetadataSpike, t.draftsReferenceSpike, t.contentsDownloadSpike, t.faiCollectorSpike, t.contentsTableSpike, t.readStateImport, true, t.stores.privateConfig, draftObjects, state.mailbox)
	if err != nil {
		if errors.Is(err, errStreamReadCount) {
			return Failure(0x000004b6), counts
		}
		var unsupported unsupportedOperation
		if errors.As(err, &unsupported) {
			counts.unsupported = unsupported.label
			counts.synchronization = unsupported.synchronization
			counts.synchronizationOptions = unsupported.synchronizationOptions
			counts.createFolder = unsupported.createFolder
			counts.setList = unsupported.setList
			counts.execution.rejectedTable = unsupported.tableFlags
			counts.recordTableRejection(unsupported.tableProfile)
		}
		if counts.tableProfileEnabled {
			_ = errors.As(err, &counts.execution.malformedROP)
		}
		code := uint32(0x80070057)
		if errors.Is(err, ErrUnsupported) {
			code = NotSupported
		}
		return executeError(code, request.MaxOutput), counts
	}
	ost := t.ostView(session.Identity().Scope, ops)
	metadata := t.metadataView(session.Identity().Scope, ops)
	// Reserve worst-case complete responses before any native reads, handle allocation
	// or Release. No prefix of a malformed/unknown batch is ever executed.
	maximumROPBuffer := 65535
	for _, op := range ops {
		if op.ID == 0x15 && op.Table.flags&2 != 0 || op.ID == 0x2c && op.Stream.packed {
			// A packing-enabled call still has one initial buffer of at most
			// 32 KiB. Additional chained buffers are optional, not emitted by
			// this profile. Unreturned data remains available to later calls.
			maximumROPBuffer = 32 << 10
		}
	}
	needed := 10 + 4*len(buffer.Handles)
	var messageSlots, attachmentSlots [MaxHandles]bool
	for i, handle := range buffer.Handles {
		object := state.mailbox.objects[handle]
		messageSlots[i] = object.message != nil || object.download != nil && object.download.mailImport != nil
		attachmentSlots[i] = object.attachment != nil
	}
	for i, op := range ops {
		switch op.ID {
		case 0x03:
			needed += openMessageMinimum
			messageSlots[op.Output] = true
		case ropOpenEmbeddedMessage:
			needed += openMessageMinimum + embeddedResponseFields
			messageSlots[op.Output] = true
		case ropReloadCachedInformation:
			needed += openMessageMinimum
		case ropGetMessageStatus, ropSetMessageStatus:
			needed += 10
		case 0x06:
			needed += 15
			messageSlots[op.Output] = true
		case ropCopyTo, ropCopyProperties:
			needed += max(10, 8+10*len(op.CopyMessageTo.included))
		case ropDeleteMessages, ropEmptyFolder, ropHardDeleteMessages, ropHardDeleteFolderContents:
			needed += deleteMessagesResponseSize
		case ropSetMessageReadFlag, ropSetReadFlags:
			needed += readFlagsResponseSize
		case ropDeleteFolder:
			needed += deleteFolderResponseSize
		case ropGetSearchCriteria:
			needed += getSearchCriteriaResponseSize
		case ropSetSearchCriteria:
			needed += 6
		case ropMoveCopyMessages, ropMoveFolder:
			needed += moveCopyMessagesMaxResponse
		case ropCopyFolder:
			if op.FolderPlacement.async && op.FolderPlacement.recursive {
				needed += folderCopyProgressSize
			} else {
				needed += moveCopyMessagesMaxResponse
			}
		case 0x0f:
			needed += readRecipientsMinimum
		case 0x22:
			needed += 6
			attachmentSlots[op.Output] = true
		case ropCreateAttachment:
			needed += 10
			attachmentSlots[op.Output] = true
		case ropSaveAttachment, ropDeleteAttachment:
			needed += 6
		case 0x52:
			ops[i].AttachmentLimit = 8
			needed += ops[i].AttachmentLimit
		case 0x04, 0x05, 0x2b, 0x5e:
			needed += 10
		case 0x2c, ropWriteStream, ropWriteAndCommitStream:
			needed += 8
		case ropSetStreamSize, ropCommitStream, ropCloneStream, ropLockRegionStream, ropUnlockRegionStream:
			needed += 6
		case ropProgress:
			needed += folderCopyProgressSize
		case 0x2e:
			needed += 14
		case ropCopyToStream:
			needed += copyToStreamMaxResponse
		case 0x21, 0x29, 0x3e, 0x3f:
			needed += 6
		case 0x12, 0x13, 0x14, 0x16:
			needed += 7
		case 0x4f:
			needed += 8
		case 0x17:
			needed += 14
		case 0x18:
			needed += 11
		case 0x19:
			needed += 12
		case 0x1b, 0x6c:
			needed += 8 + tableBookmarkWireSize
		case 0x59:
			needed += 12
		case 0x5a:
			needed += 10
		case 0x6b:
			needed += 8
		case 0x1a, 0x38, 0x81, 0x89:
			needed += 6
		case 0x15:
			needed += 9
		case 0x40:
			if op.PermissionChange.atomic() {
				if _, ok := session.Mailbox().(core.FolderUserGrantsWriter); !ok {
					counts.unsupported = publicROPLabels[op.ID]
					return executeError(NotSupported, request.MaxOutput), counts
				}
			}
			needed += 6
		case 0x43:
			needed += 30
		case 0x44:
			needed += 14
		case ropGetPerUserLongTermIDs:
			needed += 8
		case ropGetPerUserGUID:
			needed += 6
			if t.publicStore != nil {
				needed += 16
			}
		case ropGetOwningServers:
			needed += 6
			if t.publicStore != nil {
				needed += 5 + len(t.publicStore.dn)
			}
		case ropWritePerUserInformation:
			needed += 6
		case ropPublicFolderIsGhosted:
			needed += 7
		case ropReadPerUserInformation:
			needed += 9
		case 0xfe:
			needed += 166
		case 0x1c:
			needed += 15 // Private-store CreateFolder, IsExistingFolder always zero.
		case 2:
			needed += 8
		case ropSetProperties, ropSetPropertiesNoReplicate:
			if op.FAISet != nil {
				needed += 8 + 10*len(op.FAISet)
			} else if op.SpecialFolderReference != nil {
				needed += 8 + 10*len(op.SpecialFolderReference.values)
			} else {
				needed += 18
			} // Full PropertyProblem list reserved.
		case ropDeleteProperties, ropDeletePropertiesNoReplicate:
			needed += 8 + 10*len(op.DeletePropertyTags)
		case ropFastTransferSourceCopyMessages, ropFastTransferSourceCopyFolder, 0x4d, ropFastTransferSourceCopyProperties, ropTellVersion, 0x70, 0x75, 0x76, 0x77, 0x82, 0x7e, 0x74, 0x80:
			needed += 6
		case 0x72, 0x78:
			needed += 14
			if op.ID == 0x72 && op.FAIImport != nil && op.FAIImport.flags&0x10 == 0 {
				messageSlots[op.Output] = true
			}
		case 0x73:
			needed += 6
			if t.hierarchyImport {
				needed += 8
			}
		case 0x0c:
			needed += 15
		case 0x4e:
			needed += 15
			if op.TransferSize != 0xbabe {
				needed += int(op.TransferLimit)
			}
		case ropFastTransferDestinationConfigure:
			needed += 6
		case ropFastTransferDestinationPutBuffer:
			needed += fastTransferPutBufferResponseSize
		case 0x49:
			needed += addressTypesResponseSize
		case ropOptionsData:
			needed += 11
		case ropModifyRules, ropUpdateDeferredActions, ropSetLocalReplicaMidsetDeleted:
			needed += 6
		case ropGetPropertiesAll, 0x09, 0x37, 0x55, 0x5f:
			ops[i].PropertyListLimit = 8 // Common header and 16-bit element count.
			needed += ops[i].PropertyListLimit
		case 0x68:
			needed += receiveTableResponseSize
		case 0x0d, 0x0e, ropSubmitMessage, ropAbortSubmit, ropSetSpooler, ropSpoolerLockMessage, ropTransportNewMail, ropSetReceiveFolder:
			needed += 6
		case ropGetStoreState:
			needed += 10
		case ropGetTransportFolder:
			needed += 14
		case ropTransportSend:
			needed += 7
		case 0x7f:
			needed += 28 // ReturnValue, replica GUID and six-byte GLOBCNT.
		case 0x27:
			if op.InvalidMessageClass {
				needed += 6
			} else {
				_, explicit := receiveAssociation(op.MessageClass)
				if _, routed := session.Mailbox().(core.ReceiveFolderMailbox); routed {
					explicit = op.MessageClass
				}
				needed += 15 + len(explicit) // FID, matched class, and NUL.
			}
		case 0x07:
			bound := specificPropertiesObjectBound(*op.Properties, ost.bound(), metadata, messageSlots[op.Input])
			needed += bound
			if attachmentSlots[op.Input] {
				ops[i].AttachmentLimit = bound
			} else if messageSlots[op.Input] {
				for _, tag := range op.Properties.tags {
					if variableMessageProperty(tag) || t.contactsMetadataSpike && contactVariableProperty(tag) ||
						t.calendarMetadataSpike && calendarVariableProperty(tag) {
						op.Properties.hasVariableMessage = true
						break
					}
				}
			}
		case 0x56:
			count := len(op.PropertyNames)
			if count == 0 {
				count = maxNamedMappings // Bound concurrent registration/enumeration.
			}
			needed += 8 + 2*count
		}
	}
	if needed > maximumROPBuffer {
		counts.execution.at(4)
		// A response exceeding this single-buffer profile cannot be resized.
		return Failure(0x0000047d), counts
	}
	if needed > int(request.MaxOutput) {
		counts.execution.at(5)
		return insufficientStoreOutput(buffer, needed, request.MaxOutput), counts
	}
	// Sentinel requests consume only residual output after reserving every
	// other response, including fixed-size GetBuffer requests in this batch.
	_, nativeReceiveRouting := session.Mailbox().(core.ReceiveFolderMailbox)
	variableResponses := 0
	for _, op := range ops {
		if nativeReceiveRouting && op.ID == 0x68 {
			variableResponses++
		}
		if t.publicStore != nil && (op.ID == ropReadPerUserInformation || op.ID == ropGetPerUserLongTermIDs) {
			variableResponses++
		}
		if op.AttachmentLimit != 0 || op.PropertyListLimit != 0 || op.ID == 0x2c && op.Stream.count != 0 || op.OpenMessage != nil || op.ID == 0x0f || op.ID == 0x15 || op.ID == 0x4f || op.ID == 0x59 && op.Table.count != 0 || op.ID == 0x6b || op.ID == 0x4e && op.TransferSize == 0xbabe || op.requestsSearchDetails() || op.Properties != nil && op.Properties.hasVariableMessage {
			variableResponses++
		}
	}
	for i := range ops {
		if nativeReceiveRouting && ops[i].ID == 0x68 {
			limit := (min(maximumROPBuffer, int(request.MaxOutput)) - needed) / variableResponses
			ops[i].ReceiveTableLimit = receiveTableResponseSize + limit
			needed += limit
			variableResponses--
		}
		if properties := ops[i].Properties; properties != nil && properties.hasVariableMessage {
			properties.variableBudget = (min(maximumROPBuffer, int(request.MaxOutput)) - needed) / variableResponses
			needed += properties.variableBudget
			variableResponses--
		}
		if ops[i].requestsSearchDetails() {
			limit := min(maxSearchCriteriaDetails, (min(maximumROPBuffer, int(request.MaxOutput))-needed)/variableResponses)
			ops[i].Search.limit = limit
			needed += limit
			variableResponses--
		}
		if ops[i].ID == 0x2c && ops[i].Stream.count != 0 {
			limit := (min(maximumROPBuffer, int(request.MaxOutput)) - needed) / variableResponses
			ops[i].Stream.limit = uint32(min(int64(limit), int64(ops[i].Stream.count)))
			needed += int(ops[i].Stream.limit)
			variableResponses--
		}
		if ops[i].OpenMessage != nil {
			limit := (min(maximumROPBuffer, int(request.MaxOutput)) - needed) / variableResponses
			ops[i].OpenMessage.limit = openMessageMinimum + limit
			needed += limit
			variableResponses--
		}
		if ops[i].ID == 0x0f {
			limit := (min(maximumROPBuffer, int(request.MaxOutput)) - needed) / variableResponses
			ops[i].ReadRecipients.limit = readRecipientsMinimum + limit
			needed += limit
			variableResponses--
		}
		if ops[i].AttachmentLimit != 0 {
			limit := (min(maximumROPBuffer, int(request.MaxOutput)) - needed) / variableResponses
			ops[i].AttachmentLimit += limit
			needed += limit
			variableResponses--
		}
		if ops[i].PropertyListLimit != 0 {
			limit := (min(maximumROPBuffer, int(request.MaxOutput)) - needed) / variableResponses
			if ops[i].ID == 0x09 || ops[i].ID == 0x37 {
				limit -= limit % 4 // Property tags have fixed four-byte width.
			}
			ops[i].PropertyListLimit += limit
			needed += limit
			variableResponses--
		}
		if ops[i].ID == 0x15 || ops[i].ID == 0x4f {
			limit := (min(maximumROPBuffer, int(request.MaxOutput)) - needed) / variableResponses
			ops[i].Table.limit = 9 + limit
			if ops[i].ID == 0x4f {
				ops[i].Table.limit = 8 + limit
			}
			needed += limit
			variableResponses--
		}
		if ops[i].ID == 0x59 && ops[i].Table.count != 0 || ops[i].ID == 0x6b {
			limit := (min(maximumROPBuffer, int(request.MaxOutput)) - needed) / variableResponses
			if ops[i].ID == 0x6b {
				ops[i].Table.limit = 8 + limit
			} else {
				ops[i].Table.limit = 12 + limit
			}
			needed += limit
			variableResponses--
		}
		if t.publicStore != nil && (ops[i].ID == ropReadPerUserInformation || ops[i].ID == ropGetPerUserLongTermIDs) {
			limit := (min(maximumROPBuffer, int(request.MaxOutput)) - needed) / variableResponses
			ops[i].PerUser.limit = limit
			if ops[i].ID == ropReadPerUserInformation {
				ops[i].PerUser.limit = min(limit, ops[i].PerUser.desiredChunk())
			}
			needed += ops[i].PerUser.limit
			variableResponses--
		}
		if ops[i].ID == 0x4e && ops[i].TransferSize == 0xbabe {
			limit := min(int(ops[i].TransferLimit), (min(maximumROPBuffer, int(request.MaxOutput))-needed)/variableResponses)
			ops[i].TransferLimit = uint16(limit)
			needed += limit
			variableResponses--
		}
	}
	counts.execution.at(6)
	responses := make([]byte, 0, needed)
	var responseLeases []*contentsLease
	defer func() {
		for _, lease := range responseLeases {
			lease.release()
		}
	}()
	for i, op := range ops {
		ctx := ctx
		if handle := state.mailbox.logons[op.Logon]; state.mailbox.objects[handle].spooler {
			ctx = context.WithValue(ctx, spoolerLogonKey{}, handle)
		}
		var activityStarted time.Time
		if core.HasActivityObserver(ctx) {
			activityStarted = time.Now()
		}
		var response []byte
		if public := state.mailbox.objects[state.mailbox.logons[op.Logon]].public; public != nil && op.ID != 0xfe {
			switch op.ID {
			case 1:
				if _, code := state.mailbox.publicLookup(buffer.Handles[op.Input], op.Logon); code == 0 {
					state.mailbox.release(buffer.Handles[op.Input])
				}
			case ropGetPerUserGUID, ropReadPerUserInformation, ropWritePerUserInformation:
				response = public.perUserOperation(ctx, session, state.mailbox, op, buffer.Handles, false)
			default:
				response = public.folderOperation(ctx, session, state.mailbox, op, buffer.Handles)
			}
			if len(response) >= 6 && binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
			counts.execution.record(op.ID, response)
			if !activityStarted.IsZero() {
				recordROPActivity(ctx, op.ID, response, time.Since(activityStarted))
			}
			responses = append(responses, response...)
			continue
		}
		switch op.ID {
		case 0x29:
			counts.execution.recordNotificationRequest(*op.Notification)
			response = t.registerObjectNotification(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x03:
			response = openStoreMessage(ctx, session, state, op, buffer.Handles, t.faiCollectorSpike, t.mailDraftCreator != nil)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropReloadCachedInformation:
			response = reloadCachedInformation(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropOpenEmbeddedMessage:
			response = openEmbeddedMessage(ctx, session, state, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropGetMessageStatus, ropSetMessageStatus:
			response = messageStatus(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x06:
			counts.execution.recordCreateMessage(state.mailbox, op, buffer.Handles)
			response = createStoreMessage(ctx, session, state, op, buffer.Handles, t.mailDraftCreator)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropCopyTo, ropCopyProperties:
			response = copyMessageTo(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropDeleteMessages, ropHardDeleteMessages:
			counts.execution.recordDeleteMessageOptions(op.DeleteMessages)
			response = deleteMessages(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 || response[6] != 0 {
				counts.errors++
			}
		case ropEmptyFolder, ropHardDeleteFolderContents:
			response = emptyStoreFolder(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropSetMessageReadFlag:
			response = setMessageReadFlag(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropSetReadFlags:
			response = setReadFlags(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 || response[6] != 0 {
				counts.errors++
			}
		case ropMoveCopyMessages:
			response = moveCopyMessages(ctx, session, state.mailbox, op, buffer.Handles, &counts.execution)
			if binary.LittleEndian.Uint32(response[2:]) != 0 || response[len(response)-1] != 0 {
				counts.errors++
			}
		case ropMoveFolder:
			response = moveStoreFolder(ctx, session, state.mailbox, op, buffer.Handles, state.codePage)
			if binary.LittleEndian.Uint32(response[2:]) != 0 || response[len(response)-1] != 0 {
				counts.errors++
			}
		case ropCopyFolder:
			response = copyStoreFolder(ctx, session, state.mailbox, op, buffer.Handles, state.codePage)
			if response[0] == ropCopyFolder && (binary.LittleEndian.Uint32(response[2:]) != 0 || response[len(response)-1] != 0) {
				counts.errors++
			}
		case 0x0f:
			response = readOpenedRecipients(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x2b, 0x2c, 0x2e, 0x5e, ropWriteStream, ropSetStreamSize, ropCommitStream, ropCopyToStream,
			ropCloneStream, ropWriteAndCommitStream, ropLockRegionStream, ropUnlockRegionStream:
			if counts.tableProfileEnabled && op.ID == 0x2b {
				counts.execution.recordOpenStream(state.mailbox, op, buffer.Handles)
			}
			var lease *contentsLease
			response, lease = executePropertyStream(ctx, session, state.mailbox, op, buffer.Handles)
			if lease != nil {
				responseLeases = append(responseLeases, lease)
			}
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropCreateAttachment, ropSaveAttachment:
			response = executeDraftAttachment(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropDeleteAttachment:
			response = deleteDraftAttachment(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x52, 0x22:
			response = executeAttachment(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x21:
			response = openAttachmentTable(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x04, 0x05, 0x3e, ropGetRulesTable:
			var diagnostic *ExecutionDiagnostic
			if counts.tableProfileEnabled && op.ID == 0x05 {
				diagnostic = &counts.execution
			}
			response = t.openContentsTable(ctx, session, state.mailbox, op, buffer.Handles, diagnostic)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			} else if counts.tableProfileEnabled && op.ID == 0x05 && op.Table.flags&tableSoftDeletes != 0 {
				counts.execution.recoverableTable.opened = true
				counts.execution.recoverableTable.openRows = uint16(binary.LittleEndian.Uint32(response[6:]))
			}
		case 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18, 0x19, 0x1a, 0x1b,
			0x37, 0x38, 0x4f, 0x59, 0x5a, 0x6b, 0x6c, 0x81, 0x89:
			var lease *contentsLease
			var diagnostic *ExecutionDiagnostic
			if counts.tableProfileEnabled && op.ID == 0x15 {
				diagnostic = &counts.execution
			}
			response, lease = t.executeTable(ctx, session, state.mailbox, op, buffer.Handles, state.codePage, diagnostic)
			counts.recordTableBinding(state.mailbox, op, buffer.Handles, response)
			if counts.tableProfileEnabled && op.ID == 0x15 && binary.LittleEndian.Uint32(response[2:]) == 0 && binary.LittleEndian.Uint16(response[7:]) != 0 {
				if object, code := state.mailbox.lookup(buffer.Handles[op.Input], op.Logon); code == 0 {
					counts.execution.recordMailTableIdentity(object.table)
				}
			}
			if lease != nil {
				responseLeases = append(responseLeases, lease)
			}
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x40:
			response = t.modifyPermissions(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropGetPerUserLongTermIDs, ropGetPerUserGUID, ropReadPerUserInformation, ropWritePerUserInformation,
			ropGetOwningServers, ropPublicFolderIsGhosted:
			if t.publicStore != nil && op.ID != ropGetOwningServers && op.ID != ropPublicFolderIsGhosted {
				response = t.publicStore.perUserOperation(ctx, session, state.mailbox, op, buffer.Handles, true)
			} else {
				response = privatePublicFolderState(state.mailbox, op, buffer.Handles)
			}
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x43, 0x44:
			response = convertFolderID(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0xfe:
			if t.publicStore != nil && op.LogonRequest.Flags&1 == 0 {
				response = t.publicStore.logon(ctx, session, state.mailbox, op, buffer.Handles)
			} else {
				response = t.privateLogon(ctx, session, state.mailbox, identity, op, buffer.Handles)
			}
			if binary.LittleEndian.Uint32(response[2:]) == 0 {
				counts.logons++
			} else {
				counts.errors++
			}
		case 0x1c:
			response = createStoreFolder(ctx, session, state.mailbox, op, buffer.Handles)
			if t.createFolderTrace {
				counts.createFolder.record(*op.CreateFolder, response)
			}
			if binary.LittleEndian.Uint32(response[2:]) == 0 {
				counts.folders++
			} else {
				counts.errors++
			}
		case ropDeleteFolder:
			response = deleteStoreFolder(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 || response[6] != 0 {
				counts.errors++
			}
		case ropSetSearchCriteria, ropGetSearchCriteria:
			if op.ID == ropSetSearchCriteria {
				counts.execution.recordSearchRequest(op.Search)
			}
			response = executeSearchCriteria(ctx, session, state.mailbox, op, buffer.Handles, state.codePage, &counts)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 2:
			response = openStoreFolder(ctx, session, state.mailbox, op, buffer.Handles)
			counts.recordFolderOpen(state.mailbox, op, buffer.Handles, response)
			if binary.LittleEndian.Uint32(response[2:]) == 0 {
				counts.folders++
			} else {
				counts.errors++
			}
		case 1:
			handle := buffer.Handles[op.Input]
			if _, code := state.mailbox.lookup(handle, op.Logon); code == 0 {
				state.mailbox.release(handle)
			}
			// Release has no response, including for already invalid handles.
		case 0x56:
			response = getStorePropertyIDs(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x55, 0x5f:
			response = readNamedProperties(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x49:
			response = addressTypesResponse(op.Input)
		case ropProgress:
			response = pollFolderCopy(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 || response[0] == ropCopyFolder && response[len(response)-1] != 0 {
				counts.errors++
			}
		case ropOptionsData:
			_, code := state.mailbox.lookup(buffer.Handles[op.Input], op.Logon)
			response = ropError(op.ID, op.Input, code)
			if code == 0 {
				// Reserved byte and two empty fields (OptionsInfo and HelpFile).
				response = append(response, 1, 0, 0, 0, 0)
			} else {
				counts.errors++
			}
		case ropSubmitMessage, ropGetTransportFolder, ropTransportSend:
			counts.execution.recordSubmission(state.mailbox, op, buffer.Handles)
			response = t.executeSubmission(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropAbortSubmit, ropSpoolerLockMessage:
			response = t.executeSpoolerQueue(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropSetSpooler, ropTransportNewMail:
			response = executeSpooler(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropSetLocalReplicaMidsetDeleted:
			response = setLocalReplicaMidsetDeleted(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropModifyRules:
			response = modifyInboxRules(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropUpdateDeferredActions:
			response = updateDeferredActionMessages(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x7f:
			response = getLocalReplicaIDs(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x27:
			response = getReceiveFolder(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropSetReceiveFolder:
			response = setReceiveFolder(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x68:
			response = getReceiveFolderTable(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropGetStoreState:
			response = getStoreState(session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x0d, 0x0e:
			object, code := state.mailbox.lookup(buffer.Handles[op.Input], op.Logon)
			if code == 0 {
				code = t.editStoreRecipients(ctx, session, object, op, state.codePage)
			}
			response = ropError(op.ID, op.Input, code)
			if code != 0 {
				counts.errors++
			}
		case 0x70:
			uploadFailure := counts.synchronizationOptions.uploadFailure
			if !counts.synchronizationOptions.parsed {
				if op.Contents != nil {
					counts.synchronizationOptions = inspectContentsOptions(*op.Configure)
				} else {
					counts.synchronizationOptions = inspectSynchronizationOptions(*op.Configure)
				}
			}
			if op.Contents != nil {
				var diagnostic contentsConfigureFailure
				response, diagnostic = configureContents(ctx, session, state.mailbox, op, buffer.Handles)
				if diagnostic.code != 0 && counts.synchronizationOptions.configureFailure.code == 0 {
					// Preserve the first failed Configure, even after an earlier
					// successful Configure or before a later one in this batch.
					counts.synchronizationOptions = inspectContentsOptions(*op.Configure)
					counts.synchronizationOptions.configureFailure = diagnostic
					if diagnostic.stage == contentsRestrictionStage && t.unknownPropertyTrace && t.synchronizationOptionsTrace {
						object, code := state.mailbox.object(buffer.Handles[op.Input], op.Logon)
						if code == 0 {
							recordContentsBindingDiagnostic(*op.Contents, &object.binding.store.named, &counts.properties)
							counts.bindingProperties = true
						}
					}
				}
			} else {
				response = configureHierarchy(ctx, session, state.mailbox, op, buffer.Handles)
			}
			counts.synchronizationOptions.uploadFailure = uploadFailure
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x75, 0x76, 0x77:
			var diagnostic contentsUploadFailure
			response, diagnostic = uploadHierarchy(ctx, session, state.mailbox, op, buffer.Handles)
			if t.synchronizationOptionsTrace && counts.synchronizationOptions.uploadFailure.kind == 0 {
				counts.synchronizationOptions.uploadFailure = diagnostic
			}
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropFastTransferSourceCopyMessages:
			response = openFastTransferCopyMessages(ctx, session, state.mailbox, op, buffer.Handles, state.codePage)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropFastTransferSourceCopyFolder:
			response = openFastTransferCopyFolder(ctx, session, state.mailbox, op, buffer.Handles, state.codePage)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x4d, ropFastTransferSourceCopyProperties:
			response = openFastTransferCopy(ctx, session, state.mailbox, op, buffer.Handles, state.codePage)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x4e:
			var kind uint8
			if counts.execution.enabled {
				kind = executionBufferKind(state.mailbox, op, buffer.Handles)
			}
			response = getHierarchyBuffer(ctx, session, state.mailbox, op, buffer.Handles)
			counts.execution.recordBuffer(kind, response)
			counts.execution.recordContentsFailure(state.mailbox, op, buffer.Handles, response)
			counts.execution.recordContentsCompletion(state.mailbox, op, buffer.Handles, response)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x82:
			var failure transferStateFailure
			response = getHierarchyTransferState(ctx, session, state.mailbox, op, buffer.Handles, &failure)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
				if t.synchronizationOptionsTrace && transferStateRefusal.stage == 0 {
					transferStateRefusal = failure
				}
			}
		case 0x80:
			response = executeReadState(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x73:
			if t.hierarchyImport {
				var refusal hierarchyImportRefusal
				response, refusal = executeHierarchyImport(ctx, session, state.mailbox, op, buffer.Handles)
				if t.synchronizationOptionsTrace && hierarchyRefusal == 0 {
					hierarchyRefusal = refusal
				}
				if counts.tableProfileEnabled && refusal == hierarchyImportExtraProperty {
					counts.execution.hierarchyImportTags.enabled = true
					for _, property := range op.HierarchyImport.properties {
						counts.execution.hierarchyImportTags.record(property.tag)
					}
				}
			} else {
				response = t.executeFAI(ctx, session, state.mailbox, op, buffer.Handles, nil)
			}
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropFastTransferDestinationConfigure, ropFastTransferDestinationPutBuffer:
			response = executeFastTransferDestination(ctx, session, state.mailbox, op, buffer.Handles, t.faiCollectorSpike)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropTellVersion:
			_, code := state.mailbox.lookup(buffer.Handles[op.Input], op.Logon)
			response = ropError(op.ID, op.Input, code)
			if code != 0 {
				counts.errors++
			}
		case 0x78:
			response = executeImportMessageMove(ctx, session, state.mailbox, op, buffer.Handles)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x72:
			var refusal importRejection
			response, refusal = executeMessageImport(ctx, session, state.mailbox, op, buffer.Handles)
			if t.synchronizationOptionsTrace && messageImportRefusal == 0 {
				messageImportRefusal = refusal
			}
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x7e, 0x74, 0x0c:
			response = t.executeFAI(ctx, session, state.mailbox, op, buffer.Handles, messageSaveRejection)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropSetProperties, ropSetPropertiesNoReplicate:
			if op.FAISet != nil {
				response = t.executeFAI(ctx, session, state.mailbox, op, buffer.Handles, nil)
			} else if op.SpecialFolderReference != nil {
				response = setSpecialFolderReference(ctx, session, state.mailbox, op, buffer.Handles, metadata)
				if t.calendarReferenceSetTrace {
					counts.calendarSet.recordReferenceSet(response, op.SpecialFolderReference)
				}
			} else if op.FreeBusyReference != nil {
				response = setFreeBusyReference(ctx, session, state.mailbox, op, buffer.Handles)
			} else if op.Metadata != nil {
				response = setFolderMetadata(ctx, session, state.mailbox, op, buffer.Handles, metadata)
			} else {
				response = setFolderOSTValue(ctx, session, state.mailbox, op, buffer.Handles, ost)
			}
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case ropDeleteProperties, ropDeletePropertiesNoReplicate:
			for _, tag := range op.DeletePropertyTags {
				counts.properties.unknown.record(tag)
			}
			response = deleteStoreProperties(ctx, session, state.mailbox, op, buffer.Handles, metadata)
			if binary.LittleEndian.Uint32(response[2:]) != 0 {
				counts.errors++
			}
		case 0x07, ropGetPropertiesAll, 0x09:
			response = t.getStoreProperties(ctx, session, state, op, buffer.Handles, &counts.properties, ost, metadata)
			code := binary.LittleEndian.Uint32(response[2:])
			if op.ID == ropGetPropertiesAll && i == 0 && code == tableBufferTooSmall && request.MaxOutput >= uint32(maximumROPBuffer) {
				return Failure(0x000003f0), counts
			}
			if code != 0 {
				counts.errors++
			}
		}
		counts.execution.record(op.ID, response)
		if !activityStarted.IsZero() {
			recordROPActivity(ctx, op.ID, response, time.Since(activityStarted))
		}
		responses = append(responses, response...)
		if op.ID == 0x19 || op.ID == 0x1b {
			// MS-OXCTABL 3.2.5.10 and 3.2.5.12 specify which
			// bookmark failures terminate the input buffer.
			code := binary.LittleEndian.Uint32(response[2:])
			if code == NotSupported || op.ID == 0x19 && code == invalidTableBookmark {
				break
			}
		}
	}
	counts.synchronizationOptions.hierarchyRefusal = hierarchyRefusal
	counts.synchronizationOptions.messageImportRefusal = messageImportRefusal
	counts.synchronizationOptions.transferStateFailure = transferStateRefusal
	maximum := min(request.MaxOutput, uint32(maximumROPBuffer))
	responses, counts.execution.tableNotification = t.appendTableNotifications(ctx, session, state.mailbox, responses, len(buffer.Handles), maximum, state.codePage)
	var objectNotificationPending bool
	responses, counts.execution.notificationEmitted, objectNotificationPending = t.appendObjectNotifications(ctx, session, state.mailbox, responses, len(buffer.Handles), maximum)
	if (objectNotificationPending || counts.execution.tableNotification == tableNotificationPending) &&
		int(maximum)-10-4*len(buffer.Handles)-len(responses) >= pendingNotificationBytes {
		// One logical MAPI/HTTP session in this transport.
		responses = append(responses, 0x6e, 0, 0)
	}
	buffer.Operations = responses
	rop, err := buffer.Marshal()
	if err != nil {
		panic("MAPI response exceeded validated size")
	}
	body, err := ExecuteResponse(0, rop, nil, request.MaxOutput)
	if err != nil {
		panic("MAPI response exceeded reserved output")
	}
	return body, counts
}

func getStorePropertyIDs(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(0x56, op.Input, code) }
	if op.Flags != 0 && op.Flags != 2 {
		return fail(0x80070057)
	}
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	// An ICS import handle is a message object before SaveChanges and uses
	// the same mailbox-wide named-property registry as an opened message.
	if object.download != nil && object.download.mailImport == nil || object.table != nil || object.stream != nil || object.notification != nil {
		return fail(NotSupported)
	}
	fid := object.fid
	if object.isLogon {
		fid = object.binding.fids[MailboxRoot]
	}
	if _, err := object.binding.store.folder(ctx, session, fid); err != nil {
		return fail(storeError(err))
	}
	if len(op.PropertyNames) == 0 && !object.isLogon {
		return append(fail(0), 0, 0) // Only Logon enumerates the mailbox registry.
	}
	create := false
	if op.Flags == 2 && len(op.PropertyNames) > 0 {
		owner, err := mailboxOwner(ctx, session.Mailbox())
		if err != nil {
			return fail(storeError(err))
		}
		create = owner
	}
	if ctx.Err() != nil {
		return fail(0x80004005)
	}
	ids, code := object.binding.store.named.ids(ctx, op.PropertyNames, create)
	if code != 0 {
		return fail(code)
	}
	response := binary.LittleEndian.AppendUint16(fail(0), uint16(len(ids)))
	for _, id := range ids {
		response = binary.LittleEndian.AppendUint16(response, id)
	}
	return response
}

// MS-OXCROPS 2.2.15.1.1: return the unexecuted request prefix that fits after
// RopBufferTooSmall, followed by the unchanged handle table. If even that
// minimum framing cannot fit, use the Execute failure body (MS-OXCMAPIHTTP).
func insufficientStoreOutput(buffer ROPBuffer, needed int, maximum uint32) []byte {
	capacity := int(maximum) - 10 - 4*len(buffer.Handles) - 3
	if capacity < 0 {
		return Failure(0x0000047d)
	}
	response := binary.LittleEndian.AppendUint16([]byte{0xff}, uint16(needed))
	response = append(response, buffer.Operations[:min(capacity, len(buffer.Operations))]...)
	buffer.Operations = response
	rop, _ := buffer.Marshal()
	body, _ := ExecuteResponse(0, rop, nil, maximum)
	return body
}

func ropError(id, slot byte, code uint32) []byte { return append32([]byte{id, slot}, code) }

func (t *SessionTransport) privateLogon(ctx context.Context, session core.Session, objects *mailboxObjects, identity MailboxIdentity, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(0xfe, op.Output, code) }
	request := *op.LogonRequest
	if request.ESSDN == "" || !strings.EqualFold(request.ESSDN, identity.LegacyDN) || request.OpenFlags&1 != 0 {
		return fail(accessDenied)
	}
	allowedOpen := uint32(0x00000004 | 0x00000008 | 0x00000400 | 0x01000000 | 0x20000000)
	if request.Flags&0x08 != 0 {
		// Outlook LTSC sends this extra OpenFlags bit on spooler logon.
		// Ignore it for this compatibility case without changing store access.
		allowedOpen |= 0x00000040
	}
	if request.Flags&1 == 0 || request.Flags&^byte(0x0f) != 0 || request.OpenFlags & ^allowedOpen != 0 {
		return fail(NotSupported)
	}
	reader, ok := session.Mailbox().(core.MailboxStateReader)
	if !ok {
		return fail(NotSupported)
	}
	status, err := reader.MailboxState(ctx)
	if err != nil {
		return fail(storeError(err))
	}
	if !status.Owner {
		return fail(accessDenied)
	}
	catalog, err := privateFolders(ctx, session)
	if err != nil {
		return fail(storeError(err))
	}
	binding, err := t.stores.bind(ctx, catalog)
	if err != nil {
		return fail(storeError(err))
	}
	// This is the same mailbox identity advertised by Autodiscover, distinct
	// from the address-book object GUID and the RAM replica epoch.
	guid, err := mailboxGUIDWire(identity.GUID)
	if err != nil {
		return fail(0x80004005)
	}
	binding.mailboxGUID = guid
	flags := byte(1 | 2) // Reserved and native own-mailbox OwnerRight.
	if status.CanSend {
		flags |= 4
	}
	if status.OutOfOffice {
		flags |= 0x10
	}
	response, err := (PrivateLogonResponse{
		OutputHandleIndex: op.Output, LogonFlags: request.Flags & 7, ResponseFlags: flags,
		FolderIDs: binding.fids, MailboxGUID: guid, ReplicaID: 1, ReplicaGUID: binding.store.replica,
		LogonTime: time.Now().UTC(), GwartTime: addressTypeConfigRevision,
	}).Marshal()
	if err != nil {
		return fail(0x80004005)
	}
	handle, err := objects.addLogon(op.Logon, binding)
	if err != nil {
		return fail(storeError(err))
	}
	handles[op.Output] = handle
	return response
}

func openStoreFolder(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	if op.Flags != 0 {
		return ropError(2, op.Output, NotSupported)
	}
	handle, _, code := objects.openFolder(ctx, session, op.Logon, handles[op.Input], op.Folder)
	if code != 0 {
		return ropError(2, op.Output, code)
	}
	handles[op.Output] = handle
	hasRules, _ := inboxHasRules(ctx, session, objects.objects[handle].binding, op.Folder)
	return append(ropError(2, op.Output, 0), hasRules, 0)
}
