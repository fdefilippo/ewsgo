// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"mime"
	"strings"

	"ewsgo/internal/core"
	"ewsgo/internal/mailmime"
)

const (
	ropCreateAttachment     byte = 0x23
	ropSaveAttachment       byte = 0x25
	attachNumberTag              = 0x0e210003
	attachmentCoverageWords      = (core.MaxMailContentMIMEBytes + 63) / 64
)

type draftAttachment struct {
	parent                      *mailDraft
	importParent                *mailImportDraft
	calendar                    *calendarExceptionDraft
	lease                       *contentsLease
	number                      uint32
	part                        int
	name, shortName, media, cid string
	data                        []byte
	unwritten                   []uint64
	unwrittenBytes              int
	generation                  uint64
	dataSet, dirty, readOnly    bool
}

func draftAttachmentTag(tag uint32) bool {
	switch tag {
	case attachDataBinary, 0x37050003, 0x3704001f, 0x3707001f, 0x3001001f, 0x370e001f, 0x3712001f, 0x370b0003:
		return true
	}
	return false
}

func (a *draftAttachment) close() {
	if a != nil {
		a.clearUnwritten()
		a.lease.release()
		a.lease, a.data = nil, nil
		a.calendar = nil
	}
}

func (d *mailDraft) authorizeAttachments(ctx context.Context, session core.Session) error {
	if d.lease == nil {
		return core.ErrNotFound
	}
	return d.authorize(ctx, session)
}

func (a *draftAttachment) authorize(ctx context.Context, session core.Session) error {
	if a.lease == nil {
		return core.ErrNotFound
	}
	if a.importParent != nil {
		if a.importParent.closed {
			return core.ErrClosed
		}
		return a.importParent.collector.state.authorize(ctx, session)
	}
	return a.parent.authorizeAttachments(ctx, session)
}

func (a *draftAttachment) editable() error {
	if a.readOnly {
		return core.ErrAccessDenied
	}
	if a.importParent != nil {
		if a.importParent.closed {
			return core.ErrClosed
		}
		return nil
	}
	return a.parent.editable()
}

func (a *draftAttachment) setProperties(ctx context.Context, session core.Session, values faiSetRequest) ([]faiPropertyProblem, error) {
	if err := a.authorize(ctx, session); err != nil {
		return nil, err
	}
	if err := a.editable(); err != nil {
		return nil, err
	}
	if a.calendar != nil {
		return a.calendar.setAttachment(values), nil
	}
	var problems []faiPropertyProblem
	for i, value := range values {
		var code uint32
		switch value.tag {
		case attachDataBinary:
			a.data = bytes.Clone(value.value.(core.Binary))
			a.clearUnwritten()
			a.dataSet, a.dirty = true, true
			a.generation++
		case 0x37050003:
			if value.value != core.Integer(1) {
				code = NotSupported // AttachByValue.
			}
		case 0x370b0003:
			if value.value != core.Integer(-1) {
				code = NotSupported // Inline rendering via RTF position.
			}
		case 0x3704001f, 0x3707001f, 0x3001001f, 0x370e001f, 0x3712001f:
			text := string(value.value.(core.Text))
			if len(text) > 255 || !mailmime.ValidHeaderValue(text) {
				code = 0x80070057
				break
			}
			switch value.tag {
			case 0x3704001f:
				a.shortName = text
			case 0x3707001f:
				a.name = text
			case 0x370e001f:
				a.media = text
			case 0x3712001f:
				a.cid = text
			case 0x3001001f:
				// The file name is used as the display name when reopened.
				if a.name == "" && a.shortName == "" {
					a.shortName = text
				}
			}
			a.dirty = true
		default:
			code = NotSupported
		}
		if code != 0 {
			problems = append(problems, faiPropertyProblem{index: uint16(i), tag: value.tag, code: code})
		}
	}
	return problems, nil
}

func (a *draftAttachment) properties() contentsValues {
	row := make(contentsValues)
	putContentsValue(row, attachNumberTag, core.Integer(a.number))
	if a.calendar != nil {
		for tag, value := range a.calendar.attachment {
			putContentsValue(row, tag, value)
		}
		putContentsValue(row, 0x0ff70003, core.Integer(1))
		return row
	}
	putContentsValue(row, 0x37050003, core.Integer(1))
	putContentsValue(row, 0x370b0003, core.Integer(-1))
	putContentsValue(row, 0x0e200003, core.Integer(len(a.data)))
	putContentsValue(row, 0x0ff70003, core.Integer(1))
	for tag, value := range map[uint32]string{0x3704001f: a.shortName, 0x3707001f: a.name, 0x370e001f: a.media, 0x3712001f: a.cid} {
		if value != "" {
			putContentsValue(row, tag, core.Text(value))
		}
	}
	if a.dataSet {
		putContentsValue(row, attachDataBinary, core.Binary(a.data))
	}
	return row
}

func (d *mailDraft) attachmentRows(ctx context.Context, session core.Session) ([]contentsValues, error) {
	if err := d.authorizeAttachments(ctx, session); err != nil {
		return nil, err
	}
	projection, err := projectMailContents(d.staged)
	if err != nil {
		return nil, err
	}
	rows := projection.attachments
	if d.attachmentNumbers == nil {
		d.attachmentNumbers = make([]uint32, len(rows))
		for i := range rows {
			d.attachmentNumbers[i] = uint32(i)
		}
		d.nextAttachment = uint32(len(rows))
	}
	if len(rows) != len(d.attachmentNumbers) {
		return nil, core.ErrConflict
	}
	for i, row := range rows {
		number := d.attachmentNumbers[i]
		putContentsValue(row, attachNumberTag, core.Integer(number))
		xid := d.store.hierarchyXID(d.mid)
		key := sha256.Sum256(append32(xid[:], number))
		putContentsValue(row, 0x0ff90102, core.Binary(key[:]))
	}
	return rows, nil
}

func attachmentNumber(row contentsValues) uint32 {
	return uint32(row[fixedContentsProperty(attachNumberTag)].(core.Integer))
}

func findAttachmentRow(rows []contentsValues, number uint32) (contentsValues, bool) {
	for _, row := range rows {
		if attachmentNumber(row) == number {
			return row, true
		}
	}
	return nil, false
}

func executeDraftAttachment(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	response := func(code uint32) []byte { return ropError(op.ID, op.Output, code) }
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return response(code)
	}
	if op.ID == ropSaveAttachment {
		if object.attachment == nil || object.attachment.draft == nil {
			return response(NotSupported)
		}
		a := object.attachment.draft
		parent, code := objects.lookup(handles[op.Output], op.Logon)
		if code != 0 {
			return response(code)
		}
		if a.importParent != nil {
			if parent.download == nil || parent.download.mailImport != a.importParent {
				return response(NotSupported)
			}
		} else if parent.message == nil || parent.message.mailDraft != a.parent {
			return response(NotSupported)
		}
		if err := a.save(ctx, session, op.Flags); err != nil {
			return response(attachmentError(err))
		}
		return response(0)
	}
	if object.download != nil && object.download.mailImport != nil {
		draft := object.download.mailImport
		if draft.special == core.ContactsFolder {
			return response(NotSupported)
		}
		if draft.closed || draft.upload != nil {
			return response(attachmentError(core.ErrConflict))
		}
		if draft.special == core.CalendarFolder && (draft.editKey.id == "" || len(draft.files) != 0) {
			return response(NotSupported)
		}
		if err := draft.collector.state.authorize(ctx, session); err != nil {
			return response(attachmentError(err))
		}
		if len(draft.files) == maxMailAttachments {
			return response(0x000004db)
		}
		attachment := &draftAttachment{importParent: draft, number: uint32(len(draft.files)), dirty: true}
		var retained *contentsLease
		if draft.special == core.CalendarFolder {
			var err error
			retained, err = object.binding.store.reserveContentsMemory(128 << 10)
			if err != nil {
				return response(attachmentError(err))
			}
			attachment.calendar = &calendarExceptionDraft{}
		}
		if code := addDraftAttachment(objects, object, op, handles, attachment); code != 0 {
			retained.release()
			return response(code)
		}
		if attachment.calendar != nil {
			draft.pendingException, draft.fileLease = attachment.calendar, retained
		}
		draft.files = append(draft.files, nil)
		return append32(response(0), attachment.number)
	}
	if object.message == nil {
		return response(NotSupported)
	}
	if object.message.mailDraft == nil {
		return response(accessDenied)
	}
	draft := object.message.mailDraft
	if err := draft.editable(); err != nil {
		return response(attachmentError(err))
	}
	if len(objects.objects) >= maxLiveStoreHandles || objects.allocated >= maxContextHandleAllocations {
		return response(0x8007000e)
	}
	peak, err := draft.store.reserveContentsPeak(ctx, 2*maxMailProjectionBytes)
	if err != nil {
		return response(attachmentError(err))
	}
	rows, err := draft.attachmentRows(ctx, session)
	peak.release()
	if err != nil {
		return response(attachmentError(err))
	}
	if len(rows) >= maxMailAttachments {
		return response(0x000004db) // ecMaxAttachmentExceeded.
	}
	attachment := &draftAttachment{parent: draft, number: draft.nextAttachment, dirty: true}
	if code := addDraftAttachment(objects, object, op, handles, attachment); code != 0 {
		return response(code)
	}
	draft.nextAttachment++
	return append32(response(0), attachment.number)
}

func addDraftAttachment(objects *mailboxObjects, object serverObject, op Operation, handles []uint32, attachment *draftAttachment) uint32 {
	if len(objects.objects) >= maxLiveStoreHandles || objects.allocated >= maxContextHandleAllocations {
		return 0x8007000e
	}
	handle, err := objects.allocator.handle()
	if err != nil {
		return 0x8007000e
	}
	budget := 2*core.MaxMailContentMIMEBytes + 8*attachmentCoverageWords
	if attachment.calendar != nil {
		budget = 128 << 10
	}
	lease, err := object.binding.store.reserveContentsMemory(budget)
	if err != nil {
		return attachmentError(err)
	}
	attachment.lease = lease
	opened := &openedAttachment{number: attachment.number, draft: attachment}
	if object.message != nil {
		opened.message = *object.message
	}
	objects.objects[handle] = serverObject{
		logon: op.Logon, binding: object.binding, fid: object.fid,
		attachment: opened,
	}
	objects.allocated++
	handles[op.Output] = handle
	return 0
}

// Save commits to the parent draft handle. Only the parent Save dispatches
// native persistence, so other handles cannot see the attachment yet.
func (a *draftAttachment) save(ctx context.Context, session core.Session, flags byte) error {
	mode, valid := faiSaveMode(flags)
	if !valid {
		return core.ErrInvalid
	}
	if err := a.authorize(ctx, session); err != nil {
		return err
	}
	if err := a.editable(); err != nil {
		return err
	}
	if a.calendar != nil {
		if err := a.saveCalendarException(); err != nil {
			return err
		}
		a.readOnly = mode == 1
		return nil
	}
	if !a.dirty {
		a.readOnly = mode == 1
		return nil
	}
	if !a.dataSet || a.unwrittenBytes != 0 {
		return core.ErrInvalid
	}
	if a.importParent != nil {
		if err := a.importParent.saveAttachment(a); err != nil {
			return err
		}
		a.dirty, a.readOnly = false, mode == 1
		return nil
	}
	d := a.parent
	if a.part == 0 && len(d.attachmentNumbers) >= maxMailAttachments {
		return core.ErrLimit
	}
	peak, err := d.store.reserveContentsPeak(ctx, 2*maxMailProjectionBytes)
	if err != nil {
		return err
	}
	defer peak.release()
	name := a.name
	if name == "" {
		name = a.shortName
	}
	kind, _, _ := mime.ParseMediaType(a.media)
	if strings.HasPrefix(kind, "multipart/") {
		return core.ErrUnsupported
	}
	file, err := mailmime.File(name, a.media, a.cid, false, a.data)
	if err != nil {
		return err
	}
	doc, err := mailmime.ReadDraft(d.staged.MIME)
	if err != nil {
		return err
	}
	part := a.part
	if part == 0 {
		if err := doc.AppendFiles([]mailmime.Draft{file}); err != nil {
			return err
		}
		parts, err := doc.Parts()
		if err != nil || len(parts) < 2 {
			return core.ErrInvalid
		}
		part = len(parts) - 1
	} else {
		kind, params, err := mime.ParseMediaType(doc.Header.Get("Content-Type"))
		if err != nil || kind != "multipart/mixed" {
			return core.ErrConflict
		}
		parts, err := doc.Parts()
		if err != nil || part >= len(parts) {
			return core.ErrConflict
		}
		parts[part] = file
		if err := doc.SetMultipart(kind, params, parts); err != nil {
			return err
		}
	}
	if err := d.installMIME(doc); err != nil {
		return err
	}
	if a.part == 0 {
		d.attachmentNumbers = append(d.attachmentNumbers, a.number)
	}
	d.staged.Properties[core.HasAttachments] = core.Bool(true)
	a.part, a.dirty, a.readOnly = part, false, mode == 1
	a.cid = file.Header.Get("Content-Id")
	return nil
}
