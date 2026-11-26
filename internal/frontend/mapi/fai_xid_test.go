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
	"strings"
	"testing"

	"ewsgo/internal/core"
)

func variableFAIImport(width int) faiImportRequest {
	r := syntheticFAIImport()
	raw := make([]byte, 16+width)
	raw[0], raw[len(raw)-1] = 2, 3 // Different from the SourceKey GID namespace.
	r.change = changeXID(raw)
	r.predecessors = faiPCL{r.change.namespace(): r.change.local()}
	return r
}
func TestFAIXIDEveryWidthCodecAndOwnership(t *testing.T) {
	for width := 1; width <= 8; width++ {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			request := variableFAIImport(width)
			wire := faiImportWire(request)
			for n := 0; n < len(wire); n++ {
				if _, err := parseFAIImport(&reader{data: wire[:n]}, 4); !errors.Is(err, ErrMalformed) {
					t.Fatal("truncation accepted or misclassified", n)
				}
			}
			got, err := parseFAIImport(&reader{data: wire}, 4)
			if err != nil || !reflect.DeepEqual(request, got) {
				t.Fatal("variable XID roundtrip")
			}
			clear(wire)
			if !reflect.DeepEqual(request, got) {
				t.Fatal("XID retained caller buffer")
			}
			clone := cloneFAISync(&faiSyncVersion{source: got.source, change: got.change, predecessors: got.predecessors})
			got.predecessors[got.change.namespace()] = xidLocal("\x7f")
			if !reflect.DeepEqual(clone.predecessors, request.predecessors) {
				t.Fatal("PCL alias")
			}
			for _, value := range []any{request.change, request.change.local()} {
				if strings.Contains(fmt.Sprintf("%v %+v %#v %s %x", value, value, value, value, value), string(request.change.local())) {
					t.Fatal("identifier formatting leaked")
				}
			}
		})
	}
	for _, size := range []int{0, 1, 16, 25, 32} {
		r := variableFAIImport(8)
		wire := importDiagnosticWire(r.source[:], make([]byte, size), r.predecessors.marshal())
		if _, err := parseFAIImport(&reader{data: wire}, 4); !errors.Is(err, ErrUnsupported) {
			t.Fatal("out-of-range XID accepted")
		}
	}
	// GIDs still require their six-byte local part, even with variable ChangeKey.
	r := variableFAIImport(8)
	for _, size := range []int{17, 21, 23, 24} {
		wire := importDiagnosticWire(make([]byte, size), []byte(r.change), r.predecessors.marshal())
		if _, err := parseFAIImport(&reader{data: wire}, 4); !errors.Is(err, ErrUnsupported) {
			t.Fatal("non-GID source accepted")
		}
	}
}
func TestFAIXIDPCLWidthOrderingAndLimits(t *testing.T) {
	for width := 1; width <= 8; width++ {
		r := variableFAIImport(width)
		a := r.predecessors
		larger := testIncrementChangeXID(r.change)
		b := faiPCL{larger.namespace(): larger.local()}
		if a.includes(b) || !b.includes(a) {
			t.Fatal("equal-width ordering")
		}
		merged, err := mergeFAIPCL(a, b)
		if err != nil || !merged.includes(b) {
			t.Fatal("equal-width merge")
		}
		parsed, err := parseFAIPCL(append(a.marshal(), b.marshal()...))
		if err != nil || !reflect.DeepEqual(parsed, b) {
			t.Fatal("duplicate namespace maximum")
		}
	}
	a, b := variableFAIImport(6).predecessors, variableFAIImport(8).predecessors
	if a.includes(b) || b.includes(a) {
		t.Fatal("mixed width considered ancestor")
	}
	if _, err := mergeFAIPCL(a, b); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("mixed width merge")
	}
	if _, err := parseFAIPCL(append(a.marshal(), b.marshal()...)); !errors.Is(err, ErrMalformed) {
		t.Fatal("mixed width wire")
	}
	r := variableFAIImport(8)
	copy(r.source[:16], []byte(r.change[:16]))
	if _, err := parseFAIImport(&reader{data: faiImportWire(r)}, 4); !errors.Is(err, ErrMalformed) {
		t.Fatal("GID and ChangeKey reused namespace at different width")
	}
	pcl := faiPCL{}
	for i := 1; i <= maxFAIPCLReplicas; i++ {
		pcl[[16]byte{byte(i)}] = xidLocal("\x00\x00\x00\x00\x00\x00\x00\x01")
	}
	raw := pcl.marshal()
	if len(raw) != 25*maxFAIPCLReplicas {
		t.Fatal("maximum wire size")
	}
	if got, err := parseFAIPCL(raw); err != nil || !reflect.DeepEqual(got, pcl) {
		t.Fatal("maximum PCL rejected")
	}
	// 65 short entries are below the byte budget: the namespace quota is independent.
	raw = nil
	for i := 1; i <= maxFAIPCLReplicas+1; i++ {
		raw = append(raw, faiPCL{[16]byte{byte(i)}: xidLocal("\x01")}.marshal()...)
	}
	if _, err := parseFAIPCL(raw); !errors.Is(err, ErrUnsupported) {
		t.Fatal("namespace quota bypass")
	}
	for _, raw := range [][]byte{{16}, {25}, make([]byte, 1601), append([]byte{17}, make([]byte, 17)...)} {
		if _, err := parseFAIPCL(raw); err == nil {
			t.Fatal("invalid PCL accepted")
		}
	}
}
func TestFAIXIDExecuteSaveDownloadAndTable(t *testing.T) {
	for width := 1; width <= 8; width++ {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			f, box, store := newFAIExecuteFixture(t)
			request := variableFAIImport(width)
			faiRunOK(t, f, faiImportOperation(request))
			faiRunOK(t, f, faiSetOperation(3, faiCreateProperties()))
			saved := faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 2})
			mid := binary.LittleEndian.Uint64(saved[7:])
			row, err := store.readFAIRowLocked(context.Background(), store.fai.records[store.byMID[mid].id].folder, store.byMID[mid].id)
			if err != nil || row.changeKey != request.change || row.sourceKey != request.source || !bytes.Equal(row.predecessors, request.predecessors.marshal()) {
				t.Fatal("Save truncated imported identifiers")
			}
			// Close the collector and read only the committed FAI through Contents.
			faiRunOK(t, f, []byte{1, 4, 2})
			configureContentsTest(t, f, fullContentsConfig())
			raw := drainFX(t, f.run, 2, 97)
			found := false
			for _, tok := range decodeTestContents(t, raw) {
				if tok.tag == 0x65e20102 && bytes.Equal(tok.value.([]byte), []byte(request.change)) {
					found = true
				}
			}
			if !found {
				t.Fatal("FastTransfer lost variable XID")
			}
			faiRunOK(t, f, []byte{1, 4, 2})
			f.transport.EnableContentsTableSpike()
			openTableTest(t, f, 0x12)
			tags := []uint32{0x65e00102, 0x65e20102, 0x65e30102}
			faiRunOK(t, f, tableColumnsWire(2, tags...))
			rows := decodeTableRows(t, faiRunOK(t, f, tableQueryWire(2, 0, 1, 64)), tags)
			if len(rows) != 1 || !bytes.Equal(rows[0][0].([]byte), request.source[:]) || !bytes.Equal(rows[0][1].([]byte), []byte(request.change)) || !bytes.Equal(rows[0][2].([]byte), request.predecessors.marshal()) {
				t.Fatal("Table lost imported identifiers")
			}
			if box.sourceCalls != 1 {
				t.Fatal("FAI touched native contents")
			} // Only the explicit download above.
			f.state.mailbox.close()
		})
	}
}
func TestFAIXIDWidthAcrossRecordsStagingAndTombstones(t *testing.T) {
	f, _ := messageFixture(t)
	a, b := variableFAIImport(8), variableFAIImport(7)
	b.source[21]++
	da := newFAIDraftForTest(t, f, a)
	db := newFAIDraftForTest(t, f, b) // Both staged before a namespace is committed.
	saveFAIDraftForTest(t, f, da, 0)
	next, used, count := f.store.next, f.stores.contents.used, len(f.store.fai.records)
	if _, err := db.save(context.Background(), f.session, 0); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("concurrent draft changed committed namespace width")
	}
	if f.store.next != next || f.stores.contents.used != used || len(f.store.fai.records) != count {
		t.Fatal("failed Save published changes")
	}
	if _, err := f.store.newFAIDraft(context.Background(), f.session, da.root, b); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("second object changed namespace width")
	}
	if err := f.store.deleteFAI(context.Background(), f.session, da.root, faiDeleteRequest{sources: [][22]byte{a.source}}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.store.newFAIDraft(context.Background(), f.session, da.root, b); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("tombstone forgot namespace width")
	}
	own := variableFAIImport(8)
	raw := []byte(own.change)
	copy(raw[:16], f.store.replica[:])
	own.change = changeXID(raw)
	own.predecessors = faiPCL{own.change.namespace(): own.change.local()}
	if _, err := f.store.newFAIDraft(context.Background(), f.session, da.root, own); !errors.Is(err, core.ErrInvalid) {
		t.Fatal("own replica width changed")
	}
}

func TestFAIXIDLocalEditPreservesForeignHistory(t *testing.T) {
	for width := 1; width <= 8; width++ {
		t.Run(fmt.Sprint(width), func(t *testing.T) {
			f, box := messageFixture(t)
			request := variableFAIImport(width)
			d := newFAIDraftForTest(t, f, request)
			first := saveFAIDraftForTest(t, f, d, 2)
			if _, err := d.set(context.Background(), f.session, faiSetRequest{{0x7006001f, core.Text("Edited local view")}}); err != nil {
				t.Fatal(err)
			}
			latest := saveFAIDraftForTest(t, f, d, 0)
			pcl, err := parseFAIPCL(latest.predecessors)
			if err != nil || len(latest.changeKey) != 22 || latest.changeKey.namespace() != f.store.replica || !pcl.includesXID(request.change) || !pcl.includesXID(latest.changeKey) || latest.mid != first.mid {
				t.Fatal("local edit rewrote foreign history")
			}
			observed, err := f.store.observeFAIMessages(context.Background(), f.session, d.root)
			if err != nil {
				t.Fatal(err)
			}
			defer observed.close()
			if len(observed.snapshot.messages) != 1 || observed.snapshot.messages[0].changeKey != latest.changeKey || !bytes.Equal(observed.snapshot.messages[0].predecessors, latest.predecessors) || box.sourceCalls != 0 {
				t.Fatal("local edit reread changed lineage or read native contents")
			}
		})
	}
}
