// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/base64"
	"encoding/binary"
	"testing"

	"ewsgo/internal/core"
)

func copyToWire(logon, input, output, level, sendOptions byte, flags uint32, tags ...uint32) []byte {
	out := append32([]byte{0x4d, logon, input, output, level}, flags)
	out = binary.LittleEndian.AppendUint16(append(out, sendOptions), uint16(len(tags)))
	for _, tag := range tags {
		out = append32(out, tag)
	}
	return out
}

func copyPropertiesWire(logon, input, output, level, sendOptions, flags byte, tags ...uint32) []byte {
	out := binary.LittleEndian.AppendUint16([]byte{0x69, logon, input, output, level, flags, sendOptions}, uint16(len(tags)))
	for _, tag := range tags {
		out = append32(out, tag)
	}
	return out
}

func readCopyToTransfer(t *testing.T, run func([]byte, uint32) []byte, handle byte, chunkSize uint16) ([]decodedContentsToken, int) {
	t.Helper()
	wire := hierarchyBufferOperation(4, handle, chunkSize)
	var raw []byte
	var chunks []int
	for len(chunks) < 64 {
		reply := run(wire, 65535)
		if len(reply) < 15 || !bytes.HasPrefix(reply, ropError(0x4e, handle, 0)) ||
			reply[12] != 0 || int(binary.LittleEndian.Uint16(reply[13:])) != len(reply)-15 ||
			len(reply)-15 > int(chunkSize) {
			t.Fatal("invalid CopyTo buffer response")
		}
		raw = append(raw, reply[15:]...)
		chunks = append(chunks, len(raw))
		switch binary.LittleEndian.Uint16(reply[6:8]) {
		case 1:
			if len(reply) == 15 {
				t.Fatal("CopyTo partial response made no progress")
			}
		case 3:
			tokens := decodeTestContents(t, raw)
			for _, token := range tokens {
				for _, boundary := range chunks {
					if token.start < boundary && boundary < token.definitionEnd {
						t.Fatal("CopyTo split a property definition")
					}
				}
			}
			return tokens, len(chunks)
		default:
			t.Fatal("unexpected CopyTo transfer status")
		}
	}
	t.Fatal("CopyTo did not finish")
	return nil, 0
}

func copyToTestData(tokens []decodedContentsToken) (payloads [][]byte) {
	for _, token := range tokens {
		if token.tag == attachDataBinary {
			payloads = append(payloads, token.value.([]byte))
		}
	}
	return payloads
}

func TestFastTransferCopyToThroughExecute(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &attachmentFixtureMailbox{openMessageFixtureMailbox: &openMessageFixtureMailbox{messageFixtureMailbox: base}}
	f.session = &testSession{box}
	message := &box.objects[0]
	payload := bytes.Repeat([]byte{0, 0xff, 0x80, 0x10, 0x45}, 19000)
	message.MIME = []byte("Subject: =?UTF-8?Q?Pr=C3=B6va?=\r\nContent-Type: multipart/mixed; boundary=x\r\n\r\n" +
		"--x\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nTesto sintetico\r\n" +
		"--x\r\nContent-Type: application/octet-stream\r\nContent-Disposition: attachment; filename=copy.bin\r\n" +
		"Content-Transfer-Encoding: base64\r\n\r\n" + base64.StdEncoding.EncodeToString(payload) + "\r\n--x--\r\n")
	message.Recipients = []core.Recipient{{Kind: core.RecipientTo, Address: "recipient@example.invalid", DisplayName: "EWSGo test"}}
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	fid := f.fids[Inbox]
	snapshot, err := store.observeMailMessages(t.Context(), f.session, fid)
	if err != nil || len(snapshot.messages) != 1 {
		t.Fatal("CopyTo fixture unavailable", err)
	}
	f.state.codePage = 1252
	faiRunOK(t, f, openMessageOperation(4, 1, 2, fid, snapshot.messages[0].mid, 0x0fff, 0))
	run := func(wire []byte, maximum uint32) []byte {
		body, _ := f.execute(wire, maximum)
		response := storeResponse(t, body)
		f.handles = response.Handles
		return response.Operations
	}
	const output = 3
	open := copyToWire(4, 2, output, 0, 9, 0x2000)
	before := box.mimeReads
	response, _ := f.execute(append(bytes.Clone(open), 0x4d, 4, 2), 65535)
	if !bytes.Equal(response, executeError(0x80070057, 65535)) || box.mimeReads != before {
		t.Fatal("malformed CopyTo batch executed a valid prefix")
	}
	for _, refused := range []struct {
		wire []byte
		code uint32
	}{
		{copyToWire(4, 1, output, 0, 9, 0), NotSupported},
		{copyToWire(5, 2, output, 0, 9, 0), accessDenied},
		{copyToWire(4, 2, output, 0, 9, 1), 0x80070057},
		{copyToWire(4, 2, output, 0, 0x10, 0), 0x80070057},
	} {
		if !bytes.Equal(run(refused.wire, 65535), ropError(0x4d, output, refused.code)) || box.mimeReads != before {
			t.Fatal("invalid CopyTo was accepted or read MIME")
		}
	}

	baseline := store.allocator.contents.used
	if got := run(open, 65535); !bytes.Equal(got, ropError(0x4d, output, 0)) || box.mimeReads != before+1 {
		t.Fatal("CopyTo did not open the immutable message view")
	}
	// GetBuffer on a CopyTo context must not depend on the Message handle.
	run([]byte{1, 4, 2}, 65535)
	small := run(hierarchyBufferOperation(4, output, 3), 65535)
	if len(small) != 15 || binary.LittleEndian.Uint16(small[6:]) != 2 {
		t.Fatal("GetBuffer should report NoRoom before the first property")
	}
	tokens, chunks := readCopyToTransfer(t, run, output, 16<<10)
	parts := copyToTestData(tokens)
	if chunks < 4 || len(parts) != 1 || !bytes.Equal(parts[0], payload) || box.mimeReads != before+1 ||
		!contentsTestSubobjectHeadersValid(tokens) {
		t.Fatal("CopyTo attachment bytes or subobject boundaries differ")
	}
	counts := map[uint32]int{}
	for _, token := range tokens {
		counts[token.tag]++
	}
	if counts[0x40030003] != 1 || counts[0x40000003] != 1 || counts[0x40160003] != 2 ||
		counts[0x40120003] != 0 || counts[0x40140003] != 0 || counts[0x0037001f] != 1 {
		t.Fatal("CopyTo was not serialized as a messageContent root")
	}
	run([]byte{1, 4, output}, 65535)
	if store.allocator.contents.used != baseline ||
		binary.LittleEndian.Uint32(run(hierarchyBufferOperation(4, output, 32), 65535)[2:]) != invalidObject {
		t.Fatal("Release retained CopyTo state")
	}

	faiRunOK(t, f, openMessageOperation(4, 1, 2, fid, snapshot.messages[0].mid, 0x0fff, 0))
	for _, tc := range []struct {
		level          byte
		tags           []uint32
		wantSubject    bool
		wantRecipients bool
	}{
		{0, []uint32{0x0037001f, 0x0e13000d}, false, true},
		{1, nil, true, false},
	} {
		faiRunOK(t, f, copyToWire(4, 2, output, tc.level, 0, 0, tc.tags...))
		tokens, _ := readCopyToTransfer(t, run, output, 4096)
		counts := map[uint32]int{}
		for _, token := range tokens {
			counts[token.tag]++
			if token.tag == 0x0037001e && !bytes.Equal(token.value.([]byte), []byte{'P', 'r', 0xf6, 'v', 'a', 0}) {
				t.Fatal("CopyTo did not use the connection code page")
			}
		}
		if (counts[0x0037001e] == 1) != tc.wantSubject || (counts[0x40030003] == 1) != tc.wantRecipients ||
			counts[0x40000003] != 0 || counts[0x0037001f] != 0 {
			t.Fatal("CopyTo did not apply the requested filters")
		}
		run([]byte{1, 4, output}, 65535)
	}
	before = box.mimeReads
	copySubject := copyPropertiesWire(4, 2, output, 0, 1, 0, 0x0037001f)
	for length := 3; length < len(copySubject); length++ {
		response, _ := f.execute(copySubject[:length], 65535)
		if !bytes.Equal(response, executeError(0x80070057, 65535)) || box.mimeReads != before {
			t.Fatal("truncated CopyProperties request read MIME")
		}
	}
	for _, wire := range [][]byte{
		copyPropertiesWire(4, 2, output, 0, 1, 1, 0x0037001f),
		copyPropertiesWire(4, 2, output, 0, 1, 0x10, 0x0037001f),
		copyPropertiesWire(4, 2, output, 0, 1, 0),
	} {
		if !bytes.Equal(run(wire, 65535), ropError(0x69, output, 0x80070057)) || box.mimeReads != before {
			t.Fatal("CopyProperties accepted invalid flags or count")
		}
	}
	for _, tc := range []struct {
		level          byte
		tags           []uint32
		wantSubject    bool
		wantRecipients bool
	}{
		{0, []uint32{0x0037001f, 0x0e12000d}, true, true},
		{1, []uint32{0x0037001f, 0x0e12000d}, true, false},
		{0, []uint32{0x00370003}, false, false},
	} {
		wire := copyPropertiesWire(4, 2, output, tc.level, 1, 0, tc.tags...)
		if got := run(wire, 65535); !bytes.Equal(got, ropError(0x69, output, 0)) {
			t.Fatal("CopyProperties did not open a transfer")
		}
		tokens, _ := readCopyToTransfer(t, run, output, 4096)
		var subject, recipients bool
		for _, token := range tokens {
			subject = subject || token.tag == 0x0037001f
			recipients = recipients || token.tag == 0x40030003
			if token.tag == 0x40000003 || token.tag == 0x1000001f {
				t.Fatal("CopyProperties emitted an unselected attachment or body")
			}
		}
		if subject != tc.wantSubject || recipients != tc.wantRecipients || !subject && !recipients && len(tokens) != 0 {
			t.Fatal("CopyProperties did not match the requested types and level")
		}
		run([]byte{1, 4, output}, 65535)
	}
	faiRunOK(t, f, attachmentOpenWire(4, 2, 3, 0, 0))
	faiRunOK(t, f, copyPropertiesWire(4, 3, 2, 0, 1, 0, attachDataBinary))
	tokens, _ = readCopyToTransfer(t, run, 2, 16<<10)
	parts = copyToTestData(tokens)
	if len(tokens) != 1 || len(parts) != 1 || !bytes.Equal(parts[0], payload) {
		t.Fatal("CopyProperties did not select the exact binary attachment property")
	}
	run([]byte{1, 4, 2}, 65535)
	faiRunOK(t, f, copyToWire(4, 3, 2, 7, 1, 0))
	tokens, _ = readCopyToTransfer(t, run, 2, 16<<10)
	parts = copyToTestData(tokens)
	if len(parts) != 1 || !bytes.Equal(parts[0], payload) || tokens[len(tokens)-1].tag == 0x400e0003 {
		t.Fatal("direct Attachment CopyTo root differs")
	}
	message.Properties[core.ContentRevision] = core.Integer(100)
	if got := run(hierarchyBufferOperation(4, 2, 64), 65535); binary.LittleEndian.Uint32(got[2:]) != 0x80040109 {
		t.Fatal("CopyTo served stale content after a native revision change")
	}
	run([]byte{1, 4, 0}, 65535)
	if store.allocator.contents.used != 0 || store.allocator.transfers.Load() != 0 {
		t.Fatal("Logon release retained CopyTo memory or transfer quota")
	}
}
