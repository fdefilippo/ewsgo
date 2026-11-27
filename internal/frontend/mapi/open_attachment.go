// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"

	"ewsgo/internal/core"
)

type openAttachmentRequest struct {
	mode   byte
	number uint32
}

type openedAttachment struct {
	message openedMessage
	number  uint32
	draft   *draftAttachment
}

func attachmentError(err error) uint32 {
	if errors.Is(err, core.ErrConflict) {
		return 0x80040109 // MAPI_E_OBJECT_CHANGED.
	}
	return storeError(err)
}

func (m openedMessage) requireContentRevision(object core.Object) error {
	if !m.contentVersioned || object.Properties[core.ContentRevision] != m.contentRevision {
		return core.ErrConflict
	}
	return nil
}

// MIME views remain usable only while their parent and content revision
// still match. Empty appointment tables must still have no attachments.
func (s *ramStore) authorizeAttachmentContent(ctx context.Context, session core.Session, fid uint64, message openedMessage) error {
	if message.mailDraft != nil {
		return message.mailDraft.authorizeAttachments(ctx, session)
	}
	if message.calendarException != nil {
		return message.calendarException.authorize(ctx, session)
	}
	message = message.rootMessage()
	if s.isAppointmentMID(message.mid) {
		_, err := s.readOpenedAttachments(ctx, session, [16]byte{}, fid, message)
		return err
	}
	row, err := s.readOpenedMessage(ctx, session, fid, message)
	if err != nil {
		return err
	}
	if !row.associated {
		return message.requireContentRevision(row.object)
	}
	return ctx.Err()
}

func (s *ramStore) readOpenedAttachments(ctx context.Context, session core.Session, mailboxGUID [16]byte, fid uint64, message openedMessage) ([]contentsValues, error) {
	if message.mailDraft != nil {
		return message.mailDraft.attachmentRows(ctx, session)
	}
	if message.calendarException != nil {
		return nil, message.calendarException.authorize(ctx, session)
	}
	if message.embedded != nil {
		projection, err := s.readOpenedMailProjection(ctx, session, fid, message)
		if err != nil {
			return nil, err
		}
		prefix := append([]byte("EWSGo MAPI embedded attachment record key"), mailboxGUID[:]...)
		prefix = binary.LittleEndian.AppendUint64(prefix, message.mid)
		return attachmentRecordKeys(projection, prefix, ""), ctx.Err()
	}
	key, folder, err := s.openedMessageScope(ctx, session, fid, message)
	if err != nil {
		return nil, err
	}
	if key.associated {
		s.contentsMu.Lock()
		defer s.contentsMu.Unlock()
		_, err = s.readFAIRowLocked(ctx, fid, key.id)
		return nil, err // The FAI store does not contain attachments.
	}
	if key.special == core.CalendarFolder {
		reader, ok := session.Mailbox().(core.AppointmentContentsReader)
		if !ok {
			return nil, core.ErrUnsupported
		}
		item, err := readOpenedCalendarAppointment(ctx, reader, folder, key)
		if err != nil {
			return nil, err
		}
		switch item.Type {
		case core.AppointmentSingle:
		case core.AppointmentMaster:
			if item.SeriesSchedule == nil || len(item.SeriesSchedule.Exceptions) != 0 {
				return nil, core.ErrUnsupported
			}
		default:
			return nil, core.ErrUnsupported
		}
		if item.Identity.Properties[core.HasAttachments] != core.Bool(false) {
			return nil, core.ErrUnsupported
		}
		return nil, ctx.Err()
	}
	object, err := readOpenedMessageMIME(ctx, session, folder, key, message)
	if err != nil {
		return nil, err
	}
	projection, err := projectMailContents(object)
	if err != nil {
		return nil, err
	}
	// The same MIME generation and ordinal identify the Attachment object
	// across table opens, Attachment opens, and MAPI reconnects.
	prefix := append([]byte("EWSGo MAPI attachment record key"), mailboxGUID[:]...)
	prefix = binary.LittleEndian.AppendUint64(prefix, uint64(message.contentRevision))
	return attachmentRecordKeys(projection, prefix, object.ID), ctx.Err()
}

func attachmentRecordKeys(projection projectedMessage, prefix []byte, native core.ID) []contentsValues {
	prefix = append32(prefix, 0)
	for number, row := range projection.attachments {
		binary.LittleEndian.PutUint32(prefix[len(prefix)-4:], uint32(number))
		key := sha256.Sum256(append(prefix, string(native)...))
		putContentsValue(row, 0x0ff90102, core.Binary(key[:]))
	}
	return projection.attachments
}

func readOpenedMessageMIME(ctx context.Context, session core.Session, folder folderKey, key messageKey, message openedMessage) (core.Object, error) {
	if key.special != 0 {
		return core.Object{}, core.ErrUnsupported
	}
	reader, ok := session.Mailbox().(core.MailMessageMIMEReader)
	if !ok || !message.contentVersioned {
		return core.Object{}, core.ErrUnsupported
	}
	object, err := reader.ReadMailMessageMIME(ctx, folder.native, key.id, message.includeSoftDeleted)
	if err != nil {
		return core.Object{}, err
	}
	if object.Kind != core.Message || object.ID != key.id || object.Parent != folder.native {
		return core.Object{}, core.ErrNotFound
	}
	if err = message.requireContentRevision(object); err != nil {
		return core.Object{}, err
	}
	return object, ctx.Err()
}

func executeAttachment(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	index := op.Input
	if op.OpenAttachment != nil {
		index = op.Output
	}
	fail := func(code uint32) []byte { return ropError(op.ID, index, code) }
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if object.message == nil {
		return fail(nullObject)
	}
	if open := op.OpenAttachment; open != nil {
		switch open.mode {
		case 0, 3: // ReadOnly, BestAccess.
		case 1:
			return fail(accessDenied)
		default:
			return fail(0x80070057)
		}
		if len(objects.objects) >= maxLiveStoreHandles || objects.allocated >= maxContextHandleAllocations {
			return fail(0x8007000e)
		}
	}
	store := object.binding.store
	lease, err := store.reserveContentsPeak(ctx, 2*maxMailProjectionBytes)
	if err != nil {
		return fail(storeError(err))
	}
	defer lease.release()
	rows, err := store.readOpenedAttachments(ctx, session, object.binding.mailboxGUID, object.fid, *object.message)
	if err != nil {
		return fail(attachmentError(err))
	}
	if err := ctx.Err(); err != nil {
		return fail(storeError(err))
	}
	if op.OpenAttachment == nil {
		if 8+4*len(rows) > op.AttachmentLimit {
			return fail(tableBufferTooSmall)
		}
		response := binary.LittleEndian.AppendUint16(fail(0), uint16(len(rows)))
		for _, row := range rows {
			response = append32(response, attachmentNumber(row))
		}
		return response
	}
	if _, exists := findAttachmentRow(rows, op.OpenAttachment.number); !exists {
		return fail(PropertyNotFound)
	}
	handle, err := objects.allocator.handle()
	if err != nil {
		return fail(0x8007000e)
	}
	objects.objects[handle] = serverObject{
		logon: op.Logon, binding: object.binding, fid: object.fid,
		attachment: &openedAttachment{message: *object.message, number: op.OpenAttachment.number},
	}
	objects.allocated++
	handles[op.Output] = handle
	return fail(0)
}

func getOpenedAttachmentProperties(ctx context.Context, session core.Session, object serverObject, op Operation, diagnostic *StorePropertyDiagnostic) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	store := object.binding.store
	lease, err := store.reserveContentsPeak(ctx, 2*maxMailProjectionBytes)
	if err != nil {
		return fail(storeError(err))
	}
	defer lease.release()
	attachment := object.attachment
	var row contentsValues
	if attachment.draft != nil {
		if err := attachment.draft.authorize(ctx, session); err != nil {
			return fail(attachmentError(err))
		}
		row = attachment.draft.properties()
	} else {
		rows, err := store.readOpenedAttachments(ctx, session, object.binding.mailboxGUID, object.fid, attachment.message)
		if err != nil {
			return fail(attachmentError(err))
		}
		var exists bool
		row, exists = findAttachmentRow(rows, attachment.number)
		if !exists {
			return fail(PropertyNotFound)
		}
		putContentsValue(row, 0x0ff70003, core.Integer(0)) // Read-only.
	}
	if err := ctx.Err(); err != nil {
		return fail(storeError(err))
	}
	putContentsValue(row, 0x0ffe0003, core.Integer(7)) // ObjectType, MAPI_ATTACH.
	if op.ID == 0x09 || op.ID == ropGetPropertiesAll {
		tags := make([]uint32, 0, len(row))
		for property := range row {
			switch property.kind {
			case 3, 0x001f, 0x0102:
				tags = append(tags, uint32(property.id)<<16|uint32(property.kind))
			}
		}
		if op.ID == 0x09 {
			return propertyTagsResponse(op, tags)
		}
		request := *op.Properties
		request.tags = enumeratedPropertyTags(tags, request.wantUnicode)
		op.Properties = &request
		op.AttachmentLimit = op.PropertyListLimit
	}
	request := *op.Properties
	remaining := op.AttachmentLimit - 7
	for _, tag := range request.tags {
		remaining -= 5 // Reserve the worst-case flag and four-byte property error.
		if uint16(tag) == 0 {
			remaining -= 2
		}
	}
	values := make([]folderPropertyValue, len(request.tags))
	for i, tag := range request.tags {
		value := attachmentProperty(row, tag, request.wantUnicode, attachment.message.codePage, 4+remaining)
		values[i] = value
		if value.code == 0 {
			remaining -= max(0, len(value.data)-4)
			diagnostic.Values++
		} else {
			diagnostic.Errors++
		}
	}
	return propertyReadResponse(op, values)
}

func attachmentProperty(row contentsValues, tag uint32, unicode bool, codePage uint32, budget int) folderPropertyValue {
	fail := func(code uint32) folderPropertyValue { return folderPropertyValue{kind: 0x000a, code: code} }
	id, kind := uint16(tag>>16), uint16(tag)
	var value folderPropertyValue
	if kind == 0 || kind == 3 {
		if number, ok := row[contentsProperty{id: id, kind: 3}].(core.Integer); ok {
			return folderPropertyValue{kind: 3, data: append32(nil, uint32(number))}
		}
	}
	switch kind {
	case 0, 0x0102:
		if data, ok := row[contentsProperty{id: id, kind: 0x0102}].(core.Binary); ok {
			if len(data) > min(65535, budget-2) {
				return fail(0x8007000e)
			}
			return folderPropertyValue{kind: 0x0102, data: append(binary.LittleEndian.AppendUint16(nil, uint16(len(data))), data...)}
		}
	case 0x001e, 0x001f:
		// Text properties are stored as Unicode and converted on output.
	default:
		return fail(PropertyNotFound)
	}
	text, ok := row[contentsProperty{id: id, kind: 0x001f}].(core.Text)
	if !ok {
		return fail(PropertyNotFound)
	}
	if kind == 0 {
		kind = 0x001e
		if unicode {
			kind = 0x001f
		}
	}
	value.kind = kind
	value.data, ok = propertyString(string(text), kind, codePage)
	if !ok || len(value.data) > budget {
		return fail(0x8007000e)
	}
	return value
}
