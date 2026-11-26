// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"reflect"
	"testing"

	"ewsgo/internal/core"
)

func contentsConfigureOperation(c synchronizationConfigure, restriction []byte) []byte {
	out := []byte{0x70, 4, 1, 2, 1, c.sendOptions}
	out = binary.LittleEndian.AppendUint16(out, c.flags)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(restriction)))
	out = append(out, restriction...)
	out = binary.LittleEndian.AppendUint32(out, c.extraFlags)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(c.tags)))
	for _, tag := range c.tags {
		out = binary.LittleEndian.AppendUint32(out, tag)
	}
	return out
}
func newContentsDownloadFixture(t *testing.T) (*ostFixture, *messageFixtureMailbox) {
	t.Helper()
	f := newOSTFixture(t)
	f.transport.EnableHierarchyDownloadSpike()
	f.transport.EnableContentsDownloadSpike()
	f.box.page.Objects[0].Properties = core.Properties{core.DisplayName: core.Text("Native root")}
	f.handles = append(f.handles, 0xffffffff, 0xffffffff)
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	box := &messageFixtureMailbox{stateMailbox: f.box, objects: []core.Object{messageTestObject("opaque-message", object.binding.store.byFID[f.fids[Inbox]].native)}}
	f.session = &testSession{box}
	t.Cleanup(f.state.mailbox.close)
	return f, box
}
func configureContentsTest(t *testing.T, f *ostFixture, c synchronizationConfigure) {
	t.Helper()
	response, counts := f.run(contentsConfigureOperation(c, nil))
	if !bytes.Equal(response, []byte{0x70, 2, 0, 0, 0, 0}) || counts.errors != 0 {
		t.Fatal("Configure Contents failed")
	}
}
func contentsFourTestSets(t *testing.T, raw []byte) contentsSyncState {
	t.Helper()
	var state contentsSyncState
	sets := map[uint32]*idSet{idsetGivenTag: &state.given, cnsetSeenTag: &state.seen, cnsetSeenFAITag: &state.seenFAI, cnsetReadTag: &state.read}
	counts := map[uint32]int{}
	for _, token := range decodeTestContents(t, raw) {
		if target := sets[token.tag]; target != nil {
			set, err := parseIDSet(token.value.([]byte))
			if err != nil {
				t.Fatal("invalid four-state wire")
			}
			*target = set
			counts[token.tag]++
		}
	}
	for tag := range sets {
		if counts[tag] != 1 {
			t.Fatal("missing/duplicated state")
		}
	}
	return state
}
func uploadContentsWireTest(t *testing.T, f *ostFixture, state contentsSyncState) {
	t.Helper()
	uploadContentsWireTestState(t, f.run, state)
}
func uploadContentsWireTestState(t *testing.T, run func([]byte) ([]byte, storeResults), state contentsSyncState) {
	t.Helper()
	for _, item := range []struct {
		tag uint32
		set idSet
	}{{idsetGivenTag, state.given}, {cnsetSeenTag, state.seen}, {cnsetSeenFAITag, state.seenFAI}, {cnsetReadTag, state.read}} {
		raw, err := item.set.marshal()
		if err != nil {
			t.Fatal("marshal state")
		}
		begin := binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32([]byte{0x75, 4, 2}, item.tag), uint32(len(raw)))
		operations := begin
		if len(raw) > 0 {
			operations = append(operations, binary.LittleEndian.AppendUint32([]byte{0x76, 4, 2}, uint32(len(raw)))...)
			operations = append(operations, raw...)
		}
		operations = append(operations, 0x77, 4, 2)
		response, counts := run(operations)
		if counts.errors != 0 || len(response) != 6*(2+boolInt(len(raw) > 0)) {
			t.Fatal("four-state upload failed")
		}
	}
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
func TestContentsDownloadROPFullStateInitialFinalAndReconnect(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	c := fullContentsConfig()
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	store := object.binding.store
	seedFAI(t, store, faiTestRecord(store, f.fids[Inbox], 1))
	configureContentsTest(t, f, c)
	// Too small for even a marker: no cursor movement or published proof.
	data, status := getFXBuffer(t, f.run, 2, 1)
	d, _ := f.state.mailbox.object(f.handles[2], 4)
	if len(data) != 0 || status != 2 || d.download.contents.cursor != 0 {
		t.Fatal("small buffer advanced")
	}
	if store.validateContentsCheckpoint(t.Context(), d.download.contents.state.scope, d.download.contents.final) == nil {
		t.Fatal("incomplete source published checkpoint")
	}
	response, counts := f.run([]byte{0x82, 4, 2, 3})
	if counts.errors != 0 || binary.LittleEndian.Uint32(response[2:]) != 0 {
		t.Fatal("initial state source failed")
	}
	initial := contentsFourTestSets(t, drainFX(t, f.run, 3, 17))
	if !reflect.DeepEqual(initial, contentsSyncState{}) {
		t.Fatal("GetTransferState exposed pending final")
	}
	f.run([]byte{1, 4, 3})
	raw := drainFX(t, f.run, 2, 73)
	final := contentsFourTestSets(t, raw)
	changes := 0
	for _, token := range decodeTestContents(t, raw) {
		if token.tag == 0x65e00102 {
			changes++
		}
	}
	if changes != 2 || len(final.seen.replicas) == 0 || len(final.seenFAI.replicas) == 0 || len(final.read.replicas) == 0 {
		t.Fatal("full Contents/four states missing")
	}
	if err := store.validateContentsCheckpoint(t.Context(), d.download.contents.state.scope, final); err != nil {
		t.Fatal("complete response did not publish")
	}
	f.run([]byte{0x82, 4, 2, 3})
	if after := contentsFourTestSets(t, drainFX(t, f.run, 3, 17)); !reflect.DeepEqual(after, final) {
		t.Fatal("final transfer state mismatch")
	}
	f.run([]byte{1, 4, 3, 1, 4, 2})
	// New EMSMDB context with the same process/store and fresh handles.
	f.state.mailbox.close()
	f.state.mailbox = newMailboxObjects(f.transport.stores)
	f.handles = []uint32{0xffffffff, 0xffffffff, 0xffffffff, 0xffffffff}
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	f.open(int(Inbox))
	configureContentsTest(t, f, c)
	uploadContentsWireTest(t, f, final)
	raw = drainFX(t, f.run, 2, 71)
	for _, token := range decodeTestContents(t, raw) {
		if token.tag == 0x65e00102 || token.tag == 0x402d0102 || token.tag == 0x402e0102 {
			t.Fatal("reconnect resent identical content/read flags")
		}
	}
	if !reflect.DeepEqual(contentsFourTestSets(t, raw), final) || box.sourceCalls != 2 {
		t.Fatal("reconnect checkpoint/native capture")
	}
	f.state.mailbox.close()
	if f.transport.stores.transfers.Load() != 0 || f.transport.stores.contents.used != store.fai.bytes+contentsProofMemory {
		t.Fatal("context close leaked handles/buffers")
	}
}

func TestContentsDownloadFetchesNewMailAfterCheckpointAndReconnect(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	box.objects[0].MIME = []byte("Subject: Existing\r\n\r\nExisting body")
	c := fullContentsConfig()
	configureContentsTest(t, f, c)
	initial := contentsFourTestSets(t, drainFX(t, f.run, 2, 73))
	added := messageTestObject("new-native-message", box.objects[0].Parent)
	added.MIME = []byte("Subject: Newly delivered\r\n\r\nNew body")
	box.objects = append(box.objects, added)
	// Preserve the process/store and uploaded checkpoint, like reopening
	// Outlook. The source must be read anew, not the initial frozen stream.
	f.state.mailbox.close()
	f.state.mailbox = newMailboxObjects(f.transport.stores)
	f.handles = []uint32{0xffffffff, 0xffffffff, 0xffffffff, 0xffffffff}
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	f.open(int(Inbox))
	configureContentsTest(t, f, c)
	uploadContentsWireTest(t, f, initial)
	raw := drainFX(t, f.run, 2, 71)
	changes, subjects := 0, 0
	for _, token := range decodeTestContents(t, raw) {
		if token.tag == 0x65e00102 {
			changes++
		}
		if token.tag == 0x0037001f {
			subjects++
			if token.value != "Newly delivered" {
				t.Fatal("incremental stream resent old content")
			}
		}
	}
	final := contentsFourTestSets(t, raw)
	if changes != 1 || subjects != 1 || box.sourceCalls != 2 || reflect.DeepEqual(final, initial) {
		t.Fatal("newly delivered message not transferred after reconnect")
	}
	f.run([]byte{1, 4, 2})
	configureContentsTest(t, f, c)
	uploadContentsWireTest(t, f, final)
	for _, token := range decodeTestContents(t, drainFX(t, f.run, 2, 71)) {
		if token.tag == 0x65e00102 {
			t.Fatal("completed new-mail checkpoint resent content")
		}
	}
	if box.sourceCalls != 3 {
		t.Fatal("new Configure did not reobserve native contents")
	}
}

func TestContentsDownloadConversationPropertiesSurviveWire(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	box.objects = nil
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	for i, subject := range []string{"Topic", "RE: Topic", "Different"} {
		o := messageTestObject(core.ID([]string{"first", "reply", "other"}[i]), object.binding.store.byFID[f.fids[Inbox]].native)
		o.MIME = []byte("Subject: " + subject + "\r\n\r\nbody")
		box.objects = append(box.objects, o)
	}
	configureContentsTest(t, f, fullContentsConfig())
	groups := map[string][][]byte{}
	topic := ""
	for _, token := range decodeTestContents(t, drainFX(t, f.run, 2, 71)) {
		if token.tag == 0x0070001f {
			topic = token.value.(string)
		}
		if token.tag == 0x30130102 {
			groups[topic] = append(groups[topic], token.value.([]byte))
		}
	}
	if len(groups["Topic"]) != 2 || len(groups["Different"]) != 1 || !bytes.Equal(groups["Topic"][0], groups["Topic"][1]) || bytes.Equal(groups["Topic"][0], groups["Different"][0]) {
		t.Fatal("FastTransfer lost conversation properties or collapsed unrelated subjects")
	}
}
func TestContentsDownloadPreflightMalformedUnsupportedAndOutputOverflow(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	c := fullContentsConfig()
	configureContentsTest(t, f, c)
	object, _ := f.state.mailbox.object(f.handles[2], 4)
	d := object.download.contents
	ops := append([]byte{1, 4, 1}, hierarchyBufferOperation(4, 2, 2048)...)
	body, _ := f.execute(ops, 64)
	if storeResponse(t, body).Operations[0] != 0xff || box.sourceCalls != 0 || d.cursor != 0 || d.transfer != nil {
		t.Fatal("output preflight had effects")
	}
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
		t.Fatal("overflow released Folder")
	}
	for _, suffix := range [][]byte{{0xff, 4, 1}, {0x70, 4, 1, 2, 1}} {
		f.execute(append([]byte{1, 4, 1}, suffix...), 65535)
		if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 || box.sourceCalls != 0 {
			t.Fatal("invalid batch had prefix effects")
		}
	}
	f.transport.contentsDownloadSpike = false
	f.execute(append([]byte{1, 4, 1}, contentsConfigureOperation(c, nil)...), 65535)
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
		t.Fatal("disabled Contents executed prefix")
	}
}
func TestContentsDownloadReleaseAbortRevocationAndCrossObjectUse(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	c := fullContentsConfig()
	configureContentsTest(t, f, c)
	data, status := getFXBuffer(t, f.run, 2, 4)
	if len(data) != 4 || status != 1 {
		t.Fatal("partial read")
	}
	object, _ := f.state.mailbox.object(f.handles[2], 4)
	store := object.binding.store
	pending := cloneContentsState(object.download.contents.final)
	scope := object.download.contents.state.scope
	response, counts := f.run(folderOperation(f.fids[Inbox], 4, 2, 3))
	if counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) != NotSupported {
		t.Fatal("Contents used as Folder")
	}
	f.box.state.Owner = false
	response, counts = f.run(hierarchyBufferOperation(4, 2, 4096))
	if counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) != accessDenied || binary.LittleEndian.Uint16(response[6:]) != 0 {
		t.Fatal("revoked stream returned data/Done")
	}
	f.box.state.Owner = true
	f.run([]byte{1, 4, 2})
	if store.validateContentsCheckpoint(t.Context(), scope, pending) == nil || f.transport.stores.contents.used != 0 || f.transport.stores.transfers.Load() != 0 {
		t.Fatal("aborted stream published/leaked")
	}
}
func TestContentsDownloadAllocationLimitsRollback(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	c := fullContentsConfig()
	f.state.mailbox.allocated = maxContextHandleAllocations
	response, counts := f.run(contentsConfigureOperation(c, nil))
	if counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) == 0 || f.transport.stores.contents.used != 0 || f.transport.stores.transfers.Load() != 0 {
		t.Fatal("failed Configure leaked state")
	}
}
func TestContentsDownloadCanceledRequestCannotAdvance(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	c := fullContentsConfig()
	configureContentsTest(t, f, c)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	object, _ := f.state.mailbox.object(f.handles[2], 4)
	response := getContentsBuffer(ctx, f.session, object, Operation{Input: 2, TransferLimit: 4096})
	if binary.LittleEndian.Uint32(response[2:]) == 0 || object.download.contents.cursor != 0 {
		t.Fatal("cancel advanced")
	}
}
