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

func allPropertiesWire(logon, input byte, unicode bool) []byte {
	wire := []byte{ropGetPropertiesAll, logon, input, 0, 0, 0, 0}
	if unicode {
		wire[5] = 1
	}
	return wire
}

func TestGetPropertiesAllLogonAndBatchBudget(t *testing.T) {
	f, source := logonNameFixture(t)
	mapped, _ := f.transport.namespace.Mailbox(f.session.Identity())
	entryID := addressBookEntryFixture(mapped.LegacyDN)
	want := append(ropError(ropGetPropertiesAll, 0, 0), 3, 0)
	for _, tag := range []uint32{0x66190102, 0x661b0102} {
		want = append32(want, tag)
		want = binary.LittleEndian.AppendUint16(want, uint16(len(entryID)))
		want = append(want, entryID...)
	}
	tail := addressTypesResponse(0)
	for _, test := range []struct {
		unicode bool
		tag     uint32
		name    []byte
	}{
		{true, 0x661c001f, []byte{'A', 0, 0xac, 0x20, 0, 0}},
		{false, 0x661c001e, []byte{'A', 0x80, 0}},
	} {
		wire := append(allPropertiesWire(4, 0, test.unicode), 0x49, 4, 0)
		expected := append(append32(bytes.Clone(want), test.tag), test.name...)
		expected = append(expected, tail...)
		maximum := uint32(10 + 4*len(f.handles) + len(expected))
		got, _ := f.execute(wire, maximum)
		if raw := storeResponse(t, got).Operations; !bytes.Equal(raw, expected) {
			t.Fatal("Logon tagged property list or continuation error")
		}
		got, _ = f.execute(wire, maximum-1)
		expectFailure := append(ropError(ropGetPropertiesAll, 0, tableBufferTooSmall), tail...)
		if raw := storeResponse(t, got).Operations; !bytes.Equal(raw, expectFailure) {
			t.Fatal("short output did not preserve the following ROP")
		}
	}
	withReads := source.calls
	correct := allPropertiesWire(4, 0, true)
	for _, wire := range [][]byte{correct[:6], append(bytes.Clone(correct), correct[:6]...)} {
		got, _ := f.execute(wire, 65535)
		if !bytes.Equal(got, executeError(0x80070057, 65535)) || source.calls != withReads {
			t.Fatal("malformed batch reached the directory")
		}
	}
	if raw, _ := f.run(allPropertiesWire(5, 0, true)); !bytes.Equal(raw, ropError(ropGetPropertiesAll, 0, accessDenied)) {
		t.Fatal("GetPropertiesAll read the wrong logon")
	}
	folder := faiRunOK(t, f, allPropertiesWire(4, 1, true))
	if !bytes.Contains(folder, append32(nil, folderEntryIDTag)) || bytes.Contains(folder, append32(nil, 0x661c001f)) {
		t.Fatal("Folder projection did not match its object type")
	}
}

func TestGetPropertiesAllMessageAndAttachment(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &attachmentFixtureMailbox{openMessageFixtureMailbox: &openMessageFixtureMailbox{messageFixtureMailbox: base}}
	f.session = &testSession{box}
	f.state.codePage = 1252
	message := &box.objects[0]
	data := base64.StdEncoding.EncodeToString(bytes.Repeat([]byte{'X'}, 90<<10))
	message.MIME = bytes.Replace([]byte(attachmentFixtureMIME), []byte("AH+A/w=="), []byte(data), 1)
	message.Properties[core.Subject] = core.Text("Dati")
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	fid := f.fids[Inbox]
	snapshot, err := store.observeMailMessages(t.Context(), f.session, fid)
	if err != nil || len(snapshot.messages) != 1 {
		t.Fatal("message fixture unavailable", err)
	}
	faiRunOK(t, f, openMessageOperation(4, 1, 2, fid, snapshot.messages[0].mid, 0x0fff, 0))
	raw := faiRunOK(t, f, allPropertiesWire(4, 2, false))
	if !bytes.Contains(raw, append(append32(nil, 0x0037001e), "Dati\x00"...)) ||
		!bytes.Contains(raw, append(append32(nil, 0x1000001e), "body\x00"...)) {
		t.Fatal("Message tagged properties omitted the subject or body")
	}
	faiRunOK(t, f, attachmentOpenWire(4, 2, 3, 0, 1))
	raw = faiRunOK(t, f, allPropertiesWire(4, 3, false))
	binaryError := append32(append32(nil, 0x3701000a), 0x8007000e)
	if !bytes.Contains(raw, binaryError) ||
		!bytes.Contains(raw, append(append32(nil, 0x3707001e), "\xe9.bin\x00"...)) {
		t.Fatal("Attachment omitted the oversized property error or returned the wrong filename encoding")
	}
	faiRunOK(t, f, streamOpenWire(3, 2, 0, attachDataBinary))
	if raw, _ := f.run(allPropertiesWire(4, 2, true)); !bytes.Equal(raw, ropError(ropGetPropertiesAll, 2, NotSupported)) {
		t.Fatal("Stream was treated as an object with properties")
	}
}
