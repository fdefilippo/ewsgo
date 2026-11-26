// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"

	"ewsgo/internal/core"
)

func copyMessagesWire(logon, input, output, flags, sendOptions byte, mids ...uint64) []byte {
	wire := binary.LittleEndian.AppendUint16([]byte{0x4b, logon, input, output}, uint16(len(mids)))
	for _, mid := range mids {
		wire = binary.LittleEndian.AppendUint64(wire, mid)
	}
	return append(wire, flags, sendOptions)
}

func TestFastTransferCopyMessagesThroughExecute(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	f.state.codePage = 1252
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	inbox, sent := f.fids[Inbox], f.fids[SentItems]
	first := box.objects[0]
	first.MIME = []byte(attachmentFixtureMIME)
	first.Recipients = []core.Recipient{{Kind: core.RecipientTo, Address: "fx@example.invalid"}}
	second := messageTestObject("opaque-second", first.Parent)
	second.MIME = []byte("Subject: second\r\nContent-Type: text/plain\r\n\r\nsecond body")
	sentMessage := messageTestObject("opaque-sent", store.byFID[sent].native)
	box.objects = []core.Object{sentMessage}
	foreign, err := store.observeMailMessages(t.Context(), f.session, sent)
	if err != nil || len(foreign.messages) != 1 {
		t.Fatal("could not observe the foreign MID", err)
	}
	box.objects = []core.Object{first, second}
	incoming, err := store.observeMailMessages(t.Context(), f.session, inbox)
	if err != nil || len(incoming.messages) != 2 {
		t.Fatal("could not observe Inbox MIDs", err)
	}
	firstRow, secondRow := incoming.messages[0], incoming.messages[1]
	if firstRow.object.ID != first.ID || secondRow.object.ID != second.ID {
		t.Fatal("unexpected fixture order")
	}
	run := func(wire []byte, maximum uint32) []byte {
		body, _ := f.execute(wire, maximum)
		response := storeResponse(t, body)
		f.handles = response.Handles
		return response.Operations
	}
	const output = 3
	open := copyMessagesWire(4, 1, output, 0x10, 9, secondRow.mid, firstRow.mid)
	before := box.sourceCalls
	baseline := store.allocator.contents.used
	body, _ := f.execute(append(bytes.Clone(open), 0x4b, 4, 1, 2, 1, 0), 65535)
	if !bytes.Equal(body, executeError(0x80070057, 65535)) || box.sourceCalls != before {
		t.Fatal("a truncated CopyMessages request executed the valid batch prefix")
	}
	for _, tc := range []struct {
		wire []byte
		code uint32
	}{
		{copyMessagesWire(4, 0, output, 0, 9, firstRow.mid), NotSupported},
		{copyMessagesWire(5, 1, output, 0, 9, firstRow.mid), accessDenied},
		{copyMessagesWire(4, 1, output, 0, 9), 0x80070057},
		{copyMessagesWire(4, 1, output, 0x08, 9, firstRow.mid), 0x80070057},
		{copyMessagesWire(4, 1, output, 0, 0x10, firstRow.mid), 0x80070057},
		{copyMessagesWire(4, 1, output, 1, 9, firstRow.mid), accessDenied},
	} {
		if !bytes.Equal(run(tc.wire, 65535), ropError(0x4b, output, tc.code)) || box.sourceCalls != before {
			t.Fatal("invalid CopyMessages was accepted or read the folder")
		}
	}
	out := copyMessagesWire(4, 1, output, 0, 9, firstRow.mid, foreign.messages[0].mid)
	if !bytes.Equal(run(out, 65535), ropError(0x4b, output, PropertyNotFound)) ||
		f.handles[output] != 0xffffffff || store.allocator.contents.used != baseline {
		t.Fatal("a foreign MID published transfer state")
	}

	for _, withIDs := range []bool{false, true} {
		wire := bytes.Clone(open)
		if withIDs {
			wire[len(wire)-2] |= 0x20
		}
		faiRunOK(t, f, wire)
		tokens, _ := readCopyToTransfer(t, run, output, 512)
		var roots []map[uint32]any
		var root map[uint32]any
		for _, token := range tokens {
			switch token.tag {
			case 0x400c0003:
				if root != nil {
					t.Fatal("nested message without an embedded attachment")
				}
				root = map[uint32]any{}
			case 0x400d0003:
				if root == nil {
					t.Fatal("EndMessage without StartMessage")
				}
				roots = append(roots, root)
				root = nil
			default:
				if root == nil {
					t.Fatal("property outside a message")
				}
				root[token.tag] = token.value
			}
		}
		if root != nil || len(roots) != 2 || roots[0][0x0037001f] != "second" ||
			roots[0][0x1000001f] != "second body" || roots[1][0x0037001f] != "synthetic" {
			t.Fatal("CopyMessages did not preserve message boundaries, body and request order")
		}
		parts := copyToTestData(tokens)
		if len(parts) != 2 || !bytes.Equal(parts[0], []byte("first")) ||
			!bytes.Equal(parts[1], []byte{0, 127, 128, 255}) ||
			!slices.ContainsFunc(tokens, func(token decodedContentsToken) bool { return token.tag == 0x40030003 }) ||
			!contentsTestSubobjectHeadersValid(tokens) {
			t.Fatal("CopyMessages dropped recipients or by-value attachments")
		}
		for i, row := range []messageRow{secondRow, firstRow} {
			for _, tag := range []uint32{0x3a120102, 0x65e00102, 0x65e20102, 0x65e30102, 0x30080040} {
				if _, present := roots[i][tag]; present != withIDs {
					t.Fatalf("SendEntryId=%t produced incorrect presence for %08x", withIDs, tag)
				}
			}
			if withIDs && (!bytes.Equal(roots[i][0x65e00102].([]byte), row.sourceKey[:]) ||
				!bytes.Equal(roots[i][0x65e20102].([]byte), []byte(row.changeKey)) ||
				!bytes.Equal(roots[i][0x65e30102].([]byte), row.predecessors)) {
				t.Fatal("CopyMessages sent the wrong snapshot identity")
			}
			if _, present := roots[i][0x0fff0102]; present {
				t.Fatal("CopyMessages sent PidTagEntryId")
			}
		}
		run([]byte{1, 4, output}, 65535)
		if store.allocator.contents.used != baseline {
			t.Fatal("CopyMessages Release retained transfer memory")
		}
	}
}
