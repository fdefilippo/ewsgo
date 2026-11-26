// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"sync"
	"testing"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

func localIDsOperation(logon, input byte, count uint32) []byte {
	return binary.LittleEndian.AppendUint32([]byte{0x7f, logon, input}, count)
}

func TestReplicaDeletedRangeAndDeferredActionsFraming(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	rangeWire := []byte{ropSetLocalReplicaMidsetDeleted, 4, 1, 52, 0, 1, 0, 0, 0}
	rangeWire = append(rangeWire, make([]byte, 48)...)
	rangeWire[9+21] = 1
	rangeWire[9+24+21] = 2
	deferred := []byte{ropUpdateDeferredActions, 4, 0, 3, 0, 1, 2, 3, 2, 0, 4, 5}
	for _, request := range [][]byte{rangeWire, deferred} {
		for n := 1; n < len(request); n++ {
			if _, err := (ROPBuffer{Operations: request[:n], Handles: f.handles}).DecodeOperations(); err == nil {
				t.Fatalf("accepted truncated request for %#x at byte %d", request[0], n)
			}
		}
		next := []byte{ropTellVersion, 4, 0, 1, 0, 0, 0, 0, 0}
		out, result := f.run(append(bytes.Clone(request), next...))
		status := uint32(NotSupported)
		if request[0] == ropSetLocalReplicaMidsetDeleted {
			status = 0x80070057
		}
		want := append(ropError(request[0], request[2], status), ropError(ropTellVersion, 0, 0)...)
		if !bytes.Equal(out, want) || result.errors != 1 {
			t.Fatalf("local unsupported response = %x, want %x", out, want)
		}
	}
	rangeWire[9+24] = 1 // Mixed replica GUIDs within the same range.
	if _, err := (ROPBuffer{Operations: rangeWire, Handles: f.handles}).DecodeOperations(); err == nil {
		t.Fatal("accepted a range spanning separate replicas")
	}
}

func replicaDeletedOperation(guid [16]byte, ranges ...idRange) []byte {
	wire := binary.LittleEndian.AppendUint16([]byte{0x93, 4, 1}, uint16(4+48*len(ranges)))
	wire = binary.LittleEndian.AppendUint32(wire, uint32(len(ranges)))
	for _, r := range ranges {
		for _, n := range []uint64{r.low, r.high} {
			g := globalCount(n)
			wire = append(wire, guid[:]...)
			wire = append(wire, g[:]...)
			wire = append(wire, 0, 0)
		}
	}
	return wire
}

func TestReplicaDeletedRangesPersistAndProjectOnlyInTheirFolder(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	start := sqliteExecuteFixture(t, f)
	db := start()
	st := replicaStore(t, f)
	faiRunOK(t, f, localIDsOperation(4, 0, 65536))
	issued := st.localReservations[0]
	retired := idRange{issued.low + 1, issued.high - 1}
	pending := idRange{issued.high, issued.high}
	setWire := replicaDeletedOperation(st.replica, retired)

	invalid := replicaDeletedOperation(st.replica, idRange{issued.low, issued.low}, idRange{issued.high + 1, issued.high + 1})
	out, _ := f.run(invalid)
	if !bytes.Equal(out, ropError(0x93, 1, 0x80070057)) || st.retiredRangeCount != 0 {
		t.Fatal("invalid range caused a partial retirement")
	}
	claim := mapireplica.MessageImport{
		Counter: pending.low, Parent: folderGlobalCount(f.fids[Inbox]), Digest: [32]byte{1}, MIME: [32]byte{2},
	}
	if created, err := st.claimMessageImport(t.Context(), claim); err != nil || !created {
		t.Fatal("could not stage the test ID", err)
	}
	out, _ = f.run(replicaDeletedOperation(st.replica, retired, pending))
	if !bytes.Equal(out, ropError(0x93, 1, hierarchyError(core.ErrConflict))) || st.retiredRangeCount != 0 {
		t.Fatal("retired an ID already claimed for upload or committed part of the request")
	}
	if err := st.updateMessageImport(t.Context(), claim, mapireplica.CancelMessageImport); err != nil {
		t.Fatal(err)
	}
	faiRunOK(t, f, setWire)
	position := st.next
	faiRunOK(t, f, setWire)
	if st.next != position || st.retiredRangeCount != 1 {
		t.Fatal("retry advanced the replica or duplicated a range")
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	start()
	st = replicaStore(t, f)
	if !slices.Equal(st.retiredMIDs[f.fids[Inbox]], []idRange{retired}) || !st.retiredCounterLocked(retired.low) {
		t.Fatal("did not reload the abandoned IDs")
	}
	claim.Counter = retired.low
	if created, err := st.claimMessageImport(t.Context(), claim); created || !errors.Is(err, core.ErrConflict) {
		t.Fatal("an abandoned ID could be repurposed for a native import", err)
	}
	config := fullContentsConfig()
	config.flags &^= 0x10
	clientState := contentsSyncState{given: singleReplicaSet(st.replica, []idRange{{retired.low + 1, retired.high}, pending})}

	configureContentsTest(t, f, config)
	uploadContentsWireTest(t, f, clientState)
	fx := drainFX(t, f.run, 2, 97)
	var deleted []idRange
	changes := 0
	for _, token := range decodeTestContents(t, fx) {
		switch token.tag {
		case 0x65e00102:
			changes++
		case 0x67e50102:
			raw := token.value.([]byte)
			if len(raw) < 2 || binary.LittleEndian.Uint16(raw) != 1 {
				t.Fatal("expected own REPLID")
			}
			r := reader{data: raw[2:]}
			total := 0
			var err error
			deleted, err = parseGlobset(&r, &total)
			if err != nil || r.pos != len(r.data) {
				t.Fatal("invalid deleted IDSET", err)
			}
		}
	}
	final := contentsFourTestSets(t, fx)
	if !slices.Equal(deleted, []idRange{{retired.low + 1, retired.high}}) ||
		final.given.contains(st.replica, retired.high) || !final.given.contains(st.replica, pending.low) || changes == 0 {
		t.Fatal("retirement did not emit exactly the overlapping provisional IDs while preserving normal changes")
	}

	faiRunOK(t, f, []byte{1, 4, 2})
	config.flags |= 2
	configureContentsTest(t, f, config)
	uploadContentsWireTest(t, f, clientState)
	fx = drainFX(t, f.run, 2, 97)
	for _, token := range decodeTestContents(t, fx) {
		if token.tag == 0x67e50102 {
			t.Fatal("NoDeletions emitted an abandoned ID")
		}
	}
	if !contentsFourTestSets(t, fx).given.contains(st.replica, retired.high) {
		t.Fatal("NoDeletions removed the abandoned ID from Given")
	}

	faiRunOK(t, f, []byte{1, 4, 2, 1, 4, 1})
	f.open(int(SentItems))
	box.objects = nil // The fixture returns the supplied objects for the requested folder.
	config.flags &^= 2
	configureContentsTest(t, f, config)
	uploadContentsWireTest(t, f, contentsSyncState{given: singleReplicaSet(st.replica, []idRange{{retired.low, retired.low}})})
	fx = drainFX(t, f.run, 2, 97)
	for _, token := range decodeTestContents(t, fx) {
		if token.tag == 0x67e50102 {
			t.Fatal("retirement leaked into another folder")
		}
	}
	if !contentsFourTestSets(t, fx).given.contains(st.replica, retired.low) {
		t.Fatal("another folder discarded a provisional identity")
	}
}

func countValue(b []byte) uint64 {
	var n uint64
	for _, v := range b {
		n = n<<8 | uint64(v)
	}
	return n
}

func TestLocalReplicaIDsRequestFraming(t *testing.T) {
	wire := []byte{0x7f, 4, 1, 0, 0, 1, 0}
	b := ROPBuffer{Operations: wire, Handles: []uint32{1, 2}}
	ops, err := b.DecodeOperations()
	if err != nil || len(ops) != 1 || ops[0].ReplicaIDCount != 65536 || ops[0].Logon != 4 || ops[0].Input != 1 {
		t.Fatal("independent request fixture")
	}
	for n := 1; n < len(wire); n++ {
		b.Operations = wire[:n]
		if out, err := b.DecodeOperations(); err == nil || out != nil {
			t.Fatal("accepted truncation", n)
		}
	}
	b.Operations = append(bytes.Clone(wire), 3, 4, 0)
	if out, err := b.DecodeOperations(); !errors.Is(err, ErrUnsupported) || out != nil {
		t.Fatal("accepted unsupported suffix")
	}
	b.Operations = bytes.Clone(wire)
	b.Operations[2] = 2
	if _, err := b.DecodeOperations(); err == nil {
		t.Fatal("invalid slot")
	}
}

func TestLocalReplicaIDsWireAndReconnect(t *testing.T) {
	f := newOSTFixture(t)
	// This operation requires only the base RAM store, not OST value storage.
	f.transport.ostPropertySpike = false
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	row, c := f.run(localIDsOperation(4, 0, 65536))
	want := append([]byte{0x7f, 0, 0, 0, 0, 0}, store.replica[:]...)
	want = append(want, 0, 0, 0, 0, 0, 14) // Thirteen initial role FIDs.
	if !bytes.Equal(row, want) || c.errors != 0 {
		t.Fatal("success wire fixture")
	}
	if len(store.byFID) != 13 || store.next != 13+65536 {
		t.Fatal("reservation creates objects or wrong range")
	}
	f.state.mailbox = newMailboxObjects(f.transport.stores)
	f.handles = []uint32{0xffffffff, 0xffffffff}
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	row, _ = f.run(localIDsOperation(4, 0, 1))
	if len(row) != 28 || !bytes.Equal(row[6:22], store.replica[:]) || !bytes.Equal(row[22:], []byte{0, 0, 0, 1, 0, 14}) {
		t.Fatal("reconnect reuses reserved IDs or changes replica")
	}
	ids, err := store.assign(t.Context(), []folderKey{{native: "new-fixture-folder"}})
	if err != nil {
		t.Fatal(err)
	}
	wire := binary.LittleEndian.AppendUint64(nil, ids[0])
	if !bytes.Equal(wire, []byte{1, 0, 0, 0, 0, 1, 0, 15}) {
		t.Fatal("FID and GLOBCNT inconsistent")
	}
	// Reserved identifiers are not synthetic folders.
	var reserved [8]byte
	reserved[0] = 1
	copy(reserved[2:], row[22:])
	if _, err := store.folder(context.Background(), f.session, binary.LittleEndian.Uint64(reserved[:])); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("reservation created a folder")
	}
}

func TestLocalReplicaIDsRejectedBatchesHaveNoPrefixEffects(t *testing.T) {
	for _, suffix := range [][]byte{{3, 4, 0}, {0x7f, 4, 0, 1}} {
		f := newOSTFixture(t)
		store := f.state.mailbox.objects[f.handles[0]].binding.store
		before := store.next
		ops := append([]byte{1, 4, 1}, localIDsOperation(4, 0, 512)...)
		ops = append(ops, suffix...)
		body, _ := f.execute(ops, 65535)
		if binary.LittleEndian.Uint32(body[4:]) == 0 || store.next != before {
			t.Fatal("invalid batch executed prefix")
		}
		if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
			t.Fatal("invalid batch released handle")
		}
	}
	f := newOSTFixture(t)
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	before := store.next
	body, _ := f.execute(append([]byte{1, 4, 1}, localIDsOperation(4, 0, 512)...), 45)
	b := storeResponse(t, body)
	if b.Operations[0] != 0xff || binary.LittleEndian.Uint16(b.Operations[1:]) != 46 || store.next != before {
		t.Fatal("output preflight executed prefix")
	}
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
		t.Fatal("preflight released handle")
	}
	body, _ = f.execute(localIDsOperation(4, 0, 512), 46)
	if len(storeResponse(t, body).Operations) != 28 || store.next != before+512 {
		t.Fatal("exact response bound")
	}
}

func TestLocalReplicaIDsAuthorizationAndLimits(t *testing.T) {
	for _, name := range []string{"zero", "above-cap", "uint32-max", "folder", "foreign-logon", "null", "released", "denied-root", "not-owner", "state-error", "foreign-scope", "cancelled", "overflow"} {
		t.Run(name, func(t *testing.T) {
			f := newOSTFixture(t)
			store := f.state.mailbox.objects[f.handles[0]].binding.store
			op := Operation{ID: 0x7f, Logon: 4, Input: 0, ReplicaIDCount: 512}
			session := f.session
			ctx := context.Background()
			want := uint32(0x80070057)
			switch name {
			case "zero":
				op.ReplicaIDCount = 0
			case "above-cap":
				op.ReplicaIDCount = 65537
			case "uint32-max":
				op.ReplicaIDCount = 0xffffffff
			case "folder":
				op.Input = 1
				want = NotSupported
			case "foreign-logon":
				op.Logon = 5
				want = accessDenied
			case "null":
				f.handles[0] = 0xffffffff
				want = nullObject
			case "released":
				f.state.mailbox.release(f.handles[0])
				want = invalidObject
			case "denied-root":
				f.box.getErr = core.ErrAccessDenied
				want = accessDenied
			case "not-owner":
				f.box.state.Owner = false
				want = accessDenied
			case "state-error":
				f.box.stateErr = core.ErrAccessDenied
				want = accessDenied
			case "foreign-scope":
				id := f.session.Identity()
				id.Scope.Account = "other"
				session = scopedSession{Session: f.session, identity: id}
				want = accessDenied
			case "cancelled":
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
				want = 0x80004005
			case "overflow":
				store.next = maxGlobalCount - 511
				want = 0x8007000e
			}
			before := store.next
			row := getLocalReplicaIDs(ctx, session, f.state.mailbox, op, f.handles)
			if len(row) != 6 || binary.LittleEndian.Uint32(row[2:]) != want || store.next != before {
				t.Fatal("rejection/side effects", name)
			}
		})
	}
	s := &ramStore{next: maxGlobalCount - 65536}
	if _, code := s.reserveLocalIDs(context.Background(), 65536); code != 0 || s.next != maxGlobalCount {
		t.Fatal("last valid range")
	}
	if _, code := s.reserveLocalIDs(context.Background(), 1); code != 0x8007000e || s.next != maxGlobalCount {
		t.Fatal("counter wrapped")
	}
}

func TestLocalReplicaIDsConcurrentReservationsAndFIDs(t *testing.T) {
	f := newOSTFixture(t)
	s := f.state.mailbox.objects[f.handles[0]].binding.store
	type interval struct{ first, last uint64 }
	ranges := make(chan interval, 64)
	var wg sync.WaitGroup
	for i := range 32 {
		wg.Go(func() {
			first, code := s.reserveLocalIDs(context.Background(), 512)
			if code != 0 {
				t.Error("reserve")
				return
			}
			n := countValue(first[:])
			ranges <- interval{n, n + 511}
		})
		wg.Go(func() {
			ids, err := s.assign(t.Context(), []folderKey{{native: core.ID(string(rune('A' + i)))}})
			if err != nil {
				t.Error("assign")
				return
			}
			wire := binary.LittleEndian.AppendUint64(nil, ids[0])
			n := countValue(wire[2:])
			ranges <- interval{n, n}
		})
	}
	wg.Wait()
	close(ranges)
	all := []interval{{1, 13}}
	for r := range ranges {
		for _, other := range all {
			if r.first <= other.last && other.first <= r.last {
				t.Fatal("overlap")
			}
		}
		all = append(all, r)
	}
	if len(s.byFID) != 45 || s.next != 13+32*513 {
		t.Fatal("memory/counter growth")
	}
}

func FuzzLocalReplicaIDsRequest(f *testing.F) {
	f.Add([]byte{0x7f, 4, 0, 0, 0, 1, 0})
	f.Add([]byte{1, 4, 1, 0x7f, 4, 0, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, wire []byte) {
		if len(wire) > 65500 {
			t.Skip()
		}
		ops, err := (ROPBuffer{Operations: wire, Handles: []uint32{1, 2}}).DecodeOperations()
		if err != nil && ops != nil {
			t.Fatal("partial decoded batch")
		}
		if err != nil {
			return
		}
		for _, op := range ops {
			if op.ID == 0x7f {
				s := &ramStore{}
				_, code := s.reserveLocalIDs(context.Background(), op.ReplicaIDCount)
				if code == 0 && (s.next != uint64(op.ReplicaIDCount) || s.next > 65536) {
					t.Fatal("unbounded reservation")
				}
			}
		}
	})
}
