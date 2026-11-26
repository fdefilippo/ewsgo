// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
)

func TestServiceFolderReferencesMatchOpenedObjectsAndHierarchy(t *testing.T) {
	f, b := hierarchyTableFixture(t)
	binding, code := f.state.mailbox.object(f.handles[0], 4)
	if code != 0 {
		t.Fatal("missing private logon")
	}
	store := binding.binding.store
	const journalTag, notesTag = 0x36d20102, 0x36d30102
	folders := []struct {
		tag         uint32
		name, class string
		fid         uint64
		entry       []byte
	}{
		{tag: journalTag, name: "Journal", class: "IPF.Journal"},
		{tag: notesTag, name: "Notes", class: "IPF.StickyNote"},
	}
	f.open(int(Inbox))
	readEntries := func() []byte {
		t.Helper()
		row, c := f.run(folderPropertiesOperation(4, 1, 1, journalTag, notesTag))
		if c.properties.Values != 2 || len(row) != 103 || row[6] != 0 {
			t.Fatal("expected two canonical binary properties")
		}
		return row[7:]
	}
	entries := readEntries()
	f.open(int(MailboxRoot))
	if !bytes.Equal(entries, readEntries()) {
		t.Fatal("mailbox root and Inbox returned different references")
	}
	for i := range folders {
		item := &folders[i]
		value := entries[i*48:]
		if binary.LittleEndian.Uint16(value) != 46 {
			t.Fatal("invalid EntryID length")
		}
		item.entry = value[2:48]
		converted, c := f.run(conversionOperation(0x44, item.entry[22:]))
		if len(converted) != 14 || c.errors != 0 {
			t.Fatal("EntryID conversion failed without native special-folder spikes")
		}
		item.fid = binary.LittleEndian.Uint64(converted[6:])
		opened, _ := f.run(folderOperation(item.fid, 4, 0, 1))
		if !bytes.Equal(opened, []byte{2, 1, 0, 0, 0, 0, 0, 0}) {
			t.Fatal("EntryID did not open its own folder", item.name)
		}
		row, _ := f.run(folderPropertiesOperation(4, 1, 1, 0x3613001f, 0x67490014))
		class, _ := propertyString(item.class, 0x001f, 1252)
		want := binary.LittleEndian.AppendUint64(append([]byte{7, 1, 0, 0, 0, 0, 0}, class...), f.fids[IPMSubtree])
		if !bytes.Equal(row, want) {
			t.Fatal("incorrect folder class or parent", item.name)
		}
	}
	if folders[0].fid == folders[1].fid {
		t.Fatal("special folders share an identity")
	}

	f.open(int(IPMSubtree))
	hierarchyTableOpen(t, f, 0x50)
	tags := []uint32{0x67480014, 0x3001001f, 0x3613001f}
	f.run(tableColumnsWire(2, tags...))
	rowWire, _ := f.run(tableQueryWire(2, 0, 1, 256))
	rows := decodeTableRows(t, rowWire, tags)
	for _, item := range folders {
		found := false
		for _, row := range rows {
			found = found || row[0] == item.fid && row[1] == item.name && row[2] == item.class
		}
		if !found {
			t.Fatal("same object missing from the IPM subtree", item.name)
		}
	}

	view, err := store.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime)
	if err != nil {
		t.Fatal(err)
	}
	stream, err := hierarchyFastTransfer(view.replica, binding.binding.mailboxGUID, synchronizationConfigure{}, hierarchyDelta{changes: view.folders})
	if err != nil {
		t.Fatal(err)
	}
	tokens := readTestFX(t, stream.data)
	for _, item := range folders {
		refs := 0
		for _, token := range tokens {
			if token.tag == item.tag && bytes.Equal(token.value, item.entry) {
				refs++
			}
		}
		if refs != 1 {
			t.Fatal("hierarchy lost or duplicated the Inbox reference", item.name)
		}
	}

	f.open(int(Inbox))
	mappings, next := len(store.byFID), store.next
	setRequest := []byte{0x0a, 4, 1, 60, 0, 2, 0}
	setRequest = binary.LittleEndian.AppendUint32(setRequest, journalTag)
	setRequest = append(setRequest, 46, 0)
	setRequest = append(setRequest, folders[0].entry...)
	setRequest = binary.LittleEndian.AppendUint32(setRequest, notesTag)
	setRequest = append(setRequest, 0, 0)
	result, _ := f.run(setRequest)
	want := []byte{0x0a, 1, 0, 0, 0, 0, 2, 0}
	for i, item := range folders {
		want = binary.LittleEndian.AppendUint16(want, uint16(i))
		want = binary.LittleEndian.AppendUint32(want, item.tag)
		want = binary.LittleEndian.AppendUint32(want, accessDenied)
	}
	if !bytes.Equal(result, want) || len(store.byFID) != mappings || store.next != next || !bytes.Equal(entries, readEntries()) {
		t.Fatal("Set must return one access-denied problem per computed property and keep both intact")
	}
	f.open(int(SentItems))
	row, c := f.run(folderPropertiesOperation(4, 1, 1, journalTag))
	if c.properties.Errors != 1 || !bytes.Equal(row[6:], binary.LittleEndian.AppendUint32([]byte{1, 0x0a}, PropertyNotFound)) {
		t.Fatal("reference was visible outside Inbox and mailbox root")
	}
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	f.open(int(Inbox))
	if !bytes.Equal(entries, readEntries()) {
		t.Fatal("reference changed after reconnect")
	}
	b.state.Owner = false
	denied, _ := f.run(folderOperation(folders[0].fid, 4, 0, 1))
	if !bytes.Equal(denied, ropError(2, 1, accessDenied)) {
		t.Fatal("lost ownership must also deny OpenFolder with a cached FID")
	}
}
