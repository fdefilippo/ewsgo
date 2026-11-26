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

// The tag order/shape comes from the approved RAM42 ID+type diagnostic.
// All values below are synthetic; no client payload was captured or replayed.
func syntheticRAM42FAISet(extended bool) faiSetRequest {
	values := faiSetRequest{
		{0x00170003, core.Integer(1)},
		{0x001a001f, core.Text("IPM.Microsoft.FolderDesign.NamedView")},
		{0x00360003, core.Integer(2)},
		{0x0e070003, core.Integer(0x41)}, // mfRead is ignored for FAI.
		{0x30070040, core.Timestamp(hierarchyTestTime.Add(-time.Hour))},
		{0x300b0102, core.Binary{1, 2, 3}},
		{0x3ffa001f, core.Text("Synthetic client modifier")},
	}
	if extended {
		values = append(values, faiWireProperty{0x685d0003, core.Integer(1)})
	}
	values = append(values, faiWireProperty{0x7c060003, core.Integer(2)})
	if extended {
		values = append(values, faiWireProperty{0x7c070102, core.Binary{1, 2, 3}})
	}
	return append(values, faiWireProperty{0x0e1f000b, core.Bool(true)}, faiWireProperty{0x30080040, core.Timestamp(hierarchyTestTime)})
}

func assertFAIProblems(t *testing.T, raw []byte, want []faiPropertyProblem) {
	t.Helper()
	if len(raw) != 8+10*len(want) || raw[0] != 0x0a || binary.LittleEndian.Uint32(raw[2:]) != 0 || int(binary.LittleEndian.Uint16(raw[6:])) != len(want) {
		t.Fatal("SetProperties response framing/count")
	}
	for i, p := range want {
		v := raw[8+10*i:]
		if binary.LittleEndian.Uint16(v) != p.index || binary.LittleEndian.Uint32(v[2:]) != p.tag || binary.LittleEndian.Uint32(v[6:]) != p.code {
			t.Fatal("PropertyProblem index/tag/code")
		}
	}
}

func TestFAISetRAM42ShapesSaveAndRead(t *testing.T) {
	for _, extended := range []bool{false, true} {
		name := "ten_tags"
		if extended {
			name = "twelve_tags"
		}
		t.Run(name, func(t *testing.T) {
			f, _, store := newFAIExecuteFixture(t)
			t.Cleanup(f.state.mailbox.close)
			request := syntheticFAIImport()
			faiRunOK(t, f, faiImportOperation(request))
			values := syntheticRAM42FAISet(extended)
			want := []faiPropertyProblem{{5, 0x300b0102, accessDenied}, {6, 0x3ffa001f, accessDenied}}
			if extended {
				want = append(want, faiPropertyProblem{7, 0x685d0003, NotSupported})
			}
			assertFAIProblems(t, faiRunOK(t, f, faiSetOperation(3, values)), want)
			if len(store.fai.records) != 0 {
				t.Fatal("properties persisted without Save")
			}
			faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 0})
			if len(store.fai.records) != 1 {
				t.Fatal("no saved FAI")
			}
			for _, record := range store.fai.records {
				p := record.object.Properties
				if p[core.Key{ID: 0x17}] != core.Integer(1) || p[core.Key{ID: 0x36}] != core.Integer(2) || p[core.Key{ID: 0xe1f}] != core.Bool(true) || p[core.LastModifiedAt] != core.Timestamp(request.modified) || p[core.IsRead] != core.Bool(false) {
					t.Fatal("supported metadata or import timestamp not preserved")
				}
				for _, id := range []uint32{0x0e07, 0x300b, 0x3ffa, 0x685d} {
					if _, exists := p[core.Key{ID: id}]; exists {
						t.Fatal("server-owned/unsupported property persisted")
					}
				}
			}
			faiRunOK(t, f, []byte{1, 4, 2})
			c := fullContentsConfig()
			c.flags &^= 0x20 // Associated only.
			configureContentsTest(t, f, c)
			found := map[uint32]any{}
			for _, token := range decodeTestContents(t, drainFX(t, f.run, 2, 97)) {
				found[token.tag] = token.value
			}
			if found[0x00170003] != int32(1) || found[0x00360003] != int32(2) || found[0x0e1f000b] != true || found[0x0e070003] != int32(0x40) {
				t.Fatal("independent decoder did not recover supported FAI metadata")
			}
			for _, tag := range []uint32{0x300b0102, 0x3ffa001f, 0x685d0003} {
				if _, exists := found[tag]; exists {
					t.Fatal("denied metadata leaked into FastTransfer")
				}
			}
		})
	}
}

func TestFAISetRAM42CodecAndIsolation(t *testing.T) {
	values := syntheticRAM42FAISet(true)
	wire := faiSetOperation(3, values)
	decoded, err := parseFAISet(&reader{data: wire[3:]})
	if err != nil || !reflect.DeepEqual(decoded, values) {
		t.Fatal("observed tag shape not parsed with synthetic values")
	}
	for n := 0; n < len(wire)-3; n++ {
		if got, err := parseFAISet(&reader{data: wire[3 : 3+n]}); err == nil || got != nil {
			t.Fatal("truncated SetProperties exposed values")
		}
	}
	for _, bad := range []faiSetRequest{
		{{0x0017000b, core.Bool(true)}},
		{{0x00170003, core.Integer(1)}, {0x00170003, core.Integer(2)}},
		{{0x00170003, core.Integer(1)}, {0x12340003, core.Integer(2)}},
		{{0x001a001f, core.Text("IPM.Invalid.")}},
	} {
		raw := faiSetOperation(3, bad)
		if got, err := parseFAISet(&reader{data: raw[3:]}); err == nil || got != nil {
			t.Fatal("closed tag/type/class profile widened")
		}
	}
	f, _, store := newFAIExecuteFixture(t)
	t.Cleanup(f.state.mailbox.close)
	for _, input := range []byte{0, 1, 2} { // Logon, native Folder, collector.
		raw, counts := f.run(faiSetOperation(input, values))
		if counts.errors != 1 || binary.LittleEndian.Uint32(raw[2:]) != NotSupported || len(store.fai.records) != 0 {
			t.Fatal("FAI SetProperties accepted a non-draft object")
		}
	}
	f.transport.faiCollectorSpike = false
	body, _ := f.execute(wire, 65535)
	if binary.LittleEndian.Uint32(body[4:]) != NotSupported {
		t.Fatal("FAI profile enabled without opt-in")
	}
}

func TestFAISetProblemsPreflightAndAtomicFatalErrors(t *testing.T) {
	f, _, _ := newFAIExecuteFixture(t)
	t.Cleanup(f.state.mailbox.close)
	faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
	object, _ := f.state.mailbox.object(f.handles[3], 4)
	d := object.download.draft
	before, _ := core.CloneProperties(d.properties)
	leaseBefore := f.transport.stores.contents.used
	// Larger than the old fixed 18-byte reservation, smaller than 12 problems.
	batch := append([]byte{1, 4, 1}, faiSetOperation(3, syntheticRAM42FAISet(true))...)
	f.execute(batch, 100)
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 || !reflect.DeepEqual(before, d.properties) || f.transport.stores.contents.used != leaseBefore {
		t.Fatal("short output executed Release/Set prefix")
	}
	bad := append(faiSetOperation(3, syntheticRAM42FAISet(true)), 0xff, 4, 0)
	f.execute(bad, 65535)
	if !reflect.DeepEqual(before, d.properties) {
		t.Fatal("unknown suffix executed SetProperties prefix")
	}
	values := faiSetRequest{{0x00170003, core.Integer(1)}, {0x3ffa001f, core.Text("denied")}, {0x12340003, core.Integer(1)}}
	if _, err := d.set(context.Background(), f.session, values); !errors.Is(err, core.ErrUnsupported) || !reflect.DeepEqual(before, d.properties) {
		t.Fatal("fatal validation published partial draft")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := d.set(ctx, f.session, syntheticRAM42FAISet(true)); err == nil || !reflect.DeepEqual(before, d.properties) {
		t.Fatal("cancelled SetProperties published partial draft")
	}
	values = append(syntheticRAM42FAISet(true), faiWireProperty{0x70010102, core.Binary(bytes.Repeat([]byte{1}, maxFAIObjectBytes))})
	if _, err := d.set(context.Background(), f.session, values); !errors.Is(err, core.ErrUnsupported) || !reflect.DeepEqual(before, d.properties) {
		t.Fatal("quota failure published partial draft")
	}
}

func TestFAISetReadOnlyAndInvalidValueProblems(t *testing.T) {
	f, _, _ := newFAIExecuteFixture(t)
	t.Cleanup(f.state.mailbox.close)
	faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
	values := faiSetRequest{
		{0x00170003, core.Integer(3)},
		{0x00360003, core.Integer(-1)},
		{0x30080040, core.Timestamp(hierarchyTestTime.Add(time.Hour))},
		{0x0e070003, core.Integer(8)},
	}
	want := []faiPropertyProblem{{0, 0x00170003, 0x80070057}, {1, 0x00360003, 0x80070057}, {2, 0x30080040, accessDenied}, {3, 0x0e070003, NotSupported}}
	assertFAIProblems(t, faiRunOK(t, f, faiSetOperation(3, values)), want)
	object, _ := f.state.mailbox.object(f.handles[3], 4)
	d := object.download.draft
	if len(d.properties) != 3 || d.properties[core.LastModifiedAt] != core.Timestamp(hierarchyTestTime) {
		t.Fatal("rejected values changed draft")
	}
	faiRunOK(t, f, faiSetOperation(3, faiCreateProperties()))
	faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 2})
	assertFAIProblems(t, faiRunOK(t, f, faiSetOperation(3, faiSetRequest{{0x0e070003, core.Integer(0x40)}, {0x30080040, core.Timestamp(hierarchyTestTime)}})), nil)
	if d.dirty {
		t.Fatal("idempotent server-field reaffirmation dirtied saved draft")
	}
}

func TestFAISetMixedSupportedAndUnmappedProperties(t *testing.T) {
	f, _, store := newFAIExecuteFixture(t)
	t.Cleanup(f.state.mailbox.close)
	const unmapped = 0x12340003
	values := append(faiCreateProperties(), faiWireProperty{unmapped, core.Integer(17)})
	wire := faiSetOperation(3, values)
	batch := append(faiImportOperation(syntheticFAIImport()), wire...)
	out := faiRunOK(t, f, batch)
	assertFAIProblems(t, out[14:], []faiPropertyProblem{{4, unmapped, NotSupported}})

	const unmappedText = 0x1235001f
	wire = faiSetOperation(3, faiSetRequest{
		{unmappedText, core.Text("Client metadata")},
		{0x00170003, core.Integer(1)},
	})
	assertFAIProblems(t, faiRunOK(t, f, wire), []faiPropertyProblem{{0, unmappedText, NotSupported}})
	faiRunOK(t, f, []byte{0x0c, 4, 1, 3, 2})
	if len(store.fai.records) != 1 {
		t.Fatal("expected one saved associated message")
	}
	for _, record := range store.fai.records {
		properties := record.object.Properties
		if properties[core.Key{ID: 0x17}] != core.Integer(1) ||
			properties[core.Key{ID: unmapped >> 16}] != nil ||
			properties[core.Key{ID: unmappedText >> 16}] != nil {
			t.Fatal("unsupported values persisted or supported value lost")
		}
	}
}
