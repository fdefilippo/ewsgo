// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
	"ewsgo/internal/storage/sqlite"
)

type replicaProofFailure struct {
	*sqlite.Store
	fail, failUpload bool
}

func (s *replicaProofFailure) PublishMAPIContentsProof(ctx context.Context, mailbox string, epoch [16]byte, digest [32]byte) error {
	if s.fail {
		return core.ErrConflict
	}
	return s.Store.PublishMAPIContentsProof(ctx, mailbox, epoch, digest)
}

func (s *replicaProofFailure) PublishMAPIUploadProof(ctx context.Context, mailbox string, epoch [16]byte, proof mapireplica.UploadProof) error {
	if s.failUpload {
		return core.ErrConflict
	}
	return s.Store.PublishMAPIUploadProof(ctx, mailbox, epoch, proof)
}

func sqliteExecuteFixture(t *testing.T, f *ostFixture) func() *replicaProofFailure {
	t.Helper()
	path := filepath.Join(t.TempDir(), "wire.sqlite")
	return func() *replicaProofFailure {
		db, err := sqlite.Open(t.Context(), path)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = db.Close() })
		backend := &replicaProofFailure{Store: db}
		f.state.mailbox.close()
		f.transport.stores = newRAMStores()
		f.transport.stores.calendarMetadata = f.transport.calendarMetadataSpike
		f.transport.stores.contactsMetadata = f.transport.contactsMetadataSpike
		f.transport.EnableReplicaPersistence(backend, func(ctx context.Context, scope core.Scope) (string, error) {
			mailbox, err := db.BindMailbox(ctx, scope, "synthetic")
			return mailbox.MailboxID, err
		})
		f.state.mailbox = newMailboxObjects(f.transport.stores)
		t.Cleanup(f.state.mailbox.close)
		f.handles = []uint32{0xffffffff, 0xffffffff, 0xffffffff, 0xffffffff}
		logon := faiRunOK(t, f, logonOperation(f.identity.LegacyDN, 4, 0))
		for role := range f.fids {
			f.fids[role] = binary.LittleEndian.Uint64(logon[7+role*8:])
		}
		f.open(int(Inbox))
		return backend
	}

}

func TestSQLiteContentsCheckpointAndReservationResumeThroughExecute(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	start := sqliteExecuteFixture(t, f)

	db := start()
	startingFIDs := f.fids
	config := fullContentsConfig()
	config.flags &^= 0x10
	configureContentsTest(t, f, config)
	if data, status := getFXBuffer(t, f.run, 2, 1); len(data) != 0 || status != 2 {
		t.Fatal("a one-byte buffer should leave the whole stream pending")
	}
	obj, code := f.state.mailbox.object(f.handles[2], 4)
	if code != 0 {
		t.Fatalf("contents download not opened: %x", code)
	}
	d := obj.download.contents
	scope, final := d.state.scope, d.final
	st := obj.binding.store
	db.fail = true
	response, _ := f.run(hierarchyBufferOperation(4, 2, 60_000))
	if binary.LittleEndian.Uint32(response[2:]) != hierarchyError(core.ErrConflict) ||
		d.cursor != 0 || !errors.Is(st.validateContentsCheckpoint(t.Context(), scope, final), core.ErrResync) {
		t.Fatal("failed checkpoint commit moved the response cursor or published the proof")
	}
	db.fail = false
	stream := drainFX(t, f.run, 2, 61)
	wireState := contentsFourTestSets(t, stream)
	if err := st.validateContentsCheckpoint(t.Context(), scope, wireState); err != nil {
		t.Fatal("final response did not publish the wire state", err)
	}
	for i, guid := range [][16]byte{{11}, {22}} {
		if id, code := st.replicaID(t.Context(), guid); code != 0 || id != uint16(i+2) {
			t.Fatalf("register foreign replica: %d, %#x", id, code)
		}
	}
	issued := faiRunOK(t, f, []byte{0x7f, 4, 0, 2, 0, 0, 0})
	reservation := st.localReservations[0]
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}

	start()
	current := replicaStore(t, f)
	if f.fids != startingFIDs || current.replica != st.replica ||
		len(current.localReservations) != 1 || current.localReservations[0] != reservation {
		t.Fatal("new context lost the replica or previously issued IDs")
	}
	if got, code := current.replicaGUID(t.Context(), 3); code != 0 || got != ([16]byte{22}) {
		t.Fatalf("foreign REPLID was lost: %x, %#x", got, code)
	}
	next := faiRunOK(t, f, []byte{0x7f, 4, 0, 1, 0, 0, 0})
	if len(issued) != 28 || len(next) != 28 || current.localReservations[0].high != reservation.high+1 {
		t.Fatal("reservation after reopening did not advance the same interval")
	}
	box.objects = append(box.objects, messageTestObject("later-message", box.objects[0].Parent))
	configureContentsTest(t, f, config)
	uploadContentsWireTest(t, f, wireState)
	replayed := drainFX(t, f.run, 2, 71)
	changes := 0
	for _, token := range decodeTestContents(t, replayed) {
		if token.tag == 0x65e00102 {
			changes++
		}
	}
	if changes != 1 {
		t.Fatalf("expected just the post-restart message, got %d changes", changes)
	}
}

var _ mapireplica.Store = (*replicaProofFailure)(nil)

func TestSQLiteContentsReconcilesClientStateForSameFolder(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	start := sqliteExecuteFixture(t, f)
	db := start()
	config := fullContentsConfig()
	configureContentsTest(t, f, config)
	state := contentsFourTestSets(t, drainFX(t, f.run, 2, 300))
	if len(state.read.replicas) == 0 {
		t.Fatal("missing baseline read state")
	}
	state.read = idSet{}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	start()
	box.objects = append(box.objects, messageTestObject("later-message", box.objects[0].Parent))
	configureContentsTest(t, f, config)
	obj, _ := f.state.mailbox.object(f.handles[2], 4)
	download := obj.download.contents.state
	if err := download.store.validateContentsCheckpointRoot(t.Context(), download.scope, state, download.root); !errors.Is(err, core.ErrResync) {
		t.Fatal("state should have no exact proof", err)
	}
	uploadContentsWireTest(t, f, state)
	changes := 0
	for _, token := range decodeTestContents(t, drainFX(t, f.run, 2, 300)) {
		switch token.tag {
		case 0x65e00102:
			changes++
		case 0x67e50102, 0x40210102:
			t.Fatal("reconciliation deleted a message")
		}
	}
	if changes != 1 {
		t.Fatalf("expected only the new message, got %d changes", changes)
	}

	faiRunOK(t, f, []byte{1, 4, 2, 1, 4, 1})
	f.open(int(SentItems))
	configureContentsTest(t, f, config)
	uploadContentsWireTest(t, f, state)
	response, _ := f.run(hierarchyBufferOperation(4, 2, 4096))
	if len(response) < 6 || binary.LittleEndian.Uint32(response[2:]) != hierarchyError(core.ErrResync) {
		t.Fatalf("reconciliation accepted a Given from another folder: %x", response)
	}
}

func TestSQLiteContentsCheckpointPreservesReservedClientGiven(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	start := sqliteExecuteFixture(t, f)
	db := start()
	config := fullContentsConfig()
	config.flags &^= 0x10
	configureContentsTest(t, f, config)
	checkpoint := contentsFourTestSets(t, drainFX(t, f.run, 2, 71))
	st := replicaStore(t, f)
	faiRunOK(t, f, []byte{0x7f, 4, 0, 1, 0, 0, 0})
	counter := st.localReservations[0].low
	resume := cloneContentsState(checkpoint)
	resume.given = singleReplicaSet(st.replica, append(contentsRanges(resume.given), idRange{counter, counter}))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	start()
	st = replicaStore(t, f)
	box.objects = append(box.objects, messageTestObject("later-message", box.objects[0].Parent))
	configureContentsTest(t, f, config)
	uploadContentsWireTest(t, f, resume)
	wire := drainFX(t, f.run, 2, 71)
	changes := 0
	for _, token := range decodeTestContents(t, wire) {
		switch token.tag {
		case 0x65e00102:
			changes++
		case 0x67e50102, 0x40210102:
			t.Fatalf("unexpected deletion: %#x", token.tag)
		case 0x402d0102, 0x402e0102:
			raw := token.value.([]byte)
			if len(raw) < 2 || binary.LittleEndian.Uint16(raw) != 1 {
				t.Fatal("unexpected replica in read state")
			}
			total := 0
			r := reader{data: raw[2:]}
			ranges, err := parseGlobset(&r, &total)
			if err != nil || r.pos != len(r.data) || singleReplicaSet(st.replica, ranges).contains(st.replica, counter) {
				t.Fatal("unexpected read state for client ID", err)
			}
		}
	}
	final := contentsFourTestSets(t, wire)
	_, bound := st.byMID[folderID(counter)]
	if changes != 1 || !final.given.contains(st.replica, counter) || bound ||
		final.seen.contains(st.replica, counter) || final.read.contains(st.replica, counter) {
		t.Fatal("pending client ID altered server changes or disappeared from Given")
	}

	faiRunOK(t, f, []byte{1, 4, 2})
	faiRunOK(t, f, []byte{0x7f, 4, 0, 1, 0, 0, 0})
	next := st.localReservations[0].high
	final.given = singleReplicaSet(st.replica, append(contentsRanges(final.given), idRange{next, next}))
	configureContentsTest(t, f, config)
	uploadContentsWireTest(t, f, final)
	replay := drainFX(t, f.run, 2, 71)
	for _, token := range decodeTestContents(t, replay) {
		if token.tag == 0x65e00102 {
			t.Fatal("completed checkpoint replayed a message")
		}
	}
	given := contentsFourTestSets(t, replay).given
	if !given.contains(st.replica, counter) || !given.contains(st.replica, next) {
		t.Fatal("completed checkpoint lost the pending client ID")
	}

	faiRunOK(t, f, []byte{1, 4, 2})
	configureContentsTest(t, f, config)
	forged := cloneContentsState(resume)
	forged.seen = singleReplicaSet(st.replica, append(contentsRanges(forged.seen), idRange{counter, counter}))
	uploadContentsWireTest(t, f, forged)
	response, _ := f.run(hierarchyBufferOperation(4, 2, 4096))
	if len(response) < 6 || binary.LittleEndian.Uint32(response[2:]) != hierarchyError(core.ErrResync) {
		t.Fatalf("modified Seen checkpoint accepted: %x", response)
	}

	obj, _ := f.state.mailbox.object(f.handles[2], 4)
	fidCount := folderGlobalCount(f.fids[Inbox])
	stage, _, err := obj.download.contents.state.validateSetDiagnostic(
		singleReplicaSet(st.replica, []idRange{{fidCount, fidCount}}), idsetGivenTag)
	if stage != uploadGivenUnknown || !errors.Is(err, core.ErrResync) {
		t.Fatal("Given accepted a counter belonging to a folder")
	}
}

func TestSQLiteCollectorCheckpointResumesThroughExecute(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	f.transport.EnableFAICollectorSpike()
	start := sqliteExecuteFixture(t, f)
	db := start()

	config := fullContentsConfig()
	config.flags |= 0x10
	configureContentsTest(t, f, config)
	checkpoint := contentsFourTestSets(t, drainFX(t, f.run, 2, 300))
	obj, _ := f.state.mailbox.object(f.handles[2], 4)
	scope := obj.download.contents.state.scope
	faiRunOK(t, f, []byte{1, 4, 2, 0x7e, 4, 1, 2, 1})
	uploadContentsWireTest(t, f, checkpoint)
	request := syntheticFAIImport()
	faiRunOK(t, f, faiImportOperation(request))
	faiRunOK(t, f, faiSetOperation(3, faiCreateProperties()))
	f.handles = append(f.handles, 0xffffffff)
	faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 2})
	faiRunOK(t, f, []byte{0x82, 4, 2, 4})
	obj, _ = f.state.mailbox.object(f.handles[4], 4)
	d := obj.download.contents
	db.failUpload = true
	response, _ := f.run(hierarchyBufferOperation(4, 4, 60_000))
	if binary.LittleEndian.Uint32(response[2:]) != hierarchyError(core.ErrConflict) || d.cursor != 0 {
		t.Fatal("failed upload proof was accepted")
	}
	db.failUpload = false
	wireState := collectorThreeTestSets(t, drainFX(t, f.run, 4, 300), checkpoint.given)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	start()
	st := replicaStore(t, f)
	if err := st.validateContentsCheckpointRoot(t.Context(), scope, wireState, f.fids[SentItems]); !errors.Is(err, core.ErrResync) {
		t.Fatal("proof crossed folder boundary")
	}
	configureContentsTest(t, f, config)
	uploadContentsWireTest(t, f, wireState)
	result := drainFX(t, f.run, 2, 300)
	seen := contentsFourTestSets(t, result).seenFAI
	if !seen.containsSet(wireState.seenFAI) {
		t.Fatal("completed upload proof was lost after reopening")
	}
}

func TestSQLiteCollectorNormalSeenProofResumesThroughExecute(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableFAICollectorSpike()
	start := sqliteExecuteFixture(t, f)
	db := start()
	config := fullContentsConfig()
	configureContentsTest(t, f, config)
	baseline := contentsFourTestSets(t, drainFX(t, f.run, 2, 300))
	obj, _ := f.state.mailbox.object(f.handles[2], 4)
	scope := obj.download.contents.state.scope
	faiRunOK(t, f, []byte{1, 4, 2, 0x7e, 4, 1, 2, 1})
	uploadContentsWireTest(t, f, baseline)
	obj, _ = f.state.mailbox.object(f.handles[2], 4)
	collector := obj.download.collector
	root := f.fids[Inbox]
	store := replicaStore(t, f)

	// Provide the same verified CN and MID a native import will acknowledge.
	box.objects = append(box.objects, messageTestObject("imported-normal", box.objects[0].Parent))
	observed, err := store.observeMailMessages(t.Context(), f.session, root)
	if err != nil || len(observed.messages) != 2 {
		t.Fatal("failed to observe ordinary message fixture", err)
	}
	var imported messageRow
	for _, row := range observed.messages {
		if row.object.ID == "imported-normal" {
			imported = row
		}
	}
	if imported.cn == 0 {
		t.Fatal("missing normal change number")
	}
	if err = collector.prepareMutation(t.Context(), f.session, 1); err != nil {
		t.Fatal(err)
	}
	collector.state.value.seen = singleReplicaSet(store.replica,
		append(contentsRanges(baseline.seen), idRange{imported.cn, imported.cn}))
	collector.created = []uint64{folderGlobalCount(imported.mid)}
	faiRunOK(t, f, []byte{0x82, 4, 2, 3})
	clientGiven := singleReplicaSet(store.replica, append(contentsRanges(baseline.given),
		idRange{folderGlobalCount(imported.mid), folderGlobalCount(imported.mid)}))
	state := collectorThreeTestSets(t, drainFX(t, f.run, 3, 300), clientGiven)
	if !state.seen.contains(store.replica, imported.cn) {
		t.Fatal("collector omitted the committed normal change")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	start()
	store = replicaStore(t, f)
	if err := store.validateContentsCheckpointRoot(t.Context(), scope, state, root); err != nil {
		t.Fatal("normal Seen checkpoint was lost after reopening", err)
	}
	for _, tamper := range []func(*contentsSyncState){
		func(s *contentsSyncState) { s.seen = baseline.seen },
		func(s *contentsSyncState) { s.given = idSet{} },
	} {
		forged := cloneContentsState(state)
		tamper(&forged)
		if err := store.validateContentsCheckpointRoot(t.Context(), scope, forged, root); !errors.Is(err, core.ErrResync) {
			t.Fatal("accepted altered normal checkpoint")
		}
	}
	configureContentsTest(t, f, config)
	uploadContentsWireTest(t, f, state)
	for _, token := range decodeTestContents(t, drainFX(t, f.run, 2, 300)) {
		if token.tag == 0x65e00102 {
			t.Fatal("delivered a message already acknowledged by the collector")
		}
	}
}
