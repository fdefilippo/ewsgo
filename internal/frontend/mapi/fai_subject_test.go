// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"reflect"
	"strings"
	"testing"

	"ewsgo/internal/core"
)

func syntheticFAISubject(prefix, normalized string) faiSetRequest {
	return faiSetRequest{{0x003d001f, core.Text(prefix)}, {0x0e1d001f, core.Text(normalized)}}
}

func subjectEqualityRestriction(normalized string) []byte {
	wire := classEqualityRestriction(normalized)
	binary.LittleEndian.PutUint32(wire[2:6], 0x0e1d001f)
	binary.LittleEndian.PutUint32(wire[6:10], 0x0e1d001f)
	return wire
}

func TestFAISubjectCodecObservedPair(t *testing.T) {
	values := syntheticFAISubject("", "Synthetic configuration")
	wire := faiSetOperation(3, values)
	got, err := parseFAISet(&reader{data: wire[3:]})
	if err != nil || !reflect.DeepEqual(got, values) {
		t.Fatal("observed subject tag pair rejected with synthetic values")
	}
	for n := range len(wire[3:]) {
		if out, err := parseFAISet(&reader{data: wire[3 : 3+n]}); err == nil || out != nil {
			t.Fatal("truncated subject Set accepted")
		}
	}
	for _, tag := range []uint32{0x003d0003, 0x0e1d0102} {
		if faiSetTag(tag) {
			t.Fatal("unsupported subject type accepted")
		}
	}
}

func TestFAISubjectBatchSaveFastTransferTableReconnect(t *testing.T) {
	f, box, store := newFAIExecuteFixture(t)
	f.transport.EnableContentsTableSpike()
	faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
	// Reproduce two successive SetProperties operations, not one merged list.
	first := syntheticRAM42FAISet(false)
	first[1].value = core.Text("IPM.Configuration.Synthetic")
	ops := append(faiSetOperation(3, first), faiSetOperation(3, syntheticFAISubject("RE: ", "Configurazione 😀"))...)
	ops = append(ops, 0x0c, 4, 4, 3, 0x08) // Ignored bits, retain current access.
	raw, counts := f.run(ops)
	if counts.errors != 0 || len(raw) != 28+8+15 || binary.LittleEndian.Uint32(raw[30:]) != 0 || binary.LittleEndian.Uint16(raw[34:]) != 0 || binary.LittleEndian.Uint32(raw[38:]) != 0 {
		t.Fatal("successive FAI Sets/Save did not execute with property problems confined to first Set")
	}
	assertFAIProblems(t, raw[:28], []faiPropertyProblem{{5, 0x300b0102, accessDenied}, {6, 0x3ffa001f, accessDenied}})
	if len(store.fai.records) != 1 {
		t.Fatal("no FAI saved")
	}
	faiRunOK(t, f, []byte{1, 4, 2})
	c := fullContentsConfig()
	c.flags &^= 0x20
	configureContentsTest(t, f, c)
	got := map[uint32]any{}
	for _, token := range decodeTestContents(t, drainFX(t, f.run, 2, 97)) {
		got[token.tag] = token.value
	}
	if got[0x003d001f] != "RE: " || got[0x0e1d001f] != "Configurazione 😀" || got[0x0037001f] != "RE: Configurazione 😀" {
		t.Fatal("subject parts/composition lost in independent FX decoder")
	}
	// Native MIME now projects subject components too; Associated and mixed
	// selections must retain the same component capability.
	if !contentsProjectionSchema(true, true)(fixedContentsProperty(0x0e1d001f)) || !contentsProjectionSchema(false, true)(fixedContentsProperty(0x0e1d001f)) {
		t.Fatal("projected subject component capability missing")
	}
	faiRunOK(t, f, []byte{1, 4, 2})
	faiRunOK(t, f, contentsConfigureOperation(c, subjectEqualityRestriction("Configurazione 😀")))
	matched := 0
	for _, token := range decodeTestContents(t, drainFX(t, f.run, 2, 97)) {
		if token.tag == 0x0e1d001f && token.value == "Configurazione 😀" {
			matched++
		}
	}
	if matched != 1 {
		t.Fatal("FAI component equality filter did not match stored value")
	}
	f.state.mailbox.close()
	f.state.mailbox = newMailboxObjects(f.transport.stores)
	f.handles = []uint32{0xffffffff, 0xffffffff, 0xffffffff, 0xffffffff, 0xffffffff}
	faiRunOK(t, f, logonOperation(f.identity.LegacyDN, 4, 0))
	f.open(int(Inbox))
	faiRunOK(t, f, []byte{5, 4, 1, 2, 0x52})
	tags := []uint32{0x003d001f, 0x0e1d001f, 0x0037001f, 0x67aa000b}
	faiRunOK(t, f, tableColumnsWire(2, tags...))
	rows := decodeTableRows(t, faiRunOK(t, f, tableQueryWire(2, 0, 1, 4)), tags)
	if len(rows) != 1 || !reflect.DeepEqual(rows[0], []any{"RE: ", "Configurazione 😀", "RE: Configurazione 😀", true}) || box.sourceCalls != 0 {
		t.Fatal("reconnect subject table mismatch or native contents accessed")
	}
	f.state.mailbox.close()
}

func TestFAISubjectRejectedBatchHasNoPrefixEffects(t *testing.T) {
	f, _, store := newFAIExecuteFixture(t)
	faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
	object, _ := f.state.mailbox.object(f.handles[3], 4)
	before, _ := core.CloneProperties(object.download.draft.properties)
	handles := append([]uint32(nil), f.handles...)
	ops := append([]byte{1, 4, 1}, faiSetOperation(3, faiCreateProperties())...)
	ops = append(ops, faiSetOperation(3, syntheticFAISubject("", "Synthetic"))...)
	ops = append(ops, 0x04, 4, 1) // Unsupported HierarchyTable tail.
	body, counts := f.execute(ops, 65535)
	if binary.LittleEndian.Uint32(body[4:]) != NotSupported || counts.unsupported != "RopGetHierarchyTable" || !reflect.DeepEqual(handles, f.handles) || !reflect.DeepEqual(before, object.download.draft.properties) || len(store.fai.records) != 0 {
		t.Fatal("decoder did not reach tail or rejected batch changed state")
	}
}

func TestFAISubjectBoundariesAtomicityAndCoherence(t *testing.T) {
	f, _ := messageFixture(t)
	d := newFAIDraftForTest(t, f, syntheticFAIImport())
	set := func(values faiSetRequest) error {
		problems, err := d.set(context.Background(), f.session, values)
		if len(problems) != 0 {
			t.Fatal("unexpected per-property problems")
		}
		return err
	}
	for _, pair := range [][2]string{{"", ""}, {"RE: ", "Synthetic"}, {"X", strings.Repeat("😀", 126)}} {
		if err := set(syntheticFAISubject(pair[0], pair[1])); err != nil || d.properties[core.Subject] != core.Text(pair[0]+pair[1]) {
			t.Fatal("valid subject pair failed")
		}
	}
	before, _ := core.CloneProperties(d.properties)
	for _, values := range []faiSetRequest{
		syntheticFAISubject("X", strings.Repeat("😀", 127)),
		syntheticFAISubject("", strings.Repeat("x", 254)),
		{{0x003d001f, core.Bool(true)}},
		{{0x003d0003, core.Integer(1)}},
		{{0x0037001f, core.Text("Inconsistent")}},
		append(syntheticFAISubject("", "Synthetic"), faiWireProperty{0x0037001f, core.Text("Inconsistent")}),
	} {
		values = append(faiSetRequest{{0x00170003, core.Integer(2)}}, values...)
		if err := set(values); err == nil || !reflect.DeepEqual(before, d.properties) {
			t.Fatal("invalid subject changed draft")
		}
	}
	if err := set(syntheticFAISubject("RE: ", "Original")); err != nil {
		t.Fatal("pair setup")
	}
	if err := set(faiSetRequest{{0x0e1d001f, core.Text("Updated")}}); err != nil || d.properties[core.Subject] != core.Text("RE: Updated") {
		t.Fatal("partial update did not preserve other part")
	}
	if err := set(faiSetRequest{{0x003d001f, core.Text("")}}); err != nil || d.properties[core.Subject] != core.Text("Updated") {
		t.Fatal("empty prefix not preserved")
	}
	for _, mode := range []string{"inconsistent", "over_limit", "missing_part"} {
		r := faiTestRecord(f.store, d.root, 1)
		r.object.Properties[core.Key{ID: 0x3d}] = core.Text("")
		r.object.Properties[core.Key{ID: 0xe1d}] = core.Text("Synthetic")
		r.object.Properties[core.Subject] = core.Text("Synthetic")
		switch mode {
		case "inconsistent":
			r.object.Properties[core.Subject] = core.Text("Other")
		case "over_limit":
			r.object.Properties[core.Key{ID: 0xe1d}] = core.Text(strings.Repeat("x", 254))
			r.object.Properties[core.Subject] = core.Text(strings.Repeat("x", 254))
		case "missing_part":
			delete(r.object.Properties, core.Key{ID: 0x3d})
		}
		if _, err := f.store.validateFAIRecord(r); err == nil {
			t.Fatal("invalid subject namespace bypass")
		}
	}
}
