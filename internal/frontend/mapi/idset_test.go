// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"errors"
	"reflect"
	"testing"
)

var idSetTestReplica = [16]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15}

func idSetFixture(globset ...byte) []byte {
	return append(append([]byte(nil), idSetTestReplica[:]...), globset...)
}

func TestIDSetIndependentCommandFixtures(t *testing.T) {
	for _, tc := range []struct {
		name   string
		wire   []byte
		ranges []idRange
	}{
		{"singleton auto pop", []byte{6, 0, 0, 0, 0, 0, 7, 6, 0, 0, 0, 0, 0, 9, 0}, []idRange{{7, 7}, {9, 9}}},
		{"range common bytes", []byte{5, 0, 0, 0, 0, 0, 0x52, 1, 5, 0x50, 0}, []idRange{{1, 5}}},
		{"nested stack and auto pop", []byte{2, 0, 0, 3, 0, 0, 0, 1, 5, 1, 6, 0x50, 0x52, 0, 0, 0, 8, 0, 0, 0, 10, 0}, []idRange{{5, 6}, {8, 10}}},
		{"bitmask spec example", []byte{5, 0, 0, 0, 0, 0, 0x42, 1, 0xeb, 0}, []idRange{{1, 3}, {5, 5}, {7, 9}}},
		{"full 48bit range", []byte{0x52, 0, 0, 0, 0, 0, 0, 255, 255, 255, 255, 255, 255, 0}, []idRange{{0, maxGlobalCount}}},
		{"unordered overlapping ranges", []byte{5, 0, 0, 0, 0, 0, 0x52, 9, 12, 0x52, 1, 8, 0x52, 6, 10, 0}, []idRange{{1, 12}}},
		{"empty with unused common bytes", []byte{5, 0, 0, 0, 0, 0, 0}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			input := idSetFixture(tc.wire...)
			set, err := parseIDSet(input)
			if err != nil || len(set.replicas) != 1 || set.replicas[0].replica != idSetTestReplica || !reflect.DeepEqual(set.replicas[0].ranges, tc.ranges) {
				t.Fatal("independent GLOBSET fixture", err)
			}
			for _, r := range tc.ranges {
				if !set.contains(idSetTestReplica, r.low) || !set.contains(idSetTestReplica, r.high) {
					t.Fatal("interval membership")
				}
			}
			encoded, err := set.marshal()
			if err != nil {
				t.Fatal("canonical encoding", err)
			}
			again, err := parseIDSet(encoded)
			if err != nil {
				t.Fatal("canonical parse")
			}
			round, err := again.marshal()
			if err != nil || !bytes.Equal(encoded, round) {
				t.Fatal("canonical stability")
			}
			for i := range input {
				input[i] = 0
			}
			if len(tc.ranges) > 0 && !reflect.DeepEqual(set.replicas[0].ranges, tc.ranges) {
				t.Fatal("parser aliases caller")
			}
		})
	}
	// Independent serialization fixture: GUID followed by Range(1..100), End.
	encoded, err := singleReplicaSet(idSetTestReplica, []idRange{{1, 100}}).marshal()
	want := idSetFixture(0x52, 0, 0, 0, 0, 0, 1, 0, 0, 0, 0, 0, 100, 0)
	if err != nil || !bytes.Equal(encoded, want) {
		t.Fatal("independent encoder fixture")
	}
}

func TestIDSetMalformedQuotaAndReplicaOrdering(t *testing.T) {
	good := idSetFixture(6, 0, 0, 0, 0, 0, 7, 0)
	for n := 1; n < len(good); n++ {
		if partial, err := parseIDSet(good[:n]); err == nil || len(partial.replicas) != 0 {
			t.Fatal("truncated IDSET", n)
		}
	}
	for _, wire := range [][]byte{
		{0x50, 0}, {7, 0}, {0xff, 0}, {0x42, 1, 0, 0},
		{5, 0, 0, 0, 0, 0, 2, 1, 2, 0},
		{5, 0, 0, 0, 0, 0, 0x52, 9, 1, 0},
		{5, 0, 0, 0, 0, 0, 0x42, 255, 1, 0},
		{1, 0, 0x50, 0x50, 0},
	} {
		if partial, err := parseIDSet(idSetFixture(wire...)); !errors.Is(err, ErrMalformed) || len(partial.replicas) != 0 {
			t.Fatal("malformed command accepted", err)
		}
	}
	second := append([]byte(nil), good...)
	second[15]++
	if set, err := parseIDSet(append(append([]byte(nil), good...), second...)); err != nil || len(set.replicas) != 2 {
		t.Fatal("ordered replica pairs")
	}
	for _, wire := range [][]byte{append(append([]byte(nil), good...), good...), append(append([]byte(nil), second...), good...)} {
		if _, err := parseIDSet(wire); !errors.Is(err, ErrMalformed) {
			t.Fatal("duplicate or unordered GUID")
		}
	}
	var many []byte
	for i := 0; i < maxIDSetReplicas+1; i++ {
		replica := idSetTestReplica
		replica[15] = byte(i)
		many = append(many, replica[:]...)
		many = append(many, 0)
	}
	if _, err := parseIDSet(many[:maxIDSetReplicas*17]); err != nil {
		t.Fatal("replica limit boundary")
	}
	if partial, err := parseIDSet(many); !errors.Is(err, ErrUnsupported) || len(partial.replicas) != 0 {
		t.Fatal("replica quota")
	}
	ranges := append([]byte(nil), idSetTestReplica[:]...)
	for i := 0; i < maxIDSetRanges; i++ {
		ranges = append(ranges, 6, 0, 0, 0, 0, 0, 1)
	}
	if _, err := parseIDSet(append(append([]byte(nil), ranges...), 0)); err != nil {
		t.Fatal("raw emitted range limit boundary")
	}
	ranges = append(ranges, 6, 0, 0, 0, 0, 0, 1, 0)
	if partial, err := parseIDSet(ranges); !errors.Is(err, ErrUnsupported) || len(partial.replicas) != 0 {
		t.Fatal("range quota must count before normalization")
	}
	if _, err := parseIDSet(make([]byte, maxIDSetBytes+1)); !errors.Is(err, ErrUnsupported) {
		t.Fatal("byte quota")
	}
	if set, err := parseIDSet(nil); err != nil || len(set.replicas) != 0 {
		t.Fatal("empty initial state")
	}
}

func FuzzIDSet(f *testing.F) {
	f.Add(idSetFixture(6, 0, 0, 0, 0, 0, 7, 0))
	f.Add(idSetFixture(0x52, 0, 0, 0, 0, 0, 0, 255, 255, 255, 255, 255, 255, 0))
	f.Add(idSetFixture(5, 0, 0, 0, 0, 0, 0x42, 1, 0xeb, 0))
	f.Fuzz(func(t *testing.T, input []byte) {
		set, err := parseIDSet(input)
		if err != nil {
			if len(set.replicas) != 0 {
				t.Fatal("partial result")
			}
			return
		}
		encoded, err := set.marshal()
		if err != nil {
			t.Fatal("bounded parsed set cannot encode", err)
		}
		again, err := parseIDSet(encoded)
		if err != nil {
			t.Fatal("canonical set cannot decode")
		}
		round, err := again.marshal()
		if err != nil || !bytes.Equal(encoded, round) {
			t.Fatal("canonical roundtrip")
		}
	})
}
