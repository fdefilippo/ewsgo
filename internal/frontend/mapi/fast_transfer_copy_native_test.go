// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"ewsgo/internal/core"
)

func qualifyNativeFastTransferCopy(ctx context.Context, t *testing.T, session core.Session, store *ramStore, fid uint64,
	checks map[string]bool, run func([]byte, uint32) []byte) {
	t.Helper()
	lease, err := store.reserveContentsPeak(ctx, 2*core.MaxMailContentViewBytes)
	if err != nil {
		t.Fatal("native CopyTo observation budget unavailable")
	}
	defer lease.release()
	snapshot, err := store.observeMailMessages(ctx, session, fid)
	if err != nil {
		t.Fatal("native Inbox observation failed; details suppressed")
	}
	var mid uint64
	var expected [][]byte
	for _, message := range snapshot.messages {
		projection, err := projectMailContents(message.object)
		if err != nil {
			continue
		}
		var attachments [][]byte
		largest := 0
		for _, row := range projection.attachments {
			data, ok := row[fixedContentsProperty(attachDataBinary)].(core.Binary)
			if !ok {
				continue
			}
			attachments = append(attachments, data)
			largest = max(largest, len(data))
		}
		if largest >= 64<<10 {
			mid = message.mid
			expected = attachments
			break
		}
	}
	if mid == 0 {
		t.Fatal("native CopyTo requires an Inbox message with a by-value attachment of at least 64 KiB")
	}
	snapshot = messageSnapshot{}
	var retained int
	for _, payload := range expected {
		retained += cap(payload)
	}
	if err = lease.shrink(retained + 1024); err != nil {
		t.Fatal("native CopyTo observation accounting failed")
	}
	const messageSlot, copySlot = 2, 3
	reply := run(openMessageOperation(4, 1, messageSlot, fid, mid, 0x0fff, 0), 65535)
	if len(reply) < openMessageMinimum || !bytes.HasPrefix(reply, ropError(3, messageSlot, 0)) {
		t.Fatal("native CopyTo fixture did not open")
	}
	open := copyToWire(4, messageSlot, copySlot, 0, 9, 0x2000)
	if !bytes.Equal(run(open, 65535), ropError(0x4d, copySlot, 0)) {
		t.Fatal("native CopyTo could not start")
	}
	run([]byte{1, 4, messageSlot}, 65535)
	tokens, chunks := readCopyToTransfer(t, run, copySlot, 16<<10)
	got := copyToTestData(tokens)
	matches := len(got) == len(expected)
	if matches {
		for i, want := range expected {
			matches = matches && bytes.Equal(got[i], want)
		}
	}
	checks["copy_to_produces_transfer_handle"] = true
	checks["copy_to_attachment_bytes_match_mime"] = matches
	checks["copy_to_delivers_attachment_over_multiple_chunks"] = chunks >= 4
	checks["copy_to_subobjects_have_correct_wire_order"] = contentsTestSubobjectHeadersValid(tokens)
	run([]byte{1, 4, copySlot}, 65535)
	reply = run(hierarchyBufferOperation(4, copySlot, 32), 65535)
	checks["released_copy_to_handle_invalid"] = len(reply) >= 6 && binary.LittleEndian.Uint32(reply[2:]) == invalidObject
}
