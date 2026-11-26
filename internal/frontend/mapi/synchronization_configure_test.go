// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

// Independent MS-OXCROPS field fixture, including an arbitrary private
// restriction. Structural parsing does not validate restriction semantics.
var configureFixture = []byte{
	0x70, 4, 0, 1, 2, 9, 1, 1, 3, 0,
	0xaa, 0xbb, 0xcc,
	5, 0, 0, 0, 2, 0,
	0x1f, 0, 1, 0x30, 2, 1, 0xe0, 0x65,
}

func TestSynchronizationConfigureStructuralWireBoundary(t *testing.T) {
	next := []byte{1, 4, 1}
	r := reader{data: append(bytes.Clone(configureFixture[3:]), next...)}
	v, err := parseSynchronizationConfigure(&r, 2)
	if err != nil || v.output != 1 || v.kind != 2 || v.sendOptions != 9 || v.flags != 0x0101 || v.restrictionBytes != 3 || v.extraFlags != 5 || !reflect.DeepEqual(v.tags, []uint32{0x3001001f, 0x65e00102}) || !bytes.Equal(r.data[r.pos:], next) {
		t.Fatal("independent fixture or boundary")
	}
	if got := fmt.Sprint(v); got != "MAPI synchronization request [private]" || strings.Contains(got, "3001") {
		t.Fatal("incidental request formatting leaked fields")
	}
	// The tag slice is detached from the input. No restriction data is stored.
	r.data[16] ^= 0xff
	if v.tags[0] != 0x3001001f {
		t.Fatal("tag aliases input")
	}
	minimal := reader{data: []byte{1, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}}
	v, err = parseSynchronizationConfigure(&minimal, 2)
	if err != nil || minimal.done() != nil || len(v.tags) != 0 || v.restrictionBytes != 0 {
		t.Fatal("minimal hierarchy frame")
	}
}

func TestSynchronizationConfigureTruncationsAndQuota(t *testing.T) {
	for n := 0; n < len(configureFixture)-3; n++ {
		r := reader{data: configureFixture[3 : 3+n]}
		v, err := parseSynchronizationConfigure(&r, 2)
		if !errors.Is(err, ErrMalformed) || !reflect.DeepEqual(v, synchronizationConfigure{}) {
			t.Fatal("truncated request", n)
		}
	}
	for _, handles := range []int{-1, 0, 1, MaxHandles + 1} {
		r := reader{data: configureFixture[3:]}
		if _, err := parseSynchronizationConfigure(&r, handles); !errors.Is(err, ErrMalformed) {
			t.Fatal("handle quota/output index")
		}
	}
	for _, count := range []int{256, 257, 65535} {
		wire := []byte{1, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0}
		wire = binary.LittleEndian.AppendUint16(wire, uint16(count))
		wire = append(wire, make([]byte, 4*count)...)
		r := reader{data: wire}
		v, err := parseSynchronizationConfigure(&r, 2)
		if count == 256 {
			if err != nil || len(v.tags) != count || r.done() != nil {
				t.Fatal("valid tag cap")
			}
		} else if !errors.Is(err, ErrUnsupported) || !reflect.DeepEqual(v, synchronizationConfigure{}) {
			t.Fatal("tag quota")
		}
	}
}

func TestSynchronizationConfigureCodecDoesNotEnableRuntime(t *testing.T) {
	f := newOSTFixture(t)
	body, c := f.execute(append([]byte{1, 4, 1}, configureFixture...), 4096)
	if binary.LittleEndian.Uint32(body[4:]) != NotSupported || c.unsupported != "RopSynchronizationConfigure" || c.folders != 0 {
		t.Fatal("headless codec enabled runtime")
	}
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
		t.Fatal("unsupported batch released prefix")
	}
}

func FuzzSynchronizationConfigureFrame(f *testing.F) {
	f.Add(configureFixture[3:])
	f.Add([]byte{})
	f.Add(make([]byte, 13))
	f.Fuzz(func(t *testing.T, wire []byte) {
		if len(wire) > 65535 {
			t.Skip()
		}
		r := reader{data: wire}
		v, err := parseSynchronizationConfigure(&r, 2)
		if err != nil {
			if !reflect.DeepEqual(v, synchronizationConfigure{}) {
				t.Fatal("partial usable request on failure")
			}
			return
		}
		if r.pos != 13+int(v.restrictionBytes)+4*len(v.tags) || len(v.tags) > maxFolderPropertyTags || v.output > 1 {
			t.Fatal("bounds")
		}
	})
}
