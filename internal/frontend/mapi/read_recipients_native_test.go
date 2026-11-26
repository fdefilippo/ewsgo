// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"testing"

	"ewsgo/internal/core"
)

func nativeMessageProbeStore(t *testing.T, transport *SessionTransport, handle uint32) *ramStore {
	t.Helper()
	for _, state := range transport.contexts.entries {
		if state.mailbox != nil {
			object, code := state.mailbox.lookup(handle, 4)
			if code == 0 && object.isLogon {
				return object.binding.store
			}
		}
	}
	t.Fatal("Message probe Logon context unavailable")
	return nil
}

func qualifyNativeRecipients(ctx context.Context, t *testing.T, session core.Session, store *ramStore, fid uint64,
	checks map[string]bool, run func([]byte, uint32) []byte) {
	t.Helper()
	lease, err := store.reserveContentsPeak(ctx, 2*core.MaxMailContentViewBytes)
	if err != nil {
		t.Fatal("recipient probe observation budget unavailable")
	}
	defer lease.release()
	snapshot, err := store.observeMailMessages(ctx, session, fid)
	if err != nil {
		t.Fatal("native Inbox observation failed; details suppressed")
	}
	var sample messageRow
	for _, row := range snapshot.messages {
		if len(row.object.Recipients) > len(sample.object.Recipients) {
			sample = row
		}
	}
	if sample.mid == 0 {
		t.Fatal("no Inbox message with native recipients")
	}
	native, ok := session.Mailbox().(core.MailMessageEnvelopeReader)
	if !ok {
		t.Fatal("native envelope reader unavailable")
	}
	envelope, err := native.ReadMailMessageEnvelope(ctx, sample.object.Parent, sample.object.ID, false)
	if err != nil || len(envelope.Recipients) == 0 {
		t.Fatal("native recipient envelope unavailable")
	}
	const slot = 2
	opened := run(openMessageOperation(4, 1, slot, fid, sample.mid, 0x0fff, 0), 65535)
	if len(opened) < openMessageMinimum || !bytes.HasPrefix(opened, ropError(3, slot, 0)) {
		t.Fatal("native message did not open for recipient reading")
	}
	rows := make([][]byte, len(envelope.Recipients))
	for i, recipient := range envelope.Recipients {
		row, ok := encodeOpenRecipient(recipient)
		if !ok {
			t.Fatal("native recipient cannot be represented")
		}
		rows[i] = append(append32(nil, uint32(i)), row...)
	}
	first := append(append(ropError(0x0f, slot, 0), 1), rows[0]...)
	maximum := uint32(10 + 4*4 + len(first)) // Execute framing and four handle slots.
	read := readRecipientsOperation(4, slot, 0)
	checks["read_recipients_short_buffer_refuses_partial_row"] = bytes.Equal(
		run(read, maximum-1), ropError(0x0f, slot, tableBufferTooSmall))
	checks["read_recipients_first_page_matches_native"] = bytes.Equal(run(read, maximum), first)
	checks["read_recipients_missing_row_not_found"] = bytes.Equal(
		run(readRecipientsOperation(4, slot, uint32(len(rows))), 65535), ropError(0x0f, slot, PropertyNotFound))
	if len(rows) > 1 {
		offset := readRecipientsOperation(4, slot, 1)
		next := append(ropError(0x0f, slot, 0), 1)
		next = append(next, rows[1]...)
		checks["read_recipients_continuation_matches_native"] = bytes.Equal(run(offset, uint32(10+4*4+len(next))), next)
	}
	run([]byte{1, 4, slot}, 65535)
	checks["read_recipients_release_invalidates_handle"] = bytes.Equal(run(read, 65535), ropError(0x0f, slot, invalidObject))
}
