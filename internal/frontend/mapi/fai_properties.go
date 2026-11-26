// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"

	"ewsgo/internal/core"
)

// Read the version committed by this handle, rather than a pending import key
// or another handle's replacement. No property read allocates a change number.
func (d *faiDraft) committedChangeKey(ctx context.Context, session core.Session) ([]byte, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, core.ErrClosed
	}
	s := d.store
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	if _, err := s.messageAnchor(ctx, session, d.root, true); err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if !d.saved {
		return nil, core.ErrNotFound
	}
	record, exists := s.fai.records[d.id]
	if !exists || record.deleted || record.object.Revision != d.base {
		return nil, core.ErrConflict
	}
	if record.sync == nil || !record.sync.change.valid() {
		return nil, core.ErrUnsupported
	}
	return []byte(record.sync.change), nil
}

// This projection is only for local Associated drafts. Native messages,
// collector handles and FastTransfer sources do not acquire Message support.
func getFAIProperties(ctx context.Context, session core.Session, draft *faiDraft, op Operation, diagnostic *StorePropertyDiagnostic) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	key, err := draft.committedChangeKey(ctx, session)
	if err != nil && !errors.Is(err, core.ErrNotFound) {
		return fail(faiError(err, true))
	}
	request := *op.Properties
	values := make([]folderPropertyValue, len(request.tags))
	for i, tag := range request.tags {
		value := folderPropertyValue{kind: 0x000a, code: NotSupported}
		if uint16(tag>>16) == 0x65e2 && (uint16(tag) == 0 || uint16(tag) == 0x0102) {
			value.code = PropertyNotFound
			if err == nil {
				value = folderPropertyValue{kind: 0x0102, data: append(binary.LittleEndian.AppendUint16(nil, uint16(len(key))), key...)}
			}
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
