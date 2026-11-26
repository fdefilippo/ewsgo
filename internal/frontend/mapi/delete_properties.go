// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"maps"

	"ewsgo/internal/core"
)

const (
	ropDeleteProperties            = 0x0b
	ropDeletePropertiesNoReplicate = 0x7a
)

func parseDeletePropertyTags(r *reader) ([]uint32, error) {
	header := r.take(2)
	if r.err != nil {
		return nil, r.err
	}
	count := int(binary.LittleEndian.Uint16(header))
	raw := r.take(4 * count)
	if r.err != nil {
		return nil, r.err
	}
	tags := make([]uint32, count)
	for i := range tags {
		tags[i] = binary.LittleEndian.Uint32(raw[4*i:])
	}
	return tags, nil
}

func propertyProblemsResponse(id, index byte, problems []faiPropertyProblem) []byte {
	out := binary.LittleEndian.AppendUint16(ropError(id, index, 0), uint16(len(problems)))
	for _, problem := range problems {
		out = binary.LittleEndian.AppendUint16(out, problem.index)
		out = binary.LittleEndian.AppendUint32(out, problem.tag)
		out = binary.LittleEndian.AppendUint32(out, problem.code)
	}
	return out
}

func deleteStoreProperties(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32, view *metadataBatchView) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	var draft *faiDraft
	switch {
	case object.message != nil && object.message.contactDraft != nil:
		if op.ID == ropDeletePropertiesNoReplicate {
			return fail(NotSupported)
		}
		problems, err := object.message.contactDraft.stage(ctx, session, object, nil, op.DeletePropertyTags)
		if err != nil {
			return fail(faiError(err, false))
		}
		return propertyProblemsResponse(op.ID, op.Input, problems)
	case object.message != nil:
		draft = object.message.draft
		if draft == nil {
			return fail(accessDenied)
		}
	case object.attachment != nil:
		return fail(accessDenied)
	case object.download != nil:
		draft = object.download.draft
		if draft == nil {
			return fail(NotSupported)
		}
	case object.isLogon || object.table != nil || object.stream != nil || object.notification != nil:
		return fail(NotSupported)
	}
	if draft != nil {
		problems, err := draft.deleteProperties(ctx, session, op.DeletePropertyTags)
		if err != nil {
			return fail(faiError(err, false))
		}
		return propertyProblemsResponse(op.ID, op.Input, problems)
	}
	if view == nil || view.scope != session.Identity().Scope {
		return fail(NotSupported)
	}
	var problems []faiPropertyProblem
	remove := make(folderMetadata)
	for i, tag := range op.DeletePropertyTags {
		id, kind := uint16(tag>>16), uint16(tag)
		if !metadataID(id) || !deletableMetadataType(id, kind) {
			problems = append(problems, faiPropertyProblem{uint16(i), tag, NotSupported})
			continue
		}
		remove[id] = nil
	}
	updates, code := object.binding.store.updateMetadata(ctx, session, object.fid, remove, true)
	if code != 0 {
		return fail(code)
	}
	for fid, data := range updates {
		view.values[fid] = data
	}
	return propertyProblemsResponse(op.ID, op.Input, problems)
}

func deletableMetadataType(id, kind uint16) bool {
	if kind == 0 {
		return true
	}
	if id == 0x3613 {
		return kind == 0x1e || kind == 0x1f
	}
	return kind == metadataPropertyType(id)
}

func (d *faiDraft) deleteProperties(ctx context.Context, session core.Session, tags []uint32) ([]faiPropertyProblem, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, core.ErrClosed
	}
	if d.readOnly {
		return nil, core.ErrAccessDenied
	}
	if d.privateConfig {
		return nil, core.ErrUnsupported
	}
	if _, err := d.store.messageAnchor(ctx, session, d.root, true); err != nil {
		return nil, err
	}
	// Values are immutable. A shallow copy is sufficient when removing keys.
	next := maps.Clone(d.properties)
	var problems []faiPropertyProblem
	for i, tag := range tags {
		key, code := d.deletableProperty(tag, next)
		if code != 0 {
			problems = append(problems, faiPropertyProblem{uint16(i), tag, code})
			continue
		}
		delete(next, key)
		if key == core.Subject {
			// The two derived subject parts are kept or removed together.
			delete(next, core.Key{ID: 0x003d})
			delete(next, core.Key{ID: 0x0e1d})
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if len(next) != len(d.properties) {
		d.properties = next
		d.dirty = true
	}
	return problems, nil
}

func (d *faiDraft) deletableProperty(tag uint32, props core.Properties) (core.Key, uint32) {
	switch uint16(tag >> 16) {
	case 0x001a, 0x003d, 0x0e1d, 0x0e07, 0x3008, 0x300b, 0x3ffa,
		0x65e0, 0x65e1, 0x65e2, 0x65e3, 0x67aa, 0x6748, 0x674a:
		// Required, derived or server-managed properties.
		return core.Key{}, accessDenied
	case 0x685d, 0x36e2:
		return core.Key{}, NotSupported
	}
	p, err := resolveContentsIdentity(tag, &d.store.named)
	if err != nil {
		return core.Key{}, NotSupported
	}
	key := faiCoreProperty(p)
	kind := p.kind
	if kind == 0x1e {
		kind = 0x1f // MAPI text aliases refer to the same stored value.
	}
	if value, exists := props[key]; exists {
		actual, err := d.store.faiProperty(key, value)
		if err != nil || kind != 0 && kind != actual.kind {
			return core.Key{}, NotSupported
		}
		return key, 0
	}
	if kind != 0 && faiSetTag(tag&0xffff0000|uint32(kind)) {
		return key, 0 // An already absent supported property needs no change.
	}
	if kind == 0 {
		for _, supported := range [...]uint32{3, 0xb, 0x1f, 0x40, 0x102} {
			if faiSetTag(tag | supported) {
				return key, 0
			}
		}
	}
	return core.Key{}, NotSupported
}
