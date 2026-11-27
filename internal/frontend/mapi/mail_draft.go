// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"mime"
	"slices"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mailmime"
	"ewsgo/internal/mapireplica"
)

type mailDraftCreator interface {
	CreateMailDraft(context.Context, core.Session, core.ID, string, []byte) (core.Object, error)
}

func (t *SessionTransport) EnableMailDrafts(creator mailDraftCreator) { t.mailDraftCreator = creator }

type pendingMailDraft struct {
	fid, mid        uint64
	folder          core.ID
	dispatched      bool
	transportSource core.ID
}

// The context lease is exclusive while a Message handle is in use.
type mailDraft struct {
	store                      *ramStore
	creator                    mailDraftCreator
	lease                      *contentsLease
	fid, mid                   uint64
	folder                     core.ID
	ref                        string
	staged                     core.Object
	rows                       map[uint32]core.Recipient
	attachmentNumbers          []uint32
	nextAttachment             uint32
	incompleteAttachments      int
	dirty, readOnly, uncertain bool
	outbox, submitted          bool
}

func draftFolder(ctx context.Context, session core.Session, store *ramStore, fid uint64) (core.ID, error) {
	folder, err := store.messageAnchor(ctx, session, fid, false)
	if err != nil {
		return "", err
	}
	if folder.role == Outbox && folder.native == "" {
		return store.nativeOutboxDraftsFolder(ctx, session)
	}
	if folder.native == "" {
		return "", core.ErrUnsupported
	}
	current, err := session.Mailbox().Get(ctx, folder.native)
	if err != nil {
		return "", err
	}
	if current.ID != folder.native || current.Kind != core.Folder || current.Properties[core.WellKnown] != core.Text("drafts") {
		return "", core.ErrUnsupported
	}
	return folder.native, nil
}

func (s *ramStore) stageMailDraft(ctx context.Context, session core.Session, fid uint64, creator mailDraftCreator) (*mailDraft, error) {
	box := session.Mailbox()
	if _, ok := box.(core.MailDraftWriter); !ok {
		return nil, core.ErrUnsupported
	}
	if _, ok := box.(core.MailDraftReferenceFinder); !ok {
		return nil, core.ErrUnsupported
	}
	folder, err := draftFolder(ctx, session, s, fid)
	if err != nil {
		return nil, err
	}
	doc, err := mailmime.NewDraft(session.Identity().Name)
	if err != nil {
		return nil, err
	}
	body, err := mailmime.Text("", false)
	if err != nil {
		return nil, err
	}
	doc.InstallBody(body)
	raw, err := doc.Bytes()
	if err != nil {
		return nil, err
	}
	var ref [16]byte
	if _, err := rand.Read(ref[:]); err != nil {
		return nil, err
	}
	lease, err := s.reserveContentsMemory(2 * core.MaxMailContentMIMEBytes)
	if err != nil {
		return nil, err
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.mappingCountLocked() >= s.mappingLimit() {
		lease.release()
		return nil, core.ErrLimit
	}
	mid, err := s.stageIDLocked(ctx, mapireplica.Message)
	if err != nil {
		lease.release()
		return nil, err
	}
	reference := hex.EncodeToString(ref[:])
	if s.pendingMailDrafts == nil {
		s.pendingMailDrafts = make(map[string]pendingMailDraft)
	}
	outbox := s.byFID[fid] == (folderKey{role: Outbox})
	s.pendingMailDrafts[reference] = pendingMailDraft{fid: fid, mid: mid, folder: folder}
	now := core.Timestamp(time.Now().UTC())
	return &mailDraft{
		store: s, creator: creator, lease: lease, fid: fid, mid: mid, folder: folder, ref: reference, dirty: true, outbox: outbox,
		staged: core.Object{Parent: folder, Kind: core.Message, MIME: raw, Properties: core.Properties{
			core.IsDraft: core.Bool(true), core.IsRead: core.Bool(false), core.HasAttachments: core.Bool(false),
			core.LastModifiedAt: now, core.MessageSize: core.Integer(len(raw)),
		}},
	}, nil
}

func (s *ramStore) editMailDraft(ctx context.Context, session core.Session, row messageRow) (*mailDraft, error) {
	folder, err := draftFolder(ctx, session, s, row.parent)
	if err != nil {
		return nil, err
	}
	reader, ok := session.Mailbox().(core.MailMessageMIMEReader)
	if !ok {
		return nil, core.ErrUnsupported
	}
	if _, ok := session.Mailbox().(core.MailDraftWriter); !ok {
		return nil, core.ErrUnsupported
	}
	lease, err := s.reserveContentsMemory(2 * core.MaxMailContentMIMEBytes)
	if err != nil {
		return nil, err
	}
	object, err := reader.ReadMailMessageMIME(ctx, folder, row.object.ID, false)
	if err != nil {
		lease.release()
		return nil, err
	}
	if !validSavedMailDraft(object, folder) || object.ID != row.object.ID || object.Revision != row.object.Revision ||
		len(object.MIME) > core.MaxMailContentMIMEBytes || len(object.Recipients) > maxDraftRecipients {
		lease.release()
		return nil, core.ErrConflict
	}
	rows := make(map[uint32]core.Recipient, len(object.Recipients))
	for i, recipient := range object.Recipients {
		rows[uint32(i)] = recipient
	}
	return &mailDraft{store: s, fid: row.parent, mid: row.mid, folder: folder, staged: object, rows: rows, lease: lease, outbox: s.outboxFID() == row.parent}, nil
}

func (d *mailDraft) close() {
	if d == nil {
		return
	}
	if d.ref != "" {
		d.store.mu.Lock()
		if pending := d.store.pendingMailDrafts[d.ref]; !pending.dispatched || !d.outbox && d.store.durable.store == nil {
			delete(d.store.pendingMailDrafts, d.ref)
		}
		d.store.mu.Unlock()
	}
	d.lease.release()
	d.lease, d.rows, d.attachmentNumbers = nil, nil, nil
	d.staged = core.Object{}
}

func (d *mailDraft) authorize(ctx context.Context, session core.Session) error {
	d.store.mu.Lock()
	entry := d.store.spoolerQueue[d.mid]
	d.store.mu.Unlock()
	if entry.deniesAccess(ctx) {
		return core.ErrAccessDenied
	}
	folder, err := d.store.messageAnchor(ctx, session, d.fid, false)
	if err != nil {
		return err
	}
	if d.outbox {
		if folder != (folderKey{role: Outbox}) {
			return core.ErrConflict
		}
		current, err := session.Mailbox().Get(ctx, d.folder)
		if err != nil {
			return err
		}
		if current.ID != d.folder || current.Kind != core.Folder || current.Parent == "" || current.Properties[core.WellKnown] != core.Text("drafts") {
			return core.ErrConflict
		}
		if d.staged.ID != "" && !d.submitted && d.store.outboxNativeFolder(d.staged.ID) != d.folder {
			return core.ErrNotFound
		}
		return nil
	}
	if folder.native != d.folder {
		return core.ErrAccessDenied
	}
	return nil
}

func (d *mailDraft) row() messageRow {
	return messageRow{mid: d.mid, parent: d.fid, sourceKey: d.store.hierarchyXID(d.mid), object: d.staged}
}

func (d *mailDraft) editable() error {
	switch {
	case d.readOnly || d.submitted:
		return core.ErrAccessDenied
	case d.uncertain:
		return core.ErrOutcomeUnknown
	default:
		return nil
	}
}

func mailDraftSetTag(tag uint32) bool {
	return tag == 0x1000001f || tag == 0x10130102 || tag == messageInternetCP
}

func mailDraftTextTag(tag uint32) bool {
	switch tag {
	case 0x0037001f, 0x003d001f, 0x0e1d001f, 0x001a001f, 0x1000001f:
		return true
	}
	return false
}

func (d *mailDraft) set(ctx context.Context, session core.Session, values faiSetRequest) ([]faiPropertyProblem, error) {
	if err := d.editable(); err != nil {
		return nil, err
	}
	if err := d.authorize(ctx, session); err != nil {
		return nil, err
	}
	doc, err := mailmime.ReadDraft(d.staged.MIME)
	if err != nil {
		return nil, err
	}
	var problems []faiPropertyProblem
	subject, _ := d.staged.Properties[core.Subject].(core.Text)
	prefix, normalized := mailSubjectParts(string(subject))
	var wholeSubject *string
	hasPrefix, hasNormalized := false, false
	var text, html *string
	htmlCharset := "utf-8"
	changed := false
	for _, v := range values {
		if v.tag == messageInternetCP {
			code := v.value.(core.Integer)
			if code != 65001 && code != 0 {
				htmlCharset = restrictionCharset(uint32(code))
			}
		}
	}
	for i, v := range values {
		var problem uint32
		switch v.tag {
		case 0x0037001f, 0x003d001f, 0x0e1d001f:
			value := string(v.value.(core.Text))
			if !mailmime.ValidHeaderValue(value) {
				problem = 0x80070057
				break
			}
			switch v.tag {
			case 0x0037001f:
				wholeSubject = &value
			case 0x003d001f:
				prefix, hasPrefix = value, true
			case 0x0e1d001f:
				normalized, hasNormalized = value, true
			}
		case 0x1000001f:
			value := string(v.value.(core.Text))
			text = &value
		case 0x10130102:
			value, err := decodeMailText(htmlCharset, v.value.(core.Binary))
			if htmlCharset == "" || err != nil {
				problem = 0x80070057
				break
			}
			html = &value
		case 0x001a001f:
			if v.value != core.Text("IPM.Note") {
				problem = NotSupported
			}
		case 0x0e070003:
			// Unsent and read state are projected from the native draft.
			if v.value.(core.Integer) & ^core.Integer(0x0000000b) != 0 {
				problem = NotSupported
			}
		case sentMailServerIDTag:
			// Zimbra creates the Sent copy in its native Sent folder.
			// The default destination may be reaffirmed by the client.
			raw, _ := v.value.(core.Binary)
			if !d.store.isDefaultSentServerID(raw) {
				problem = NotSupported
			}
		case messageInternetCP:
			// The MIME body records its encoding.
		default:
			problem = NotSupported
		}
		if problem != 0 {
			problems = append(problems, faiPropertyProblem{index: uint16(i), tag: v.tag, code: problem})
		}
	}
	if wholeSubject != nil || hasPrefix || hasNormalized {
		if wholeSubject != nil && !(hasPrefix && hasNormalized) {
			subject = core.Text(*wholeSubject)
		} else {
			subject = core.Text(prefix + normalized)
		}
		doc.Header.Set("Subject", mime.QEncoding.Encode("utf-8", string(subject)))
		changed = true
	}
	if text != nil || html != nil {
		body, err := composeDraftBody(text, html)
		if err != nil {
			return nil, err
		}
		if err := doc.ReplaceBody(body); err != nil {
			return nil, err
		}
		changed = true
	}
	if changed {
		if err := d.installMIME(doc); err != nil {
			return nil, err
		}
		d.staged.Properties[core.Subject] = subject
	}
	return problems, nil
}

func composeDraftBody(text, html *string) (mailmime.Draft, error) {
	if html == nil {
		return mailmime.Text(*text, false)
	}
	if text == nil {
		return mailmime.Text(*html, true)
	}
	plain, err := mailmime.Text(*text, false)
	if err != nil {
		return mailmime.Draft{}, err
	}
	rich, err := mailmime.Text(*html, true)
	if err != nil {
		return mailmime.Draft{}, err
	}
	alternative, err := mailmime.Text("", false)
	if err != nil {
		return mailmime.Draft{}, err
	}
	err = alternative.SetMultipart("multipart/alternative", nil, []mailmime.Draft{plain, rich})
	return alternative, err
}

func (d *mailDraft) installMIME(doc mailmime.Draft) error {
	raw, err := doc.Bytes()
	if err != nil {
		return err
	}
	if len(raw) > core.MaxMailContentMIMEBytes {
		return core.ErrLimit
	}
	d.staged.MIME = raw
	d.staged.Properties[core.MessageSize] = core.Integer(len(raw))
	d.dirty = true
	return nil
}

func (d *mailDraft) setRecipients(ctx context.Context, session core.Session, changes []draftRecipientChange, clearAll bool) error {
	if err := d.editable(); err != nil {
		return err
	}
	if err := d.authorize(ctx, session); err != nil {
		return err
	}
	rows, err := applyRecipientChanges(d.rows, changes, clearAll)
	if err != nil {
		return err
	}
	doc, err := mailmime.ReadDraft(d.staged.MIME)
	if err != nil {
		return err
	}
	list, err := installRecipientHeaders(&doc, rows)
	if err != nil {
		return err
	}
	if err := d.installMIME(doc); err != nil {
		return err
	}
	d.rows, d.staged.Recipients = rows, list
	return nil
}

func draftRecipientIDs(rows map[uint32]core.Recipient) []uint32 {
	ids := make([]uint32, 0, len(rows))
	for id := range rows {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	return ids
}

func (d *mailDraft) save(ctx context.Context, session core.Session, flags byte) (uint64, error) {
	mode, valid := faiSaveMode(flags)
	if !valid {
		return 0, core.ErrInvalid
	}
	if err := d.authorize(ctx, session); err != nil {
		return 0, err
	}
	if !d.dirty {
		d.readOnly = d.readOnly || mode == 1
		return d.mid, nil
	}
	if d.readOnly {
		return 0, core.ErrAccessDenied
	}
	store := d.store
	store.contentsMu.Lock()
	defer store.contentsMu.Unlock()
	if store.isQueuedSubmission(d.mid) {
		return 0, core.ErrConflict
	}
	if d.outbox && d.staged.ID != "" {
		if err := store.editableOutboxDraft(d.staged.ID, d.folder); err != nil {
			return 0, err
		}
	}
	var saved core.Object
	var err error
	id := d.staged.ID
	if id == "" {
		if err := store.setDraftDispatched(ctx, d.ref, true); err != nil {
			return 0, err
		}
		saved, err = d.creator.CreateMailDraft(ctx, session, d.folder, d.ref, d.staged.MIME)
		if err == nil {
			if !validSavedMailDraft(saved, d.folder) {
				err = core.ErrOutcomeUnknown
			} else {
				err = store.bindMailDraft(ctx, d.fid, d.mid, saved.ID, d.ref)
			}
		}
	} else {
		if d.uncertain {
			return 0, core.ErrOutcomeUnknown
		}
		writer, ok := session.Mailbox().(core.MailDraftWriter)
		if !ok {
			return 0, core.ErrUnsupported
		}
		saved, err = writer.SaveMailDraft(ctx, id, d.staged.Revision, d.staged.MIME)
		if err == nil && (!validSavedMailDraft(saved, d.folder) || saved.ID != id || saved.Revision <= d.staged.Revision) {
			err = core.ErrOutcomeUnknown
		}
	}
	if err != nil {
		if id == "" && !errors.Is(err, core.ErrOutcomeUnknown) {
			err = store.resetRejectedDraft(ctx, d.ref, err)
		}
		d.uncertain = errors.Is(err, core.ErrOutcomeUnknown)
		return 0, err
	}
	d.uncertain = false
	if d.outbox && id != "" {
		store.mu.Lock()
		store.localNotifications.publish(objectNotification{flag: notifyModified | notifyMessage, fid: d.fid, mid: d.mid})
		store.mu.Unlock()
	}
	d.staged.ID, d.staged.Revision = saved.ID, saved.Revision
	for _, key := range []core.Key{core.ContentRevision, core.MetadataRevision, core.LastModifiedAt, core.IsRead} {
		if value := saved.Properties[key]; value != nil {
			d.staged.Properties[key] = value
		}
	}
	d.dirty, d.readOnly = false, mode == 1
	return d.mid, nil
}

func validSavedMailDraft(object core.Object, folder core.ID) bool {
	return object.ID != "" && object.Parent == folder && object.Kind == core.Message && object.Revision > 0 &&
		object.Properties[core.IsDraft] == core.Bool(true)
}

func (s *ramStore) bindMailDraft(ctx context.Context, fid, mid uint64, id core.ID, reference string) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := messageKey{id: id}
	if existing := s.byMessage[key]; existing != 0 && existing != mid {
		return core.ErrOutcomeUnknown
	}
	version := s.messages[key]
	if version.present && version.parent != fid {
		return core.ErrOutcomeUnknown
	}
	if other, exists := s.byMID[mid]; exists && other != key {
		return core.ErrOutcomeUnknown
	}
	pending := s.pendingMailDrafts[reference]
	outbox := s.byFID[fid] == (folderKey{role: Outbox})
	if outbox && pending.folder == "" && s.outboxDrafts[id].folder == "" {
		return core.ErrOutcomeUnknown
	}
	if s.durable.store != nil && s.byMessage[key] == 0 {
		if pending.mid != mid || pending.fid != fid || s.next >= maxGlobalCount {
			return core.ErrOutcomeUnknown
		}
		binding, err := persistedMessageBinding(key, mid)
		if err != nil {
			return errors.Join(core.ErrOutcomeUnknown, err)
		}
		mutation := mapireplica.Mutation{StagedBindings: []mapireplica.Binding{binding}}
		s.addDraftAcknowledgment(&mutation, reference, pending, id)
		if err := s.advanceReplicaLocked(ctx, mutation); err != nil {
			return errors.Join(core.ErrOutcomeUnknown, err)
		}
	}
	if outbox && pending.folder != "" {
		s.linkOutboxDraftLocked(id, reference, pending)
	}
	if s.byMessage == nil {
		s.byMessage, s.byMID = make(map[messageKey]uint64), make(map[uint64]messageKey)
	}
	s.byMessage[key], s.byMID[mid] = mid, key
	delete(s.pendingMailDrafts, reference)
	return nil
}
