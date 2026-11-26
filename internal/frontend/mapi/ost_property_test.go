// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"slices"
	"testing"

	"ewsgo/internal/core"
)

func TestOSTPropertyAbsentWireAndAuthorization(t *testing.T) {
	transport, session, box, cookies := storeFixture(t)
	transport.EnableUnknownPropertyTrace()
	identity, _ := transport.namespace.Mailbox(session.Identity())
	body, cookies := storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 4, 0), []uint32{0xffffffff, 0xffffffff}, 4096)
	logon := storeResponse(t, body)
	fid := binary.LittleEndian.Uint64(logon.Operations[7+int(Inbox)*8:])
	body, cookies = storeExecute(t, transport, session, cookies, folderOperation(fid, 4, 0, 1), logon.Handles, 4096)
	opened := storeResponse(t, body)
	var state *contextState
	for _, s := range transport.contexts.entries {
		state = s
	}
	read := func(logon, input byte, tags ...uint32) ([]byte, storeResults) {
		t.Helper()
		wire, _ := (ROPBuffer{Operations: folderPropertiesOperation(logon, input, 1, tags...), Handles: opened.Handles}).Marshal()
		body, counts := transport.executeStore(context.Background(), session, state, identity, Execute{ROP: wire, MaxOutput: 4096})
		return storeResponse(t, body).Operations, counts
	}
	for _, tc := range []struct {
		tags []uint32
		want []byte
	}{
		{[]uint32{0x7c040102}, []byte{7, 1, 0, 0, 0, 0, 1, 0x0a, 0x0f, 1, 4, 0x80}},
		{[]uint32{0x7c040000}, []byte{7, 1, 0, 0, 0, 0, 1, 0x0a, 0, 0x0a, 0x0f, 1, 4, 0x80}},
		{[]uint32{0x7c040102, 0x7c040102, 0x7c040000, 0x7c040003}, []byte{7, 1, 0, 0, 0, 0, 1, 0x0a, 0x0f, 1, 4, 0x80, 0x0a, 0x0f, 1, 4, 0x80, 0x0a, 0, 0x0a, 0x0f, 1, 4, 0x80, 0x0a, 2, 1, 4, 0x80}},
	} {
		row, counts := read(4, 1, tc.tags...)
		if !bytes.Equal(row, tc.want) || counts.errors != 0 || counts.properties.Errors != len(tc.tags) || counts.properties.Values != 0 || counts.properties.Other != len(tc.tags) || len(counts.properties.PropertyNames()) != 0 {
			t.Fatal("absent OST wire response or unchanged diagnostic vocabulary")
		}
	}
	row, counts := read(4, 1, 0x7c040102, 0x67480014, 0x7c040003)
	want := append([]byte{7, 1, 0, 0, 0, 0, 1, 0x0a, 0x0f, 1, 4, 0x80, 0}, binary.LittleEndian.AppendUint64(nil, fid)...)
	want = append(want, 0x0a, 2, 1, 4, 0x80)
	if !bytes.Equal(row, want) || counts.properties.Values != 1 || counts.properties.Errors != 2 || !slices.Equal(counts.properties.PropertyNames(), []string{"folder_id"}) || !slices.Equal(counts.properties.UnknownProperties().Tags, []string{"0x7c040102", "0x7c040003"}) {
		t.Fatal("mixed OST errors changed known Folder value or tag diagnostic")
	}
	for _, tag := range []uint32{0x7c040003, 0x7c04000a, 0x7c04001f, 0x7c041102, 0x7c050102} {
		row, _ := read(4, 1, tag)
		if !bytes.Equal(row, []byte{7, 1, 0, 0, 0, 0, 1, 0x0a, 2, 1, 4, 0x80}) {
			t.Fatal("other type or property claimed absent")
		}
	}
	row, counts = read(4, 0, 0x7c040102)
	if !bytes.Equal(row, []byte{7, 0, 0, 0, 0, 0, 1, 0x0a, 2, 1, 4, 0x80}) || !counts.properties.Logon || counts.properties.Folder || counts.properties.Errors != 1 || counts.errors != 0 {
		t.Fatal("OST request implicitly converted Logon to Folder")
	}
	row, counts = read(5, 1, 0x7c040102)
	if binary.LittleEndian.Uint32(row[2:]) != accessDenied || counts.properties.Errors != 0 {
		t.Fatal("foreign Logon bypassed authorization")
	}
	box.getErr = core.ErrAccessDenied
	row, counts = read(4, 1, 0x7c040102)
	if !bytes.Equal(row, []byte{7, 1, 5, 0, 7, 0x80}) || counts.properties.Errors != 0 || counts.errors != 1 {
		t.Fatal("absence returned instead of fresh native authorization failure")
	}
	box.getErr = nil
	// A response too small for the complete batch must not execute Release.
	ops := append([]byte{1, 4, 1}, folderPropertiesOperation(4, 1, 1, 0x7c040102)...)
	body, cookies = storeExecute(t, transport, session, cookies, ops, opened.Handles, 29)
	if storeResponse(t, body).Operations[0] != 0xff {
		t.Fatal("OST output preflight accepted undersized batch")
	}
	row, _ = read(4, 1, 0x7c040102)
	if !bytes.Equal(row, []byte{7, 1, 0, 0, 0, 0, 1, 0x0a, 0x0f, 1, 4, 0x80}) {
		t.Fatal("output preflight released the Folder handle")
	}
	_, cookies = storeExecute(t, transport, session, cookies, []byte{1, 4, 1}, opened.Handles, 4096)
	row, counts = read(4, 1, 0x7c040102)
	if binary.LittleEndian.Uint32(row[2:]) != invalidObject || counts.properties.Errors != 0 {
		t.Fatal("released Folder returned a property absence")
	}
}
