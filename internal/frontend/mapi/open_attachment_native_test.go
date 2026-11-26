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

func qualifyNativeAttachments(ctx context.Context, t *testing.T, session core.Session, store *ramStore, fid uint64,
	checks map[string]bool, run func([]byte, uint32) []byte, stream bool) int {
	t.Helper()
	lease, err := store.reserveContentsPeak(ctx, 2*core.MaxMailContentViewBytes)
	if err != nil {
		t.Fatal("native attachment probe observation budget unavailable")
	}
	defer lease.release()
	snapshot, err := store.observeMailMessages(ctx, session, fid)
	if err != nil {
		t.Fatal("native Inbox observation failed; details suppressed")
	}
	var mid uint64
	var rows []contentsValues
	var number int
	for _, message := range snapshot.messages {
		projection, err := projectMailContents(message.object)
		if err != nil {
			continue
		}
		for i, row := range projection.attachments {
			data, _ := row[fixedContentsProperty(0x37010102)].(core.Binary)
			_, named := row[fixedContentsProperty(0x3707001f)].(core.Text)
			if named && len(data) > 0 && len(data) <= 60<<10 && (!stream || len(data) >= 4) {
				mid, rows, number = message.mid, projection.attachments, i
				break
			}
		}
		if mid != 0 {
			break
		}
	}
	if mid == 0 {
		t.Fatal("no Inbox message with a named by-value attachment suitable for the probe")
	}
	if err = lease.shrink(snapshot.retainedBytes + maxMailProjectionBytes); err != nil {
		t.Fatal("native observation accounting failed")
	}
	const messageSlot, attachmentSlot = 2, 3
	opened := run(openMessageOperation(4, 1, messageSlot, fid, mid, 0x0fff, 0), 65535)
	if len(opened) < openMessageMinimum || !bytes.HasPrefix(opened, ropError(3, messageSlot, 0)) {
		t.Fatal("native Message did not open for attachment reading")
	}
	const tableSlot = 3
	openTable := []byte{0x21, 4, messageSlot, tableSlot, 0x40}
	if !bytes.Equal(run(openTable, 65535), ropError(0x21, tableSlot, 0)) {
		t.Fatal("native Attachment Table did not open")
	}
	tags := []uint32{0x0e210003, 0x3707001f, 0x370b0003, 0x0ff60102, 0x0ff90102, 0x37010102}
	if !bytes.Equal(run(tableColumnsWire(tableSlot, tags...), 65535), append(ropError(0x12, tableSlot, 0), 0)) {
		t.Fatal("native Attachment Table did not accept columns")
	}
	var recordKey []byte
	matches := true
	for i, row := range rows {
		page := decodeTableRows(t, run(tableQueryWire(tableSlot, 0, 1, 1), 65535), tags)
		if len(page) != 1 {
			t.Fatal("native Attachment Table page did not have one row")
		}
		name, hasName := row[fixedContentsProperty(0x3707001f)].(core.Text)
		var expected any = PropertyNotFound
		if hasName {
			expected = string(name)
		}
		got := page[0]
		instance, _ := got[3].([]byte)
		record, _ := got[4].([]byte)
		matches = matches && got[0] == uint32(i) && got[1] == expected && got[2] == ^uint32(0) &&
			len(instance) == 8 && len(record) == 32 && got[5] == PropertyNotFound
		if i == number {
			recordKey = record
		}
	}
	checks["attachment_table_pages_match_mime_metadata"] = matches
	checks["attachment_table_exhausted"] = len(decodeTableRows(t, run(tableQueryWire(tableSlot, 0, 1, 1), 65535), tags)) == 0
	run([]byte{1, 4, tableSlot}, 65535)
	checks["released_attachment_table_invalid"] = bytes.Equal(run(tableQueryWire(tableSlot, 0, 1, 1), 65535), ropError(0x15, tableSlot, invalidObject))
	list := []byte{0x52, 4, messageSlot}
	want := binary.LittleEndian.AppendUint16(ropError(0x52, messageSlot, 0), uint16(len(rows)))
	for i := range rows {
		want = append32(want, uint32(i))
	}
	checks["attachment_numbers_match_mime_projection"] = bytes.Equal(run(list, 65535), want)
	missing := attachmentOpenWire(4, messageSlot, attachmentSlot, 0, uint32(len(rows)))
	checks["missing_attachment_not_found"] = bytes.Equal(run(missing, 65535), ropError(0x22, attachmentSlot, PropertyNotFound))
	open := attachmentOpenWire(4, messageSlot, attachmentSlot, 3, uint32(number))
	checks["by_value_attachment_opens"] = bytes.Equal(run(open, 65535), ropError(0x22, attachmentSlot, 0))
	props := folderPropertiesOperation(4, attachmentSlot, 1, 0x0e210003, 0x3707001f, 0x37050003, 0x37010102, 0x0ff90102)
	got := run(props, 65535)
	name := rows[number][fixedContentsProperty(0x3707001f)].(core.Text)
	nameWire, ok := propertyString(string(name), 0x1f, 0)
	if !ok {
		t.Fatal("native filename cannot be encoded")
	}
	data := rows[number][fixedContentsProperty(0x37010102)].(core.Binary)
	want = append32(append(ropError(7, attachmentSlot, 0), 0), uint32(number))
	want = append32(append(want, nameWire...), 1)
	want = append(binary.LittleEndian.AppendUint16(want, uint16(len(data))), data...)
	want = append(binary.LittleEndian.AppendUint16(want, uint16(len(recordKey))), recordKey...)
	checks["attachment_filename_bytes_and_record_key_match"] = bytes.Equal(got, want)
	if stream {
		qualifyNativeAttachmentStream(t, checks, run, attachmentSlot, data)
	}
	run([]byte{1, 4, attachmentSlot}, 65535)
	checks["released_attachment_handle_invalid"] = bytes.Equal(run(props, 65535), ropError(7, attachmentSlot, invalidObject))
	return len(rows)
}

func qualifyNativeAttachmentStream(t *testing.T, checks map[string]bool, run func([]byte, uint32) []byte, attachmentSlot byte, data []byte) {
	t.Helper()
	const slot = 1 // Reuse the Folder slot after the attachment has opened.
	n := uint32(len(data))
	open := run(streamOpenWire(attachmentSlot, slot, 0, 0x37010102), 65535)
	if !bytes.Equal(open, append32(ropError(0x2b, slot, 0), n)) {
		t.Fatal("native Attachment Stream did not open with correct size")
	}
	size := []byte{0x5e, 4, slot}
	checks["attachment_stream_size_matches_mime"] = bytes.Equal(run(size, 65535), append32(ropError(0x5e, slot, 0), n))
	var result []byte
	read := streamReadWire(slot, uint16((len(data)+3)/4))
	for range 4 {
		part := decodeStreamRead(t, run(read, 65535), slot)
		result = append(result, part...)
	}
	checks["attachment_stream_chunks_match_mime"] = bytes.Equal(result, data)
	checks["attachment_stream_end_returns_zero_bytes"] = len(decodeStreamRead(t, run(read, 65535), slot)) == 0
	seek := streamSeekWire(slot, 2, -3)
	checks["attachment_stream_can_seek_from_end"] = bytes.Equal(run(seek, 65535), binary.LittleEndian.AppendUint64(ropError(0x2e, slot, 0), uint64(n-3)))
	packed := append32(streamReadWire(slot, 0xbabe), 80<<10)
	checks["attachment_stream_packed_read_after_seek"] = bytes.Equal(decodeStreamRead(t, run(packed, 65535), slot), data[n-3:])
	run([]byte{1, 4, slot}, 65535)
	checks["released_attachment_stream_invalid"] = bytes.Equal(run(size, 65535), ropError(0x5e, slot, invalidObject))
}
