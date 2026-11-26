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

func TestAttachmentTableThroughExecute(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	box := &attachmentFixtureMailbox{openMessageFixtureMailbox: &openMessageFixtureMailbox{messageFixtureMailbox: base}}
	f.session = &testSession{box}
	message := &box.objects[0]
	message.MIME = []byte(attachmentFixtureMIME)
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	fid := f.fids[Inbox]
	snapshot, err := store.observeMailMessages(t.Context(), f.session, fid)
	if err != nil || len(snapshot.messages) != 1 {
		t.Fatal("attachment table fixture unavailable", err)
	}
	openMessage := openMessageOperation(4, 1, 2, fid, snapshot.messages[0].mid, 0x0fff, 0)
	openTable := []byte{0x21, 4, 2, 3, 0x40}
	query := tableQueryWire(3, 0, 1, 1)
	faiRunOK(t, f, openMessage)

	maxOutput := uint32(10 + 4*len(f.handles) + 6)
	f.execute(openTable, maxOutput-1)
	if box.mimeReads != 0 {
		t.Fatal("output preflight admitted a MIME read")
	}
	body, counts := f.execute(openTable, maxOutput)
	opened := storeResponse(t, body)
	if counts.errors != 0 || !bytes.Equal(opened.Operations, ropError(0x21, 3, 0)) {
		t.Fatal("minimal Attachment Table response")
	}

	f.handles = opened.Handles
	table := f.state.mailbox.objects[f.handles[3]].table
	allocation := store.allocator.contents.used
	if table.updating() || len(table.rows) != 2 {
		t.Fatal("attachment view must be fixed for its MIME generation")
	}
	defaultTags := []uint32{0x0e210003, 0x0ff60102, 0x0ff90102, 0x370b0003}
	defaults := decodeTableRows(t, faiRunOK(t, f, tableQueryWire(3, 1, 1, 2)), defaultTags)
	if len(defaults) != 2 || defaults[0][0] != uint32(0) || defaults[1][0] != uint32(1) ||
		defaults[0][3] != ^uint32(0) || bytes.Equal(defaults[0][1].([]byte), defaults[1][1].([]byte)) {
		t.Fatal("required Attachment Table columns or ordinal identity")
	}
	tags := []uint32{tableMIDTag, 0x0e210003, 0x3707001f, 0x37010102, 0x0ff90102, 0x3703001f}
	faiRunOK(t, f, tableColumnsWire(3, tags...))
	f.transport.EnableSynchronizationOptionsTrace()
	f.transport.EnableUnknownPropertyTrace()
	body, _ = f.execute(query, uint32(10+4*len(f.handles)+10))
	if got := storeResponse(t, body).Operations; !bytes.Equal(got, ropError(0x15, 3, tableBufferTooSmall)) || table.cursor != 0 {
		t.Fatal("short QueryRows changed the cursor")
	}
	for number, name := range []string{"first.txt", "é.bin"} {
		reply, counts := f.run(query)
		page := decodeTableRows(t, reply, tags)
		rows, requested, missing := counts.execution.AttachmentTable()
		if rows != "1" || !slices.Equal(requested.Tags, []string{"0x674a0014", "0x0e210003", "0x3707001f", "0x37010102", "0x0ff90102", "0x3703001f"}) ||
			!slices.Equal(missing.Tags, []string{"0x37010102"}) {
			t.Fatal("attachment table diagnostic lost the row count or column shape", rows, requested.Tags, missing.Tags)
		}
		instanceID := binary.LittleEndian.Uint64(defaults[number][1].([]byte))
		if len(page) != 1 || page[0][0] != instanceID || page[0][1] != uint32(number) || page[0][2] != name ||
			page[0][3] != PropertyNotFound || !bytes.Equal(page[0][4].([]byte), defaults[number][2].([]byte)) ||
			page[0][5] != []string{".txt", ".bin"}[number] {
			t.Fatal("unexpected Attachment Table page")
		}
	}
	if box.mimeReads != 1 {
		t.Fatal("table navigation fetched full MIME")
	}

	faiRunOK(t, f, []byte{1, 4, 2}) // The Table has its own handle lifetime.
	faiRunOK(t, f, tableSeekWire(3, 0, 0))
	decodeTableRows(t, faiRunOK(t, f, query), tags)
	before := box.mimeReads
	for _, test := range []struct {
		wire []byte
		code uint32
	}{
		{[]byte{0x21, 4, 1, 3, 0}, nullObject},
		{[]byte{0x21, 5, 1, 3, 0}, accessDenied},
	} {
		got, _ := f.run(test.wire)
		if !bytes.Equal(got, ropError(0x21, 3, test.code)) || box.mimeReads != before {
			t.Fatal("wrong handle type or logon reached the MIME reader")
		}
	}
	message.Properties[core.ContentRevision] = message.Properties[core.ContentRevision].(core.Integer) + 1
	if got, _ := f.run(query); !bytes.Equal(got, ropError(0x15, 3, 0x80040109)) || table.cursor != 1 {
		t.Fatal("changed MIME generation served stale rows or moved cursor")
	}
	parent := message.Parent
	message.Parent = "another-folder"
	if got, _ := f.run(query); !bytes.Equal(got, ropError(0x15, 3, PropertyNotFound)) || table.cursor != 1 {
		t.Fatal("moved message remained visible through its original Table")
	}
	message.Parent = parent
	faiRunOK(t, f, []byte{1, 4, 3})
	if got, _ := f.run(query); !bytes.Equal(got, ropError(0x15, 3, invalidObject)) || store.allocator.contents.used >= allocation {
		t.Fatal("Release did not retire the table and its memory")
	}

	faiRunOK(t, f, openMessage)
	quota, err := store.reserveContentsMemory(maxStoreContentsMemory - 2*maxMailProjectionBytes + 1)
	if err != nil {
		t.Fatal("could not arrange quota fixture", err)
	}
	defer quota.release()
	before = box.mimeReads
	if got, _ := f.run(openTable); !bytes.Equal(got, ropError(0x21, 3, NotSupported)) || box.mimeReads != before {
		t.Fatal("retained-store quota permitted MIME loading")
	}
	quota.release()

	message.MIME = []byte("Subject: no attachment\r\n\r\nbody\r\n")
	message.Properties[core.ContentRevision] = message.Properties[core.ContentRevision].(core.Integer) + 1
	faiRunOK(t, f, openMessage)
	standard := bytes.Clone(openTable)
	standard[4] = 0
	faiRunOK(t, f, standard)
	if rows := decodeTableRows(t, faiRunOK(t, f, query), defaultTags); len(rows) != 0 {
		t.Fatal("attachment-free message did not open an empty table")
	}
}

func TestAttachmentTableFlags(t *testing.T) {
	for flags := 0; flags < 256; flags++ {
		buf := ROPBuffer{Operations: []byte{0x21, 4, 0, 1, byte(flags)}, Handles: []uint32{1, 2}}
		got, err := buf.decodeTableOperations(true, true, true, true, false, false, false, false, false, true, true, true)
		if (err == nil) != (flags == 0 || flags == 0x40) {
			t.Fatalf("unexpected flag acceptance 0x%x: %v", flags, err)
		}
		if err == nil && (len(got) != 1 || got[0].Output != 1 || got[0].Table.flags != byte(flags)) {
			t.Fatal("Attachment Table request fields decoded incorrectly")
		}
	}
}
