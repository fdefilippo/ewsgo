// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"

	"ewsgo/internal/core"
)

func testXIDLocal(v [6]byte) xidLocal { return xidLocal(v[:]) }
func testIncrementChangeXID(x changeXID) changeXID {
	b := []byte(x)
	b[len(b)-1]++
	return changeXID(b)
}
func faiTestXID(replica byte, counter byte) [22]byte {
	var x [22]byte
	x[0] = replica
	x[21] = counter
	return x
}
func faiImportWire(request faiImportRequest) []byte {
	out := []byte{request.output, request.flags, 4, 0}
	out = appendFAIRopBinary(out, 0x65e00102, request.source[:])
	out = binary.LittleEndian.AppendUint32(out, 0x30080040)
	out = binary.LittleEndian.AppendUint64(out, fileTime(request.modified))
	out = appendFAIRopBinary(out, 0x65e20102, []byte(request.change))
	return appendFAIRopBinary(out, 0x65e30102, request.predecessors.marshal())
}
func appendFAIRopBinary(out []byte, tag uint32, value []byte) []byte {
	out = binary.LittleEndian.AppendUint32(out, tag)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(value)))
	return append(out, value...)
}
func syntheticFAIImport() faiImportRequest {
	key := faiTestXID(1, 3)
	var set [16]byte
	copy(set[:], key[:16])
	var local [6]byte
	copy(local[:], key[16:])
	return faiImportRequest{output: 3, flags: 0x10, source: faiTestXID(1, 1), change: gidChangeXID(key), modified: hierarchyTestTime, predecessors: faiPCL{set: testXIDLocal(local)}}
}
func TestFAIImportCodecOrderedHeadersAndTruncation(t *testing.T) {
	request := syntheticFAIImport()
	wire := faiImportWire(request)
	r := reader{data: append(bytes.Clone(wire), 0xff)}
	got, err := parseFAIImport(&r, 4)
	if err != nil || !reflect.DeepEqual(got, request) || r.pos != len(wire) {
		t.Fatal("import framing/header decoding")
	}
	for n := 0; n < len(wire); n++ {
		r := reader{data: wire[:n]}
		if got, err := parseFAIImport(&r, 4); err == nil || !reflect.DeepEqual(got, faiImportRequest{}) {
			t.Fatal("partial import exposed", n)
		}
	}
	for _, flags := range []byte{0, 0x10, 0x40, 0x50} {
		candidate := bytes.Clone(wire)
		candidate[1] = flags
		r := reader{data: candidate}
		got, err := parseFAIImport(&r, 4)
		if err != nil || r.done() != nil || got.flags != flags {
			t.Fatal("documented import flag rejected")
		}
	}
	for _, flags := range []byte{0x11, 0x90} {
		bad := bytes.Clone(wire)
		bad[1] = flags
		r := reader{data: bad}
		if _, err := parseFAIImport(&r, 4); !errors.Is(err, ErrUnsupported) {
			t.Fatal("unknown import flag accepted")
		}
	}
	bad := bytes.Clone(wire)
	bad[4] = 0xe2
	if _, err := parseFAIImport(&reader{data: bad}, 4); !errors.Is(err, ErrMalformed) {
		t.Fatal("headers reordered")
	}
	request.change = testIncrementChangeXID(request.change)
	if _, err := parseFAIImport(&reader{data: faiImportWire(request)}, 4); !errors.Is(err, ErrMalformed) {
		t.Fatal("change absent from PCL accepted")
	}
}
func TestFAIPCLComparisonMergingAndQuotas(t *testing.T) {
	a := syntheticFAIImport().predecessors
	b := faiPCL{[16]byte{2}: testXIDLocal([6]byte{0, 0, 0, 0, 0, 4})}
	merged, err := mergeFAIPCL(a, b)
	if err != nil || a.includes(b) || b.includes(a) || !merged.includes(a) || !merged.includes(b) {
		t.Fatal("PCL ancestry/conflict")
	}
	raw := merged.marshal()
	parsed, err := parseFAIPCL(raw)
	if err != nil || !reflect.DeepEqual(parsed, merged) || !bytes.Equal(raw, parsed.marshal()) {
		t.Fatal("PCL canonical roundtrip")
	}
	older := faiPCL{[16]byte{1}: testXIDLocal([6]byte{0, 0, 0, 0, 0, 2})}
	if !a.includes(older) || older.includes(a) {
		t.Fatal("PCL counter order")
	}
	for _, raw := range [][]byte{nil, {22}, make([]byte, 23), {21, 1}, make([]byte, 25*maxFAIPCLReplicas+1)} {
		if _, err := parseFAIPCL(raw); err == nil {
			t.Fatal("bad PCL accepted")
		}
	}
	for i := 0; i < maxFAIPCLReplicas; i++ {
		a[[16]byte{byte(i + 1)}] = testXIDLocal([6]byte{1})
	}
	if _, err := mergeFAIPCL(a, faiPCL{[16]byte{99}: testXIDLocal([6]byte{1})}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatal("PCL quota bypassed")
	}
}
func TestFAISetCodecAndDeleteSourceList(t *testing.T) {
	payload := binary.LittleEndian.AppendUint32(nil, 0x67aa000b)
	payload = append(payload, 1)
	payload = binary.LittleEndian.AppendUint32(payload, 0x001a001f)
	for _, r := range "IPM.Configuration.WorkHours" {
		payload = binary.LittleEndian.AppendUint16(payload, uint16(r))
	}
	payload = append(payload, 0, 0)
	payload = appendFAIRopBinary(payload, 0x7c080102, []byte{0, 1, 2})
	wire := binary.LittleEndian.AppendUint16(nil, uint16(len(payload)+2))
	wire = binary.LittleEndian.AppendUint16(wire, 3)
	wire = append(wire, payload...)
	values, err := parseFAISet(&reader{data: wire})
	if err != nil || len(values) != 3 || values[0].value != core.Bool(true) || !bytes.Equal(values[2].value.(core.Binary), []byte{0, 1, 2}) {
		t.Fatal("SetProperties scalar framing")
	}
	for n := 0; n < len(wire); n++ {
		if v, err := parseFAISet(&reader{data: wire[:n]}); err == nil || v != nil {
			t.Fatal("partial property set exposed")
		}
	}
	bad := bytes.Clone(wire)
	bad[8] = 0
	if _, err := parseFAISet(&reader{data: bad}); !errors.Is(err, ErrUnsupported) {
		t.Fatal("Associated false accepted")
	}
	source := faiTestXID(2, 1)
	deletes := binary.LittleEndian.AppendUint32([]byte{2, 1, 0}, 0x00001102)
	deletes = binary.LittleEndian.AppendUint32(deletes, 1)
	deletes = binary.LittleEndian.AppendUint16(deletes, 22)
	deletes = append(deletes, source[:]...)
	request, err := parseFAIDeletes(&reader{data: deletes})
	if err != nil || request.flags != 2 || len(request.sources) != 1 || request.sources[0] != source {
		t.Fatal("delete MV_BINARY framing")
	}
	for n := 0; n < len(deletes); n++ {
		if v, err := parseFAIDeletes(&reader{data: deletes[:n]}); err == nil || !reflect.DeepEqual(v, faiDeleteRequest{}) {
			t.Fatal("partial delete exposed")
		}
	}
	deletes[0] = 1
	if _, err := parseFAIDeletes(&reader{data: deletes}); !errors.Is(err, ErrUnsupported) {
		t.Fatal("hierarchy delete accepted")
	}
}
func TestFAIOpenSaveCodec(t *testing.T) {
	if output, _, err := parseCollectorOpen(&reader{data: []byte{3, 1}}, 4, false); err != nil || output != 3 {
		t.Fatal("open collector framing")
	}
	if _, _, err := parseCollectorOpen(&reader{data: []byte{3, 0}}, 4, false); !errors.Is(err, ErrUnsupported) {
		t.Fatal("hierarchy collector accepted")
	}
	for flags := 0; flags < 256; flags++ {
		mode := byte(flags) & 7
		input, got, err := parseFAISave(&reader{data: []byte{3, byte(flags)}}, 4)
		if mode == 0 || mode == 1 || mode == 2 || mode == 4 {
			if err != nil || input != 3 || got != mode {
				t.Fatalf("save flags %02x", flags)
			}
		} else if mode == 6 {
			if err != nil || input != 3 || got != 2 {
				t.Fatalf("combined writable save flags %02x", flags)
			}
		} else if !errors.Is(err, ErrUnsupported) || input != 0 || got != 0 {
			t.Fatalf("combined flags %02x accepted", flags)
		}
	}
	for _, raw := range [][]byte{nil, {3}, {4, 0x0a}} {
		if input, flags, err := parseFAISave(&reader{data: raw}, 4); !errors.Is(err, ErrMalformed) || input != 0 || flags != 0 {
			t.Fatal("malformed Save exposed partial operation")
		}
	}
}
func FuzzFAICodecs(f *testing.F) {
	f.Add(faiImportWire(syntheticFAIImport()))
	f.Add(faiSetOperation(3, syntheticFAISubject("RE: ", "Synthetic 😀"))[3:])
	for _, extended := range []bool{false, true} {
		f.Add(faiSetOperation(3, syntheticRAM42FAISet(extended))[3:])
	}
	for _, class := range append(syntheticFAIClasses, string(make([]byte, 256))) {
		f.Add(faiSetOperation(3, faiSetRequest{{0x001a001f, core.Text(class)}})[3:])
	}
	for width := 1; width <= 8; width++ {
		r := variableFAIImport(width)
		f.Add(faiImportWire(r))
		f.Add(r.predecessors.marshal())
	}
	f.Add([]byte{3, 1})
	f.Fuzz(func(t *testing.T, raw []byte) {
		if len(raw) > 128<<10 {
			return
		}
		_, _ = parseFAIImport(&reader{data: raw}, 4)
		_, _ = parseFAISet(&reader{data: raw})
		_, _ = parseFAIDeletes(&reader{data: raw})
		_, _, _ = parseCollectorOpen(&reader{data: raw}, 4, false)
		_, _, _ = parseFAISave(&reader{data: raw}, 4)
		p, err := parseFAIPCL(raw)
		if err == nil {
			got, err := parseFAIPCL(p.marshal())
			if err != nil || !reflect.DeepEqual(p, got) {
				t.Fatal("canonical PCL mismatch")
			}
		}
	})
}
