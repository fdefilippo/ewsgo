// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"testing"

	"ewsgo/internal/core"
)

func createAssociatedWire(input, output byte, fid uint64, associated byte) []byte {
	wire := binary.LittleEndian.AppendUint16([]byte{0x06, 4, input, output}, 1252)
	return append(binary.LittleEndian.AppendUint64(wire, fid), associated)
}

func TestIdleAssociatedDraftsAllowNormalContentsTable(t *testing.T) {
	f, _, store := newFAIExecuteFixture(t)
	f.transport.EnableContentsTableSpike()
	before := store.allocator.contents.used
	// The number of CreateMessage ROPs seen during the online Outlook lab.
	for slot := byte(4); slot < 34; slot++ {
		f.handles = append(f.handles, 0xffffffff)
		faiRunOK(t, f, createAssociatedWire(1, slot, f.fids[Inbox], 1))
	}
	out := faiRunOK(t, f, []byte{5, 4, 1, 3, 0x10})
	if len(out) != 10 || binary.LittleEndian.Uint32(out[6:]) != 1 {
		t.Fatal("idle configuration drafts made the native message unavailable")
	}
	faiRunOK(t, f, []byte{1, 4, 3})

	used := store.allocator.contents.used
	faiRunOK(t, f, faiSetOperation(4, faiSetRequest{{0x0037001f, core.Text("Synthetic view")}}))
	if store.allocator.contents.used != used {
		t.Fatal("SetProperties retained its workspace")
	}
	faiRunOK(t, f, []byte{0x0c, 4, 3, 4, 2})
	if store.allocator.contents.used != used+store.fai.bytes {
		t.Fatal("SaveChangesMessage retained workspace beyond the committed record")
	}
	for slot := byte(4); slot < 34; slot++ {
		faiRunOK(t, f, []byte{1, 4, slot})
	}
	if store.allocator.contents.used != before+store.fai.bytes {
		t.Fatal("Release retained draft memory")
	}
}

func TestCreateAssociatedNavigationLinkThroughExecute(t *testing.T) {
	f, box, store := newFAIExecuteFixture(t)
	f.transport.EnableContentsTableSpike()
	f.transport.EnableUnknownPropertyTrace()
	f.transport.EnableSynchronizationOptionsTrace()
	fid := f.fids[Inbox]
	const folderType, ordinal, group = 0x684f0102, 0x684b0102, 0x6851001f
	kind := core.Binary{0, 4, 3, 2, 0, 0, 0, 0, 0xc0, 0, 0, 0, 0, 0, 0, 0x46}
	base := store.allocator.contents.used
	create := createAssociatedWire(1, 3, fid, 1)
	stage := func(values faiSetRequest) uint64 {
		t.Helper()
		result := faiRunOK(t, f, append(bytes.Clone(create), faiSetOperation(3, values)...))
		if len(result) != 23 || result[6] != 1 || !bytes.Equal(result[15:], []byte{0x0a, 3, 0, 0, 0, 0, 0, 0}) {
			t.Fatal("CreateMessage and SetProperties did not share the new handle")
		}
		return binary.LittleEndian.Uint64(result[7:])
	}
	save := func(mid uint64) {
		t.Helper()
		result := faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 2})
		if len(result) != 15 || binary.LittleEndian.Uint64(result[7:]) != mid {
			t.Fatal("SaveChangesMessage changed the reserved MID")
		}
		faiRunOK(t, f, []byte{1, 4, 3})
	}
	props := func(order byte) faiSetRequest {
		return faiSetRequest{
			{0x001a001f, core.Text("IPM.Microsoft.WunderBar.Link")},
			{ordinal, core.Binary{order}},
			{folderType, kind},
		}
	}
	discarded := stage(props(0))
	faiRunOK(t, f, []byte{1, 4, 3})
	if len(store.fai.records) != 0 || store.allocator.contents.used != base {
		t.Fatal("Release published or leaked an unsaved message")
	}
	first := stage(props(9))
	if first == discarded || store.byMID[first] != (messageKey{}) {
		t.Fatal("CreateMessage reused or prematurely published a reservation")
	}
	// The newly created handle can read staged data before Save.
	staged := faiRunOK(t, f, getSpecificWire(3, folderType))
	want := append([]byte{0, 16, 0}, kind...)
	if !bytes.Equal(staged[6:], want) {
		t.Fatal("GetPropertiesSpecific did not see the staged link")
	}
	save(first)
	second := stage(props(2))
	save(second)
	for _, mid := range []uint64{first, second} {
		record := store.fai.records[store.byMID[mid].id]
		if record.sync == nil || record.sync.source != store.hierarchyXID(mid) || record.sync.serverNew {
			t.Fatal("Save did not publish matching source key and MID")
		}
	}
	faiRunOK(t, f, openMessageOperation(4, 1, 3, fid, first, 1252, 0))
	if actual := faiRunOK(t, f, getSpecificWire(3, folderType))[6:]; !bytes.Equal(actual, want) {
		t.Fatal("Reopening the saved link lost its folder type")
	}
	faiRunOK(t, f, []byte{1, 4, 3})

	faiRunOK(t, f, []byte{1, 4, 2})
	openTableTest(t, f, 0x12)
	tags := []uint32{0x674a0014, ordinal, group}
	faiRunOK(t, f, tableColumnsWire(2, tags...))
	requireSortSuccess(t, f, tableSortWire(2, 0, 0, 0, tableSortKey{ordinal, 0}))
	restriction := binary.LittleEndian.AppendUint32([]byte{4, 4}, folderType)
	restriction = binary.LittleEndian.AppendUint32(restriction, folderType)
	restriction = append(binary.LittleEndian.AppendUint16(restriction, uint16(len(kind))), kind...)
	row := decodeFindRow(t, faiRunOK(t, f, tableFindWire(2, 0, 0, restriction, nil)), tags)
	if row[0] != second || !bytes.Equal(row[1].([]byte), []byte{2}) || row[2] != PropertyNotFound {
		t.Fatal("FindRow lost binary matching, sort order or missing-cell behavior")
	}
	if contentsProjectionSchema(true, false)(fixedContentsProperty(folderType)) || contentsProjectionSchema(false, true)(fixedContentsProperty(0x684f1003)) {
		t.Fatal("navigation metadata leaked outside its associated property type")
	}
	// The same ID has a different multivalue type in the free/busy protocol.
	out, diagnostics := f.run(tableSortWire(2, 0, 0, 0, tableSortKey{0x684f1003, 0}))
	profiles, unknown := diagnostics.execution.TableProfiles(), diagnostics.properties.UnknownProperties()
	if !bytes.Equal(out, ropError(0x13, 2, NotSupported)) || len(profiles) != 1 || profiles[0] != "sort_table:associated:binding" ||
		len(unknown.Tags) != 1 || unknown.Tags[0] != "0x684f1003" {
		t.Fatal("sort binding rejection lost its safe diagnostic")
	}
	faiRunOK(t, f, []byte{1, 4, 2})
	config := fullContentsConfig()
	config.flags &^= 0x20
	faiRunOK(t, f, contentsConfigureOperation(config, restriction))
	var exported int
	for _, token := range decodeTestContents(t, drainFX(t, f.run, 2, 89)) {
		if token.tag == folderType && bytes.Equal(token.value.([]byte), kind) {
			exported++
		}
	}
	if exported != 2 || box.sourceCalls != 0 {
		t.Fatal("FAI export changed link data or read native mail")
	}
}

func getSpecificWire(input byte, tags ...uint32) []byte {
	wire := binary.LittleEndian.AppendUint16([]byte{0x07, 4, input, 0, 0, 1, 0}, uint16(len(tags)))
	for _, tag := range tags {
		wire = binary.LittleEndian.AppendUint32(wire, tag)
	}
	return wire
}

func TestCreateNormalMessageErrorPreservesFollowingROP(t *testing.T) {
	f, _, store := newFAIExecuteFixture(t)
	f.transport.EnableSynchronizationOptionsTrace()
	wire := append(createAssociatedWire(1, 3, f.fids[Inbox], 0), getSpecificWire(1, 0x67480014)...)
	result, counts := f.run(wire)
	if counts.errors != 1 || !bytes.HasPrefix(result, append(ropError(0x06, 3, NotSupported), ropError(0x07, 1, 0)...)) ||
		f.handles[3] != 0xffffffff || len(store.fai.records) != 0 {
		t.Fatal("unsupported Normal creation interrupted a valid ROP or changed state")
	}
	if modes := counts.execution.CreateMessageModes(); len(modes) != 1 || modes[0] != "normal" {
		t.Fatal("technical diagnostic lost the Normal creation mode")
	}
	if folders := counts.execution.CreateMessageFolders(); len(folders) != 1 || folders[0] != "native" {
		t.Fatal("technical diagnostic lost the native target")
	}
	_, counts = f.run(createAssociatedWire(0, 3, f.fids[Outbox], 0))
	if folders := counts.execution.CreateMessageFolders(); len(folders) != 1 || folders[0] != "outbox" {
		t.Fatal("technical diagnostic lost the Outbox target")
	}
}

func TestAssociatedBinaryPropertyRespectsExecuteBudget(t *testing.T) {
	f, _, _ := newFAIExecuteFixture(t)
	faiRunOK(t, f, createAssociatedWire(1, 3, f.fids[Inbox], 1))
	const roamingXML, importance = 0x7c080102, 0x00170003
	content := core.Binary(bytes.Repeat([]byte("synthetic"), 900))
	faiRunOK(t, f, faiSetOperation(3, faiSetRequest{{roamingXML, content}}))
	request := getSpecificWire(3, roamingXML, importance)
	got := faiRunOK(t, f, request)
	expected := binary.LittleEndian.AppendUint16([]byte{0}, uint16(len(content)))
	expected = append32(append(expected, content...), 1)
	if !bytes.Equal(got[6:], expected) {
		t.Fatal("room available in Execute was not used for the entire binary")
	}
	body, counts := f.execute(request, 256)
	got = storeResponse(t, body).Operations
	expected = append32([]byte{1, 0x0a}, 0x8007000e)
	expected = append32(append(expected, 0), 1)
	if counts.errors != 0 || !bytes.Equal(got[6:], expected) {
		t.Fatal("an oversized property prevented another cell from being returned")
	}
}

func TestReopenedConfigurationReturnsRoamingPropertiesInSameBatch(t *testing.T) {
	f, _, _ := newFAIExecuteFixture(t)
	const roamingDatatypes, roamingDictionary = 0x7c060003, 0x7c070102
	fid := f.fids[Inbox]
	created := faiRunOK(t, f, createAssociatedWire(1, 3, fid, 1))
	mid := binary.LittleEndian.Uint64(created[7:])
	dictionary := core.Binary(`<UserConfiguration><Info version="Outlook.12"/><Data/></UserConfiguration>`)
	faiRunOK(t, f, faiSetOperation(3, faiSetRequest{
		{0x001a001f, core.Text("IPM.Configuration.Synthetic")},
		{roamingDatatypes, core.Integer(2)},
		{roamingDictionary, dictionary},
	}))
	faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 2})
	faiRunOK(t, f, []byte{1, 4, 3})

	open := openMessageOperation(4, 1, 3, fid, mid, 1252, 3)
	reply := faiRunOK(t, f, append(open, getSpecificWire(3, roamingDatatypes, roamingDictionary)...))
	want := append(ropError(0x07, 3, 0), 0) // PropertyRow with no errors.
	want = append32(want, 2)
	want = binary.LittleEndian.AppendUint16(want, uint16(len(dictionary)))
	want = append(want, dictionary...)
	if !bytes.HasSuffix(reply, want) {
		t.Fatal("the reopened configuration did not return the saved stream type and dictionary")
	}
}
