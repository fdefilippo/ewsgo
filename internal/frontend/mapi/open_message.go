// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"time"

	"ewsgo/internal/core"
)

const (
	openMessageMinimum            = 14
	openMessageIncludeSoftDeleted = 4
	openMessageWorkspace          = 2*maxFAIObjectBytes + 128<<10
)

type openMessageRequest struct {
	codePage uint16
	mode     byte
	mid      uint64
	limit    int
}

type openedMessage struct {
	mid                uint64
	includeSoftDeleted bool
	codePage           uint32
	recipientDigest    [32]byte
	contentRevision    core.Integer
	contentVersioned   bool
	embedded           *embeddedMessageRef
	calendarException  *draftAttachment
	embeddedReadOnly   bool
	draft              *faiDraft
	mailDraft          *mailDraft
	contactDraft       *contactDraft
	appointmentDraft   *appointmentDraft
}

func (s *ramStore) openedMessageScope(ctx context.Context, session core.Session, fid uint64, message openedMessage) (messageKey, folderKey, error) {
	if session == nil || session.Identity().Scope != s.scope {
		return messageKey{}, folderKey{}, core.ErrAccessDenied
	}
	s.mu.Lock()
	spooled := s.spoolerQueue[message.mid]
	key, known := s.byMID[message.mid]
	version, observed := s.messages[key]
	outboxFolder := s.outboxDrafts[key.id].folder
	outboxFID := s.byKey[folderKey{role: Outbox}]
	requested := s.byFID[fid]
	s.mu.Unlock()
	if spooled.deniesAccess(ctx) {
		return messageKey{}, folderKey{}, core.ErrAccessDenied
	}
	if !key.associated && key.special == 0 && outboxFolder != "" && fid != outboxFID {
		return messageKey{}, folderKey{}, core.ErrNotFound
	}
	// Once a message leaves the virtual Outbox, the old snapshot can still
	// refer to it until the native folder is observed again. The native
	// envelope read below verifies the requested folder against the source.
	nativeAfterOutbox := outboxFID != 0 && version.parent == outboxFID && outboxFolder == "" &&
		!key.associated && key.special == 0 && requested.native != ""
	if !known || key.retired != 0 || !nativeAfterOutbox && observed && (version.parent != fid ||
		!version.present && (key.associated || key.special != 0 || !message.includeSoftDeleted)) ||
		!observed && (key.associated || key.special != 0) {
		return messageKey{}, folderKey{}, core.ErrNotFound
	}
	special := key.special
	if special == core.ContactsFolder && s.trash != "" && requested.native == s.trash {
		special = 0
	}
	folder, err := s.scopedMessageAnchor(ctx, session, fid, key.associated, special)
	if err != nil {
		return messageKey{}, folderKey{}, err
	}
	if folder == (folderKey{role: Outbox}) && !key.associated {
		folder.native = outboxFolder
		if folder.native == "" {
			return messageKey{}, folderKey{}, core.ErrNotFound
		}
	}
	return key, folder, nil
}

func (s *ramStore) readOpenedMessage(ctx context.Context, session core.Session, fid uint64, message openedMessage) (messageRow, error) {
	if message.embedded != nil {
		lease, err := s.reserveContentsPeak(ctx, 2*maxMailProjectionBytes)
		if err != nil {
			return messageRow{}, err
		}
		defer lease.release()
		contents, err := s.readOpenedMailProjection(ctx, session, fid, message)
		if err != nil {
			return messageRow{}, err
		}
		return contents.embeddedRow(message.mid), nil
	}
	if message.mailDraft != nil {
		if err := message.mailDraft.authorize(ctx, session); err != nil {
			return messageRow{}, err
		}
		return message.mailDraft.row(), nil
	}
	if s.isDeferredActionFolder(fid) {
		view, err := s.observeDeferredActions(ctx, session, fid)
		if err != nil {
			return messageRow{}, err
		}
		for _, row := range view.messages {
			if row.mid == message.mid {
				return row, nil
			}
		}
		return messageRow{}, core.ErrNotFound
	}

	if s.isFreeBusyFolder(fid) {
		if err := s.authorizeLocalMessage(ctx, session, fid, true); err != nil {
			return messageRow{}, err
		}
		row := s.freeBusyMessage()
		if message.mid == 0 || row.mid != message.mid {
			return messageRow{}, core.ErrNotFound
		}
		return row, nil
	}
	key, folder, err := s.openedMessageScope(ctx, session, fid, message)
	if err != nil {
		return messageRow{}, err
	}
	if key.associated {
		s.contentsMu.Lock()
		defer s.contentsMu.Unlock()
		return s.readFAIRowLocked(ctx, fid, key.id)
	}
	if key.special == core.ContactsFolder {
		return s.readOpenedContact(ctx, session, fid, message.mid, key, folder)
	}
	if reader, ok := session.Mailbox().(core.AppointmentContentsReader); ok && key.special == core.CalendarFolder {
		item, err := readOpenedCalendarAppointment(ctx, reader, folder, key)
		if err != nil {
			return messageRow{}, err
		}
		object, err := calendarAppointmentMessage(item, folder.native)
		if err != nil {
			return messageRow{}, err
		}
		read, _ := object.Properties[core.IsRead].(core.Bool)
		row := s.observedOpenedMessage(fid, message.mid, key, object)
		row.special, row.read = key.special, bool(read)
		return row, ctx.Err()
	}
	if key.special != 0 {
		items, err := readCalendarSearchItems(ctx, session.Mailbox(), folder.native, key.special)
		if err != nil {
			return messageRow{}, err
		}
		for _, item := range items {
			if item.ID == key.id && item.Kind == core.Message && item.Parent == folder.native {
				read, _ := item.Properties[core.IsRead].(core.Bool)
				return messageRow{parent: fid, mid: message.mid, special: key.special, read: bool(read), object: item}, ctx.Err()
			}
		}
		return messageRow{}, core.ErrNotFound
	}
	reader, ok := session.Mailbox().(core.MailMessageEnvelopeReader)
	if !ok {
		return messageRow{}, core.ErrUnsupported
	}
	object, err := reader.ReadMailMessageEnvelope(ctx, folder.native, key.id, message.includeSoftDeleted)
	if err != nil {
		return messageRow{}, err
	}
	if object.Kind != core.Message || object.ID != key.id || object.Parent != folder.native {
		return messageRow{}, core.ErrNotFound
	}
	if err = ctx.Err(); err != nil {
		return messageRow{}, err
	}
	return s.observedOpenedMessage(fid, message.mid, key, object), nil
}

func readOpenedCalendarAppointment(ctx context.Context, reader core.AppointmentContentsReader, folder folderKey, key messageKey) (core.Appointment, error) {
	native, valid := strings.CutPrefix(string(key.id), "a:")
	if !valid || native == "" {
		return core.Appointment{}, core.ErrNotFound
	}
	id := core.ID(core.AppointmentIDPrefix + native)
	item, err := reader.GetCalendarAppointment(ctx, folder.native, id)
	if err != nil {
		return core.Appointment{}, err
	}
	if item.Identity.ID != id || item.Identity.Parent != folder.native || item.Identity.Kind != core.AppointmentItem {
		return core.Appointment{}, core.ErrConflict
	}
	return item, ctx.Err()
}

func (s *ramStore) observedOpenedMessage(fid, mid uint64, key messageKey, object core.Object) messageRow {
	row := messageRow{parent: fid, mid: mid, object: object, sourceKey: s.hierarchyXID(mid)}
	s.mu.Lock()
	version, observed := s.messages[key]
	s.mu.Unlock()
	if observed && version.present && version.parent == fid && version.nativeRevision == object.Revision {
		row.changeKey = s.messageChangeKey(version)
		row.predecessors = s.messagePredecessors(version)
	}
	return row
}

func openStoreMessage(ctx context.Context, session core.Session, state *contextState, op Operation, handles []uint32, editFAI, editMail bool) []byte {
	fail := func(code uint32) []byte { return ropError(3, op.Output, code) }
	objects := state.mailbox
	input, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if input.download != nil || input.table != nil || input.message != nil || input.attachment != nil || input.stream != nil || input.notification != nil {
		return fail(nullObject)
	}
	request := op.OpenMessage
	mode := request.mode & 3
	if mode == 2 {
		return fail(0x80070057)
	}
	cp := uint32(request.codePage)
	if cp == 0x0fff {
		cp = state.codePage
	}
	if len(objects.objects) >= maxLiveStoreHandles || objects.allocated >= maxContextHandleAllocations {
		return fail(0x8007000e)
	}
	store := input.binding.store
	fid := op.Folder
	if !input.isLogon && input.fid != fid {
		if _, err := store.folder(ctx, session, input.fid); err != nil {
			return fail(storeError(err))
		}
	}
	if store.isSpoolerQueue(fid) {
		parent, err := store.spoolerMessageParent(ctx, session, request.mid)
		if err != nil {
			return fail(storeError(err))
		}
		fid = parent
	}
	if store.isSearchFolder(fid) {
		parent, err := resolveSearchMessageParent(ctx, session, input.binding, fid, request.mid)
		if err != nil {
			return fail(storeError(err))
		}
		fid = parent
	}
	if mode == 1 {
		store.mu.Lock()
		associated := store.byMID[request.mid].associated
		store.mu.Unlock()
		local := associated || store.isFreeBusyFolder(fid)
		_, canWriteContact := session.Mailbox().(core.ContactWriter)
		contactEdit := editFAI && canWriteContact && store.isContactMID(request.mid)
		_, canWriteAppointment := session.Mailbox().(core.AppointmentDetailsWriter)
		appointmentEdit := editFAI && canWriteAppointment && store.isAppointmentMID(request.mid)
		if local && !editFAI || !local && !editMail && !contactEdit && !appointmentEdit {
			return fail(accessDenied)
		}
	}
	lease, err := store.reserveContentsPeak(ctx, openMessageWorkspace)
	if err != nil {
		return fail(storeError(err))
	}
	defer lease.release()
	message := openedMessage{mid: request.mid, includeSoftDeleted: request.mode&openMessageIncludeSoftDeleted != 0, codePage: cp}
	row, err := store.readOpenedMessage(ctx, session, fid, message)
	if err != nil {
		return fail(storeError(err))
	}
	response, code := openMessageResponse(op.ID, op.Output, row, request.limit)
	if code != 0 {
		return fail(code)
	}
	message.recipientDigest = digestRecipients(row.object.Recipients)
	if row.special == 0 {
		message.contentRevision, message.contentVersioned = row.object.Properties[core.ContentRevision].(core.Integer)
	}
	if row.associated && editFAI && (mode != 0 || isRuleOrganizer(row.object.Properties)) {
		message.draft, err = store.editOpenedFAI(ctx, row)
		if err != nil {
			return fail(faiError(err, false))
		}
		message.draft.readOnly = mode == 0
	}
	if row.privateConfig && editFAI && mode != 0 {
		message.draft, err = store.editFreeBusyMessage(row)
		if err != nil {
			return fail(faiError(err, false))
		}
	}
	if !row.associated && !row.privateConfig && mode != 0 {
		if editFAI && row.special == core.ContactsFolder && row.object.Parent != store.trash && !message.includeSoftDeleted {
			message.contactDraft, err = store.readEditableContact(ctx, session, row)
			if err != nil && (mode == 1 || !errors.Is(err, core.ErrUnsupported) && !errors.Is(err, core.ErrLimit)) {
				return fail(faiError(err, false))
			}
		} else if editFAI && row.special == core.CalendarFolder {
			// The native Calendar read above returns only live items scoped
			// to this folder, even when OpenSoftDeleted is also requested.
			message.appointmentDraft, err = store.readEditableAppointment(ctx, session, row)
			if err != nil && (mode == 1 || !errors.Is(err, core.ErrUnsupported) && !errors.Is(err, core.ErrLimit)) {
				return fail(faiError(err, false))
			}
		} else if editMail && row.special == 0 && !message.includeSoftDeleted && row.object.Properties[core.IsDraft] == core.Bool(true) {
			message.mailDraft, err = store.editMailDraft(ctx, session, row)
			if err != nil && (mode == 1 || !errors.Is(err, core.ErrUnsupported)) {
				return fail(faiError(err, false))
			}
		} else if mode == 1 {
			return fail(accessDenied)
		}
	}
	handle, err := objects.allocator.handle()
	if err != nil {
		message.draft.close()
		message.mailDraft.close()
		message.contactDraft.close()
		message.appointmentDraft.close()
		return fail(0x8007000e)
	}
	objects.objects[handle] = serverObject{logon: op.Logon, binding: input.binding, fid: fid, message: &message}
	objects.allocated++
	handles[op.Output] = handle
	return response
}

func openedSubject(props core.Properties) (subject, prefix, normalized core.Text) {
	subject, _ = props[core.Subject].(core.Text)
	p, n := mailSubjectParts(string(subject))
	prefix, normalized = core.Text(p), core.Text(n)
	if value, ok := props[core.Key{ID: 0x003d}].(core.Text); ok {
		prefix = value
	}
	if value, ok := props[core.Key{ID: 0x0e1d}].(core.Text); ok {
		normalized = value
	}
	return subject, prefix, normalized
}

const ropReloadCachedInformation = 0x10
const ropGetMessageStatus = 0x1f
const ropSetMessageStatus = 0x20

func messageStatus(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	// GetMessageStatus uses the SetMessageStatus response ID (MS-OXCROPS 2.2.6.9.2).
	reply := func(code uint32) []byte { return ropError(ropSetMessageStatus, op.Input, code) }
	object, code := objects.object(handles[op.Input], op.Logon)
	if code == NotSupported || code == 0 && (object.isLogon || object.download != nil) {
		return reply(nullObject)
	}
	if code != 0 {
		return reply(code)
	}
	if op.ID == ropSetMessageStatus {
		const inConflict, remoteFlags = 0x0800, 0x3000
		if op.MessageStatusMask & ^uint32(inConflict|remoteFlags) != 0 {
			return reply(0x80070057)
		}
		requested := op.MessageStatusFlags & op.MessageStatusMask
		if requested&inConflict != 0 {
			return reply(accessDenied)
		}
		if requested&remoteFlags != 0 {
			return reply(NotSupported)
		}
	}
	store := object.binding.store
	lease, err := store.reserveContentsPeak(ctx, openMessageWorkspace)
	if err != nil {
		return reply(storeError(err))
	}
	defer lease.release()
	row, err := store.readOpenedMessage(ctx, session, object.fid, openedMessage{mid: op.MessageStatusMID})
	if err != nil {
		return reply(storeError(err))
	}
	if row.associated || row.special != 0 || row.privateConfig || row.object.ID == "" {
		return reply(PropertyNotFound)
	}
	// Active native mail has no remote-download or conflict state.
	return append32(reply(0), 0)
}

func reloadCachedInformation(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if object.message == nil {
		return fail(NotSupported)
	}
	store := object.binding.store
	lease, err := store.reserveContentsPeak(ctx, openMessageWorkspace)
	if err != nil {
		return fail(storeError(err))
	}
	defer lease.release()
	row, err := store.readOpenedMessage(ctx, session, object.fid, *object.message)
	if err != nil {
		return fail(storeError(err))
	}
	response, code := openMessageResponse(op.ID, op.Input, row, op.OpenMessage.limit)
	if code != 0 {
		return fail(code)
	}
	object.message.recipientDigest = digestRecipients(row.object.Recipients)
	return response
}

func openMessageResponse(rop, index byte, message messageRow, limit int) ([]byte, uint32) {
	object := message.object
	var hasNamed byte
	if message.associated || message.special != 0 || nativeMeetingKind(object.Properties) != 0 {
		// Native envelopes carry backend metadata in private core namespaces.
		// FAI and calendar search rows carry MAPI named properties here.
		for key := range object.Properties {
			if key.Set != "" {
				hasNamed = 1
				break
			}
		}
	}
	response := append(ropError(rop, index, 0), hasNamed)
	_, prefix, normalized := openedSubject(object.Properties)
	for _, value := range []core.Text{prefix, normalized} {
		if value == "" {
			response = append(response, 1) // TypedString, Empty.
			continue
		}
		encoded, ok := propertyString(string(value), 0x001f, 0)
		if !ok || len(encoded) > maxFolderStringBytes {
			return nil, NotSupported
		}
		response = append(append(response, 4), encoded...) // TypedString, Unicode.
	}
	if len(object.Recipients) > core.MaxMailContentMessages {
		return nil, NotSupported
	}
	response = binary.LittleEndian.AppendUint16(response, uint16(len(object.Recipients)))
	meeting, organizer := hasCalendarRecipientRows(message), receivedMeetingOrganizer(message)
	if meeting {
		response = binary.LittleEndian.AppendUint16(response, uint16(len(meetingRecipientColumns)))
		for _, tag := range meetingRecipientColumns {
			response = append32(response, tag)
		}
	} else {
		response = append(response, 0, 0) // No additional recipient columns.
	}
	response = append(response, 0) // RowCount.
	if len(response) > limit {
		return nil, tableBufferTooSmall
	}
	rowCount := len(response) - 1
	for index, recipient := range object.Recipients {
		if response[rowCount] == 255 {
			break
		}
		row, ok := encodeMessageRecipient(recipient, meeting, organizer && index == 0)
		if !ok {
			return nil, NotSupported
		}
		if len(row) > limit-len(response) {
			break
		}
		response = append(response, row...)
		response[rowCount]++
	}
	return response, 0
}

func encodeOpenRecipient(recipient core.Recipient) ([]byte, bool) {
	return encodeMessageRecipient(recipient, false, false)
}

func encodeMessageRecipient(recipient core.Recipient, meeting, organizer bool) ([]byte, bool) {
	var role byte
	switch recipient.Kind {
	case core.RecipientTo:
		role = 1
	case core.RecipientCc:
		role = 2
	case core.RecipientBcc:
		role = 3
	default:
		return nil, false
	}
	address, ok := propertyString(recipient.Address, 0x001f, 0)
	if !ok || recipient.Address == "" {
		return nil, false
	}
	flags := uint16(0x020b) // Unicode, email present, SMTP.
	wire := append(binary.LittleEndian.AppendUint16(nil, flags), address...)
	if recipient.DisplayName != "" {
		name, ok := propertyString(recipient.DisplayName, 0x001f, 0)
		if !ok {
			return nil, false
		}
		binary.LittleEndian.PutUint16(wire, flags|0x10)
		wire = append(wire, name...)
	}
	if meeting {
		wire = binary.LittleEndian.AppendUint16(wire, uint16(len(meetingRecipientColumns)))
		wire = append(wire, 0) // PropertyRow with no errors.
		for _, value := range meetingRecipientValues(recipient, organizer) {
			switch v := value.(type) {
			case core.Integer:
				wire = append32(wire, uint32(v))
			case core.Bool:
				wire = append(wire, 1)
			case core.Text:
				wire = append(wire, contentsUnicode(string(v))...)
			case core.Binary:
				if len(v) > 65535 {
					return nil, false
				}
				wire = binary.LittleEndian.AppendUint16(wire, uint16(len(v)))
				wire = append(wire, v...)
			}
		}
	} else {
		wire = append(wire, 0, 0, 0) // RecipientColumnCount and empty PropertyRow.
	}
	if len(wire) > 65535 {
		return nil, false
	}
	row := binary.LittleEndian.AppendUint16([]byte{role}, 1200)
	row = append(row, 0, 0) // Reserved.
	row = binary.LittleEndian.AppendUint16(row, uint16(len(wire)))
	return append(row, wire...), true
}

func getOpenedMessageProperties(ctx context.Context, session core.Session, object serverObject, op Operation, diagnostic *StorePropertyDiagnostic) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	diagnostic.Message = true
	if attachment := object.message.calendarException; attachment != nil {
		return readCalendarExceptionProperties(ctx, session, object, op, diagnostic)
	}
	store := object.binding.store
	request := *op.Properties
	loadContents := (object.message.contentVersioned || object.message.mailDraft != nil) && slices.ContainsFunc(request.tags, messageContentPropertyTag)
	embedded := object.message.embedded != nil
	workspace := openMessageWorkspace
	if loadContents || embedded {
		workspace = 2 * maxMailProjectionBytes
	}
	lease, err := store.reserveContentsPeak(ctx, workspace)
	if err != nil {
		return fail(storeError(err))
	}
	defer lease.release()
	draft := object.message.draft
	if draft != nil {
		draft.mu.Lock()
		defer draft.mu.Unlock()
	}
	var row messageRow
	var contents projectedMessage
	if embedded {
		contents, err = store.readOpenedMailProjection(ctx, session, object.fid, *object.message)
		if err != nil {
			return fail(attachmentError(err))
		}
		row = contents.embeddedRow(object.message.mid)
	} else {
		row, err = store.messagePropertyRow(ctx, session, object)
	}
	if err != nil {
		if err == core.ErrConflict && draft != nil && draft.dirty {
			return fail(faiError(err, true))
		}
		return fail(storeError(err))
	}
	var meetingValues contentsValues
	if !row.associated && row.special == 0 && nativeMeetingKind(row.object.Properties) != 0 {
		meetingValues = make(contentsValues)
		projectMeetingProperties(meetingValues, row.object.Properties)
		store.projectMeetingDelivery(meetingValues, row.object, session.Identity())
	}
	if op.ID == ropGetPropertiesAll || op.ID == 0x09 && (row.special == core.CalendarFolder || meetingValues != nil) {
		request.tags = allMessagePropertyTags(row, request, &store.named)
		if meetingValues != nil {
			if err := registerMeetingNames(ctx, &store.named); err != nil {
				return fail(storeError(err))
			}
			store.named.mu.Lock()
			for property := range meetingValues {
				id := property.id
				if id == 0 {
					id = store.named.byKey[property.name]
				}
				if id != 0 {
					request.tags = append(request.tags, uint32(id)<<16|uint32(property.kind))
				}
			}
			store.named.mu.Unlock()
			request.tags = enumeratedPropertyTags(request.tags, request.wantUnicode)
		}
		op.Properties = &request
	}
	writable := draft != nil && !draft.readOnly || object.message.mailDraft != nil && !object.message.mailDraft.readOnly ||
		object.message.contactDraft != nil && !object.message.contactDraft.readOnly ||
		object.message.appointmentDraft != nil && !object.message.appointmentDraft.readOnly
	var contentError uint32
	if loadContents && !embedded {
		contents, err = store.readOpenedMailProjection(ctx, session, object.fid, *object.message)
		if err != nil {
			contentError = attachmentError(err)
		}
	}
	var hasDeferred folderPropertyValue
	if !embedded && !isDeferredAction(row) && !row.associated && row.special == 0 && slices.ContainsFunc(request.tags, func(tag uint32) bool { return tag>>16 == hasDAMs>>16 }) {
		hasDeferred = openedMessageHasDeferredActions(ctx, session, row)
	}

	values := make([]folderPropertyValue, len(request.tags))
	for i, tag := range request.tags {
		var value folderPropertyValue
		switch {
		case loadContents && messageContentPropertyTag(tag):
			if contentError != 0 {
				value = folderPropertyValue{kind: 0x000a, code: contentError}
			} else if op.ID == 0x09 {
				if contents.values[fixedContentsProperty(tag)] == nil {
					value.code = PropertyNotFound
				}
			} else {
				value = openedMailContentProperty(contents.values, tag, request.wantUnicode, object.message.codePage, request.variableBudget)
			}
		case hasDeferred.kind != 0 && (tag == hasDAMs || tag == hasDAMs&0xffff0000):
			value = hasDeferred
		case row.special == core.ContactsFolder:
			var handled bool
			value, handled = openedContactField(row, tag, &store.named, request.wantUnicode, object.message.codePage, request.variableBudget)
			if !handled {
				value = openedMessageProperty(row, object.binding, tag, request.wantUnicode, object.message.codePage, writable, request.variableBudget)
			}
		case row.special != 0:
			var handled bool
			calendarRequest := request
			if op.ID == 0x09 {
				calendarRequest.variableBudget = 0xffff // Only the tag is returned.
			}
			value, handled = openedCalendarProperty(row, tag, &store.named, calendarRequest, object.message.codePage)
			if !handled {
				value = openedMessageProperty(row, object.binding, tag, request.wantUnicode, object.message.codePage, writable, request.variableBudget)
			}
		case meetingValues != nil:
			var handled bool
			partsRequest := request
			if op.ID == 0x09 {
				partsRequest.variableBudget = 0xffff
			}
			value, handled = openedMeetingProperty(meetingValues, tag, &store.named, partsRequest, object.message.codePage)
			if !handled {
				value = openedMessageProperty(row, object.binding, tag, request.wantUnicode, object.message.codePage, writable, request.variableBudget)
			}
		default:
			value = openedMessageProperty(row, object.binding, tag, request.wantUnicode, object.message.codePage, writable, request.variableBudget)
		}
		if value.code == 0 && (variableMessageProperty(tag) || row.special == core.ContactsFolder && contactVariableProperty(tag) ||
			(row.special == core.CalendarFolder || meetingValues != nil) && (value.kind == 0x001e || value.kind == 0x001f || value.kind == 0x0102)) {
			request.variableBudget -= max(0, len(value.data)-4) // Four bytes reserved for a property error.
		}
		values[i] = value
		if value.code != 0 {
			diagnostic.Errors++
		} else {
			diagnostic.Values++
		}
	}
	return propertyReadResponse(op, values)
}

func allMessagePropertyTags(row messageRow, request specificProperties, names *namedPropertyRegistry) []uint32 {
	tags := append(slices.Clone(request.tags), 0x30070040)
	switch {
	case isDeferredAction(row):
		tags = append(tags, damTags[:]...)
	case row.associated:
		for key := range row.object.Properties {
			if key.Set == "" && key.ID <= 0x7fff {
				if kind := associatedSpecificPropertyType(uint16(key.ID)); kind != 0 {
					tags = append(tags, key.ID<<16|uint32(kind))
				}
			}
		}
	case row.privateConfig:
		tags = append(tags, 0x6842000b, 0x684b000b, 0x67aa000b)
	case row.special == core.ContactsFolder:
		tags = append(tags, contactPropertyTags(row.object.Properties, names)...)
	case row.special != 0:
		if row.special == core.CalendarFolder {
			for _, tag := range appointmentFixedProperties {
				if row.object.Properties[core.Key{ID: tag >> 16}] != nil {
					tags = append(tags, tag)
				}
			}
		}
		names.mu.Lock()
		for _, field := range calendarProperties {
			if id := names.byKey[field.property.name]; id != 0 && row.object.Properties[field.key] != nil {
				tags = append(tags, uint32(id)<<16|uint32(field.property.kind))
			}
		}
		names.mu.Unlock()
	}
	return enumeratedPropertyTags(tags, request.wantUnicode)
}

// The caller holds the draft lock, if any. A newly created associated message
// has a handle and properties but has no globally visible registry entry.
func (s *ramStore) messagePropertyRow(ctx context.Context, session core.Session, object serverObject) (messageRow, error) {
	draft := object.message.draft
	if appointment := object.message.appointmentDraft; appointment != nil && object.message.mid == 0 {
		return appointment.unsavedRow(ctx, session, object)
	}
	if contact := object.message.contactDraft; contact != nil && object.message.mid == 0 {
		if err := contact.refresh(ctx, session, object); err != nil {
			return messageRow{}, err
		}
		if object.message.mid == 0 {
			return contact.unsavedRow(ctx, session, object)
		}
	}
	if draft != nil && draft.newMID != 0 && !draft.saved {
		if err := s.authorizeLocalMessage(ctx, session, object.fid, draft.privateConfig); err != nil {
			return messageRow{}, err
		}
		return messageRow{
			parent: object.fid, mid: draft.newMID, associated: !draft.privateConfig, privateConfig: draft.privateConfig,
			object: core.Object{Properties: draft.properties},
		}, nil
	}
	row, err := s.readOpenedMessage(ctx, session, object.fid, *object.message)
	if err != nil {
		return messageRow{}, err
	}
	if draft != nil && draft.dirty {
		if row.object.Revision != draft.base {
			return messageRow{}, core.ErrConflict
		}
		row.object.Properties = draft.properties
	}
	if contact := object.message.contactDraft; contact != nil {
		return contact.preview(row)
	}
	if appointment := object.message.appointmentDraft; appointment != nil {
		return appointment.preview(row)
	}
	return row, nil
}

func openedMessageProperty(row messageRow, binding storeBinding, tag uint32, unicode bool, cp uint32, writable bool, binaryBudget int) folderPropertyValue {
	id, requested := uint16(tag>>16), uint16(tag)
	missing := folderPropertyValue{kind: 0x000a, code: PropertyNotFound}

	if isDeferredAction(row) {
		for _, candidate := range damTags {
			if uint16(candidate>>16) == id {
				values := make(contentsValues, 1)
				putContentsValue(values, candidate, deferredActionProperty(row, binding, candidate))
				if requested == 0 {
					tag = candidate
				}
				return tableCell(values, tag, nil, cp)
			}
		}
	}

	var value folderPropertyValue
	var text core.Text
	switch id {
	case 0x001a:
		text = mailMessageClass(row.object.Properties)
		if row.associated || row.privateConfig || isDeferredAction(row) {
			text, _ = row.object.Properties[core.Key{ID: uint32(id)}].(core.Text)
		} else if row.special == core.ContactsFolder {
			text = contactClass
			if row.mid == 0 {
				text, _ = row.object.Properties[core.Key{ID: uint32(id)}].(core.Text)
			}
		} else if row.special != 0 {
			text = core.Text(searchCalendarClass(row.special))
		}
	case 0x0e07:
		var flags uint32
		if row.special == 0 {
			_, known := row.object.Properties[core.IsRead].(core.Bool)
			if row.associated || !known {
				return missing
			}
			flags = mailContentsFlags(row.object, row.object.Properties[core.HasAttachments] == core.Bool(true))
			if binding.store.isQueuedSubmission(row.mid) {
				flags |= 4
			}
		} else if row.read {
			flags = 1
		}
		value = folderPropertyValue{kind: 3, data: append32(nil, flags)}
	case 0x0e17: // MessageStatus for active native mail has no remote or conflict flags.
		if row.associated || row.special != 0 || row.object.ID == "" {
			return missing
		}
		value = folderPropertyValue{kind: 3, data: append32(nil, 0)}
	case flagStatusTag >> 16, flagCompleteTimeTag >> 16, followupIconTag >> 16:
		if row.associated || row.privateConfig || row.special != 0 {
			return missing
		}
		values := contentsValues{}
		projectMailFollowUp(values, row.object.Properties)
		if requested == 0 {
			tag |= 0x0003
			if id == flagCompleteTimeTag>>16 {
				tag = flagCompleteTimeTag
			}
		}
		return tableCell(values, tag, nil, cp)
	case 0x0e1b, 0x0e69:
		if row.associated || row.special != 0 {
			return missing
		}
		key := core.HasAttachments
		if id == 0x0e69 {
			key = core.IsRead
		}
		flag, known := row.object.Properties[key].(core.Bool)
		if !known {
			return missing
		}
		data := byte(0)
		if flag {
			data = 1
		}
		value = folderPropertyValue{kind: 0x000b, data: []byte{data}}
	case 0x0e08: // MessageSize.
		size, known := row.object.Properties[core.MessageSize].(core.Integer)
		if !known || size < 0 || size > 1<<31-1 {
			return missing
		}
		value = folderPropertyValue{kind: 3, data: append32(nil, uint32(size))}
	case 0x0e09, 0x0ff9, 0x0ffa, 0x0fff:
		if row.associated || (row.special != 0 && row.special != core.ContactsFolder && row.special != core.CalendarFolder) || row.object.ID == "" {
			return missing
		}
		var entry []byte
		switch id {
		case 0x0fff:
			entry = marshalMessageEntryID(binding.mailboxGUID, binding.store.replica, row.parent, row.mid)
		case 0x0ff9:
			// Compare as opaque bytes. A MID and a FID come from the same
			// replica counter. The key therefore survives a parent change.
			entry = marshalFolderEntryID(binding.mailboxGUID, binding.store.replica, row.mid)
		case 0x0ffa:
			entry = binding.mailboxGUID[:]
		default:
			entry = marshalFolderEntryID(binding.mailboxGUID, binding.store.replica, row.parent)
		}
		value = folderPropertyValue{kind: 0x0102, data: append(binary.LittleEndian.AppendUint16(nil, uint16(len(entry))), entry...)}
	case 0x0ffe:
		value = folderPropertyValue{kind: 3, data: append32(nil, 5)} // MAPI_MESSAGE.
	case 0x65e0:
		if row.sourceKey == ([22]byte{}) {
			return missing
		}
		value = folderPropertyValue{kind: 0x0102, data: append([]byte{22, 0}, row.sourceKey[:]...)}
	case 0x0037, 0x003d, 0x0e1d:
		if row.associated && row.object.Properties[core.Subject] == nil && row.object.Properties[core.Key{ID: uint32(id)}] == nil {
			return missing
		}
		subject, prefix, normalized := openedSubject(row.object.Properties)
		switch id {
		case 0x0037:
			text = subject
		case 0x003d:
			text = prefix
		case 0x0e1d:
			text = normalized
		}
	case 0x0e06, 0x3007, 0x3008:
		key := core.ReceivedAt
		if id == 0x3007 {
			key = core.Key{ID: 0x3007}
		} else if id == 0x3008 {
			key = core.LastModifiedAt
		}
		stamp, ok := row.object.Properties[key].(core.Timestamp)
		if !ok {
			return missing
		}
		value = folderPropertyValue{kind: 0x0040, data: binary.LittleEndian.AppendUint64(nil, fileTime(time.Time(stamp)))}
	case 0x0ff7: // AccessLevel.
		var accessLevel uint32
		if writable {
			accessLevel = 1
		}
		value = folderPropertyValue{kind: 3, data: append32(nil, accessLevel)}
	case 0x65e2:
		if !row.changeKey.valid() {
			return missing
		}
		value = folderPropertyValue{kind: 0x0102, data: append(binary.LittleEndian.AppendUint16(nil, uint16(len(row.changeKey))), []byte(row.changeKey)...)}
	case 0x6748, 0x674a:
		number := row.parent
		if id == 0x674a {
			number = row.mid
			if number == 0 {
				return missing
			}
		}
		value = folderPropertyValue{kind: 0x0014, data: binary.LittleEndian.AppendUint64(nil, number)}
	default:
		if row.privateConfig && (id == 0x6842 || id == 0x684b || id == 0x67aa) {
			flag, ok := row.object.Properties[core.Key{ID: uint32(id)}].(core.Bool)
			if !ok {
				return missing
			}
			data := byte(0)
			if flag {
				data = 1
			}
			value = folderPropertyValue{kind: 0x000b, data: []byte{data}}
			break
		}
		if !row.associated {
			return missing
		}
		property := row.object.Properties[core.Key{ID: uint32(id)}]
		kind := associatedSpecificPropertyType(id)
		if property == nil || !contentsValueType(kind, property) {
			return missing
		}
		switch kind {
		case 0x001f:
			text = property.(core.Text)
		case 0x0003:
			value = folderPropertyValue{kind: 3, data: append32(nil, uint32(property.(core.Integer)))}
		case 0x0102:
			data := property.(core.Binary)
			if requested != 0 && requested != kind {
				return folderPropertyValue{kind: 0x000a, code: NotSupported}
			}
			if len(data)+2 > binaryBudget+4 {
				return folderPropertyValue{kind: 0x000a, code: 0x8007000e}
			}
			value = folderPropertyValue{kind: 0x0102, data: append(binary.LittleEndian.AppendUint16(nil, uint16(len(data))), data...)}
		default:
			return missing
		}
	}
	if value.kind == 0 {
		kind := requested
		if kind == 0 {
			kind = 0x001e
			if unicode {
				kind = 0x001f
			}
		}
		data, ok := propertyString(string(text), kind, cp)
		if !ok || len(data) > maxFolderStringBytes {
			return folderPropertyValue{kind: 0x000a, code: NotSupported}
		}
		return folderPropertyValue{kind: kind, data: data}
	}
	if requested != 0 && requested != value.kind {
		return folderPropertyValue{kind: 0x000a, code: NotSupported}
	}
	return value
}

func isAssociatedSpecificBinary(tag uint32) bool {
	kind := uint16(tag)
	return (kind == 0 || kind == 0x0102) && associatedSpecificPropertyType(uint16(tag>>16)) == 0x0102
}

func associatedSpecificPropertyType(id uint16) uint16 {
	switch id {
	case 0x3001:
		return 0x001f
	case 0x0017, 0x0036, 0x7c06: // 0x7c06 is PidTagRoamingDatatypes.
		return 0x0003
	case 0x7c07, 0x7c08:
		return 0x0102
	default:
		return associatedMetadataPropertyType(uint32(id))
	}
}
