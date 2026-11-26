// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"errors"
	"reflect"
	"testing"

	"ewsgo/internal/core"
)

func TestSetListDiagnosticAtDecodedRejection(t *testing.T) {
	unknown := setListOperation(4, 1)
	for _, prefix := range [][]byte{{1, 4, 1}, append(metadataOperation(4, 1), 1, 4, 1)} {
		batch := append(bytes.Clone(prefix), unknown...)
		b := ROPBuffer{Operations: batch, Handles: []uint32{1, 2}}
		for _, trace := range []bool{false, true} {
			ops, err := b.decodeTracedFolderOperations(true, true, true, true, trace)
			var rejected unsupportedOperation
			if ops != nil || !errors.As(err, &rejected) || rejected.label != "RopSetProperties" {
				t.Fatal("rejection changed")
			}
			a, tags, n, h, r := rejected.setList.Fields()
			if !trace {
				if a != "none" {
					t.Fatal("default diagnostic enabled")
				}
				continue
			}
			if a != "list_only" || !reflect.DeepEqual(tags, []string{"0x3613001e", "0x1234000b", "0x12350102"}) || n != "3" || h != "0" || r != "0" {
				t.Fatal("wrong boundary")
			}
		}
	}
	// An earlier unknown opcode blocks traversal; payload-like bytes are never
	// scanned for a later SetProperties. A malformed supported prefix also stops.
	for _, prefix := range [][]byte{{0xff, 4, 1}, {2, 4, 1, 1, 0, 0}, {0x0a, 4, 1, 1, 0}} {
		_, err := (ROPBuffer{Operations: append(prefix, unknown...), Handles: []uint32{1, 2}}).decodeTracedFolderOperations(true, true, true, true, true)
		var rejected unsupportedOperation
		if errors.As(err, &rejected) && rejected.setList.state != 0 {
			t.Fatal("scanned past failed prefix")
		}
	}
	// High/reserved IDs are filtered at exactly the same boundary as first ROP.
	for _, id := range []uint16{0, 0x8000, 0xffff} {
		ops := append([]byte{1, 4, 1}, setPropertyBinaryFixture(4, 1, uint32(id)<<16|0x102)...)
		_, err := (ROPBuffer{Operations: ops, Handles: []uint32{1, 2}}).decodeTracedFolderOperations(true, true, true, true, true)
		var rejected unsupportedOperation
		if !errors.As(err, &rejected) {
			t.Fatal("filter fixture")
		}
		_, tags, _, _, _ := rejected.setList.Fields()
		if len(tags) != 0 || rejected.setList.tags[0] != 0 {
			t.Fatal("filtered ID retained")
		}
	}
}

func TestSetListPrefixDiagnosticHasNoExecutionEffects(t *testing.T) {
	f := newOSTFixture(t)
	f.transport.EnableFolderMetadataSpike()
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	ops := append(metadataOperation(4, 1), 1, 4, 1)
	ops = append(ops, setListOperation(4, 1)...)
	beforeCalls := f.box.calls
	beforeOwner := f.box.stateCalls
	off, _ := f.execute(ops, 65535)
	f.transport.EnableSetListTrace()
	on, counts := f.execute(ops, 65535)
	a, tags, n, _, _ := counts.setList.Fields()
	if !bytes.Equal(on, off) || a != "list_only" || n != "3" || !reflect.DeepEqual(tags, []string{"0x3613001e", "0x1234000b", "0x12350102"}) {
		t.Fatal("wire or diagnostic")
	}
	if len(object.binding.store.metadata) != 0 || f.box.calls != beforeCalls || f.box.stateCalls != beforeOwner {
		t.Fatal("prefix performed write/query")
	}
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
		t.Fatal("prefix Release executed")
	}
}

func TestSetListDiagnosticDistinguishesRejectedMessageSet(t *testing.T) {
	first := faiSetOperation(1, faiSetRequest{{0x0037001f, core.Text("Synthetic")}})
	second := faiSetOperation(1, faiSetRequest{
		{0x001a001f, core.Text("IPM.Note")},
		{0x65f30003, core.Integer(42)},
	})
	batch := ROPBuffer{Operations: append(first, second...), Handles: []uint32{1, 2}}
	for _, trace := range []bool{false, true} {
		ops, err := batch.decodeFAIOperations(true, true, true, true, trace, true, true, true)
		var rejected unsupportedOperation
		if ops != nil || !errors.As(err, &rejected) || rejected.label != "RopSetProperties" {
			t.Fatal("unexpected decoder result")
		}
		state, tags, _, _, _ := rejected.setList.Fields()
		if trace {
			if state != "list_only" || !reflect.DeepEqual(tags, []string{"0x001a001f", "0x65f30003"}) {
				t.Fatal("trace reported the first Set instead of the rejection")
			}
		} else if state != "none" {
			t.Fatal("trace active without opt-in")
		}
	}
}

func FuzzSetListPrefixDecoder(f *testing.F) {
	f.Add(setListOperation(4, 1))
	f.Add(metadataOperation(4, 1))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		b := ROPBuffer{Operations: append([]byte{1, 4, 1}, data...), Handles: []uint32{1, 2, 3, 4}}
		ops, err := b.decodeTracedFolderOperations(true, true, true, true, true)
		if err == nil {
			return
		}
		if ops != nil {
			t.Fatal("partial decoded batch")
		}
		var u unsupportedOperation
		if !errors.As(err, &u) {
			return
		}
		if u.setList.visited > 16 || u.setList.low > u.setList.visited {
			t.Fatal("unbounded diagnostics")
		}
		for _, tag := range u.setList.tags[:u.setList.low] {
			if id := uint16(tag >> 16); id == 0 || id >= 0x8000 {
				t.Fatal("private ID retained")
			}
		}
	})
}
