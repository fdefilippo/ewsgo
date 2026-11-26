// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"maps"
	"slices"
	"strings"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

func contactImportProperty(p contentsProperty) bool {
	if contactSchema(p) {
		return true
	}
	if p.name != (propertyName{}) {
		return false
	}
	switch uint32(p.id)<<16 | uint32(p.kind) {
	case 0x001a001f, 0x0037001f, 0x003d001f, 0x0e1d001f, 0x0e070003, 0x0e1b000b,
		0x30070040, 0x30080040, 0x300b0102, 0x65e00102, 0x65e20102, 0x65e30102, 0x67aa000b:
		return true
	}
	return false
}

func (d *mailImportDraft) typedImportValues(allows func(contentsProperty) bool) (contentsValues, error) {
	values := make(contentsValues)
	if d.upload == nil {
		for tag, value := range d.props {
			p, err := resolveContentsIdentity(tag, &d.collector.state.store.named)
			if err != nil || values[p] != nil {
				return nil, core.ErrInvalid
			}
			values[p] = value
		}
		return values, nil
	}
	raw, err := d.content()
	if err != nil {
		return nil, err
	}
	r := reader{data: raw}
	for r.pos < len(raw) {
		tag := r.u32()
		if r.err != nil {
			return nil, core.ErrInvalid
		}
		if tag == 0x40160003 {
			part := r.u32()
			if r.err != nil || (part != 0x0e12000d && part != 0x0e13000d) {
				return nil, core.ErrUnsupported
			}
			continue
		}
		p := fixedContentsProperty(tag)
		if p.id >= 0x8000 {
			p.name, err = readTransferPropertyName(&r)
			if err != nil {
				return nil, err
			}
			p.id = 0
		}
		if !allows(p) {
			return nil, core.ErrUnsupported
		}
		if values[p] != nil {
			return nil, core.ErrInvalid
		}
		if p.kind == 0x1003 {
			count := r.u32()
			if r.err != nil || count > 3 {
				return nil, core.ErrInvalid
			}
			slots := make(core.Int32s, count)
			for i := range slots {
				slots[i] = int32(r.u32())
			}
			if r.err != nil {
				return nil, core.ErrInvalid
			}
			values[p] = slots
			continue
		}
		value, err := readMessageImportProperty(&r, p.kind)
		if err != nil {
			return nil, err
		}
		values[p] = value
	}
	return values, nil
}

func contactImportPatch(values contentsValues) (core.ContactPatch, error) {
	class, _ := values[fixedContentsProperty(0x001a001f)].(core.Text)
	flags, _ := values[fixedContentsProperty(0x0e070003)].(core.Integer)
	if !strings.EqualFold(string(class), contactClass) || flags&(16|64) != 0 ||
		values[fixedContentsProperty(0x0e1b000b)] == core.Bool(true) ||
		values[fixedContentsProperty(0x67aa000b)] == core.Bool(true) {
		return nil, core.ErrUnsupported
	}
	for slot := range contactEmailFields {
		typeKey, _ := contactProperty(core.Key{Set: contactAddressSet, ID: uint32(0x8082 + 0x10*slot)})
		if kind, _ := values[typeKey].(core.Text); kind != "" && !strings.EqualFold(string(kind), "SMTP") {
			return nil, core.ErrUnsupported
		}
	}
	patch := make(core.ContactPatch)
	size := 0
	for property, value := range values {
		field := contactFieldForProperty(property)
		if field == "" {
			continue
		}
		text, ok := value.(core.Text)
		if !ok || !validContactText(field, string(text)) {
			return nil, core.ErrInvalid
		}
		size += len(text) + len(field)
		patch[field] = string(text)
	}
	if len(patch) == 0 {
		return nil, core.ErrInvalid
	}
	if size > maxContactEditBytes {
		return nil, core.ErrLimit
	}
	return patch, nil
}

func (d *mailImportDraft) saveContact(ctx context.Context, session core.Session) (uint64, error) {
	c := d.collector
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, core.ErrClosed
	}
	values, err := d.typedImportValues(contactImportProperty)
	if err != nil {
		return 0, err
	}
	patch, err := contactImportPatch(values)
	if err != nil {
		return 0, err
	}
	if err := c.prepareNormalMutation(ctx, session); err != nil {
		return 0, err
	}
	cn, err := c.state.store.saveContactImport(ctx, session, c.state.root, d, patch)
	if err != nil {
		return 0, err
	}
	c.acknowledgeNormal(d.counter, cn, d.editKey.id == "")
	d.saved = true
	d.close()
	return folderID(d.counter), nil
}

func (s *ramStore) saveContactImport(ctx context.Context, session core.Session, fid uint64, d *mailImportDraft, patch core.ContactPatch) (uint64, error) {
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	folder, err := s.scopedMessageAnchor(ctx, session, fid, false, core.ContactsFolder)
	if err != nil {
		return 0, err
	}
	if folder.native != d.folder {
		return 0, core.ErrConflict
	}
	if d.editKey.id != "" {
		return s.updateContactImport(ctx, session.Mailbox(), fid, d, patch)
	}
	writer, ok := session.Mailbox().(core.ContactImportWriter)
	if !ok {
		return 0, core.ErrUnsupported
	}
	var data []byte
	for _, field := range slices.Sorted(maps.Keys(patch)) {
		data = binary.LittleEndian.AppendUint32(data, uint32(len(field)))
		data = append(data, field...)
		data = binary.LittleEndian.AppendUint32(data, uint32(len(patch[field])))
		data = append(data, patch[field]...)
	}
	p := mapireplica.MessageImport{Counter: d.counter, Parent: folderGlobalCount(fid), Digest: d.requestDigest(data)}
	rand.Read(p.ContactReference[:])
	newImport, err := s.claimMessageImport(ctx, p)
	if err != nil {
		return 0, err
	}
	var contact core.Contact
	if newImport {
		contact, err = writer.CreateContactWithReference(ctx, folder.native, patch, p.ContactReference)
		if err != nil {
			return 0, s.messageImportFailure(ctx, p, err)
		}
	} else {
		s.mu.Lock()
		p = s.pendingMessageImports[folderID(p.Counter)]
		s.mu.Unlock()
		contact, err = writer.FindContactByReference(ctx, folder.native, p.ContactReference)
		if err != nil {
			// The caller still cannot tell whether the create committed.
			return 0, core.ErrOutcomeUnknown
		}
	}
	if contact.ImportReference != p.ContactReference || p.NativeID != "" && p.NativeID != contact.Identity.ID ||
		len(contact.Identity.ID) > maxHierarchyText || !validMailText(string(contact.Identity.ID)) {
		return 0, core.ErrOutcomeUnknown
	}
	obj, err := contactMessage(contact, folder.native)
	if err != nil {
		return 0, errors.Join(core.ErrOutcomeUnknown, err)
	}
	fingerprint, _, err := messageFingerprint(obj)
	if err != nil {
		return 0, errors.Join(core.ErrOutcomeUnknown, err)
	}
	if p.NativeID == "" {
		p.NativeID = obj.ID
		if err := s.updateMessageImport(ctx, p, mapireplica.RecordMessageImport); err != nil {
			return 0, errors.Join(core.ErrOutcomeUnknown, err)
		}
	}
	return s.publishMessageImport(ctx, p, d, obj.Revision, true, fingerprint)
}

// The contents lock serializes this edit with other ICS imports and contact
// observations. Zimbra's revision guard also covers independent SOAP writers.
func (s *ramStore) updateContactImport(ctx context.Context, box core.Mailbox, fid uint64, d *mailImportDraft, patch core.ContactPatch) (uint64, error) {
	current, err := s.prepareMessageImportEdit(fid, d)
	if err != nil {
		return 0, err
	}
	key := d.editKey
	reader, canRead := box.(core.ContactReader)
	writer, canWrite := box.(core.ContactWriter)
	if !canRead || !canWrite {
		return 0, core.ErrUnsupported
	}
	before, err := reader.GetContact(ctx, d.folder, key.id)
	if err != nil {
		return 0, err
	}
	if before.Identity.ID != key.id || before.Identity.Parent != d.folder || before.Identity.Kind != core.ContactItem ||
		before.Identity.Revision != current.nativeRevision {
		return 0, core.ErrConflict
	}
	// ICS imports complete contents. Clear absent mapped fields within the
	// native merge, preserving Zimbra attributes outside this projection.
	for _, prop := range contactTextFields(&before) {
		if _, present := patch[prop.field]; !present && prop.field != "" && *prop.value != "" {
			patch[prop.field] = ""
		}
	}
	for i, field := range contactEmailFields {
		if _, present := patch[field]; !present && before.Emails[i] != "" {
			patch[field] = ""
		}
	}
	after, err := writer.UpdateContact(ctx, before, patch)
	if err != nil {
		return 0, err
	}
	obj, err := contactMessage(after, d.folder)
	if err != nil || obj.ID != key.id || obj.Revision < before.Identity.Revision {
		return 0, core.ErrOutcomeUnknown
	}
	fingerprint, _, err := messageFingerprint(obj)
	if err != nil {
		return 0, core.ErrOutcomeUnknown
	}
	return s.publishMessageImportEdit(ctx, d, current, obj.Revision, fingerprint)
}

func (s *ramStore) contactAwaitingImport(fid uint64, c core.Contact) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, pending := range s.pendingMessageImports {
		if pending.Special() == core.ContactsFolder && folderID(pending.Parent) == fid &&
			(pending.NativeID == c.Identity.ID || pending.ContactReference == c.ImportReference) {
			return true
		}
	}
	return false
}
