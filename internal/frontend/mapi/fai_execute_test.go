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
	"unicode/utf16"

	"ewsgo/internal/core"
)

func faiSetOperation(input byte, values faiSetRequest) []byte {
	var payload []byte
	for _, p := range values {
		payload = binary.LittleEndian.AppendUint32(payload, p.tag)
		switch v := p.value.(type) {
		case core.Integer:
			payload = binary.LittleEndian.AppendUint32(payload, uint32(v))
		case core.Text:
			for _, ch := range utf16.Encode([]rune(v)) {
				payload = binary.LittleEndian.AppendUint16(payload, ch)
			}
			payload = append(payload, 0, 0)
		case core.Binary:
			payload = binary.LittleEndian.AppendUint16(payload, uint16(len(v)))
			payload = append(payload, v...)
		case core.Timestamp:
			payload = binary.LittleEndian.AppendUint64(payload, fileTime(time.Time(v)))
		case core.Bool:
			payload = append(payload, byte(boolInt(bool(v))))
		}
	}
	out := binary.LittleEndian.AppendUint16([]byte{0x0a, 4, input}, uint16(len(payload)+2))
	out = binary.LittleEndian.AppendUint16(out, uint16(len(values)))
	return append(out, payload...)
}
func faiCreateProperties() faiSetRequest {
	return faiSetRequest{{0x001a001f, core.Text("IPM.Microsoft.FolderDesign.NamedView")}, {0x30070040, core.Timestamp(hierarchyTestTime.Add(-time.Hour))}, {0x7006001f, core.Text("Synthetic view")}, {0x70010102, core.Binary{1, 2, 3}}}
}
func faiImportOperation(r faiImportRequest) []byte {
	return append([]byte{0x72, 4, 2}, faiImportWire(r)...)
}
func faiDeleteOperation(sources ...[22]byte) []byte {
	out := binary.LittleEndian.AppendUint32([]byte{0x74, 4, 2, 2, 1, 0}, 0x00001102)
	out = binary.LittleEndian.AppendUint32(out, uint32(len(sources)))
	for _, s := range sources {
		out = binary.LittleEndian.AppendUint16(out, 22)
		out = append(out, s[:]...)
	}
	return out
}
func faiRunOK(t *testing.T, f *ostFixture, ops []byte) []byte {
	t.Helper()
	raw, counts := f.run(ops)
	if len(raw) == 0 && counts.errors == 0 && ops[0] == 1 {
		return raw
	}
	if counts.errors != 0 || len(raw) < 6 || binary.LittleEndian.Uint32(raw[2:]) != 0 {
		t.Fatal("FAI ROP failed")
	}
	return raw
}
func newFAIExecuteFixture(t *testing.T) (*ostFixture, *messageFixtureMailbox, *ramStore) {
	t.Helper()
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableFAICollectorSpike()
	f.handles = append(f.handles, 0xffffffff)
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	faiRunOK(t, f, []byte{0x7e, 4, 1, 2, 1})
	return f, box, object.binding.store
}

func TestFAIImportDoesNotConsumeATransferSlot(t *testing.T) {
	f, _, _ := newFAIExecuteFixture(t)
	// Keep the uploader and three other contexts live while staging its message.
	for n := 1; n < maxContextTransfers; n++ {
		slot := byte(len(f.handles))
		f.handles = append(f.handles, 0xffffffff)
		faiRunOK(t, f, []byte{0x7e, 4, 1, slot, 1})
	}
	budget := f.transport.stores.contents.used
	faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
	faiRunOK(t, f, faiSetOperation(3, faiCreateProperties()))
	if f.transport.stores.transfers.Load() != maxContextTransfers || f.transport.stores.contents.used != budget+maxFAIObjectBytes {
		t.Fatal("message staging used the transfer limit or bypassed its memory budget")
	}
	faiRunOK(t, f, []byte{1, 4, 3})
	if f.transport.stores.transfers.Load() != maxContextTransfers || f.transport.stores.contents.used != budget {
		t.Fatal("releasing a staged message changed transfer accounting or leaked memory")
	}
	f.state.mailbox.close()
	if f.transport.stores.transfers.Load() != 0 || f.transport.stores.contents.used != 0 {
		t.Fatal("closing context did not release all transfers")
	}
}

func collectorThreeTestSets(t *testing.T, raw []byte, given idSet) contentsSyncState {
	t.Helper()
	state := contentsSyncState{given: cloneIDSet(given)}
	sets := map[uint32]*idSet{cnsetSeenTag: &state.seen, cnsetSeenFAITag: &state.seenFAI, cnsetReadTag: &state.read}
	counts := map[uint32]int{}
	for _, token := range decodeTestContents(t, raw) {
		if token.tag == idsetGivenTag {
			t.Fatal("collector emitted Given")
		}
		if target := sets[token.tag]; target != nil {
			set, err := parseIDSet(token.value.([]byte))
			if err != nil {
				t.Fatal("collector state malformed")
			}
			*target = set
			counts[token.tag]++
		}
	}
	for tag := range sets {
		if counts[tag] != 1 {
			t.Fatal("collector state missing/duplicate")
		}
	}
	return state
}
func TestFAIExecuteCompleteCreateReadUpdateDeleteAndReconnect(t *testing.T) {
	f, box, store := newFAIExecuteFixture(t)
	request := syntheticFAIImport()
	imported := faiRunOK(t, f, faiImportOperation(request))
	if len(imported) != 14 || binary.LittleEndian.Uint64(imported[6:]) != 0 || imported[1] != 3 {
		t.Fatal("import response MID must be zero")
	}
	faiRunOK(t, f, faiSetOperation(3, faiCreateProperties()))
	if len(store.fai.records) != 0 {
		t.Fatal("Set persisted before Save")
	}
	saved := faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 2})
	if len(saved) != 15 || saved[1] != 4 || saved[6] != 3 || binary.LittleEndian.Uint64(saved[7:]) == 0 {
		t.Fatal("save response handle indexes/MID")
	}
	mid := binary.LittleEndian.Uint64(saved[7:])
	set := faiSetOperation(3, faiSetRequest{{0x7006001f, core.Text("Changed view")}})
	set[0] = 0x79
	if got := faiRunOK(t, f, set); !bytes.Equal(got, propertyProblemsResponse(0x79, 3, nil)) {
		t.Fatalf("SetPropertiesNoReplicate returned %x", got)
	}
	saved = faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 0})
	if binary.LittleEndian.Uint64(saved[7:]) != mid {
		t.Fatal("local save changed MID")
	}
	// Export the collector state with no Given. Incomplete streams publish nothing.
	faiRunOK(t, f, []byte{0x82, 4, 2, 4})
	object, _ := f.state.mailbox.object(f.handles[4], 4)
	stream := object.download.contents.transfer.stream
	if object.download.contents.transfer.lease.size() != len(stream.data)+16*len(stream.atoms) {
		t.Fatal("collector state retained the maximum encoding reservation")
	}
	permit := object.download.contents.permit
	if permit == nil || permit.ready {
		t.Fatal("state grant prematurely published")
	}
	state := collectorThreeTestSets(t, drainFX(t, f.run, 4, 31), idSet{})
	if !permit.ready {
		t.Fatal("complete state source did not publish")
	}
	faiRunOK(t, f, []byte{1, 4, 4, 1, 4, 2, 0x7e, 4, 1, 2, 1})
	// Upload download state to a fresh collector, then delete only its local FAI.
	uploadContentsWireTest(t, f, state)
	faiRunOK(t, f, faiDeleteOperation(request.source))
	faiRunOK(t, f, []byte{1, 4, 2}) // No final state: mutation exists but no new grant published.
	// Reconnect retains the FAI tombstone and all process-epoch identities.
	f.state.mailbox.close()
	f.state.mailbox = newMailboxObjects(f.transport.stores)
	f.handles = []uint32{0xffffffff, 0xffffffff, 0xffffffff, 0xffffffff, 0xffffffff}
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	f.open(int(Inbox))
	config := fullContentsConfig()
	configureContentsTest(t, f, config)
	uploadContentsWireTest(t, f, state)
	// The imported object was never in Given, so its deletion produces no row.
	raw := drainFX(t, f.run, 2, 89)
	for _, token := range decodeTestContents(t, raw) {
		if token.tag == 0x65e00102 && bytes.Equal(token.value.([]byte), request.source[:]) {
			t.Fatal("deleted FAI returned")
		}
	}
	if box.sourceCalls != 1 {
		t.Fatal("collector read native messages")
	}
	f.state.mailbox.close()
	if f.transport.stores.transfers.Load() != 0 || f.transport.stores.contents.used != store.fai.bytes+faiStatePermitMemory+contentsProofMemory {
		t.Fatal("FAI context cleanup leaked")
	}
}
func TestFAIExecuteSaveReadViaContentsAndDelta(t *testing.T) {
	f, box, store := newFAIExecuteFixture(t)
	request := syntheticFAIImport()
	faiRunOK(t, f, faiImportOperation(request))
	faiRunOK(t, f, faiSetOperation(3, faiCreateProperties()))
	faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 0})
	faiRunOK(t, f, []byte{1, 4, 2})
	config := fullContentsConfig()
	configureContentsTest(t, f, config)
	firstRaw := drainFX(t, f.run, 2, 97)
	first := contentsFourTestSets(t, firstRaw)
	matches := 0
	for _, token := range decodeTestContents(t, firstRaw) {
		if token.tag == 0x7006001f && token.value == "Synthetic view" {
			matches++
		}
	}
	if matches != 1 {
		t.Fatal("FAI properties not reread through Contents")
	}
	faiRunOK(t, f, []byte{1, 4, 2, 0x7e, 4, 1, 2, 1})
	uploadContentsWireTest(t, f, first)
	newer := request
	newer.change = gidChangeXID(faiTestXID(1, 4))
	newer.predecessors = faiPCL{[16]byte{1}: testXIDLocal([6]byte{0, 0, 0, 0, 0, 4})}
	newer.modified = request.modified.Add(time.Minute)
	faiRunOK(t, f, faiImportOperation(newer))
	values := faiCreateProperties()
	values[2].value = core.Text("Imported change")
	faiRunOK(t, f, faiSetOperation(3, values))
	faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 0})
	faiRunOK(t, f, []byte{0x82, 4, 2, 4})
	state := collectorThreeTestSets(t, drainFX(t, f.run, 4, 43), first.given)
	faiRunOK(t, f, []byte{1, 4, 4, 1, 4, 2})
	configureContentsTest(t, f, config)
	uploadContentsWireTest(t, f, state)
	raw := drainFX(t, f.run, 2, 47)
	for _, token := range decodeTestContents(t, raw) {
		if token.tag == 0x65e00102 {
			t.Fatal("already imported change resent despite retained Given")
		}
	}
	if !reflect.DeepEqual(contentsFourTestSets(t, raw), state) || len(store.fai.records) != 1 || box.sourceCalls != 2 {
		t.Fatal("collector/download reconnect state")
	}
}
func TestFAIExecuteMalformedNormalOutputAndReleaseBeforeSave(t *testing.T) {
	f, _, store := newFAIExecuteFixture(t)
	r := syntheticFAIImport()
	before := f.transport.stores.contents.used
	allocated := f.state.mailbox.allocated
	// Truncated normal framing must prevent the valid import prefix as well.
	normal := r
	normal.flags = 0
	truncated := faiImportOperation(normal)
	ops := append(faiImportOperation(r), truncated[:len(truncated)-1]...)
	body, _ := f.execute(ops, 65535)
	if !bytes.Equal(body, executeError(0x80070057, 65535)) || f.transport.stores.contents.used != before || f.state.mailbox.allocated != allocated {
		t.Fatal("invalid batch executed prefix")
	}
	body, _ = f.execute(faiImportOperation(r), 32)
	if len(body) == 0 || f.transport.stores.contents.used != before || f.state.mailbox.allocated != allocated {
		t.Fatal("short output executed import")
	}
	faiRunOK(t, f, faiImportOperation(r))
	faiRunOK(t, f, faiSetOperation(3, faiCreateProperties()))
	body, _ = f.execute([]byte{0x0c, 4, 4, 3, 0}, 32)
	if len(body) == 0 || len(store.fai.records) != 0 {
		t.Fatal("short output saved draft")
	}
	faiRunOK(t, f, []byte{1, 4, 2})
	response, counts := f.run([]byte{0x0c, 4, 4, 3, 0})
	if counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) == 0 || len(store.fai.records) != 0 {
		t.Fatal("released collector allowed save")
	}
	f.state.mailbox.close()
	if f.transport.stores.contents.used != 0 || f.transport.stores.transfers.Load() != 0 {
		t.Fatal("aborted draft leaked")
	}
}
func TestFAIExecuteTypedHandlesNativeDeletionAndDisabledFlag(t *testing.T) {
	f, box, store := newFAIExecuteFixture(t)
	wrongImport := faiImportOperation(syntheticFAIImport())
	wrongImport[2] = 1
	for _, op := range [][]byte{{0x7e, 4, 2, 3, 1}, wrongImport, {0x0c, 4, 4, 1, 0}, folderPropertiesOperation(4, 2, 1, 0x3001001f)} {
		response, counts := f.run(op)
		if counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) == 0 {
			t.Fatal("wrong handle kind accepted")
		}
	}
	// A native message key can never authorize a FAI deletion.
	snapshot, err := store.observeMailMessages(context.Background(), f.session, f.fids[Inbox])
	if err != nil {
		t.Fatal(err)
	}
	response, counts := f.run(faiDeleteOperation(snapshot.messages[0].sourceKey))
	if counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) != accessDenied || len(store.fai.records) != 0 {
		t.Fatal("native key accepted by FAI delete")
	}
	f.transport.faiCollectorSpike = false
	before := f.state.mailbox.allocated
	body, _ := f.execute([]byte{0x7e, 4, 1, 3, 1}, 65535)
	if binary.LittleEndian.Uint32(body[4:]) != NotSupported || f.state.mailbox.allocated != before {
		t.Fatal("collector enabled without explicit flag")
	}
	if box.sourceCalls != 1 {
		t.Fatal("wrong-kind requests read native source")
	}
}

func TestFAIExecuteCommitBatchPreflightAndStateSourceAbort(t *testing.T) {
	f, _, store := newFAIExecuteFixture(t)
	r := syntheticFAIImport()
	faiRunOK(t, f, faiImportOperation(r))
	faiRunOK(t, f, faiSetOperation(3, faiCreateProperties()))
	before := f.transport.stores.contents.used
	forbidden := append([]byte{0x0c, 4, 4, 3, 0}, faiDeleteOperation(faiTestXID(99, 1))...)
	body, _ := f.execute(forbidden, 65535)
	if binary.LittleEndian.Uint32(body[4:]) != NotSupported || len(store.fai.records) != 0 || f.transport.stores.contents.used != before {
		t.Fatal("commit prefix before forbidden delete")
	}
	// A malformed Set prefix is also rejected before any Save or staging change.
	broken := append(faiSetOperation(3, faiCreateProperties()), 0x72, 4, 2)
	body, _ = f.execute(broken, 65535)
	if binary.LittleEndian.Uint32(body[4:]) == 0 || len(store.fai.records) != 0 {
		t.Fatal("malformed suffix accepted")
	}
	faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 0})
	faiRunOK(t, f, []byte{0x82, 4, 2, 4})
	source, _ := f.state.mailbox.object(f.handles[4], 4)
	permit := source.download.contents.permit
	if permit == nil {
		t.Fatal("missing pending permit")
	}
	// A tiny buffer cannot publish; releasing the source cancels its grant.
	if data, status := getFXBuffer(t, f.run, 4, 1); len(data) != 0 || status != 2 || permit.ready {
		t.Fatal("short buffer published")
	}
	faiRunOK(t, f, []byte{1, 4, 4})
	if len(store.faiStatePermits) != 0 {
		t.Fatal("aborted state source retained unpublished grant")
	}
	faiRunOK(t, f, []byte{0x82, 4, 2, 4})
	state := collectorThreeTestSets(t, drainFX(t, f.run, 4, 23), idSet{})
	if len(state.seenFAI.replicas) != 1 {
		t.Fatal("repeat state export lost saved changes")
	}
}
func TestFAIExecuteRevocationQuotaAndStaleSave(t *testing.T) {
	f, _, store := newFAIExecuteFixture(t)
	r := syntheticFAIImport()
	faiRunOK(t, f, faiImportOperation(r))
	faiRunOK(t, f, faiSetOperation(3, faiCreateProperties()))
	f.box.state.Owner = false
	response, counts := f.run([]byte{0x0c, 4, 4, 3, 0})
	if counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) != accessDenied || len(store.fai.records) != 0 {
		t.Fatal("revoked owner saved")
	}
	f.box.state.Owner = true
	lease, err := store.reserveContentsMemory(maxStoreContentsMemory - f.transport.stores.contents.used)
	if err != nil {
		t.Fatal(err)
	}
	response, counts = f.run([]byte{0x0c, 4, 4, 3, 0})
	if counts.errors != 1 || len(store.fai.records) != 0 || len(store.faiStatePermits) != 0 {
		t.Fatal("quota failure changed archive/state")
	}
	lease.release()
	faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 0})
	// A newer import can stage twice, but only the first non-force Save wins.
	r.change = gidChangeXID(faiTestXID(1, 4))
	r.predecessors = faiPCL{[16]byte{1}: testXIDLocal([6]byte{0, 0, 0, 0, 0, 4})}
	r.modified = r.modified.Add(time.Minute)
	faiRunOK(t, f, faiImportOperation(r))
	faiRunOK(t, f, faiSetOperation(3, faiCreateProperties()))
	r.output = 4
	faiRunOK(t, f, faiImportOperation(r))
	faiRunOK(t, f, faiSetOperation(4, faiCreateProperties()))
	faiRunOK(t, f, []byte{0x0c, 4, 3, 3, 0})
	response, counts = f.run([]byte{0x0c, 4, 3, 4, 0})
	if counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) != 0x80040109 {
		t.Fatal("stale Save did not report ObjectChanged")
	}
}

func FuzzFAIOperationalBatch(f *testing.F) {
	f.Add(saveReadTestOperations(1))
	f.Add([]byte{0x7e, 4, 1, 2, 1})
	f.Add(faiImportOperation(syntheticFAIImport()))
	f.Add(faiSetOperation(3, faiCreateProperties()))
	f.Add([]byte{0x0c, 4, 4, 3, 0})
	f.Add(faiDeleteOperation(faiTestXID(1, 1)))
	f.Add([]byte{ropReloadCachedInformation, 4, 3}) // Both reserved bytes are missing.
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 60000 {
			return
		}
		b := ROPBuffer{Operations: raw, Handles: []uint32{1, 2, 3, 4, 5}}
		ops, err := b.decodeFAIOperations(true, true, true, true, true, true, true, true)
		if err != nil {
			if ops != nil {
				t.Fatal("partial decoded batch returned")
			}
			return
		}
		var committed byte
		for _, op := range ops {
			if committed != 0 && op.ID != 1 && !(committed == 0x0c && op.ID == 0x07) {
				t.Fatal("operation after archive commit")
			}
			if op.ID == 0x72 && (op.FAIImport == nil || op.FAIImport.flags&0x10 == 0) {
				t.Fatal("Normal import accepted")
			}
			if op.ID == 0x74 || op.ID == 0x0c {
				committed = op.ID
			}
		}
	})
}

func TestFAIExecuteCalendarLocalConfigurationWithFilterAndNoEventPort(t *testing.T) {
	f, _ := newCalendarFixture(t)
	f.transport.EnableHierarchyDownloadSpike()
	f.transport.EnableContentsDownloadSpike()
	f.transport.EnableFAICollectorSpike()
	f.handles = append(f.handles, 0xffffffff, 0xffffffff, 0xffffffff)
	entry := calendarEntry(t, f)
	resolved, _ := f.run(conversionOperation(0x44, entry[22:]))
	fid := binary.LittleEndian.Uint64(resolved[6:])
	f.run(folderOperation(fid, 4, 0, 1))
	faiRunOK(t, f, []byte{0x7e, 4, 1, 2, 1})
	r := syntheticFAIImport()
	faiRunOK(t, f, faiImportOperation(r))
	properties := faiCreateProperties()
	properties[0].value = core.Text("IPM.Configuration.WorkHours")
	faiRunOK(t, f, faiSetOperation(3, properties))
	faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 0})
	faiRunOK(t, f, []byte{1, 4, 2})
	c := fullContentsConfig()
	c.flags &^= 0x20 // FAI + ReadState, without Normal.
	restriction := binary.LittleEndian.AppendUint32([]byte{4, 4}, 0x001a001f)
	restriction = binary.LittleEndian.AppendUint32(restriction, 0x001a001f)
	for _, ch := range utf16.Encode([]rune("IPM.Configuration.WorkHours")) {
		restriction = binary.LittleEndian.AppendUint16(restriction, ch)
	}
	restriction = append(restriction, 0, 0)
	faiRunOK(t, f, contentsConfigureOperation(c, restriction))
	raw := drainFX(t, f.run, 2, 79)
	matches := 0
	for _, token := range decodeTestContents(t, raw) {
		if token.tag == 0x001a001f && token.value == "IPM.Configuration.WorkHours" {
			matches++
		}
	}
	if matches != 1 {
		t.Fatal("Calendar local FAI class restriction not applied")
	}
	faiRunOK(t, f, []byte{1, 4, 2})
	c.flags |= 0x20
	response, counts := f.run(contentsConfigureOperation(c, nil))
	if counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) != NotSupported {
		t.Fatal("Calendar native event Contents enabled")
	}
	f.state.mailbox.close()
}
