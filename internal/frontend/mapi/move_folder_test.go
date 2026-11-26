// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"
	"unicode/utf16"

	"ewsgo/internal/core"
)

func moveFolderWire(source, destination, async, unicode byte, fid uint64, name string) []byte {
	out := binary.LittleEndian.AppendUint64([]byte{0x35, 4, source, destination, async, unicode}, fid)
	if unicode == 0 {
		return append(append(out, name...), 0)
	}
	for _, unit := range utf16.Encode([]rune(name)) {
		out = binary.LittleEndian.AppendUint16(out, unit)
	}
	return append(out, 0, 0)
}

func checkMoveFolderResponse(t *testing.T, f *ostFixture, wire []byte, code uint32) {
	t.Helper()
	out, result := f.run(wire)
	want := ropError(0x35, wire[2], code)
	if code == 0x503 {
		want = binary.LittleEndian.AppendUint32(want, uint32(wire[3]))
	}
	want = append(want, 0)
	if !bytes.Equal(out, want) || (result.errors != 0) != (code != 0) {
		t.Fatalf("MoveFolder response %x, want %x, errors %d", out, want, result.errors)
	}
}

func TestMoveFolderExecute(t *testing.T) {
	ctx := context.Background()
	f := newCreateFixture(t)
	for _, id := range []core.ID{"source", "target"} {
		f.box.page.Objects = append(f.box.page.Objects, core.Object{ID: id, Kind: core.Folder, Parent: f.box.Root(),
			Revision: 1, Properties: core.Properties{core.DisplayName: core.Text(id), core.MetadataRevision: core.Integer(1)}})
	}
	box := &placementTestMailbox{stateMailbox: f.box}
	f.session = &testSession{box}
	store := f.state.mailbox.objects[f.handles[1]].binding.store
	root := f.fids[IPMSubtree]
	first, err := store.observeHierarchy(ctx, f.session, root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	fid, target := store.byKey[folderKey{native: "source"}], store.byKey[folderKey{native: "target"}]
	row := hierarchyRow(t, first, fid)
	wire := moveFolderWire(1, 2, 0, 1, fid, "moved €")
	checkMoveFolderResponse(t, f, wire, 0x503)
	faiRunOK(t, f, folderOperation(target, 4, 0, 2))
	checkMoveFolderResponse(t, f, moveFolderWire(2, 1, 0, 1, fid, "wrong parent"), 0x8004010f)
	checkMoveFolderResponse(t, f, moveFolderWire(1, 2, 0, 1, f.fids[Inbox], "protected"), accessDenied)
	checkMoveFolderResponse(t, f, moveFolderWire(1, 2, 0, 1, fid, "bad/name"), 0x80070057)
	checkMoveFolderResponse(t, f, moveFolderWire(1, 2, 1, 1, fid, "async"), NotSupported)
	if _, result := f.execute(wire, 10+4*uint32(len(f.handles))+10); result.errors != 0 || box.updates != 0 {
		t.Fatal("rejected batch wrote to the mailbox")
	}
	checkMoveFolderResponse(t, f, wire, 0)
	checkMoveFolderResponse(t, f, moveFolderWire(2, 2, 0, 1, fid, "moved €"), 0)
	checkMoveFolderResponse(t, f, moveFolderWire(2, 2, 0, 0, fid, "ansi \x80"), 0)
	if box.updates != 2 {
		t.Fatalf("native writes %d, want 2", box.updates)
	}
	after, err := store.observeHierarchy(ctx, f.session, root, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	moved := hierarchyRow(t, after, fid)
	if moved.parent != target || moved.name != "ansi €" || moved.cn <= row.cn || moved.sourceKey != row.sourceKey {
		t.Fatal("hierarchy changed identity or retained stale placement")
	}
	for i := 1; i < len(wire); i++ {
		buf := ROPBuffer{Operations: wire[:i], Handles: f.handles}
		if _, err := buf.DecodeOperations(); err == nil {
			t.Fatalf("truncation accepted at byte %d", i)
		}
	}
}

func TestCopyFolderBatch(t *testing.T) {
	f := newCreateFixture(t)
	openInbox := folderOperation(f.fids[Inbox], 4, 0, 2)
	for _, unicode := range []byte{0, 1} {
		wire := binary.LittleEndian.AppendUint64([]byte{ropCopyFolder, 4, 1, 2, 0, 1, unicode}, f.fids[Inbox])
		if unicode == 0 {
			wire = append(wire, 'x', 0)
		} else {
			wire = append(wire, 'x', 0, 0, 0)
		}
		for size := 1; size < len(wire); size++ {
			if _, err := (ROPBuffer{Operations: wire[:size], Handles: f.handles}).DecodeOperations(); err == nil {
				t.Fatalf("truncated CopyFolder accepted at byte %d", size)
			}
		}
		f.handles[2] = 0xffffffff
		out, result := f.run(append(bytes.Clone(wire), openInbox...))
		want := binary.LittleEndian.AppendUint32(ropError(ropCopyFolder, 1, moveCopyNullDestination), 2)
		want = append(want, 0, 2, 2, 0, 0, 0, 0, 0, 0)
		if !bytes.Equal(out, want) || result.errors != 1 {
			t.Fatalf("CopyFolder without destination followed by OpenFolder = %x, want %x", out, want)
		}
		out, result = f.run(append(bytes.Clone(wire), openInbox...))
		want = append(ropError(ropCopyFolder, 1, NotSupported), 0, 2, 2, 0, 0, 0, 0, 0, 0)
		if !bytes.Equal(out, want) || result.errors != 1 {
			t.Fatalf("CopyFolder followed by OpenFolder = %x, want %x", out, want)
		}
	}
}
