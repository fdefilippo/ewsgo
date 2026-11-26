// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"reflect"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/core"
)

// Opaque synthetic values, not captured EntryIDs. Include empty and extension
// indexes, and bytes resembling tags/ROPs to pin preservation without scanning.
func syntheticAdditionalEntryIDs() []byte {
	b := append32(nil, 6)
	for _, item := range [][]byte{{0xa, 0x1c, 0xff}, nil, {1}, {2, 3}, nil, []byte("PRIVATE-extension")} {
		b = binary.LittleEndian.AppendUint16(b, uint16(len(item)))
		b = append(b, item...)
	}
	return b
}

func additionalEntryIDsOperation(logon, input byte, value []byte) []byte {
	b := append32([]byte{0xa, logon, input, 0, 0, 1, 0}, 0x36d81102)
	b = append(b, value...)
	binary.LittleEndian.PutUint16(b[3:], uint16(len(b)-5))
	return b
}

func TestAdditionalEntryIDsDecoderAndDiagnostic(t *testing.T) {
	raw := additionalEntryIDsOperation(4, 1, syntheticAdditionalEntryIDs())
	decode := func(b []byte, enabled bool) ([]Operation, error) {
		return (ROPBuffer{Operations: b, Handles: []uint32{1, 2}}).decodeFolderOperations(false, true, true, enabled)
	}
	if _, err := decode(raw, false); err == nil {
		t.Fatal("default enabled")
	}
	ops, err := decode(raw, true)
	if err != nil || len(ops) != 1 || !bytes.Equal(ops[0].Metadata[0x36d8], syntheticAdditionalEntryIDs()) {
		t.Fatal("closed array fixture")
	}
	for i := 1; i < len(raw); i++ {
		if _, err := decode(raw[:i], true); err == nil {
			t.Fatal("truncation accepted")
		}
	}
	for _, bad := range [][]byte{append32(nil, 33), append32(nil, 0xffffffff), {1, 0, 0, 0, 0xff, 0xff}} {
		if _, err := decode(additionalEntryIDsOperation(4, 1, bad), true); err == nil {
			t.Fatal("count or length unbounded")
		}
	}
	wrong := bytes.Clone(raw)
	wrong[7] = 2
	wrong[8] = 1
	if _, err := decode(wrong, true); err == nil {
		t.Fatal("wrong type")
	}
	maximum := append32(nil, 32)
	maximum = append(maximum, make([]byte, 64)...)
	if _, err := decode(additionalEntryIDsOperation(4, 1, maximum), true); err != nil {
		t.Fatal("32 empty slots rejected")
	}
	// The whole closed list can contain all four IDs, in arbitrary order.
	combined := metadataOperation(4, 1)
	combined = append32(combined, 0x36d81102)
	combined = append(combined, syntheticAdditionalEntryIDs()...)
	combined[5] = 4
	binary.LittleEndian.PutUint16(combined[3:], uint16(len(combined)-5))
	if ops, err := decode(combined, true); err != nil || len(ops[0].Metadata) != 4 {
		t.Fatal("four-property closed list")
	}
	mixed := bytes.Clone(raw)
	mixed = append32(mixed, 0x12340003)
	mixed = append32(mixed, 42)
	mixed[5] = 2
	binary.LittleEndian.PutUint16(mixed[3:], uint16(len(mixed)-5))
	if _, err := decode(mixed, true); err == nil {
		t.Fatal("unknown mixed list")
	}
	d := InspectSetList(setPropertyWire(t, mixed))
	state, tags, n, _, _ := d.Fields()
	if state != "list_only" || n != "2" || !reflect.DeepEqual(tags, []string{"0x36d81102", "0x12340003"}) {
		t.Fatal("bounded array traversal")
	}
	if strings.Contains(fmt.Sprintf("%v %#v %v %#v", ops[0].Metadata, ops[0].Metadata, d, d), "PRIVATE") {
		t.Fatal("format exposed value")
	}
	for i := range raw {
		raw[i] = 0
	}
	if !bytes.Equal(ops[0].Metadata[0x36d8], syntheticAdditionalEntryIDs()) {
		t.Fatal("input alias")
	}
	// Other multivalued properties still stop at the tag.
	other := additionalEntryIDsOperation(4, 1, syntheticAdditionalEntryIDs())
	other[9] = 0xd9
	if state, _, _, _, _ := InspectSetList(setPropertyWire(t, other)).Fields(); state != "stopped_type" {
		t.Fatal("arbitrary multivalue traversal")
	}
	if state, _, _, _, _ := InspectSetList(setPropertyWire(t, additionalEntryIDsOperation(4, 1, append32(nil, 33)))).Fields(); state != "limit" {
		t.Fatal("diagnostic count limit")
	}
}

func TestAdditionalEntryIDsReadbackReconnectAndEmpty(t *testing.T) {
	f := newOSTFixture(t)
	f.transport.EnableFolderMetadataSpike()
	value := syntheticAdditionalEntryIDs()
	read := folderPropertiesOperation(4, 1, 0, 0x36d81102)
	// A successfully decoded prefix releases an unrelated folder only.
	f.handles = append(f.handles, 0xffffffff)
	f.run(folderOperation(f.fids[SentItems], 4, 0, 2))
	rop, cnt := f.run(append(append([]byte{1, 4, 2}, additionalEntryIDsOperation(4, 1, value)...), read...))
	want := append([]byte{0xa, 1, 0, 0, 0, 0, 0, 0, 7, 1, 0, 0, 0, 0, 0}, value...)
	if !bytes.Equal(rop, want) || cnt.errors != 0 || cnt.properties.Values != 1 {
		t.Fatal("prefix and exact typed readback")
	}
	if _, code := f.state.mailbox.object(f.handles[2], 4); code == 0 {
		t.Fatal("prefix not executed")
	}
	unspecified, _ := f.run(folderPropertiesOperation(4, 1, 0, 0x36d80000))
	if !bytes.Equal(unspecified, append([]byte{7, 1, 0, 0, 0, 0, 1, 2, 0x11, 0}, value...)) {
		t.Fatal("unspecified type")
	}
	f.state.mailbox = newMailboxObjects(f.transport.stores)
	f.handles = []uint32{0xffffffff, 0xffffffff}
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	f.open(int(Inbox))
	got, _ := f.run(read)
	if !bytes.Equal(got, want[8:]) {
		t.Fatal("reconnect")
	}
	// Zero entries is present, not NotFound; replacement does not keep old tail.
	f.run(additionalEntryIDsOperation(4, 1, append32(nil, 0)))
	got, _ = f.run(read)
	if !bytes.Equal(got, []byte{7, 1, 0, 0, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatal("empty array")
	}
	f.open(int(SentItems))
	got, _ = f.run(read)
	if !bytes.Equal(got, []byte{7, 1, 0, 0, 0, 0, 1, 0xa, 0x0f, 1, 4, 0x80}) {
		t.Fatal("absent array")
	}
}

func TestAdditionalEntryIDsProjectNativeJunkWithoutReplacingClientSlots(t *testing.T) {
	f := newOSTFixture(t)
	f.transport.EnableFolderMetadataSpike()
	f.box.page.Objects = append(f.box.page.Objects, core.Object{
		ID: "opaque-junk", Parent: f.box.Root(), Kind: core.Folder,
		Properties: core.Properties{core.WellKnown: core.Text("junkemail"), core.DisplayName: core.Text("Spam name")},
	})
	read := folderPropertiesOperation(4, 1, 0, 0x36d81102)
	check := func(wire []byte, other [][]byte) {
		t.Helper()
		if len(wire) < 11 || !bytes.Equal(wire[:7], []byte{7, 1, 0, 0, 0, 0, 0}) {
			t.Fatal("missing AdditionalRenEntryIds")
		}
		r := reader{data: wire[7:]}
		if n := r.u32(); int(n) != len(other) {
			t.Fatalf("got %d slots, want %d", n, len(other))
		}
		for i, want := range other {
			h := r.take(2)
			if len(h) != 2 {
				t.Fatal("missing slot length")
			}
			got := r.take(int(binary.LittleEndian.Uint16(h)))
			if i == junkEntryIDIndex {
				if len(got) != folderEntryIDBytes {
					t.Fatal("expected canonical Junk EntryID")
				}
				fid := fidFromGlobalCounter(got[38:44])
				object, _ := f.state.mailbox.object(f.handles[1], 4)
				if key := object.binding.store.byFID[fid]; key.native != "opaque-junk" {
					t.Fatal("Junk reference did not resolve to its native folder")
				}
			} else if !bytes.Equal(got, want) {
				t.Fatalf("changed slot %d", i)
			}
		}
		if r.done() != nil {
			t.Fatal("unexpected trailing bytes")
		}
	}
	initial, _ := f.run(read)
	check(initial, make([][]byte, 5))
	written := syntheticAdditionalEntryIDs()
	f.run(additionalEntryIDsOperation(4, 1, written))
	for _, role := range []SpecialFolderRole{Inbox, MailboxRoot} {
		f.open(int(role))
		got, _ := f.run(read)
		check(got, [][]byte{{0xa, 0x1c, 0xff}, nil, {1}, {2, 3}, nil, []byte("PRIVATE-extension")})
	}
	f.open(int(Inbox))
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	if got := object.binding.store.metadata[object.fid][additionalEntryIDs]; !bytes.Equal(got, written) {
		t.Fatal("projection overwrote saved metadata")
	}
}

func TestAdditionalEntryIDsPublishJunkInInboxHierarchy(t *testing.T) {
	f := newHierarchyDownloadFixture(t)
	f.transport.EnableFolderMetadataSpike()
	f.box.page.Objects = append(f.box.page.Objects, core.Object{
		ID: "opaque-junk", Parent: f.box.Root(), Kind: core.Folder,
		Properties: core.Properties{core.WellKnown: core.Text("junkemail"), core.DisplayName: core.Text("Spam name")},
	})
	f.open(int(Inbox))
	f.run(additionalEntryIDsOperation(4, 1, syntheticAdditionalEntryIDs()))
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	store := object.binding.store
	view, err := store.observeHierarchy(t.Context(), f.session, f.fids[MailboxRoot], hierarchyTestTime)
	if err != nil {
		t.Fatal(err)
	}
	all, _ := f.run(folderPropertiesOperation(4, 1, 0, 0x36d81102))
	want, err := additionalEntryIDSlots(all[7:])
	if err != nil || len(want) != 6 {
		t.Fatal("missing direct Inbox property")
	}
	for _, excluded := range []bool{false, true} {
		config := synchronizationConfigure{}
		if excluded {
			config.tags = []uint32{0x36d80000}
		}
		stream, err := hierarchyFastTransfer(store.replica, object.binding.mailboxGUID, config, hierarchyDelta{changes: view.folders})
		if err != nil {
			t.Fatal(err)
		}
		var source []byte
		count := 0
		for _, token := range readTestFX(t, stream.data) {
			if token.tag == 0x65e00102 {
				source = token.value
			}
			if token.tag != 0x36d81102 {
				continue
			}
			key := store.hierarchyXID(f.fids[Inbox])
			if !bytes.Equal(source, key[:]) {
				t.Fatal("only Inbox should carry additional references in the hierarchy")
			}
			r := reader{data: token.value}
			if r.u32() != uint32(len(want)) {
				t.Fatal("FastTransfer changed the number of slots")
			}
			for i, slot := range want {
				if !bytes.Equal(r.take(int(r.u32())), slot) {
					t.Fatalf("FastTransfer changed Inbox slot %d", i)
				}
			}
			if r.done() != nil {
				t.Fatal("malformed FastTransfer property")
			}
			count++
		}
		if !excluded && count != 1 || excluded && count != 0 {
			t.Fatal("missing Inbox property or filter ignored")
		}
	}
}

func TestAdditionalEntryIDsAtomicQuotaAccessAndSnapshot(t *testing.T) {
	f := newOSTFixture(t)
	f.transport.EnableFolderMetadataSpike()
	f.run(metadataOperation(4, 1))
	obj, _ := f.state.mailbox.object(f.handles[1], 4)
	s := obj.binding.store
	before := cloneMetadata(s.metadata[obj.fid])
	huge := binary.LittleEndian.AppendUint16(append32(nil, 1), 32754)
	huge = append(huge, make([]byte, 32754)...)
	rop, _ := f.run(additionalEntryIDsOperation(4, 1, huge))
	if !bytes.Equal(rop, ropError(0xa, 1, 0x8007000e)) || !reflect.DeepEqual(before, s.metadata[obj.fid]) {
		t.Fatal("shared byte budget atomicity")
	}
	value := syntheticAdditionalEntryIDs()
	f.run(additionalEntryIDsOperation(4, 1, value))
	before = cloneMetadata(s.metadata[obj.fid])
	f.box.getErr = core.ErrAccessDenied
	rop, _ = f.run(additionalEntryIDsOperation(4, 1, append32(nil, 0)))
	f.box.getErr = nil
	if !bytes.Equal(rop, ropError(0xa, 1, accessDenied)) || !reflect.DeepEqual(before, s.metadata[obj.fid]) {
		t.Fatal("current native access")
	}
	f.box.state.Owner = false
	rop, _ = f.run(additionalEntryIDsOperation(4, 1, append32(nil, 0)))
	f.box.state.Owner = true
	if !bytes.Equal(rop, ropError(0xa, 1, accessDenied)) || !reflect.DeepEqual(before, s.metadata[obj.fid]) {
		t.Fatal("current owner")
	}
	// Unknown list suffix cannot execute the Release prefix or partial write.
	mixed := additionalEntryIDsOperation(4, 1, value)
	mixed = append32(mixed, 0x12340003)
	mixed = append32(mixed, 1)
	mixed[5] = 2
	binary.LittleEndian.PutUint16(mixed[3:], uint16(len(mixed)-5))
	f.execute(append([]byte{1, 4, 1}, mixed...), 65535)
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 || !reflect.DeepEqual(before, s.metadata[obj.fid]) {
		t.Fatal("unknown batch effects")
	}
	view := f.transport.metadataView(f.session.Identity().Scope, nil)
	s.setMetadata(context.Background(), f.session, obj.fid, folderMetadata{0x36d8: append32(nil, 0)})
	if !bytes.Equal(view.property(obj.fid, 0x36d81102, false, 1252).data, value) || view.bound(0x36d81102) < len(value) {
		t.Fatal("snapshot or bound")
	}
	// Oversized response preflight cannot release an object.
	large := binary.LittleEndian.AppendUint16(append32(nil, 1), 32700)
	large = append(large, make([]byte, 32700)...)
	if _, code := s.setMetadata(context.Background(), f.session, obj.fid, folderMetadata{0x36d8: large}); code != 0 {
		t.Fatal("large array setup")
	}
	ops := append([]byte{1, 4, 1}, folderPropertiesOperation(4, 1, 0, 0x36d81102, 0x36d81102, 0x36d81102)...)
	f.execute(ops, 65535)
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
		t.Fatal("response bound executed Release")
	}
	h := newHierarchyFixture(t)
	fid := h.binding.fids[Inbox]
	a := h.snapshot(t, MailboxRoot, hierarchyTestTime)
	if _, code := h.store.setMetadata(context.Background(), h.session, fid, folderMetadata{0x36d8: value}); code != 0 {
		t.Fatal("hierarchy set")
	}
	b := h.snapshot(t, MailboxRoot, hierarchyTestTime.Add(time.Second))
	if hierarchyRow(t, a, fid).cn == hierarchyRow(t, b, fid).cn {
		t.Fatal("array CN unchanged")
	}
	h.store.setMetadata(context.Background(), h.session, fid, folderMetadata{0x36d8: value})
	c := h.snapshot(t, MailboxRoot, hierarchyTestTime.Add(2*time.Second))
	if !reflect.DeepEqual(b, c) {
		t.Fatal("identical array changed hierarchy")
	}
}
