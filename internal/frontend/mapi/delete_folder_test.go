// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

func TestEmptyLocalFolderExecute(t *testing.T) {
	f := newCreateFixture(t)
	f.transport.EnableFAICollectorSpike()
	parent := binary.LittleEndian.Uint64(faiRunOK(t, f, createOperation(4, 1, 2, "empty parent", "", false))[6:])
	child := binary.LittleEndian.Uint64(faiRunOK(t, f, createOperation(4, 2, 3, "empty child", "", false))[6:])
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	rootFAI := createDeletionTestFAI(t, store, f.session, parent, 1)
	childFAI := createDeletionTestFAI(t, store, f.session, child, 2)
	query := folderPropertiesOperation(4, 2, 0, 0x3001001f)
	for _, tc := range []struct {
		rop, async, associated byte
		code                   uint32
	}{
		{ropHardDeleteFolderContents, 2, 1, NotSupported},
		{ropEmptyFolder, 0, 3, NotSupported},
		{ropEmptyFolder, 0, 0, 0},
	} {
		request := append([]byte{tc.rop, 4, 2, tc.async, tc.associated}, query...)
		out, _ := f.run(request)
		want := append(append(ropError(tc.rop, 2, tc.code), 0), ropError(7, 2, 0)...)
		if !bytes.HasPrefix(out, want) {
			t.Fatalf("empty followed by GetProperties: %x", out)
		}
		_, err := store.folder(t.Context(), f.session, child)
		if tc.code != 0 && err != nil || tc.code == 0 && !errors.Is(err, core.ErrNotFound) ||
			store.fai.records[childFAI.object.ID].deleted || store.fai.records[rootFAI.object.ID].deleted {
			t.Fatal("soft deletion lost data or changed a rejected request", err)
		}
	}
	if !store.dynamic[store.byFID[child].dynamic].softDeleted {
		t.Fatal("soft deletion lost the recoverable folder definition")
	}
	for _, associated := range []byte{0, 1, 1} {
		out, _ := f.run([]byte{ropHardDeleteFolderContents, 4, 2, 0, associated})
		if !bytes.Equal(out, append(ropError(ropHardDeleteFolderContents, 2, 0), 0)) {
			t.Fatalf("hard empty response: %x", out)
		}
		if _, ok := store.dynamic[store.byFID[child].dynamic]; ok || !store.fai.records[childFAI.object.ID].deleted ||
			store.fai.records[rootFAI.object.ID].deleted != (associated != 0) {
			t.Fatal("hard empty did not apply the requested FAI scope")
		}
	}
	if _, err := store.folder(t.Context(), f.session, parent); err != nil {
		t.Fatal("hard empty removed the selected folder", err)
	}
	if record := store.fai.records[rootFAI.object.ID]; len(record.object.Properties) != 0 || store.messages[store.byMID[rootFAI.mid]].present {
		t.Fatal("hard empty retained the payload or left the MID active")
	}
}

func TestEmptyLocalFolderSQLiteAtomicReopen(t *testing.T) {
	f := newHierarchyFixture(t)
	reopen := sqliteReplicaFixture(t, f)
	db := reopen()
	ctx := t.Context()
	parent, code := f.store.createFolder(ctx, f.session, f.binding.fids[IPMSubtree], createFolderRequest{kind: 1, name: "empty durable parent"})
	if code != 0 {
		t.Fatalf("create parent: %#x", code)
	}
	child, code := f.store.createFolder(ctx, f.session, parent, createFolderRequest{kind: 1, name: "empty durable child"})
	if code != 0 {
		t.Fatalf("create child: %#x", code)
	}
	row := createDeletionTestFAI(t, f.store, f.session, parent, 1)
	version := f.store.replicaPosition()
	if err := db.CommitMAPIReplica(ctx, f.store.durable.mailbox, version, mapireplica.Mutation{HighWater: version.HighWater + 1}); err != nil {
		t.Fatal(err)
	}
	if err := f.store.emptyLocalFolder(ctx, f.session, parent, true, true); !errors.Is(err, core.ErrConflict) {
		t.Fatal("stale process committed a hard empty", err)
	}
	if _, err := f.store.folder(ctx, f.session, child); err != nil || f.store.fai.records[row.object.ID].deleted {
		t.Fatal("failed commit changed the folder or FAI", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db = reopen()
	if err := f.store.emptyLocalFolder(ctx, f.session, parent, true, true); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopen()
	if _, err := f.store.folder(ctx, f.session, parent); err != nil {
		t.Fatal("selected folder lost after reopen", err)
	}
	if _, err := f.store.folder(ctx, f.session, child); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("child survived after reopen", err)
	}
	record := f.store.fai.records[row.object.ID]
	if !record.deleted || record.folder != parent || len(record.object.Properties) != 0 || f.store.messages[f.store.byMID[row.mid]].present {
		t.Fatal("FAI removal did not survive reopen")
	}
}

func createDeletionTestFAI(t *testing.T, store *ramStore, session core.Session, fid uint64, source byte) messageRow {
	t.Helper()
	request := syntheticFAIImport()
	request.source = faiTestXID(1, source)
	draft, err := store.newFAIDraft(t.Context(), session, fid, request)
	if err != nil {
		t.Fatal(err)
	}
	defer draft.close()
	if _, err = draft.set(t.Context(), session, faiCreateProperties()); err != nil {
		t.Fatal(err)
	}
	row, err := draft.save(t.Context(), session, 2)
	if err != nil {
		t.Fatal(err)
	}
	return row
}

func TestDeleteRAMFolderSubtreeWithAssociatedContents(t *testing.T) {
	f := newCreateFixture(t)
	f.transport.EnableFAICollectorSpike()
	f.transport.EnableFolderMetadataSpike()
	binding := f.state.mailbox.objects[f.handles[0]].binding
	store := binding.store
	parent := binary.LittleEndian.Uint64(faiRunOK(t, f, createOperation(4, 1, 2, "subtree", "", false))[6:])
	child := binary.LittleEndian.Uint64(faiRunOK(t, f, createOperation(4, 2, 3, "child", "", false))[6:])

	ctx := t.Context()
	draft, err := store.newFAIDraft(ctx, f.session, child, syntheticFAIImport())
	if err != nil {
		t.Fatal("associated fixture unavailable", err)
	}
	defer draft.close()
	if _, err := draft.set(ctx, f.session, faiCreateProperties()); err != nil {
		t.Fatal("associated fixture properties", err)
	}
	row, err := draft.save(ctx, f.session, 2)
	if err != nil {
		t.Fatal("associated fixture save", err)
	}
	draft.close()
	before := store.fai.bytes
	remove := func(slot, flags byte) []byte {
		return binary.LittleEndian.AppendUint64([]byte{ropDeleteFolder, 4, slot, flags}, parent)
	}
	for _, tc := range []struct {
		slot, flags, partial byte
		code                 uint32
	}{
		{1, 0x80, 0, 0x80070057},
		{2, deleteFolderMessages | deleteFolderSubfolders, 0, PropertyNotFound},
		{1, 0, 1, 0},
		{1, deleteFolderMessages, 1, 0},
		{1, deleteFolderSubfolders, 1, 0},
	} {
		reply, _ := f.run(remove(tc.slot, tc.flags))
		if !bytes.Equal(reply, append(ropError(ropDeleteFolder, tc.slot, tc.code), tc.partial)) {
			t.Fatalf("unexpected DeleteFolder response for flags 0x%x: %x", tc.flags, reply)
		}
	}
	for _, fid := range []uint64{parent, child} {
		if _, err := store.folder(ctx, f.session, fid); err != nil {
			t.Fatal("partial operation removed a folder", err)
		}
	}
	root := binding.fids[IPMSubtree]
	previous, err := store.observeHierarchy(ctx, f.session, root, hierarchyTestTime)
	if err != nil {
		t.Fatal("initial hierarchy unavailable", err)
	}
	initial := hierarchyState{store: store, root: root}
	full, err := initial.planHierarchy(ctx, f.session, previous)
	if err != nil {
		t.Fatal("initial hierarchy could not be planned", err)
	}
	flags := byte(deleteFolderMessages | deleteFolderSubfolders)
	faiRunOK(t, f, remove(1, flags))
	for _, fid := range []uint64{parent, child} {
		if _, err := store.folder(ctx, f.session, fid); !errors.Is(err, core.ErrNotFound) {
			t.Fatal("soft-deleted subtree remained active", err)
		}
		if !store.dynamic[store.byFID[fid].dynamic].softDeleted {
			t.Fatal("soft-deleted folder identity was not retained")
		}
	}
	if store.fai.bytes != before || store.fai.records[row.object.ID].object.ID != row.object.ID {
		t.Fatal("soft delete discarded the recoverable payload")
	}
	current, err := store.observeHierarchy(ctx, f.session, root, hierarchyTestTime.Add(time.Minute))
	if err != nil {
		t.Fatal("hierarchy unavailable after delete", err)
	}
	state := hierarchyState{store: store, root: root, given: full.finalGiven, seen: full.finalSeen}
	delta, err := state.planHierarchy(ctx, f.session, current)
	if err != nil || len(delta.removed) != 2 || !slices.Contains(delta.removed, parent) || !slices.Contains(delta.removed, child) {
		t.Fatal("hierarchy did not report both deleted folders", err)
	}
	faiRunOK(t, f, remove(1, flags|deleteFolderHard))
	if len(store.dynamic) != 0 || len(store.fai.records) != 0 || store.fai.bytes != 0 || store.fai.lease.size() != 0 {
		t.Fatal("hard delete did not purge folders and associated contents")
	}
	if store.byMID[row.mid].id != row.object.ID || store.messages[store.byMID[row.mid]].present {
		t.Fatal("hard deletion reused the associated identity or left it active")
	}
}

func TestHiddenMetadataAcrossFolderLifecycle(t *testing.T) {
	f := newCreateFixture(t)
	f.transport.EnableFolderMetadataSpike()
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	set := []byte{0x0a, 4, 2, 7, 0, 1, 0, 0x0b, 0, 0xf4, 0x10, 1}
	read := folderPropertiesOperation(4, 2, 0, folderHiddenTag)
	want := append(append(ropError(0x0a, 2, 0), 0, 0), append(ropError(7, 2, 0), 0, 1)...)
	var first uint64
	for i := range 12 {
		created, _ := f.run(createOperation(4, 1, 2, fmt.Sprintf("synthetic-hidden-%d", i), "", false))
		if len(created) != 15 || binary.LittleEndian.Uint32(created[2:]) != 0 {
			t.Fatalf("create folder %d failed: %x", i, created)
		}
		if i == 0 {
			first = binary.LittleEndian.Uint64(created[6:])
		}
		if wire, _ := f.run(append(bytes.Clone(set), read...)); !bytes.Equal(wire, want) {
			t.Fatalf("folder %d did not preserve Hidden: %x", i, wire)
		}
		f.run([]byte{1, 4, 2})
	}
	if len(store.metadata) != 12 {
		t.Fatalf("metadata stored for %d folders", len(store.metadata))
	}
	used := store.metadataBytes
	charge := store.metadata[first].accountedBytes()
	for _, flag := range []byte{0, deleteFolderHard} {
		remove := binary.LittleEndian.AppendUint64([]byte{0x1d, 4, 1, flag}, first)
		if wire, _ := f.run(remove); !bytes.Equal(wire, append(ropError(0x1d, 1, 0), 0)) {
			t.Fatalf("delete flag 0x%02x: %x", flag, wire)
		}
		if flag == deleteFolderHard {
			used -= charge
		}
		if store.metadataBytes != used {
			t.Fatalf("delete flag 0x%02x left incorrect metadata accounting", flag)
		}
	}
	if len(store.metadata) != 11 {
		t.Fatalf("hard delete retained the metadata bag")
	}
}

func TestHiddenFolderLifecycle(t *testing.T) {
	f := newCreateFixture(t)
	f.transport.EnableFolderMetadataSpike()
	ctx := context.Background()
	binding := f.state.mailbox.objects[f.handles[0]].binding
	create := createOperation(4, 1, 2, "synthetic-hidden", "", false)
	created, _ := f.run(create)
	fid := binary.LittleEndian.Uint64(created[6:])
	parent := binding.fids[IPMSubtree]
	read := folderPropertiesOperation(4, 2, 0, folderHiddenTag)
	set := []byte{0x0a, 4, 2, 7, 0, 1, 0, 0x0b, 0, 0xf4, 0x10, 9}
	wire, _ := f.run(append(bytes.Clone(set), read...))
	if !bytes.Equal(wire, []byte{0xa, 2, 0, 0, 0, 0, 0, 0, 7, 2, 0, 0, 0, 0, 0, 1}) {
		t.Fatal("nonzero boolean did not round-trip as true")
	}
	table, err := captureHierarchyTable(ctx, f.session, binding, parent, 0, false)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, row := range table.rows {
		for tag, value := range row {
			if tag.id == folderHiddenTag>>16 && value == core.Bool(true) {
				found = true
			}
		}
	}
	table.close()
	if !found {
		t.Fatal("hidden folder or its flag missing from active table")
	}
	snapshot, err := binding.store.observeHierarchy(ctx, f.session, parent, hierarchyTestTime)
	if err != nil {
		t.Fatal(err)
	}
	entry := hierarchyRow(t, snapshot, fid)
	if entry.hidden == nil || !*entry.hidden {
		t.Fatal("hidden state missing from hierarchy snapshot")
	}
	for _, exclude := range []bool{false, true} {
		config := synchronizationConfigure{}
		if exclude {
			config.tags = []uint32{folderHiddenTag}
		}
		transfer, err := hierarchyFastTransfer(snapshot.replica, binding.mailboxGUID, config, hierarchyDelta{changes: []hierarchyFolder{entry}})
		if err != nil {
			t.Fatal(err)
		}
		found := false
		for _, token := range readTestFX(t, transfer.data) {
			if token.tag == folderHiddenTag {
				found = bytes.Equal(token.value, []byte{1, 0})
				if exclude || !found {
					t.Fatalf("unexpected Hidden FastTransfer property %x", token.value)
				}
			}
		}
		if !exclude && !found {
			t.Fatal("Hidden missing from FastTransfer")
		}
	}
	set[len(set)-1] = 0
	wire, _ = f.run(append(bytes.Clone(set), read...))
	if wire[len(wire)-1] != 0 {
		t.Fatal("explicit false lost")
	}

	remove := func(slot, flags byte, target uint64) []byte {
		return binary.LittleEndian.AppendUint64([]byte{0x1d, 4, slot, flags}, target)
	}
	wire, _ = f.run(remove(3, 0, fid)) // Unbound parent handle.
	if !bytes.Equal(wire, append(ropError(0x1d, 3, nullObject), 0)) {
		t.Fatal("invalid parent accepted")
	}
	f.run(createOperation(4, 2, 3, "synthetic-child", "", false))
	wire, _ = f.run(remove(1, 0, fid))
	if !bytes.Equal(wire, append(ropError(0x1d, 1, 0), 1)) {
		t.Fatal("DeleteFolder did not report partial completion")
	}
	child := f.state.mailbox.objects[f.handles[3]].fid
	wire, _ = f.run(remove(2, deleteFolderHard, child))
	if !bytes.Equal(wire, append(ropError(0x1d, 2, 0), 0)) {
		t.Fatal("hard delete of empty child failed")
	}

	wire, _ = f.run(append(remove(1, 0, fid), read...))
	if !bytes.Equal(wire, append(append(ropError(0x1d, 1, 0), 0), ropError(7, 2, PropertyNotFound)...)) {
		t.Fatal("soft delete or stale handle outcome incorrect")
	}
	if stored := binding.store.dynamic[binding.store.byFID[fid].dynamic]; !stored.softDeleted {
		t.Fatal("soft delete discarded the stored folder")
	}
	newFolder, _ := f.run(create)
	if len(newFolder) != 15 || binary.LittleEndian.Uint64(newFolder[6:]) == fid {
		t.Fatal("replayed create did not allocate a distinct FID")
	}
	wire, _ = f.run(remove(1, deleteFolderHard, fid))
	if !bytes.Equal(wire, append(ropError(0x1d, 1, 0), 0)) || binding.store.metadata[fid] != nil {
		t.Fatal("hard delete did not purge the old folder metadata")
	}
}
