// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"reflect"
	"sync"
	"testing"

	"ewsgo/internal/core"
)

func replicaStore(t *testing.T, f *ostFixture) *ramStore {
	t.Helper()
	object, code := f.state.mailbox.object(f.handles[0], 4)
	if code != 0 {
		t.Fatal("fixture Logon unavailable")
	}
	return object.binding.store
}

func TestReplicaConversionsIndependentWireAndNoObjectCreation(t *testing.T) {
	f, _ := newCalendarFixture(t)
	s := replicaStore(t, f)
	next, folders, messages := s.next, len(s.byFID), len(s.byMID)
	// Fixed protocol bytes: GUID is opaque; counter is not reversed. Nonzero
	// padding is ignored on input and canonicalized to zero on output.
	long, _ := hex.DecodeString("102132435465768798a9bacbdcedfe0f010203040506aabb")
	want, _ := hex.DecodeString("4400000000000200010203040506")
	out, c := f.run(conversionOperation(0x44, long))
	if !bytes.Equal(out, want) || c.errors != 0 {
		t.Fatal("foreign replica independent wire fixture")
	}
	reverse, c := f.run(conversionOperation(0x43, want[6:]))
	canonical := append(bytes.Clone(long[:22]), 0, 0)
	if !bytes.Equal(reverse, append([]byte{0x43, 0, 0, 0, 0, 0}, canonical...)) || c.errors != 0 {
		t.Fatal("foreign replica inverse wire fixture")
	}
	fid := binary.LittleEndian.Uint64(out[6:])
	before := append([]uint32(nil), f.handles...)
	opened, c := f.run(folderOperation(fid, 4, 0, 1))
	if !bytes.Equal(opened, ropError(2, 1, PropertyNotFound)) || c.errors != 1 || !reflect.DeepEqual(before, f.handles) {
		t.Fatal("namespace registration granted object access")
	}
	// A local unassigned counter also converts without allocating an object.
	copy(long[:16], s.replica[:])
	out, c = f.run(conversionOperation(0x44, long))
	if c.errors != 0 || !bytes.Equal(out[6:], []byte{1, 0, 1, 2, 3, 4, 5, 6}) {
		t.Fatal("unassigned local counter rejected")
	}
	if s.next != next || len(s.byFID) != folders || len(s.byMID) != messages {
		t.Fatal("conversion allocated an object or change number")
	}
	// Replacing the entire client context keeps the replica mapping in the
	// account's store, independently of handle lifetime.
	f.state.mailbox = newMailboxObjects(f.transport.stores)
	f.handles = []uint32{0xffffffff, 0xffffffff}
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	reverse, c = f.run(conversionOperation(0x43, want[6:]))
	if c.errors != 0 || !bytes.Equal(reverse[6:], canonical) {
		t.Fatal("reconnect lost foreign replica namespace")
	}
}

func TestReplicaConversionAcceptsMessageAndChangeCounters(t *testing.T) {
	f, _ := messageFixture(t)
	row := messageObserve(t, f, Inbox).messages[0]
	f.store.calendarMetadata = true
	objects := newMailboxObjects(f.stores)
	handle, err := objects.addLogon(4, f.binding)
	if err != nil {
		t.Fatal("fixture Logon handle")
	}
	for _, id := range []uint64{row.mid, folderID(row.cn), folderID(row.readCN)} {
		op := Operation{ID: 0x43, Logon: 4, Input: 0, Folder: id}
		out := convertFolderID(context.Background(), f.session, objects, op, []uint32{handle})
		if len(out) != 30 || binary.LittleEndian.Uint32(out[2:]) != 0 {
			t.Fatal("message/change counter treated as a Folder lookup")
		}
		op.ID = 0x44
		copy(op.LongTermID[:], out[6:])
		out = convertFolderID(context.Background(), f.session, objects, op, []uint32{handle})
		if len(out) != 14 || binary.LittleEndian.Uint64(out[6:]) != id {
			t.Fatal("message/change counter round trip")
		}
	}
}

func TestReplicaConversionZeroAndUnknownCases(t *testing.T) {
	f, _ := newCalendarFixture(t)
	s := replicaStore(t, f)
	long := make([]byte, 24)
	long[16] = 1
	out, _ := f.run(conversionOperation(0x44, long))
	if !bytes.Equal(out, ropError(0x44, 0, 0x80070057)) {
		t.Fatal("zero GUID accepted")
	}
	long[0], long[16] = 99, 0
	out, _ = f.run(conversionOperation(0x44, long))
	if !bytes.Equal(out, append(ropError(0x44, 0, 0), make([]byte, 8)...)) || len(s.foreignReplicas) != 0 || s.foreignReplicaLease != nil {
		t.Fatal("zero counter must return zero ObjectId without registration")
	}
	for _, id := range []uint64{0, 2, 0xffff} {
		out, _ = f.run(conversionOperation(0x43, binary.LittleEndian.AppendUint64(nil, id)))
		if !bytes.Equal(out, ropError(0x43, 0, PropertyNotFound)) {
			t.Fatal("unknown REPLID accepted")
		}
	}
	out, _ = f.run(conversionOperation(0x43, binary.LittleEndian.AppendUint64(nil, 1)))
	if len(out) != 30 || !bytes.Equal(out[6:22], s.replica[:]) || !bytes.Equal(out[22:], make([]byte, 8)) {
		t.Fatal("known REPLID zero counter incorrectly looked up")
	}
}

func TestReplicaRegistryQuotaBudgetCancellationAndScope(t *testing.T) {
	stores, binding, session := ramFixture(t)
	s := binding.store
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, code := s.replicaID(ctx, [16]byte{99}); code == 0 || len(s.foreignReplicas) != 0 || s.foreignReplicaLease != nil {
		t.Fatal("cancelled request changed registry")
	}
	if _, code := s.replicaGUID(ctx, 1); code == 0 {
		t.Fatal("cancelled inverse request succeeded")
	}
	lease, err := s.reserveContentsMemory(maxStoreContentsMemory)
	if err != nil {
		t.Fatal("exhaustion fixture")
	}
	if _, code := s.replicaID(context.Background(), [16]byte{99}); code != 0x8007000e || s.foreignReplicaLease != nil || len(s.foreignReplicas) != 0 {
		t.Fatal("budget refusal partially registered GUID")
	}
	lease.release()
	for i := range maxForeignReplicas {
		guid := [16]byte{99}
		binary.LittleEndian.PutUint32(guid[1:], uint32(i))
		id, code := s.replicaID(context.Background(), guid)
		if code != 0 || id != uint16(i+2) {
			t.Fatal("unique REPLID allocation")
		}
		got, code := s.replicaGUID(context.Background(), id)
		if code != 0 || got != guid {
			t.Fatal("registered GUID inverse")
		}
	}
	if _, code := s.replicaID(context.Background(), [16]byte{100}); code != parameterOverflow || len(s.foreignReplicas) != maxForeignReplicas || s.foreignReplicaLease.size() != foreignReplicaRegistryBytes || stores.contents.used != foreignReplicaRegistryBytes {
		t.Fatal("quota or permanent shared memory accounting")
	}
	for guid, want := range map[[16]byte]uint16{s.replica: 1, {99}: 2} {
		if id, code := s.replicaID(context.Background(), guid); code != 0 || id != want {
			t.Fatal("full registry refuses known mapping")
		}
	}
	catalog, _ := privateFolders(context.Background(), session)
	catalog.scope.Account = "other"
	other, err := stores.bind(context.Background(), catalog)
	if err != nil {
		t.Fatal("other account fixture")
	}
	if _, code := other.store.replicaGUID(context.Background(), 2); code != PropertyNotFound {
		t.Fatal("foreign registry leaked across accounts")
	}
}

func TestReplicaRegistryConcurrentStableUniqueAllocation(t *testing.T) {
	_, binding, _ := ramFixture(t)
	s := binding.store
	var wg sync.WaitGroup
	ids := make([]uint16, 64)
	codes := make([]uint32, 64)
	for i := range ids {
		wg.Go(func() { ids[i], codes[i] = s.replicaID(context.Background(), [16]byte{byte(10 + i%8)}) })
	}
	wg.Wait()
	seen := map[uint16]byte{}
	for i, id := range ids {
		guid := byte(10 + i%8)
		if codes[i] != 0 || id < 2 || seen[id] != 0 && seen[id] != guid {
			t.Fatal("concurrent registry collision")
		}
		seen[id] = guid
		got, code := s.replicaID(context.Background(), [16]byte{guid})
		if code != 0 || got != id {
			t.Fatal("concurrent duplicate GUID registration")
		}
	}
	if len(seen) != 8 || len(s.foreignReplicas) != 8 || s.foreignReplicaLease.size() != foreignReplicaRegistryBytes {
		t.Fatal("duplicate reservation or replica row")
	}
}

func TestReplicaConversionPreflightAndAuthorizationAreAtomic(t *testing.T) {
	for _, mode := range []string{"output", "malformed", "owner", "scope", "root"} {
		t.Run(mode, func(t *testing.T) {
			f, b := newCalendarFixture(t)
			s := replicaStore(t, f)
			before := append([]uint32(nil), f.handles...)
			long := make([]byte, 24)
			long[0], long[16] = 99, 1
			ops, maximum := conversionOperation(0x44, long), uint32(65535)
			switch mode {
			case "output":
				maximum = 24
			case "malformed":
				ops = append(ops, 0xff)
			case "owner":
				b.state.Owner = false
			case "scope":
				identity := f.session.Identity()
				identity.Scope.Account = "other"
				f.session = scopedSession{Session: f.session, identity: identity}
			case "root":
				b.getErr = core.ErrAccessDenied
			}
			_, c := f.execute(ops, maximum)
			if len(s.foreignReplicas) != 0 || s.foreignReplicaLease != nil || !reflect.DeepEqual(f.handles, before) || mode != "output" && mode != "malformed" && c.errors == 0 {
				t.Fatal("preflight/access refusal changed replica registry")
			}
		})
	}
}

type foreignReplicaTestSession struct{ core.Session }

func (foreignReplicaTestSession) Identity() core.Identity {
	return core.Identity{Scope: core.Scope{Backend: "fixture", Account: "another-mailbox"}}
}

func TestReplicaConversionStoresOnlyTranslationInAuthorizedMailbox(t *testing.T) {
	f, _ := newCalendarFixture(t)
	open := sqliteExecuteFixture(t, f)
	db := open()
	s := replicaStore(t, f)
	ctx := t.Context()
	before, err := db.LoadMAPIReplica(ctx, s.durable.mailbox)
	if err != nil {
		t.Fatal(err)
	}
	var long [24]byte
	long[0] = 7
	copy(long[16:22], binary.LittleEndian.AppendUint64(nil, f.fids[Inbox])[2:])
	op := Operation{ID: 0x44, Logon: 4, Input: 0, LongTermID: long}
	out := convertFolderID(ctx, foreignReplicaTestSession{f.session}, f.state.mailbox, op, f.handles)
	if !bytes.Equal(out, ropError(0x44, 0, accessDenied)) {
		t.Fatalf("foreign mailbox conversion result %x", out)
	}
	after, err := db.LoadMAPIReplica(ctx, s.durable.mailbox)
	if err != nil || !reflect.DeepEqual(after, before) {
		t.Fatalf("denied request changed durable state: %v", err)
	}
	wire := conversionOperation(0x44, long[:])
	reply := faiRunOK(t, f, wire)
	if len(reply) != 14 || binary.LittleEndian.Uint16(reply[6:8]) != 2 {
		t.Fatalf("foreign translation result %x", reply)
	}
	id := binary.LittleEndian.Uint64(reply[6:])
	if out, _ := f.run(folderOperation(id, 4, 0, 1)); !bytes.Equal(out, ropError(2, 1, PropertyNotFound)) {
		t.Fatalf("foreign translation resolved a live local folder: %x", out)
	}
	after, err = db.LoadMAPIReplica(ctx, s.durable.mailbox)
	if err != nil || after.Version.Epoch != before.Version.Epoch ||
		after.Version.HighWater != before.Version.HighWater+1 || !reflect.DeepEqual(after.Bindings, before.Bindings) ||
		len(after.ForeignReplicas) != 1 || after.ForeignReplicas[0] != ([16]byte{7}) {
		t.Fatalf("translation did not isolate replica and object tables: %v", err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	open()
	if repeated := faiRunOK(t, f, wire); !bytes.Equal(repeated, reply) {
		t.Fatal("foreign REPLID changed after restart")
	}
}
