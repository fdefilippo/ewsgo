// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"ewsgo/internal/core"
)

func deletePropertiesOperation(id, slot byte, tags ...uint32) []byte {
	wire := binary.LittleEndian.AppendUint16([]byte{id, 4, slot}, uint16(len(tags)))
	for _, tag := range tags {
		wire = append32(wire, tag)
	}
	return wire
}

func TestDeleteFolderMetadataBatch(t *testing.T) {
	f := newOSTFixture(t)
	f.transport.EnableFolderMetadataSpike()
	set := metadataOperation(4, 1)
	remove := deletePropertiesOperation(ropDeletePropertiesNoReplicate, 1, 0x36da0102, 0x3001001f)
	get := folderPropertiesOperation(4, 1, 1, 0x36da0102)
	body, _ := f.execute(append(bytes.Clone(set), remove[:len(remove)-1]...), 65535)
	if binary.LittleEndian.Uint32(body[4:]) != 0x80070057 {
		t.Fatal("truncated delete did not reject the batch")
	}
	missing := append32(append(ropError(7, 1, 0), 1, 0x0a), PropertyNotFound)
	if out, _ := f.run(get); !bytes.Equal(out, missing) {
		t.Fatal("the prefix of a malformed batch mutated the folder")
	}

	batch := append(append(bytes.Clone(set), remove...), get...)
	out, counts := f.run(batch)
	want := append(ropError(0x0a, 1, 0), 0, 0)
	// The property problem preserves its original index and tag.
	want = append(want, 0x7a, 1, 0, 0, 0, 0, 1, 0, 1, 0)
	want = append32(append32(want, 0x3001001f), NotSupported)
	want = append(want, missing...)
	if !bytes.Equal(out, want) || counts.errors != 0 {
		t.Fatalf("delete batch got %x, want %x", out, want)
	}

	obj := f.state.mailbox.objects[f.handles[1]]
	store := obj.binding.store
	used := store.metadataBytes
	faiRunOK(t, f, deletePropertiesOperation(ropDeleteProperties, 1, 0x36da0102))
	if store.metadataBytes != used || len(store.metadata[obj.fid]) != 2 {
		t.Fatal("deleting an absent property changed the remaining metadata")
	}
}

func TestOpenDeleteSaveAssociatedProperties(t *testing.T) {
	f, _, store := newFAIExecuteFixture(t)
	ctx := context.Background()
	root := f.fids[Inbox]
	faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
	faiRunOK(t, f, faiSetOperation(3, append(faiCreateProperties(),
		faiWireProperty{0x0037001f, core.Text("Subject to remove")})))
	saved := faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 2})
	mid := binary.LittleEndian.Uint64(saved[7:])
	key := store.byMID[mid]
	before := store.fai.records[key.id]

	open := openMessageOperation(4, 1, 3, root, mid, 1252, 0)
	remove := deletePropertiesOperation(ropDeletePropertiesNoReplicate, 3, 0x70010102)
	address := addressTypesResponse(0)
	out, counts := f.run(append(append(open, remove...), 0x49, 4, 0))
	if !bytes.HasSuffix(out, append(ropError(0x7a, 3, accessDenied), address...)) || counts.errors != 1 {
		t.Fatalf("read-only delete stopped independent ROPs or returned the wrong access status: %x", out)
	}

	faiRunOK(t, f, []byte{1, 4, 3})
	used := store.allocator.contents.used
	open = openMessageOperation(4, 1, 3, root, mid, 1252, 1)
	remove = deletePropertiesOperation(ropDeletePropertiesNoReplicate, 3, 0x70010102, 0x00370000, 0x001a001f)
	out, counts = f.run(append(open, remove...))
	want := append(ropError(0x7a, 3, 0), 1, 0, 2, 0)
	want = append32(append32(want, 0x001a001f), accessDenied)
	if counts.errors != 0 || !bytes.HasSuffix(out, want) {
		t.Fatalf("writable Open→Delete did not report the expected property problem: %x", out)
	}
	out, _ = f.run(folderPropertiesOperation(4, 3, 1, 0x0037001f, 0x0ff70003))
	want = append32(append(ropError(7, 3, 0), 1, 0x0a), PropertyNotFound)
	want = append32(append(want, 0), 1)
	if !bytes.Equal(out, want) {
		t.Fatalf("staged deletion and read/write AccessLevel are not visible: %x", out)
	}
	row, err := store.readOpenedMessage(ctx, f.session, root, openedMessage{mid: mid})
	if err != nil || row.object.Properties[core.Subject] != core.Text("Subject to remove") {
		t.Fatal("deleting staged properties changed the committed row before Save")
	}

	out, _ = f.run([]byte{0x0c, 4, 4, 3, 1})
	if len(out) != 15 || binary.LittleEndian.Uint32(out[2:]) != 0 || binary.LittleEndian.Uint64(out[7:]) != mid {
		t.Fatalf("unexpected explicit Save result: %x", out)
	}
	after := store.fai.records[key.id]
	if after.object.Revision != before.object.Revision+1 || after.sync.change == before.sync.change ||
		after.object.Properties[core.Subject] != nil || after.object.Properties[core.Key{ID: 0x7001}] != nil ||
		after.object.Properties[core.Key{ID: 0x001a}] == nil {
		t.Fatal("Save did not commit only the successful removals and advance lineage")
	}
	out, _ = f.run(remove)
	if !bytes.Equal(out, ropError(0x7a, 3, accessDenied)) {
		t.Fatal("Save with KeepOpenReadOnly left write access")
	}
	faiRunOK(t, f, []byte{1, 4, 3})
	if store.allocator.contents.used != used+after.bytes-before.bytes {
		t.Fatal("Release leaked the explicit edit lease")
	}
}
