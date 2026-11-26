// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func readStateWire(changes ...readStateChange) []byte {
	var data []byte
	for _, c := range changes {
		data = binary.LittleEndian.AppendUint16(data, 22)
		data = append(data, c.source[:]...)
		data = append(data, byte(boolInt(c.read)))
	}
	return append(binary.LittleEndian.AppendUint16([]byte{0x80, 4, 2}, uint16(len(data))), data...)
}

type readStateMailbox struct {
	*messageFixtureMailbox
	gets, sets, failAt int
	getHook, setHook   func()
	wrongResult        bool
}

func (b *readStateMailbox) Get(ctx context.Context, id core.ID) (core.Object, error) {
	for _, object := range b.objects {
		if object.ID == id {
			b.gets++
			if b.getHook != nil {
				b.getHook()
			}
			object.Properties, _ = core.CloneProperties(object.Properties)
			return object, nil
		}
	}
	return b.stateMailbox.Get(ctx, id)
}
func (b *readStateMailbox) SetRead(_ context.Context, id core.ID, read bool) (core.Object, error) {
	b.sets++
	if b.failAt != 0 && b.sets == b.failAt {
		return core.Object{}, core.ErrAccessDenied
	}
	for i := range b.objects {
		if b.objects[i].ID == id {
			o := &b.objects[i]
			o.Properties[core.IsRead] = core.Bool(read)
			o.Revision++
			o.Properties[core.MetadataRevision] = core.Integer(o.Revision)
			o.Properties[core.LastModifiedAt] = core.Timestamp(hierarchyTestTime.Add(time.Minute))
			if b.setHook != nil {
				b.setHook()
			}
			result := *o
			if b.wrongResult {
				result.Parent = "different-parent"
			}
			return result, nil
		}
	}
	return core.Object{}, core.ErrNotFound
}

func readStateFixture(t *testing.T) (*ostFixture, *readStateMailbox, messageSnapshot) {
	t.Helper()
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableFAICollectorSpike()
	f.transport.EnableReadStateImport()
	b := &readStateMailbox{messageFixtureMailbox: box}
	f.session = &testSession{b}
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	snapshot, err := object.binding.store.observeMailMessages(context.Background(), f.session, f.fids[Inbox])
	if err != nil {
		t.Fatal("fixture observation", err)
	}
	return f, b, snapshot
}
func readStateCollector(t *testing.T, f *ostFixture) *faiCollector {
	t.Helper()
	faiRunOK(t, f, []byte{0x7e, 4, 1, 2, 1})
	o, _ := f.state.mailbox.object(f.handles[2], 4)
	return o.download.collector
}
func readStateReply(t *testing.T, f *ostFixture, wire []byte, expected uint32) {
	t.Helper()
	raw, _ := f.run(wire)
	if len(raw) != 6 || raw[0] != 0x80 || binary.LittleEndian.Uint32(raw[2:]) != expected {
		t.Fatal("unexpected read-state ROP result")
	}
}
func collectorContentsState(t *testing.T, raw []byte) contentsSyncState {
	t.Helper()
	var out contentsSyncState
	sets := map[uint32]*idSet{cnsetSeenTag: &out.seen, cnsetSeenFAITag: &out.seenFAI, cnsetReadTag: &out.read}
	seen := map[uint32]bool{}
	for _, token := range decodeTestContents(t, raw) {
		if token.tag == idsetGivenTag {
			t.Fatal("uploader exported Given")
		}
		if target := sets[token.tag]; target != nil {
			set, err := parseIDSet(token.value.([]byte))
			if err != nil || seen[token.tag] {
				t.Fatal("invalid upload state stream")
			}
			*target, seen[token.tag] = set, true
		}
	}
	if len(seen) != 3 {
		t.Fatal("missing uploader state")
	}
	return out
}

func TestReadStateCodecBoundsDuplicatesAndWholeBatchPreflight(t *testing.T) {
	change := readStateChange{source: [22]byte{1, 2, 3}, read: true}
	wire := readStateWire(change)
	for n := 3; n < len(wire); n++ {
		if _, err := parseReadStateChanges(&reader{data: wire[3:n]}); !errors.Is(err, ErrMalformed) {
			t.Fatal("truncated read-state payload accepted")
		}
	}
	parsed, err := parseReadStateChanges(&reader{data: readStateWire(change, change)[3:]})
	if err != nil || len(parsed) != 1 || parsed[0] != change {
		t.Fatal("idempotent duplicate not coalesced")
	}
	other := change
	other.read = false
	if _, err := parseReadStateChanges(&reader{data: readStateWire(change, other)[3:]}); !errors.Is(err, ErrUnsupported) {
		t.Fatal("contradictory duplicate accepted")
	}
	tooMany := make([]readStateChange, maxReadStateChanges+1)
	if _, err := parseReadStateChanges(&reader{data: readStateWire(tooMany...)[3:]}); !errors.Is(err, ErrUnsupported) {
		t.Fatal("record bound ignored")
	}
	bad := append([]byte(nil), wire...)
	bad[len(bad)-1] = 2
	if _, err := parseReadStateChanges(&reader{data: bad[3:]}); !errors.Is(err, ErrMalformed) {
		t.Fatal("invalid Boolean accepted")
	}
	if bytes.Contains([]byte(fmt.Sprintf("%v %#v", change, change)), []byte("source")) {
		t.Fatal("request formatting not private")
	}
	f, b, snap := readStateFixture(t)
	readStateCollector(t, f)
	valid := readStateWire(readStateChange{source: snap.messages[0].sourceKey, read: true})
	for _, suffix := range [][]byte{valid[:len(valid)-1], append(valid, 0xff, 4, 2)} {
		f.execute(append([]byte{1, 4, 1}, suffix...), 65535)
		if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 || b.sets != 0 || b.gets != 0 {
			t.Fatal("malformed batch executed a prefix or native access")
		}
	}
	f.execute(valid, 12)
	if b.gets != 0 || b.sets != 0 {
		t.Fatal("insufficient output had native effects")
	}
	f.transport.readStateImport = false
	_, counts := f.execute(append([]byte{1, 4, 1}, valid...), 65535)
	if counts.unsupported != "RopSynchronizationImportReadStateChanges" || b.sets != 0 {
		t.Fatal("disabled import executed")
	}
}

func TestReadStateSuccessCheckpointReconnectAndNewMail(t *testing.T) {
	f, box, snap := readStateFixture(t)
	cfg := fullContentsConfig()
	configureContentsTest(t, f, cfg)
	initial := contentsFourTestSets(t, drainFX(t, f.run, 2, 71))
	faiRunOK(t, f, []byte{1, 4, 2})
	c := readStateCollector(t, f)
	uploadContentsWireTest(t, f, initial)
	change := readStateChange{source: snap.messages[0].sourceKey, read: true}
	readStateReply(t, f, readStateWire(change), 0)
	store := c.state.store
	v := store.messages[messageKey{id: box.objects[0].ID}]
	if box.sets != 1 || !v.read || v.cn != snap.messages[0].cn || v.readCN <= snap.messages[0].readCN || v.modified != snap.messages[0].modified {
		t.Fatal("read import changed content version/date or failed to advance read state")
	}
	// An idempotent retry need not issue another native write.
	readStateReply(t, f, readStateWire(change), 0)
	if box.sets != 1 {
		t.Fatal("retry wrote the same flag again")
	}
	faiRunOK(t, f, []byte{0x82, 4, 2, 3})
	updated := cloneContentsState(initial)
	updated.read = cloneIDSet(c.state.value.read)
	download := contentsStateForScopeTest(t, store, f.fids[Inbox], cfg)
	if store.validateContentsCheckpointRoot(t.Context(), download, updated, f.fids[Inbox]) == nil {
		t.Fatal("unconsumed upload source published a grant")
	}
	// Abandon the export, then retry it. The next source must still bind Read.
	faiRunOK(t, f, []byte{1, 4, 3, 0x82, 4, 2, 3})
	exported := collectorContentsState(t, drainFX(t, f.run, 3, 17))
	updated.read = exported.read
	if !updated.read.contains(store.replica, v.readCN) || store.validateContentsCheckpointRoot(t.Context(), download, updated, f.fids[Inbox]) != nil {
		t.Fatal("completed collector Read grant cannot replay")
	}
	forged := cloneContentsState(updated)
	forged.read = singleReplicaSet(store.replica, []idRange{{1, v.readCN}})
	if store.validateContentsCheckpointRoot(t.Context(), download, forged, f.fids[Inbox]) == nil {
		t.Fatal("collector grant allowed unrelated read counters")
	}
	faiRunOK(t, f, []byte{1, 4, 3, 1, 4, 2})
	added := messageTestObject("new-after-read-upload", box.objects[0].Parent)
	added.MIME = []byte("Subject: New after import\r\n\r\nbody")
	box.objects = append(box.objects, added)
	f.state.mailbox.close()
	f.state.mailbox = newMailboxObjects(f.transport.stores)
	f.handles = []uint32{0xffffffff, 0xffffffff, 0xffffffff, 0xffffffff}
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	f.open(int(Inbox))
	configureContentsTest(t, f, cfg)
	uploadContentsWireTest(t, f, updated)
	changes := 0
	for _, token := range decodeTestContents(t, drainFX(t, f.run, 2, 73)) {
		if token.tag == 0x65e00102 {
			changes++
		}
		if token.tag == 0x402d0102 || token.tag == 0x402e0102 {
			raw := token.value.([]byte)
			if len(raw) < 2 || binary.LittleEndian.Uint16(raw) != 1 {
				t.Fatal("invalid read delta replica")
			}
			set, err := parseIDSet(append(append([]byte(nil), store.replica[:]...), raw[2:]...))
			if err != nil || set.contains(store.replica, folderGlobalCount(snap.messages[0].mid)) {
				t.Fatal("acknowledged read state echoed to the importing client")
			}
		}
	}
	if changes != 1 {
		t.Fatal("new message missing after read import/reconnect")
	}
}

func contentsStateForScopeTest(t *testing.T, store *ramStore, root uint64, cfg synchronizationConfigure) [32]byte {
	t.Helper()
	c := contentsConfigure{frame: cfg}
	plan, err := compileContentsFilter(c, &store.named, contentsProjectionSchema(true, true))
	if err != nil {
		t.Fatal("scope fixture")
	}
	scope, err := contentsScopeFingerprint(root, cfg, plan)
	if err != nil {
		t.Fatal("scope fixture")
	}
	return scope
}

func TestReadStateValidationNoWritesAndPartialFailure(t *testing.T) {
	for _, name := range []string{"unknown", "foreign", "moved", "owner", "no_writer", "pending", "wrong_handle", "quota"} {
		t.Run(name, func(t *testing.T) {
			f, b, snap := readStateFixture(t)
			c := readStateCollector(t, f)
			change := readStateChange{source: snap.messages[0].sourceKey, read: true}
			switch name {
			case "unknown":
				change.source[21] ^= 0x40
			case "foreign":
				change.source[0] ^= 0xff
			case "moved":
				b.objects[0].Parent = "another-folder"
			case "owner":
				b.state.Owner = false
			case "no_writer":
				f.session = &testSession{b.messageFixtureMailbox}
			case "pending":
				upload := hierarchyUpload{kind: 0x75, property: cnsetReadTag, size: 1}
				if c.state.upload(context.Background(), f.session, upload) != nil {
					t.Fatal("pending fixture")
				}
			case "quota":
				c.state.store.mu.Lock()
				c.state.store.next = maxGlobalCount
				c.state.store.mu.Unlock()
			}
			wire := readStateWire(change)
			if name == "wrong_handle" {
				wire[2] = 1
			}
			raw, counts := f.run(wire)
			if counts.errors != 1 || len(raw) != 6 || binary.LittleEndian.Uint32(raw[2:]) == 0 || b.sets != 0 {
				t.Fatal("invalid target/context/quota reached a native write")
			}
		})
	}
	f, b, snap := readStateFixture(t)
	c := readStateCollector(t, f)
	second := messageTestObject("second-native-message", b.objects[0].Parent)
	b.objects = append(b.objects, second)
	view, err := c.state.store.observeMailMessages(context.Background(), f.session, f.fids[Inbox])
	if err != nil {
		t.Fatal("second observation", err)
	}
	changes := []readStateChange{}
	for _, row := range view.messages {
		changes = append(changes, readStateChange{source: row.sourceKey, read: true})
	}
	b.failAt = 2
	before := cloneContentsState(c.state.value)
	readStateReply(t, f, readStateWire(changes...), accessDenied)
	if b.sets != 2 || !reflect.DeepEqual(before, c.state.value) || b.objects[0].Properties[core.IsRead] != core.Bool(true) || b.objects[1].Properties[core.IsRead] != core.Bool(false) {
		t.Fatal("partial failure was hidden, rolled back or acknowledged")
	}
	b.failAt = 0
	readStateReply(t, f, readStateWire(readStateChange{source: snap.messages[0].sourceKey, read: true}), 0)
	if b.sets != 2 {
		t.Fatal("partial prefix retry was not idempotent")
	}
}

type conditionalReadMailbox struct {
	*readStateMailbox
	beforeWrite func()
	revisions   []core.Revision
}

func (b *conditionalReadMailbox) SetMailFlag(_ context.Context, id core.ID, expected core.Revision, flag core.MailFlag, read bool) (core.Object, error) {
	b.revisions = append(b.revisions, expected)
	if b.beforeWrite != nil {
		b.beforeWrite()
	}
	object := &b.objects[0]
	if id != object.ID || flag != core.MailFlagRead {
		return core.Object{}, core.ErrInvalid
	}
	if expected != object.Revision {
		return core.Object{}, core.ErrConflict
	}
	object.Properties[core.IsRead] = core.Bool(read)
	object.Revision++
	return *object, nil
}

func TestReadStateImportUsesNativeRevision(t *testing.T) {
	for _, moved := range []bool{false, true} {
		name := "unchanged"
		if moved {
			name = "concurrent_move"
		}
		t.Run(name, func(t *testing.T) {
			f, legacy, snap := readStateFixture(t)
			box := &conditionalReadMailbox{readStateMailbox: legacy}
			f.session = &testSession{box}
			collector := readStateCollector(t, f)
			object := &box.objects[0]
			revision := object.Revision
			before := cloneContentsState(collector.state.value)
			id := messageKey{id: object.ID}
			previous := collector.state.store.messages[id]
			expected := uint32(0)
			if moved {
				box.beforeWrite = func() {
					object.Parent = "destination"
					object.Revision++
				}
				expected = 0x80040802
			}
			readStateReply(t, f, readStateWire(readStateChange{source: snap.messages[0].sourceKey, read: true}), expected)
			if len(box.revisions) != 1 || box.revisions[0] != revision || legacy.sets != 0 {
				t.Fatal("import did not use exactly one conditional write with the observed revision")
			}
			current := collector.state.store.messages[id]
			if moved {
				if object.Parent != "destination" || object.Properties[core.IsRead] != core.Bool(false) ||
					current.read != previous.read || current.readCN != previous.readCN ||
					!reflect.DeepEqual(before, collector.state.value) {
					t.Fatal("conflict changed native or ICS read state")
				}
			} else if object.Properties[core.IsRead] != core.Bool(true) || !current.read ||
				current.readCN <= previous.readCN || reflect.DeepEqual(before, collector.state.value) {
				t.Fatal("successful write did not advance native and ICS read state")
			}
		})
	}
}

func TestReadStateFAIIgnoredAndNativeFailuresNotAcknowledged(t *testing.T) {
	t.Run("fai_ignored_without_native_writer", func(t *testing.T) {
		f, b, _ := readStateFixture(t)
		c := readStateCollector(t, f)
		request := syntheticFAIImport()
		faiRunOK(t, f, faiImportOperation(request))
		faiRunOK(t, f, faiSetOperation(3, faiCreateProperties()))
		faiRunOK(t, f, []byte{0x0c, 4, 1, 3, 0})
		before := cloneContentsState(c.state.value)
		f.session = &testSession{b.messageFixtureMailbox}
		readStateReply(t, f, readStateWire(readStateChange{source: request.source, read: true}), 0)
		if b.gets != 0 || b.sets != 0 || !reflect.DeepEqual(before, c.state.value) {
			t.Fatal("FAI flag was written or altered synchronization state")
		}
	})
	for _, scenario := range []string{"second_parent", "wrong_result", "late_revocation", "cancel_after_write"} {
		t.Run(scenario, func(t *testing.T) {
			f, b, _ := readStateFixture(t)
			c := readStateCollector(t, f)
			b.objects = append(b.objects, messageTestObject("second-native-message", b.objects[0].Parent))
			view, err := c.state.store.observeMailMessages(context.Background(), f.session, f.fids[Inbox])
			if err != nil {
				t.Fatal("fixture observation")
			}
			var changes []readStateChange
			for _, row := range view.messages {
				changes = append(changes, readStateChange{source: row.sourceKey, read: true})
			}
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch scenario {
			case "second_parent":
				b.objects[1].Parent = "another-folder"
			case "wrong_result":
				b.wrongResult = true
			case "late_revocation":
				b.setHook = func() { b.state.Owner = false }
			case "cancel_after_write":
				b.setHook = cancel
			}
			before := cloneContentsState(c.state.value)
			if c.importReadStates(ctx, f.session, changes) == nil || !reflect.DeepEqual(before, c.state.value) {
				t.Fatal("failed native batch acknowledged")
			}
			want := 1
			if scenario == "second_parent" {
				want = 0
			}
			if b.sets != want {
				t.Fatal("prevalidation or late stop failed")
			}
		})
	}
}
