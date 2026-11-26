// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"testing"

	"ewsgo/internal/core"
)

func reverseNamesOperation(logon, slot byte, ids ...uint16) []byte {
	out := binary.LittleEndian.AppendUint16([]byte{0x55, logon, slot}, uint16(len(ids)))
	for _, id := range ids {
		out = binary.LittleEndian.AppendUint16(out, id)
	}
	return out
}

func expectedNamedProperties(op, slot byte, ids []uint16, names ...propertyName) []byte {
	out := binary.LittleEndian.AppendUint16(ropError(op, slot, 0), uint16(len(names)))
	for _, id := range ids {
		out = binary.LittleEndian.AppendUint16(out, id)
	}
	return append(out, propertyIDsOperation(0, 0, 0, names...)[6:]...)
}

func TestReadNamedPropertiesMixedBatchAndOutputLimit(t *testing.T) {
	transport, session, _, cookies := storeFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	name := propertyName{Set: [16]byte{1}, Kind: 1, Name: "é😀"}
	lid := propertyName{Set: name.Set, LID: 0x12345678}
	reverse := reverseNamesOperation(3, 0, 0x8002, 0x3001, 0xfffe, 0x8001, 0x8002)
	ops := append(logonOperation(identity.LegacyDN, 3, 0), propertyIDsOperation(3, 0, 2, name, lid)...)
	ops = append(ops, reverse...)
	ops = append(ops, 0x5f, 3, 0, 0, 0)
	body, cookies := storeExecute(t, transport, session, cookies, ops, []uint32{0xffffffff}, 4096)
	response := storeResponse(t, body)
	want := expectedNamedProperties(0x55, 0, nil, lid, propertyName{Set: psMAPI, LID: 0x3001}, propertyName{Kind: 0xff}, name, lid)
	want = append(want, expectedNamedProperties(0x5f, 0, nil)...)
	if len(response.Operations) != 166+12+len(want) || !bytes.Equal(response.Operations[166+12:], want) {
		t.Fatal("same-batch reverse lookup, duplicate or registry/object scope differs")
	}
	// A list that does not fit gets a six-byte ROP error. The next ROP still runs.
	ops = append(reverse, 0x5f, 3, 0, 0, 0)
	body, _ = storeExecute(t, transport, session, cookies, ops, response.Handles, 64)
	want = append(ropError(0x55, 0, tableBufferTooSmall), expectedNamedProperties(0x5f, 0, nil)...)
	if got := storeResponse(t, body).Operations; !bytes.Equal(got, want) {
		t.Fatal("oversized list was truncated or prevented the next ROP")
	}
}

func TestQueryNamedPropertiesOnAssociatedMessage(t *testing.T) {
	f, _, _ := newFAIExecuteFixture(t)
	name := propertyName{Set: [16]byte{1}, Kind: 1, Name: "é😀"}
	lid := propertyName{Set: [16]byte{2}, LID: 99}
	unused := propertyName{Set: name.Set, LID: 123}
	faiRunOK(t, f, propertyIDsOperation(4, 0, 2, name, lid, unused))
	faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
	faiRunOK(t, f, faiSetOperation(3, append(faiCreateProperties(),
		faiSetRequest{{0x8001001f, core.Text("private value")}, {0x8002000b, core.Bool(true)}}...)))
	check := func(slot, flags byte, set *[16]byte, ids []uint16, names ...propertyName) {
		t.Helper()
		op := []byte{0x5f, 4, slot, flags, 0}
		if set != nil {
			op[4] = 2 // Any nonzero value indicates a GUID.
			op = append(op, set[:]...)
		}
		got := faiRunOK(t, f, op)
		if !bytes.Equal(got, expectedNamedProperties(0x5f, slot, ids, names...)) {
			t.Fatalf("wrong named-property list for slot %d and flags %#x", slot, flags)
		}
	}
	check(0, 0, nil, nil) // Store.
	check(1, 0, nil, nil) // Folder.
	check(3, 0x80, nil, []uint16{0x8001, 0x8002}, name, lid)
	check(3, 1, nil, []uint16{0x8002}, lid)
	check(3, 2, nil, []uint16{0x8001}, name)
	check(3, 3, nil, nil)
	check(3, 0, &name.Set, []uint16{0x8001}, name)
	check(3, 1, &name.Set, nil)
	check(3, 0, &[16]byte{}, nil)
	saved := faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 2})
	mid := binary.LittleEndian.Uint64(saved[7:])
	folder, _ := f.state.mailbox.lookup(f.handles[1], 4)
	faiRunOK(t, f, openMessageOperation(4, 1, 4, folder.fid, mid, 0x0fff, 0))
	check(4, 0, nil, []uint16{0x8001, 0x8002}, name, lid)
	for _, tc := range []struct {
		logon, slot byte
		code        uint32
	}{{4, 2, NotSupported}, {5, 3, accessDenied}} {
		got, _ := f.run([]byte{0x5f, tc.logon, tc.slot, 0, 0})
		if !bytes.Equal(got, ropError(0x5f, tc.slot, tc.code)) {
			t.Fatal("typed handle or logon scope bypass")
		}
	}
}

func TestNamedPropertyReadDecodersRejectTruncatedBatch(t *testing.T) {
	reverse := reverseNamesOperation(3, 0, 0x3001)
	query := append([]byte{0x5f, 3, 0, 0, 1}, make([]byte, 16)...)
	for _, op := range [][]byte{reverse, query} {
		for size := 1; size < len(op); size++ {
			ops := append(propertyIDsOperation(3, 0, 2, propertyName{Set: [16]byte{1}}), op[:size]...)
			buffer := ROPBuffer{Operations: ops, Handles: []uint32{1}}
			if got, err := buffer.DecodeOperations(); err == nil || got != nil {
				t.Fatalf("accepted incomplete ROP %#x at length %d", op[0], size)
			}
		}
	}
}

func TestNamedPropertyReadsOnNativeMessageAndAttachment(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &attachmentFixtureMailbox{openMessageFixtureMailbox: &openMessageFixtureMailbox{messageFixtureMailbox: base}}
	f.session = &testSession{box}
	message := &box.objects[0]
	message.MIME = []byte(attachmentFixtureMIME)
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	fid := f.fids[Inbox]
	snapshot, err := store.observeMailMessages(t.Context(), f.session, fid)
	if err != nil || len(snapshot.messages) != 1 {
		t.Fatal("fixture unavailable", err)
	}
	faiRunOK(t, f, openMessageOperation(4, 1, 2, fid, snapshot.messages[0].mid, 0x0fff, 0))
	faiRunOK(t, f, attachmentOpenWire(4, 2, 3, 0, 0))
	before := box.mimeReads
	name := propertyName{Set: [16]byte{1}, LID: 42}
	faiRunOK(t, f, propertyIDsOperation(4, 0, 2, name))
	for _, slot := range []byte{2, 3} {
		read := reverseNamesOperation(4, slot, 0x8001)
		query := []byte{0x5f, 4, slot, 0, 0}
		got := faiRunOK(t, f, append(read, query...))
		want := append(expectedNamedProperties(0x55, slot, nil, name), expectedNamedProperties(0x5f, slot, nil)...)
		if !bytes.Equal(got, want) || box.mimeReads != before {
			t.Fatal("native object returned unexpected names or downloaded MIME")
		}
	}
	message.Properties[core.ContentRevision] = message.Properties[core.ContentRevision].(core.Integer) + 1
	got, _ := f.run([]byte{0x5f, 4, 3, 0, 0})
	if !bytes.Equal(got, ropError(0x5f, 3, 0x80040109)) || box.mimeReads != before {
		t.Fatal("query accepted a stale attachment")
	}
}
