// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"strings"

	"ewsgo/internal/core"
)

const rwRulesStreamTag = 0x68020102

func isRuleOrganizer(properties core.Properties) bool {
	class, _ := properties[core.Key{ID: 0x001a}].(core.Text)
	return strings.EqualFold(string(class), "IPM.RuleOrganizer") || strings.EqualFold(string(class), "IPM.RuleOrganizer.ClientRules")
}

func (s *propertyStream) maxSize() int64 {
	if s.fai != nil {
		return maxFAIObjectBytes - 4096
	}
	return core.MaxMailContentMIMEBytes
}

func (d *faiDraft) ruleStreamContent(ctx context.Context, session core.Session) (core.Binary, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return nil, core.ErrClosed
	}
	if _, err := d.store.messageAnchor(ctx, session, d.root, true); err != nil {
		return nil, err
	}
	if d.privateConfig || !isRuleOrganizer(d.properties) {
		return nil, core.ErrUnsupported
	}
	data, ok := d.properties[core.Key{ID: rwRulesStreamTag >> 16}].(core.Binary)
	if !ok {
		return nil, core.ErrNotFound
	}
	return data, nil
}

func (d *faiDraft) editRuleStream(ctx context.Context, session core.Session, previous []byte, size int, position int64, data []byte) error {
	// Charge the temporary buffer in addition to the memory used by draft.set.
	lease, err := d.store.reserveContentsMemory(size)
	if err != nil {
		return err
	}
	defer lease.release()
	next := make(core.Binary, size)
	copy(next, previous)
	if len(data) != 0 {
		copy(next[position:], data)
	}
	_, err = d.set(ctx, session, faiSetRequest{{tag: rwRulesStreamTag, value: next}})
	return err
}

func openRuleStream(ctx context.Context, session core.Session, objects *mailboxObjects, object serverObject, op Operation, handles []uint32) []byte {
	result := func(code uint32) []byte { return ropError(op.ID, op.Output, code) }
	mode := op.Stream.mode
	if mode > 2 {
		return result(0x80070057)
	}
	if len(objects.objects) >= maxLiveStoreHandles || objects.allocated >= maxContextHandleAllocations {
		return result(0x8007000e)
	}
	draft := object.message.draft
	content, err := draft.ruleStreamContent(ctx, session)
	if err != nil && !(mode == 2 && err == core.ErrNotFound) {
		if err == core.ErrNotFound {
			return result(PropertyNotFound)
		}
		return result(attachmentError(err))
	}
	writable := mode != 0 && !draft.readOnly
	if draft.readOnly && mode != 0 {
		return result(0x80030005)
	}
	handle, err := objects.allocator.handle()
	if err != nil {
		return result(0x8007000e)
	}
	if mode == 2 {
		if err := draft.editRuleStream(ctx, session, nil, 0, 0, nil); err != nil {
			return result(attachmentError(err))
		}
		content = nil
	}
	objects.objects[handle] = serverObject{logon: op.Logon, binding: object.binding, fid: object.fid,
		stream: &propertyStream{fai: draft, writable: writable}}
	objects.allocated++
	handles[op.Output] = handle
	return append32(result(0), uint32(len(content)))
}
