// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
	"time"
)

func TestLogonRequestFullBatchAndIndependentFixture(t *testing.T) {
	logon := fixture(t, "logonRequest")
	// An arbitrary LogonId (0x9a) is not a handle index. The output slot
	// contains the unallocated sentinel and does not need an input object.
	batch := append(bytes.Clone(logon), []byte{1, 0x9a, 1}...)
	buffer := ROPBuffer{Operations: batch, Handles: []uint32{0, 0xffffffff}}
	ops, err := buffer.DecodeOperations()
	want := LogonRequest{Flags: 9, OpenFlags: 0x2100040c, StoreState: 0xa1b2c3d4, ESSDN: "/o=Synthetic/cn=Mailbox"}
	if err != nil || len(ops) != 2 || ops[0].ID != 0xfe || ops[0].Logon != 0x9a || ops[0].Input != 0 || ops[0].Output != 1 || ops[0].LogonRequest == nil || *ops[0].LogonRequest != want || ops[1].ID != 1 {
		t.Fatal("Logon/Release full batch decoding", err)
	}
	// A valid prefix must never yield executable operations when the tail is
	// malformed or unknown. This differs from first-ROP-only diagnostics.
	for cut := 1; cut < len(logon); cut++ {
		v := buffer
		v.Operations = logon[:cut]
		if partial, err := v.DecodeOperations(); err != ErrMalformed || partial != nil {
			t.Fatal("truncated Logon accepted", cut)
		}
	}
	for _, tail := range [][]byte{{1}, {1, 0x9a}, {1, 0x9a, 2}, {2, 0x9a, 1}, {0xff, 0x9a, 1}} {
		v := buffer
		v.Operations = append(bytes.Clone(logon), tail...)
		if partial, err := v.DecodeOperations(); err == nil || partial != nil {
			t.Fatal("invalid trailing ROP returned partial operations")
		}
	}
}

func TestLogonRequestValidationAndEmptyESSDN(t *testing.T) {
	valid := fixture(t, "logonRequest")
	for _, alter := range []func([]byte){
		func(b []byte) { b[2] = 2 },
		func(b []byte) { b[len(b)-1] = 'x' },
		func(b []byte) { b[15] = 0 },
		func(b []byte) { b[15] = 0x80 },
		func(b []byte) { b[15] = 0x1f },
		func(b []byte) { binary.LittleEndian.PutUint16(b[12:], 4097) },
		func(b []byte) { binary.LittleEndian.PutUint16(b[12:], 65535) },
	} {
		bad := bytes.Clone(valid)
		alter(bad)
		if _, err := (ROPBuffer{Operations: bad, Handles: []uint32{0, 0xffffffff}}).DecodeOperations(); !errors.Is(err, ErrMalformed) {
			t.Fatal("malformed Logon accepted", err)
		}
	}
	empty := bytes.Clone(valid[:14])
	binary.LittleEndian.PutUint16(empty[12:], 0)
	ops, err := (ROPBuffer{Operations: empty, Handles: []uint32{0, 0xffffffff}}).DecodeOperations()
	if err != nil || len(ops) != 1 || ops[0].LogonRequest.ESSDN != "" {
		t.Fatal("zero-sized ESSDN forbidden by codec", err)
	}
	// No policy decisions are hidden in the reader: even unknown flag bits
	// reach semantic validation intact and do not imply successful execution.
	empty[3] = 0xff
	binary.LittleEndian.PutUint32(empty[4:], 0xffffffff)
	ops, err = (ROPBuffer{Operations: empty, Handles: []uint32{0, 0xffffffff}}).DecodeOperations()
	if err != nil || ops[0].LogonRequest.Flags != 0xff || ops[0].LogonRequest.OpenFlags != 0xffffffff {
		t.Fatal("request flags silently rewritten")
	}
	for _, count := range []int{1024, 1025} {
		ops, err := (ROPBuffer{Operations: bytes.Repeat([]byte{1, 0, 0}, count), Handles: []uint32{1}}).DecodeOperations()
		if count == 1024 && (err != nil || len(ops) != count) || count == 1025 && (err != ErrMalformed || ops != nil) {
			t.Fatal("batch operation limit", count, err)
		}
	}
}

func syntheticPrivateLogon() PrivateLogonResponse {
	v := PrivateLogonResponse{
		OutputHandleIndex: 1, LogonFlags: 7, ResponseFlags: 0x17,
		MailboxGUID: [16]byte{0x33, 0x22, 0x11, 0, 0x55, 0x44, 0x77, 0x66, 0x88, 0x99, 0xaa, 0xbb, 0xcc, 0xdd, 0xee, 0xff},
		ReplicaGUID: [16]byte{0xff, 0xee, 0xdd, 0xcc, 0xbb, 0xaa, 0x99, 0x88, 0x77, 0x66, 0x55, 0x44, 0x33, 0x22, 0x11, 0},
		ReplicaID:   0x0102, GwartTime: 0x1020304050607080,
		LogonTime: time.Date(2026, 11, 16, 1, 2, 3, 0, time.FixedZone("synthetic+02", 2*3600)),
	}
	for i := range v.FolderIDs {
		v.FolderIDs[i] = uint64(0x010203040501+i)<<16 | 0x0102
	}
	return v
}

func TestPrivateLogonResponseIndependentWireFixture(t *testing.T) {
	v := syntheticPrivateLogon()
	before := v
	wire, err := v.Marshal()
	if err != nil || len(wire) != 166 || !bytes.Equal(wire, fixture(t, "privateLogonSuccess")) {
		t.Fatal("private Logon fixed layout, GUID order or UTC encoding", err)
	}
	if !reflect.DeepEqual(before, v) {
		t.Fatal("marshal mutated input")
	}
	// Verify integration with transport framing/MaxRopOut without ever
	// assigning an actual handle or invoking SessionTransport execution.
	framed, err := (ROPBuffer{Operations: wire, Handles: []uint32{0, 123}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := ExecuteResponse(0, framed, nil, uint32(len(framed)-1)); err != ErrMalformed {
		t.Fatal("Logon response bypasses MaxRopOut")
	}
	if _, err := ExecuteResponse(0, framed, nil, uint32(len(framed))); err != nil {
		t.Fatal(err)
	}
}

func TestPrivateLogonResponseRequiresSuppliedIdentitiesAndFlags(t *testing.T) {
	for _, alter := range []func(*PrivateLogonResponse){
		func(v *PrivateLogonResponse) { v.LogonFlags = 0 },
		func(v *PrivateLogonResponse) { v.LogonFlags = 9 },
		func(v *PrivateLogonResponse) { v.ResponseFlags = 0 },
		func(v *PrivateLogonResponse) { v.ResponseFlags |= 0x80 },
		func(v *PrivateLogonResponse) { v.MailboxGUID = [16]byte{} },
		func(v *PrivateLogonResponse) { v.ReplicaGUID = [16]byte{} },
		func(v *PrivateLogonResponse) { v.ReplicaID = 0 },
		func(v *PrivateLogonResponse) { v.FolderIDs[12] = 0 },
		func(v *PrivateLogonResponse) { v.LogonTime = time.Time{} },
		func(v *PrivateLogonResponse) { v.LogonTime = time.Date(65536, 1, 1, 0, 0, 0, 0, time.UTC) },
	} {
		v := syntheticPrivateLogon()
		alter(&v)
		if wire, err := v.Marshal(); err != ErrMalformed || wire != nil {
			t.Fatal("incomplete private response serialized")
		}
	}
}

func FuzzLogonBatch(f *testing.F) {
	f.Add([]byte{0xfe, 0xff, 0, 1, 4, 0, 0, 0, 0, 0, 0, 0, 0, 0})
	f.Add([]byte{0xfe})
	f.Fuzz(func(t *testing.T, data []byte) {
		ops, err := (ROPBuffer{Operations: data, Handles: []uint32{0xffffffff}}).DecodeOperations()
		if err != nil && ops != nil {
			t.Fatal("failed batch returned partial operations")
		}
	})
}
