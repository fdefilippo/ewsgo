// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"testing"

	"ewsgo/internal/core"
)

func TestFAISaveIgnoredFlagsExecuteAndHandleLifetime(t *testing.T) {
	for _, flags := range []byte{0x08, 0x09, 0x0a, 0x0c, 0x0e, 0xf8, 0xf9, 0xfa, 0xfc} {
		t.Run(fmt.Sprintf("%02x", flags), func(t *testing.T) {
			f, box, store := newFAIExecuteFixture(t)
			faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
			object, _ := f.state.mailbox.object(f.handles[3], 4)
			d := object.download.draft
			// Two successive Sets followed by Save, as in the public Microsoft
			// example, using only synthetic properties and values.
			ops := append(faiSetOperation(3, faiCreateProperties()), faiSetOperation(3, syntheticFAISubject("", "Synthetic configuration"))...)
			ops = append(ops, 0x0c, 4, 1, 3, flags)
			out, counts := f.run(ops)
			if counts.errors != 0 || len(out) != 8+8+15 || out[17] != 1 || out[22] != 3 || binary.LittleEndian.Uint64(out[23:]) == 0 || len(store.fai.records) != 1 {
				t.Fatal("two Sets/Save response indexes or commit failed")
			}
			_, code := f.state.mailbox.object(f.handles[3], 4)
			mode := flags & 7
			if code != 0 || d.closed || d.readOnly != (mode == 1) {
				t.Fatal("ignored flag changed handle lifetime/access")
			}
			if !d.closed {
				_, err := d.set(context.Background(), f.session, faiSetRequest{{0x7006001f, core.Text("Synthetic edit")}})
				if mode == 1 && !errors.Is(err, core.ErrAccessDenied) || mode != 1 && err != nil {
					t.Fatal("post-Save access differs from defined mode")
				}
			}
			if box.sourceCalls != 0 {
				t.Fatal("FAI save reached native content")
			}
		})
	}
}

func TestFAISaveCleanDraftRetainsHandleAndAppliesReadOnly(t *testing.T) {
	for _, flags := range []byte{0, 1, 2, 4, 0x08, 0x09, 0x0a, 0x0c} {
		t.Run(fmt.Sprintf("%02x", flags), func(t *testing.T) {
			f, box := messageFixture(t)
			d := newFAIDraftForTest(t, f, syntheticFAIImport())
			first := saveFAIDraftForTest(t, f, d, 0x0a)
			before := f.store.next
			second := saveFAIDraftForTest(t, f, d, flags)
			if !reflect.DeepEqual(first, second) || f.store.next != before || len(f.store.fai.records) != 1 || d.closed || d.readOnly != (flags&7 == 1) {
				t.Fatal("clean Save changed version or ignored lifetime mode")
			}
			if d.lease == nil || d.properties == nil {
				t.Fatal("clean Save discarded open draft memory")
			}
			if d.readOnly {
				saveFAIDraftForTest(t, f, d, 0x0a)
				if _, err := d.set(context.Background(), f.session, faiSetRequest{{0x7006001f, core.Text("denied")}}); !errors.Is(err, core.ErrAccessDenied) {
					t.Fatal("Save upgraded a read-only draft")
				}
			}
			if box.sourceCalls != 0 {
				t.Fatal("clean FAI save accessed native source")
			}
		})
	}
}

func TestFAISaveIgnoredFlagsRejectedTailHasNoPrefixEffects(t *testing.T) {
	f, _, store := newFAIExecuteFixture(t)
	faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
	faiRunOK(t, f, faiSetOperation(3, faiCreateProperties()))
	before := append([]uint32(nil), f.handles...)
	// Parse the complete batch before executing even a valid Save prefix.
	ops := append([]byte{0x0c, 4, 1, 3, 0x0a}, 0x04, 4, 1, 4, 0)
	body, counts := f.execute(ops, 32768)
	if binary.LittleEndian.Uint32(body[4:]) != NotSupported || counts.unsupported != "RopGetHierarchyTable" || len(store.fai.records) != 0 || !reflect.DeepEqual(before, f.handles) {
		t.Fatal("rejected tail executed Save or changed handles")
	}
	faiRunOK(t, f, []byte{0x0c, 4, 1, 3, 0x0a})
}

func TestFAISaveIgnoredFlagsRetainConflictAndForceSemantics(t *testing.T) {
	f, _ := messageFixture(t)
	a := newFAIDraftForTest(t, f, syntheticFAIImport())
	b := newFAIDraftForTest(t, f, syntheticFAIImport())
	first := saveFAIDraftForTest(t, f, a, 0x0a)
	before, size := f.store.next, f.store.fai.bytes
	for _, flags := range []byte{0x0a, 0x0e, 0xfa, 0x0b} {
		_, err := b.save(context.Background(), f.session, flags)
		want := core.ErrConflict
		if flags == 0x0b {
			want = core.ErrUnsupported
		}
		if !errors.Is(err, want) || b.saved || b.closed || f.store.next != before || f.store.fai.bytes != size {
			t.Fatal("rejected Save changed state or ignored conflict")
		}
	}
	if _, err := b.set(context.Background(), f.session, faiSetRequest{{0x7006001f, core.Text("Forced synthetic view")}}); err != nil {
		t.Fatal(err)
	}
	forced := saveFAIDraftForTest(t, f, b, 0x0c)
	if forced.mid != first.mid || forced.cn <= first.cn || b.readOnly || b.closed || forced.object.Properties[core.Key{ID: 0x7006}] != core.Text("Forced synthetic view") {
		t.Fatal("ignored bits changed ForceSave semantics")
	}
}
