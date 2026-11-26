// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"reflect"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func hierarchyConfigureOperation(logon, input, output byte) []byte {
	return []byte{0x70, logon, input, output, 2, 1, 1, 1, 0, 0, 1, 0, 0, 0, 0, 0}
}
func hierarchyBufferOperation(logon, input byte, size uint16) []byte {
	return binary.LittleEndian.AppendUint16([]byte{0x4e, logon, input}, size)
}

func newHierarchyDownloadFixture(t *testing.T) *ostFixture {
	f := newOSTFixture(t)
	f.transport.EnableHierarchyDownloadSpike()
	f.box.page.Objects[0].Properties = core.Properties{core.DisplayName: core.Text("Native root")}
	f.handles = append(f.handles, 0xffffffff, 0xffffffff)
	f.open(int(MailboxRoot))
	rop, counts := f.run(hierarchyConfigureOperation(4, 1, 2))
	if !bytes.Equal(rop, []byte{0x70, 2, 0, 0, 0, 0}) || counts.errors != 0 {
		t.Fatal("Configure failed")
	}
	return f
}

func getFXBuffer(t *testing.T, run func([]byte) ([]byte, storeResults), input byte, size uint16) ([]byte, uint16) {
	t.Helper()
	response, counts := run(hierarchyBufferOperation(4, input, size))
	if len(response) < 15 || counts.errors != 0 || binary.LittleEndian.Uint32(response[2:]) != 0 || response[12] != 0 || int(binary.LittleEndian.Uint16(response[13:])) != len(response)-15 {
		t.Fatal("GetBuffer response framing")
	}
	return response[15:], binary.LittleEndian.Uint16(response[6:])
}

func drainFX(t *testing.T, run func([]byte) ([]byte, storeResults), input byte, size uint16) []byte {
	t.Helper()
	var out []byte
	for i := 0; i < 100000; i++ {
		data, status := getFXBuffer(t, run, input, size)
		out = append(out, data...)
		if status == 3 {
			return out
		}
		if status != 1 || len(data) == 0 {
			t.Fatal("no progress or invalid status")
		}
	}
	t.Fatal("bounded stream did not finish")
	return nil
}

type fxTestToken struct {
	tag   uint32
	value []byte
}

// Independent minimal FastTransfer reader: markers have no value; Given has
// the binary exception; fixed 64-bit values and Unicode/binary use FX framing.
func readTestFX(t *testing.T, raw []byte) []fxTestToken {
	t.Helper()
	var out []fxTestToken
	for len(raw) > 0 {
		if len(raw) < 4 {
			t.Fatal("split/truncated tag")
		}
		tag := binary.LittleEndian.Uint32(raw)
		raw = raw[4:]
		token := fxTestToken{tag: tag}
		switch tag {
		case 0x40120003, 0x40130003, 0x40140003, 0x403a0003, 0x403b0003:
		case 0x36d81102:
			r := reader{data: raw}
			count := r.u32()
			if count > maxAdditionalEntryIDs {
				t.Fatal("invalid multivalue count")
			}
			for range count {
				r.take(int(r.u32()))
			}
			if r.err != nil {
				t.Fatal("truncated multivalue binary property")
			}
			token.value, raw = raw[:r.pos], raw[r.pos:]
		default:
			length := 0
			if uint16(tag) == 0x0014 || uint16(tag) == 0x0040 {
				length = 8
			} else if uint16(tag) == 0x000b {
				length = 2
			} else if uint16(tag) == 3 && tag != idsetGivenTag {
				length = 4
			} else if tag == 0x40170003 || uint16(tag) == 0x0102 || uint16(tag) == 0x001f {
				if len(raw) < 4 {
					t.Fatal("missing binary/string length")
				}
				length = int(binary.LittleEndian.Uint32(raw))
				raw = raw[4:]
			} else {
				t.Fatal("unexpected FX property type")
			}
			if length > len(raw) {
				t.Fatal("truncated property")
			}
			token.value = append([]byte(nil), raw[:length]...)
			raw = raw[length:]
		}
		out = append(out, token)
	}
	return out
}

func testFXSets(t *testing.T, tokens []fxTestToken) (given, seen idSet) {
	t.Helper()
	for _, token := range tokens {
		var err error
		if token.tag == idsetGivenTag {
			given, err = parseIDSet(token.value)
		}
		if token.tag == cnsetSeenTag {
			seen, err = parseIDSet(token.value)
		}
		if err != nil {
			t.Fatal("invalid state in stream")
		}
	}
	return
}

func uploadFXSets(t *testing.T, f *ostFixture, given, seen idSet) {
	t.Helper()
	for _, entry := range []struct {
		tag uint32
		set idSet
	}{{idsetGivenTag, given}, {cnsetSeenTag, seen}} {
		raw, err := entry.set.marshal()
		if err != nil {
			t.Fatal("test set")
		}
		begin := binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32([]byte{0x75, 4, 2}, entry.tag), uint32(len(raw)))
		rop, counts := f.run(begin)
		if !bytes.Equal(rop, []byte{0x75, 2, 0, 0, 0, 0}) || counts.errors != 0 {
			t.Fatal("Begin wire")
		}
		if len(raw) > 0 {
			cont := binary.LittleEndian.AppendUint32([]byte{0x76, 4, 2}, uint32(len(raw)))
			rop, counts = f.run(append(cont, raw...))
			if !bytes.Equal(rop, []byte{0x76, 2, 0, 0, 0, 0}) || counts.errors != 0 {
				t.Fatal("Continue wire")
			}
		}
		rop, counts = f.run([]byte{0x77, 4, 2})
		if !bytes.Equal(rop, []byte{0x77, 2, 0, 0, 0, 0}) || counts.errors != 0 {
			t.Fatal("End wire")
		}
	}
}

func TestHierarchyDownloadFullChunkedStateAndNoChanges(t *testing.T) {
	f := newHierarchyDownloadFixture(t)
	// GetTransferState freezes initial empty sets; it must stay initial even
	// if its separate source is read after the main transfer completes.
	rop, _ := f.run([]byte{0x82, 4, 2, 3})
	if !bytes.Equal(rop, []byte{0x82, 3, 0, 0, 0, 0}) {
		t.Fatal("state source")
	}
	uploadFXSets(t, f, idSet{}, idSet{})
	data := drainFX(t, f.run, 2, 23)
	tokens := readTestFX(t, data)
	folders := 0
	for _, token := range tokens {
		if token.tag == 0x40120003 {
			folders++
		}
	}
	if folders != 14 || tokens[0].tag != 0x40120003 || tokens[len(tokens)-1].tag != 0x40140003 {
		t.Fatal("real hierarchy or final markers")
	}
	given, seen := testFXSets(t, tokens)
	if len(given.replicas) != 1 || len(seen.replicas) != 1 {
		t.Fatal("final state absent")
	}
	initial := readTestFX(t, drainFX(t, f.run, 3, 64))
	g, s := testFXSets(t, initial)
	if len(g.replicas) != 0 || len(s.replicas) != 0 || initial[len(initial)-1].tag != 0x403b0003 {
		t.Fatal("state source changed after creation")
	}
	f.run([]byte{1, 4, 3})
	f.run([]byte{0x82, 4, 2, 3})
	final := readTestFX(t, drainFX(t, f.run, 3, 64))
	g, s = testFXSets(t, final)
	gb, _ := g.marshal()
	wantg, _ := given.marshal()
	sb, _ := s.marshal()
	wants, _ := seen.marshal()
	if !bytes.Equal(gb, wantg) || !bytes.Equal(sb, wants) {
		t.Fatal("completed GetTransferState is not final")
	}
	f.run([]byte{1, 4, 2, 1, 4, 3})
	f.run(hierarchyConfigureOperation(4, 1, 2))
	uploadFXSets(t, f, given, seen)
	unchanged := readTestFX(t, drainFX(t, f.run, 2, 29))
	for _, token := range unchanged {
		if token.tag == 0x40120003 || token.tag == 0x40130003 {
			t.Fatal("no-change sync invented change or deletion")
		}
	}
}

func TestHierarchyDownloadDeltaDeletionUsesREPLIDAndReplay(t *testing.T) {
	f := newHierarchyDownloadFixture(t)
	f.box.page.Objects = append(f.box.page.Objects, core.Object{ID: "ordinary", Parent: f.box.Root(), Kind: core.Folder, Properties: core.Properties{core.DisplayName: core.Text("ordinary")}})
	first := readTestFX(t, drainFX(t, f.run, 2, 127))
	given, seen := testFXSets(t, first)
	object, _ := f.state.mailbox.object(f.handles[2], 4)
	store := object.binding.store
	removedFID := store.byKey[folderKey{native: "ordinary"}]
	f.box.page.Objects = f.box.page.Objects[:len(f.box.page.Objects)-1]
	f.box.page.Objects[1].Properties[core.DisplayName] = core.Text("renamed")
	f.run([]byte{1, 4, 2})
	f.run(hierarchyConfigureOperation(4, 1, 2))
	uploadFXSets(t, f, given, seen)
	delta := readTestFX(t, drainFX(t, f.run, 2, 31))
	changes, deletions := 0, 0
	for _, token := range delta {
		if token.tag == 0x40120003 {
			changes++
		}
		if token.tag == 0x67e50102 {
			deletions++
			counter := globalCount(folderGlobalCount(removedFID))
			want := append([]byte{1, 0, 6}, counter[:]...)
			want = append(want, 0)
			if !bytes.Equal(token.value, want) {
				t.Fatal("deletion is not REPLID-based IDSET singleton")
			}
		}
	}
	if changes != 1 || deletions != 1 {
		t.Fatal("real rename/deletion delta")
	}
	finalGiven, finalSeen := testFXSets(t, delta)
	if finalGiven.contains(store.replica, folderGlobalCount(removedFID)) {
		t.Fatal("deleted object in final state")
	}
	f.run([]byte{1, 4, 2})
	f.run(hierarchyConfigureOperation(4, 1, 2))
	uploadFXSets(t, f, finalGiven, finalSeen)
	for _, token := range readTestFX(t, drainFX(t, f.run, 2, 127)) {
		if token.tag == 0x40120003 || token.tag == 0x40130003 {
			t.Fatal("replay repeats delta")
		}
	}
}

func TestHierarchyDownloadOutputReservationAbortAndPermission(t *testing.T) {
	f := newHierarchyDownloadFixture(t)
	object, _ := f.state.mailbox.object(f.handles[2], 4)
	d := object.download
	// Too-small entire Execute must not Release a prefix or prepare/advance.
	batch := append([]byte{1, 4, 1}, hierarchyBufferOperation(4, 2, 512)...)
	f.execute(batch, 64)
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 || d.stream != nil || d.cursor != 0 {
		t.Fatal("output failure executed prefix")
	}
	// Sentinel respects residual output, and chunks never exceed its bound.
	op := append(hierarchyBufferOperation(4, 2, 0xbabe), 0xff, 0xff)
	body, _ := f.execute(op, 64)
	rop := storeResponse(t, body).Operations
	if len(rop) > 64-10-4*len(f.handles) || len(rop) < 15 || d.cursor == 0 {
		t.Fatal("sentinel output bound")
	}
	cursor := d.cursor
	f.box.state.Owner = false
	rop, counts := f.run(hierarchyBufferOperation(4, 2, 32))
	if counts.errors != 1 || binary.LittleEndian.Uint32(rop[2:]) != accessDenied || d.cursor != cursor {
		t.Fatal("revocation leaks buffered bytes or advances")
	}
	f.box.state.Owner = true
	// A separately captured initial state lets a replacement context redo the
	// unfinished download. Releasing the old source frees its quota slot.
	f.run([]byte{1, 4, 2})
	if f.transport.stores.transfers.Load() != 0 {
		t.Fatal("aborted source slot leaked")
	}
	f.run(hierarchyConfigureOperation(4, 1, 2))
	all := readTestFX(t, drainFX(t, f.run, 2, 127))
	count := 0
	for _, token := range all {
		if token.tag == 0x40120003 {
			count++
		}
	}
	if count != 14 {
		t.Fatal("aborted download was acknowledged as completed")
	}
}

func TestHierarchyDownloadHandleKindsAndUnsupportedBatch(t *testing.T) {
	f := newHierarchyDownloadFixture(t)
	handle := f.handles[2]
	for _, operation := range [][]byte{
		folderPropertiesOperation(4, 2, 0, 0x3001001f),
		folderOperation(f.fids[MailboxRoot], 4, 2, 3),
		ostSetOperation(4, 2, []byte{1}),
	} {
		rop, counts := f.run(operation)
		if counts.errors != 1 || binary.LittleEndian.Uint32(rop[2:]) != NotSupported {
			t.Fatal("download interpreted as Folder")
		}
	}
	bad := hierarchyConfigureOperation(4, 1, 3)
	bad[6] |= 2 // NoDeletions is explicitly outside this profile.
	f.execute(append([]byte{1, 4, 2}, bad...), 65535)
	if _, code := f.state.mailbox.object(handle, 4); code != 0 {
		t.Fatal("unsupported configure executed Release prefix")
	}
	if body, _ := f.execute([]byte{0x82, 4, 2, 9}, 65535); binary.LittleEndian.Uint32(body[4:]) == 0 {
		t.Fatal("invalid output table index")
	}
}

func TestFastTransferIndependentEmptyWireAndAtoms(t *testing.T) {
	f, err := hierarchyFastTransfer(idSetTestReplica, [16]byte{}, synchronizationConfigure{}, hierarchyDelta{})
	want := []byte{3, 0, 0x3a, 0x40, 3, 0, 0x17, 0x40, 0, 0, 0, 0, 2, 1, 0x96, 0x67, 0, 0, 0, 0, 3, 0, 0x3b, 0x40, 3, 0, 0x14, 0x40}
	if err != nil || !bytes.Equal(f.data, want) {
		t.Fatal("independent empty final state grammar")
	}
	stream := &fastTransfer{}
	stream.word(0x40120003)
	stream.fixed(0x30080040, 0)
	stream.variable(0x65e00102, []byte{1, 2, 3, 4})
	for _, test := range []struct{ start, size, end int }{{0, 3, 0}, {0, 7, 4}, {4, 7, 8}, {8, 7, 8}, {8, 8, 16}, {16, 7, 20}, {24, 1, 25}} {
		if stream.chunkEnd(test.start, test.size) != test.end {
			t.Fatal("split a fixed atom or failed to split variable bytes")
		}
	}
	if fileTime(time.Date(1601, 1, 1, 0, 0, 0, 0, time.UTC)) != 0 || fileTime(time.Unix(0, 0)) != 116444736000000000 {
		t.Fatal("independent FILETIME epoch fixture")
	}
	for _, token := range readTestFX(t, stream.data) {
		if token.tag == 0x30080040 && !bytes.Equal(token.value, make([]byte, 8)) {
			t.Fatal("fixed time layout")
		}
	}
}

func TestHierarchyDownloadConfigureFlagsAndGivenBinaryAlias(t *testing.T) {
	valid := synchronizationConfigure{kind: 2, sendOptions: 1, flags: 1}
	for _, change := range []func(*synchronizationConfigure){
		func(c *synchronizationConfigure) { c.kind = 1 }, func(c *synchronizationConfigure) { c.restrictionBytes = 1 },
		func(c *synchronizationConfigure) { c.flags = 0 }, func(c *synchronizationConfigure) { c.sendOptions = 0 },
		func(c *synchronizationConfigure) { c.sendOptions |= 2 }, func(c *synchronizationConfigure) { c.sendOptions |= 4 },
		func(c *synchronizationConfigure) { c.extraFlags = 2 }, func(c *synchronizationConfigure) { c.tags = make([]uint32, maxFolderPropertyTags+1) },
	} {
		bad := valid
		change(&bad)
		if validateHierarchyConfigure(bad) == nil {
			t.Fatal("unsupported option ignored")
		}
	}
	for _, change := range []func(*synchronizationConfigure){func(c *synchronizationConfigure) { c.flags |= 0x100 }, func(c *synchronizationConfigure) { c.sendOptions |= 0x68 }, func(c *synchronizationConfigure) { c.extraFlags = 0x80000001 }} {
		c := valid
		change(&c)
		if validateHierarchyConfigure(c) != nil {
			t.Fatal("required reserved/unknown bit semantics")
		}
	}
	f := newHierarchyFixture(t)
	s := newTestHierarchyState(t, f, MailboxRoot)
	uploadHierarchySet(t, s, f.session, 0x40170102, idSet{})
	if !reflect.DeepEqual(s.given, idSet{}) {
		t.Fatal("Given Binary alias")
	}
}

func TestHierarchyDownloadQuotasAndContextCleanup(t *testing.T) {
	f := newHierarchyDownloadFixture(t)
	object, _ := f.state.mailbox.object(f.handles[2], 4)
	for n := 1; n < maxContextTransfers; n++ {
		if _, err := f.state.mailbox.addDownload(object, &hierarchyDownload{}); err != nil {
			t.Fatal("context quota boundary")
		}
	}
	if _, err := f.state.mailbox.addDownload(object, &hierarchyDownload{}); err == nil || f.transport.stores.transfers.Load() != maxContextTransfers {
		t.Fatal("context quota")
	}
	f.state.mailbox.release(f.handles[0])
	if f.transport.stores.transfers.Load() != 0 {
		t.Fatal("Logon Release leaked sources")
	}
	allocator := newRAMStores()
	var contexts []*mailboxObjects
	for n := 0; n < maxLiveTransfers; n++ {
		objects := newMailboxObjects(allocator)
		if _, err := objects.addDownload(serverObject{}, &hierarchyDownload{}); err != nil {
			t.Fatal("global quota boundary")
		}
		contexts = append(contexts, objects)
	}
	if _, err := newMailboxObjects(allocator).addDownload(serverObject{}, &hierarchyDownload{}); err == nil {
		t.Fatal("global source quota")
	}
	for _, objects := range contexts {
		objects.close()
		objects.close()
	}
	if allocator.transfers.Load() != 0 {
		t.Fatal("cleanup not idempotent")
	}
	for _, mode := range []string{"replace", "revoke", "expiry"} {
		t.Run(mode, func(t *testing.T) {
			f := newHierarchyDownloadFixture(t)
			switch mode {
			case "replace":
				f.state.busy = false
				_, code := f.transport.contexts.create(f.state.scope, f.state.endpoint, f.state.id, Connect{})
				if code != 0 {
					t.Fatal("replacement")
				}
			case "revoke":
				f.transport.contexts.finish(f.state, true)
			case "expiry":
				f.state.busy = false
				f.transport.contexts.prune(f.state.expires.Add(time.Second))
			}
			if f.transport.stores.transfers.Load() != 0 {
				t.Fatal("dropped context leaked transfer slot")
			}
		})
	}
	stream := &fastTransfer{}
	stream.variable(0x65e00102, make([]byte, maxFastTransferBytes))
	if stream.err == nil || len(stream.data) > maxFastTransferBytes {
		t.Fatal("stream byte quota")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := newHierarchyState(ctx, nil, &ramStore{}, 0); err == nil {
		t.Fatal("canceled factory")
	}
}

func TestHierarchyDownloadRejectsForeignStateOnWire(t *testing.T) {
	f := newHierarchyDownloadFixture(t)
	object, _ := f.state.mailbox.object(f.handles[2], 4)
	replica := object.binding.store.replica
	replica[0] ^= 1
	raw, _ := singleReplicaSet(replica, []idRange{{1, 1}}).marshal()
	begin := binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32([]byte{0x75, 4, 2}, cnsetSeenTag), uint32(len(raw)))
	f.run(begin)
	f.run(append(binary.LittleEndian.AppendUint32([]byte{0x76, 4, 2}, uint32(len(raw))), raw...))
	response, counts := f.run([]byte{0x77, 4, 2})
	if counts.errors != 1 || !bytes.Equal(response, []byte{0x77, 2, 5, 8, 4, 0x80}) || len(object.download.state.seen.replicas) != 0 {
		t.Fatal("foreign epoch was acknowledged or silently reset")
	}
	response, counts = f.run(hierarchyBufferOperation(4, 2, 64))
	if counts.errors != 1 || binary.LittleEndian.Uint16(response[6:]) != 0 || object.download.stream != nil {
		t.Fatal("invalid pending state produced Done")
	}
}

func FuzzHierarchyStoreOperations(f *testing.F) {
	f.Add(hierarchyConfigureOperation(4, 1, 2))
	f.Add([]byte{0x4e, 4, 2, 0xbe, 0xba, 0xff, 0xff})
	f.Add([]byte{0x76, 4, 2, 1, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, raw []byte) {
		ops, err := (ROPBuffer{Operations: raw, Handles: []uint32{1, 2, 3, 4}}).decodeStoreOperations(true, true)
		if err != nil {
			if ops != nil {
				t.Fatal("partial decoded batch")
			}
			return
		}
		if len(ops) > 1024 {
			t.Fatal("operation quota")
		}
		for _, op := range ops {
			if int(op.Input) >= 4 || op.Configure != nil && int(op.Output) >= 4 || op.Upload != nil && len(op.Upload.data) > maxIDSetBytes {
				t.Fatal("unbounded decoded operation")
			}
		}
	})
}
