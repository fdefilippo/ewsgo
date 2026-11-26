// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func newHierarchyCollectorFixture(t *testing.T) *ostFixture {
	t.Helper()
	f := newOSTFixture(t)
	f.transport.EnableHierarchyDownloadSpike()
	f.transport.EnableFAICollectorSpike()
	f.handles = append(f.handles, 0xffffffff, 0xffffffff)
	faiRunOK(t, f, []byte{0x7e, 4, 1, 2, 0})
	return f
}

func uploadCollectorRaw(t *testing.T, run func([]byte) ([]byte, storeResults), tag uint32, raw []byte) {
	t.Helper()
	ops := [][]byte{binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32([]byte{0x75, 4, 2}, tag), uint32(len(raw)))}
	if len(raw) > 0 {
		ops = append(ops, append(binary.LittleEndian.AppendUint32([]byte{0x76, 4, 2}, uint32(len(raw))), raw...))
	}
	ops = append(ops, []byte{0x77, 4, 2})
	for _, op := range ops {
		response, counts := run(op)
		if counts.errors != 0 || len(response) != 6 || binary.LittleEndian.Uint32(response[2:]) != 0 {
			t.Fatal("collector state upload failed")
		}
	}
}

func hierarchyCollectorSeen(t *testing.T, raw []byte) idSet {
	t.Helper()
	tokens := readTestFX(t, raw)
	if len(tokens) != 3 || tokens[0].tag != 0x403a0003 || tokens[1].tag != cnsetSeenTag || tokens[2].tag != 0x403b0003 {
		t.Fatal("uploader must emit only Seen within state markers")
	}
	seen, err := parseIDSet(tokens[1].value)
	if err != nil {
		t.Fatal("invalid Seen")
	}
	return seen
}

func TestHierarchyCollectorWireStateAndImmutableSource(t *testing.T) {
	f := newHierarchyCollectorFixture(t)
	object, _ := f.state.mailbox.object(f.handles[2], 4)
	store := object.binding.store
	if !object.download.hierarchyCollector || object.download.collector != nil {
		t.Fatal("collector kinds conflated")
	}
	faiRunOK(t, f, []byte{0x82, 4, 2, 3})
	if initial := hierarchyCollectorSeen(t, drainFX(t, f.run, 3, 11)); len(initial.replicas) != 0 {
		t.Fatal("empty uploader invented Seen")
	}
	faiRunOK(t, f, []byte{1, 4, 3})
	// Given is a client download set: ignore even foreign/unparseable payload.
	uploadCollectorRaw(t, f.run, idsetGivenTag, []byte{0xff, 0xaa})
	first := singleReplicaSet(store.replica, []idRange{{1, 1}})
	raw, _ := first.marshal()
	uploadCollectorRaw(t, f.run, cnsetSeenTag, raw)
	faiRunOK(t, f, []byte{0x82, 4, 2, 3})
	second := singleReplicaSet(store.replica, []idRange{{3, 3}})
	raw, _ = second.marshal()
	uploadCollectorRaw(t, f.run, cnsetSeenTag, raw)
	state := hierarchyCollectorSeen(t, drainFX(t, f.run, 3, 11))
	if !state.contains(store.replica, 1) || state.contains(store.replica, 3) {
		t.Fatal("previous source changed after later upload")
	}
	faiRunOK(t, f, []byte{1, 4, 3, 0x82, 4, 2, 3})
	state = hierarchyCollectorSeen(t, drainFX(t, f.run, 3, 11))
	if !state.contains(store.replica, 1) || !state.contains(store.replica, 3) || state.contains(store.replica, 2) {
		t.Fatal("Seen not unioned or invented counters")
	}
	if len(object.download.state.given.replicas) != 0 {
		t.Fatal("Given retained by uploader")
	}
	rawBuffer, counts := f.run(hierarchyBufferOperation(4, 2, 64))
	if counts.errors != 1 || binary.LittleEndian.Uint32(rawBuffer[2:]) != NotSupported {
		t.Fatal("collector used as download")
	}
	if executionBufferKind(f.state.mailbox, Operation{Input: 2, Logon: 4}, f.handles) != 0 {
		t.Fatal("collector classified as hierarchy stream")
	}
	f.state.mailbox.close()
	if f.transport.stores.transfers.Load() != 0 {
		t.Fatal("transfer quota leaked")
	}
}

func TestHierarchyCollectorRevocationPendingAndForeignState(t *testing.T) {
	f := newHierarchyCollectorFixture(t)
	object, _ := f.state.mailbox.object(f.handles[2], 4)
	s := object.download.state
	foreign := singleReplicaSet([16]byte{99}, []idRange{{1, 1}})
	raw, _ := foreign.marshal()
	faiRunOK(t, f, binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32([]byte{0x75, 4, 2}, cnsetSeenTag), uint32(len(raw))))
	response, counts := f.run([]byte{0x82, 4, 2, 3})
	if counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) == 0 {
		t.Fatal("pending upload published state")
	}
	faiRunOK(t, f, append(binary.LittleEndian.AppendUint32([]byte{0x76, 4, 2}, uint32(len(raw))), raw...))
	response, counts = f.run([]byte{0x77, 4, 2})
	if counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) != 0x80040805 || len(s.seen.replicas) != 0 || s.pending == nil {
		t.Fatal("foreign state committed or pending upload lost")
	}
	f.box.state.Owner = false
	response, counts = f.run([]byte{0x82, 4, 2, 3})
	if counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) != accessDenied {
		t.Fatal("revoked owner read state")
	}
	response, counts = f.run([]byte{0x77, 4, 2})
	if counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) != accessDenied {
		t.Fatal("revoked owner committed state")
	}
}

func TestHierarchyCollectorDecoderGuardsAndNoPrefix(t *testing.T) {
	for _, flag := range []byte{0, 1, 255} {
		b := ROPBuffer{Operations: []byte{0x7e, 4, 1, 2, flag}, Handles: []uint32{1, 2, 3}}
		ops, err := b.decodeFAIOperations(false, true, false, false, false, false, false, true)
		if err != nil || len(ops) != 1 || ((ops[0].Flags != 0) != (flag != 0)) {
			t.Fatal("Boolean collector decoding")
		}
	}
	f := newHierarchyCollectorFixture(t)
	before := len(f.state.mailbox.objects)
	batch := append([]byte{1, 4, 1, 0x7e, 4, 1, 3, 0}, []byte{0x73, 4, 3}...)
	body, counts := f.execute(batch, 65535)
	if !bytes.Equal(body, executeError(0x80070057, 65535)) || len(f.state.mailbox.objects) != before {
		t.Fatal("truncated hierarchy import executed prefix")
	}
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
		t.Fatal("Release prefix executed")
	}
	for _, off := range []string{"collector", "hierarchy"} {
		if off == "collector" {
			f.transport.faiCollectorSpike = false
		} else {
			f.transport.faiCollectorSpike = true
			f.transport.hierarchyDownloadSpike = false
		}
		_, counts = f.execute([]byte{1, 4, 1, 0x7e, 4, 1, 3, 0}, 65535)
		if counts.unsupported != "RopSynchronizationOpenCollector" || len(f.state.mailbox.objects) != before {
			t.Fatal("disabled collector had effects")
		}
	}
	f.transport.hierarchyDownloadSpike = true
	_, counts = f.execute([]byte{1, 4, 1, 0x7e, 4, 1, 3, 0}, 1)
	if counts.errors != 0 || len(f.state.mailbox.objects) != before {
		t.Fatal("output preflight mutated handles")
	}
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
		t.Fatal("short output executed prefix")
	}
}

func TestHierarchyImportRefusalDoesNotDiscardAdjacentOperations(t *testing.T) {
	f := newHierarchyCollectorFixture(t)
	f.transport.EnableSynchronizationOptionsTrace()
	collector, _ := f.state.mailbox.object(f.handles[2], 4)
	store := collector.binding.store
	source := hierarchyXID(store.replica, 42)
	change := hierarchyXID(store.replica, 43)

	wire := []byte{0x73, 4, 2, 6, 0}
	wire = appendFAIRopBinary(wire, 0x65e10102, nil)
	wire = appendFAIRopBinary(wire, 0x65e00102, source[:])
	wire = binary.LittleEndian.AppendUint32(wire, 0x30080040)
	wire = binary.LittleEndian.AppendUint64(wire, 134000000000000000)
	wire = appendFAIRopBinary(wire, 0x65e20102, change[:])
	wire = appendFAIRopBinary(wire, 0x65e30102, append([]byte{22}, change[:]...))
	wire = binary.LittleEndian.AppendUint32(wire, 0x3001001f)
	wire = append(wire, 'T', 0, 0, 0)
	wire = binary.LittleEndian.AppendUint16(wire, 1)
	wire = binary.LittleEndian.AppendUint32(wire, 0x3613001e)
	wire = append(wire, []byte("IPF.Note\x00")...)

	ops, err := (ROPBuffer{Operations: wire, Handles: f.handles}).decodeFAIOperations(false, true, false, false, false, false, true, true)
	if err != nil || len(ops) != 1 || ops[0].HierarchyImport == nil {
		t.Fatal("completed hierarchy import request did not decode", err)
	}
	imported := ops[0].HierarchyImport
	if imported.parent != ([22]byte{}) || imported.source != source || imported.change != gidChangeXID(change) ||
		fileTime(time.Time(imported.modified)) != 134000000000000000 || imported.name != "T" ||
		!bytes.Equal(imported.predecessors, append([]byte{22}, change[:]...)) ||
		len(imported.properties) != 1 || imported.properties[0].tag != 0x3613001e ||
		!bytes.Equal(imported.properties[0].wire, []byte("IPF.Note\x00")) {
		t.Fatal("hierarchy import lost source, version or conversion data")
	}

	before := len(store.byFID)
	batch := append([]byte{0x82, 4, 2, 3}, wire...)
	batch = append(batch, 0x82, 4, 2, 3)
	response, counts := f.run(batch)
	want := append(ropError(0x82, 3, 0), ropError(0x73, 2, NotSupported)...)
	want = append(want, ropError(0x82, 3, 0)...)
	if !bytes.Equal(response, want) || counts.unsupported != "" || counts.errors != 1 {
		t.Fatal("import refusal discarded adjacent ROPs or returned a false success")
	}
	stage, executed, firstError, _, _ := counts.execution.Fields()
	if stage != "executed" || !reflect.DeepEqual(executed, []string{"get_transfer_state", "import_hierarchy"}) || firstError != "import_hierarchy:not_supported" {
		t.Fatal("operation-level refusal was not logged")
	}
	if len(store.byFID) != before || len(collector.download.state.seen.replicas) != 0 {
		t.Fatal("refused import changed hierarchy or synchronization state")
	}
}

func TestHierarchyCollectorQuotaAndRejectedMessageImport(t *testing.T) {
	f := newHierarchyCollectorFixture(t)
	f.handles = append(f.handles, 0xffffffff, 0xffffffff)
	faiRunOK(t, f, []byte{0x7e, 4, 1, 3, 0})
	faiRunOK(t, f, []byte{0x7e, 4, 1, 4, 0})
	faiRunOK(t, f, []byte{0x7e, 4, 1, 5, 0})
	response, counts := f.run([]byte{0x7e, 4, 1, 5, 0})
	if counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) != NotSupported {
		t.Fatal("collector bypassed shared quota")
	}
	response, counts = f.run(faiImportOperation(syntheticFAIImport()))
	if counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) != NotSupported {
		t.Fatal("FAI imported through hierarchy context")
	}
	faiRunOK(t, f, []byte{1, 4, 5, 0x7e, 4, 1, 5, 0})
}

func TestHierarchyCollectorSeenQuotaFailureIsAtomic(t *testing.T) {
	f := newHierarchyFixture(t)
	s := newTestHierarchyState(t, f, MailboxRoot)
	// Two individually bounded sparse sets whose union exceeds the range quota.
	f.store.next = 20000
	a, b := []idRange{}, []idRange{}
	for i := uint64(1); i <= maxIDSetRanges; i++ {
		a = append(a, idRange{4 * i, 4 * i})
		b = append(b, idRange{4*i + 2, 4*i + 2})
	}
	upload := func(set idSet) error {
		raw, err := set.marshal()
		if err != nil {
			return err
		}
		for _, op := range []hierarchyUpload{{kind: 0x75, property: cnsetSeenTag, size: uint32(len(raw))}, {kind: 0x76, size: uint32(len(raw)), data: raw}, {kind: 0x77}} {
			if err = s.uploadMode(context.Background(), f.session, op, true); err != nil {
				return err
			}
		}
		return nil
	}
	if err := upload(singleReplicaSet(f.store.replica, a)); err != nil {
		t.Fatal(err)
	}
	before := cloneIDSet(s.seen)
	if err := upload(singleReplicaSet(f.store.replica, b)); !errors.Is(err, ErrUnsupported) || !reflect.DeepEqual(s.seen, before) || s.pending == nil {
		t.Fatal("union exceeded quota or changed committed state")
	}
	// Canceled calls preserve the staged upload as well.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := s.uploadMode(ctx, f.session, hierarchyUpload{kind: 0x77}, true); !errors.Is(err, context.Canceled) || !reflect.DeepEqual(s.seen, before) {
		t.Fatal("canceled upload committed")
	}
	if len(s.given.replicas) != 0 {
		t.Fatal(core.ErrInvalid)
	}
}

func TestHierarchyCollectorUnassignedCountersAndUnsupportedState(t *testing.T) {
	for _, count := range []uint64{0, 1 << 40} {
		f := newHierarchyFixture(t)
		s := newTestHierarchyState(t, f, MailboxRoot)
		raw, err := singleReplicaSet(f.store.replica, []idRange{{count, count}}).marshal()
		if err != nil {
			t.Fatal(err)
		}
		for _, op := range []hierarchyUpload{{kind: 0x75, property: cnsetSeenTag, size: uint32(len(raw))}, {kind: 0x76, size: uint32(len(raw)), data: raw}} {
			if err := s.uploadMode(context.Background(), f.session, op, true); err != nil {
				t.Fatal(err)
			}
		}
		if err := s.uploadMode(context.Background(), f.session, hierarchyUpload{kind: 0x77}, true); !errors.Is(err, core.ErrResync) || len(s.seen.replicas) != 0 {
			t.Fatal("unassigned Seen counter accepted")
		}
	}
	f := newHierarchyFixture(t)
	s := newTestHierarchyState(t, f, MailboxRoot)
	if err := s.uploadMode(context.Background(), f.session, hierarchyUpload{kind: 0x75, property: cnsetSeenFAITag}, true); !errors.Is(err, core.ErrUnsupported) || s.pending != nil {
		t.Fatal("Contents state enabled on hierarchy collector")
	}
}
