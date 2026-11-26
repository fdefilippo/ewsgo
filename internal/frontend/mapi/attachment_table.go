// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"

	"ewsgo/internal/core"
)

func openAttachmentTable(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	result := func(code uint32) []byte { return ropError(op.ID, op.Output, code) }
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return result(code)
	}
	if object.message == nil {
		return result(nullObject)
	}
	if len(objects.objects) >= maxLiveStoreHandles || objects.allocated >= maxContextHandleAllocations {
		return result(0x8007000e)
	}

	store := object.binding.store
	lease, err := store.reserveContentsPeak(ctx, 2*maxMailProjectionBytes)
	if err != nil {
		return result(storeError(err))
	}
	defer func() { lease.release() }()
	message := *object.message
	rows, err := store.readOpenedAttachments(ctx, session, object.binding.mailboxGUID, object.fid, message)
	if err != nil {
		return result(attachmentError(err))
	}
	if err = ctx.Err(); err != nil {
		return result(storeError(err))
	}
	handle, err := objects.allocator.handle()
	if err != nil {
		return result(0x8007000e)
	}

	// Keep a detached metadata view. The binary payload belongs to the
	// opened Attachment, and must neither remain in RAM here nor appear in a row.
	used := 1024 + 4*maxTableColumns
	for number, row := range rows {
		delete(row, fixedContentsProperty(0x37010102))
		instance := append32(append32(nil, handle), uint32(number))
		putContentsValue(row, 0x0ff60102, core.Binary(instance))
		// Outlook can request PidTagMid first to construct the attachment
		// row's instance key. Give each row its own identity in this table.
		putContentsValue(row, tableMIDTag, core.Integer(binary.LittleEndian.Uint64(instance)))
		used += 2048
		for _, value := range row {
			switch v := value.(type) {
			case core.Text:
				used += len(v)
			case core.Binary:
				used += len(v)
			}
		}
	}
	if err = lease.shrink(used); err != nil {
		return result(storeError(err))
	}
	table := &contentsTable{
		attachmentMessage: &message,
		flags:             op.Table.flags,
		rows:              rows,
		baseRows:          rows,
		columns:           []uint32{0x0e210003, 0x0ff60102, 0x0ff90102, 0x370b0003},
		lease:             lease,
	}
	objects.objects[handle] = serverObject{logon: op.Logon, binding: object.binding, fid: object.fid, table: table}
	objects.allocated++
	handles[op.Output] = handle
	lease = nil // The table handle now owns the retained lease.
	return result(0)
}

func (t *contentsTable) authorizationError(err error) uint32 {
	if t.attachmentMessage != nil {
		return attachmentError(err)
	}
	return storeError(err)
}
