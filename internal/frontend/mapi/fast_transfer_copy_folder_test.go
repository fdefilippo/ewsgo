// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"ewsgo/internal/core"
)

func TestTellVersionOnFastTransferHandle(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	const output = 3
	faiRunOK(t, f, []byte{ropFastTransferSourceCopyFolder, 4, 1, output, 0, 9})
	wire := []byte{ropTellVersion, 4, output, 0x0f, 0, 0x54, 0x80, 0x08, 0}
	transfer := f.handles[output]
	if reply := faiRunOK(t, f, wire); !bytes.Equal(reply, ropError(ropTellVersion, output, 0)) ||
		f.handles[output] != transfer {
		t.Fatalf("version declaration changed the handle or response: %x", reply)
	}
	wrongLogon := bytes.Clone(wire)
	wrongLogon[1]++
	if reply, _ := f.run(wrongLogon); !bytes.Equal(reply, ropError(ropTellVersion, output, accessDenied)) {
		t.Fatalf("another logon could declare a version on the transfer: %x", reply)
	}
	buffer := ROPBuffer{Operations: wire[:len(wire)-1], Handles: f.handles}
	if _, err := buffer.DecodeOperations(); !errors.Is(err, ErrMalformed) {
		t.Fatal("short version passed validation", err)
	}
	faiRunOK(t, f, []byte{1, 4, output})
	if reply, _ := f.run(wire); !bytes.Equal(reply, ropError(ropTellVersion, output, invalidObject)) {
		t.Fatalf("version after Release: %x", reply)
	}
}

func TestFastTransferCopyFolderThroughExecute(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	inbox := f.fids[Inbox]
	seedFAI(t, store, faiTestRecord(store, inbox, 1), faiTestRecord(store, f.fids[SentItems], 2))
	box.objects[0].MIME = []byte(attachmentFixtureMIME)
	box.objects[0].Recipients = []core.Recipient{{Kind: core.RecipientTo, Address: "folder-copy@example.invalid"}}
	run := func(wire []byte, maximum uint32) []byte {
		body, _ := f.execute(wire, maximum)
		response := storeResponse(t, body)
		f.handles = response.Handles
		return response.Operations
	}
	const output = 3
	open := []byte{0x4c, 4, 1, output, 0, 9}
	for _, unsupported := range []byte{1, 0x10} {
		wire := bytes.Clone(open)
		wire[4] = unsupported
		if reply := run(wire, 65535); !bytes.Equal(reply, ropError(0x4c, output, NotSupported)) {
			t.Fatalf("unsupported flags %02x: %x", unsupported, reply)
		}
	}
	baseline := store.allocator.contents.used
	faiRunOK(t, f, open)
	f.box.stateErr = core.ErrAccessDenied
	if reply := run(hierarchyBufferOperation(4, output, 512), 65535); len(reply) != 15 ||
		!bytes.HasPrefix(reply, ropError(0x4e, output, accessDenied)) {
		t.Fatalf("transfer read after revocation: %x", reply)
	}
	f.box.stateErr = nil
	tokens, _ := readCopyToTransfer(t, run, output, 512)
	var markers []uint32
	var folderName, class string
	for _, token := range tokens {
		switch token.tag {
		case 0x40090003, 0x40100003, 0x400c0003, 0x400d0003, 0x400b0003:
			markers = append(markers, token.tag)
		case 0x3001001f:
			if len(markers) == 1 {
				folderName, _ = token.value.(string)
			}
		case 0x3613001f:
			class, _ = token.value.(string)
		}
	}
	if !reflect.DeepEqual(markers, []uint32{0x40090003, 0x40100003, 0x400d0003, 0x400c0003, 0x400d0003, 0x400b0003}) ||
		folderName != "arbitrary locale/name" || class != "IPF.Note" {
		t.Fatalf("folder properties or structure: %x, %q, %q", markers, folderName, class)
	}
	parts := copyToTestData(tokens)
	if len(parts) != 2 || !bytes.Equal(parts[0], []byte("first")) || !bytes.Equal(parts[1], []byte{0, 127, 128, 255}) {
		t.Fatal("folder export dropped by-value attachments")
	}
	run([]byte{1, 4, output}, 65535)
	if store.allocator.contents.used != baseline {
		t.Fatal("CopyFolder Release retained transfer memory")
	}
}
