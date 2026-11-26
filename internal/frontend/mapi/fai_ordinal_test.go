// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/binary"
	"reflect"
	"testing"

	"ewsgo/internal/core"
)

// RAM48 provided only these public tag types; every value is synthetic.
func syntheticRAM48FAISet() faiSetRequest {
	values := syntheticRAM42FAISet(false)
	values[7] = values[6]
	values[6] = faiWireProperty{0x36e20003, core.Integer(3)}
	return values
}

func TestFAIOrdinalMostProblemPreservesOtherProperties(t *testing.T) {
	f, _, store := newFAIExecuteFixture(t)
	t.Cleanup(f.state.mailbox.close)
	faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
	values := syntheticRAM48FAISet()
	wire := faiSetOperation(3, values)
	decoded, err := parseFAISet(&reader{data: wire[3:]})
	if err != nil || !reflect.DeepEqual(decoded, values) {
		t.Fatal("OrdinalMost scalar recognition failed")
	}
	assertFAIProblems(t, faiRunOK(t, f, wire), []faiPropertyProblem{
		{5, 0x300b0102, accessDenied}, {6, 0x36e20003, NotSupported}, {7, 0x3ffa001f, accessDenied},
	})
	faiRunOK(t, f, []byte{0x0c, 4, 1, 3, 2})
	for _, record := range store.fai.records {
		if _, ok := record.object.Properties[core.Key{ID: 0x36e2}]; ok || record.object.Properties[core.Key{ID: 0x17}] != core.Integer(1) {
			t.Fatal("unsupported ordinal persisted or supported value lost")
		}
	}
	faiRunOK(t, f, []byte{1, 4, 2})
	c := fullContentsConfig()
	c.flags &^= 0x20
	configureContentsTest(t, f, c)
	for _, token := range decodeTestContents(t, drainFX(t, f.run, 2, 97)) {
		if token.tag == 0x36e20003 {
			t.Fatal("OrdinalMost exported from FAI")
		}
	}
}

func TestFAIOrdinalMostPreflightAndTargetIsolation(t *testing.T) {
	f, _, _ := newFAIExecuteFixture(t)
	t.Cleanup(f.state.mailbox.close)
	faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
	o, _ := f.state.mailbox.object(f.handles[3], 4)
	before, _ := core.CloneProperties(o.download.draft.properties)
	batch := append([]byte{1, 4, 1}, faiSetOperation(3, syntheticRAM48FAISet())...)
	f.execute(batch, 100)
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 || !reflect.DeepEqual(before, o.download.draft.properties) {
		t.Fatal("insufficient response executed Release/Set")
	}
	for _, input := range []byte{0, 1, 2} {
		out, counts := f.run(faiSetOperation(input, syntheticRAM48FAISet()))
		if counts.errors != 1 || binary.LittleEndian.Uint32(out[2:]) != NotSupported {
			t.Fatal("OrdinalMost enabled writing non-FAI objects")
		}
	}
	// Parse the Set itself; truncated values must never be accepted.
	wire := faiSetOperation(3, syntheticRAM48FAISet())
	for n := 0; n < len(wire)-3; n++ {
		if _, err := parseFAISet(&reader{data: wire[3 : 3+n]}); err == nil {
			t.Fatal("truncated OrdinalMost Set accepted")
		}
	}
	bad := faiSetOperation(3, faiSetRequest{{0x36e2000b, core.Bool(true)}})
	if _, err := parseFAISet(&reader{data: bad[3:]}); err == nil {
		t.Fatal("OrdinalMost wrong type accepted")
	}
}
