// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"

	"ewsgo/internal/core"
)

const (
	readRecipientsMinimum = 7
	recipientListChanged  = 0x80040109
)

type readRecipientsRequest struct {
	rowID uint32
	limit int
}

// Frame the request before any ROP can run. Row properties are interpreted
// by the editable Message using its code page.
func skipModifyRecipients(r *reader) {
	columns := r.take(2)
	if r.err != nil {
		return
	}
	r.take(4 * int(binary.LittleEndian.Uint16(columns)))
	rows := r.take(2)
	if r.err != nil {
		return
	}
	for range int(binary.LittleEndian.Uint16(rows)) {
		header := r.take(7) // RowId, RecipientType, RecipientRowSize.
		if r.err != nil {
			return
		}
		r.take(int(binary.LittleEndian.Uint16(header[5:])))
		if r.err != nil {
			return
		}
	}
}

func digestRecipients(recipients []core.Recipient) [32]byte {
	// The ordered list contains only strings and numeric roles. Retain a digest
	// so a handle can detect reordered or changed rows without retaining addresses.
	encoded, _ := json.Marshal(recipients)
	return sha256.Sum256(encoded)
}

func readOpenedRecipients(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if object.message == nil {
		return fail(nullObject)
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
	recipients := row.object.Recipients
	if len(recipients) > core.MaxMailContentMessages {
		return fail(NotSupported)
	}
	if object.message.mailDraft == nil && digestRecipients(recipients) != object.message.recipientDigest {
		return fail(recipientListChanged)
	}
	request := op.ReadRecipients
	var ids []uint32
	if draft := object.message.mailDraft; draft != nil {
		ids = draftRecipientIDs(draft.rows)
	} else {
		ids = make([]uint32, len(recipients))
		for i := range ids {
			ids[i] = uint32(i)
		}
	}
	start := 0
	for start < len(ids) && ids[start] != request.rowID {
		start++
	}
	if start == len(ids) {
		return fail(PropertyNotFound)
	}
	response := append(fail(0), 0)
	for i := start; i < len(ids) && response[6] < 255; i++ {
		wire, ok := encodeOpenRecipient(recipients[i])
		if !ok {
			return fail(NotSupported)
		}
		if 4+len(wire) > request.limit-len(response) {
			break
		}
		response = append(append32(response, ids[i]), wire...)
		response[6]++
	}
	if response[6] == 0 {
		return fail(tableBufferTooSmall)
	}
	return response
}
