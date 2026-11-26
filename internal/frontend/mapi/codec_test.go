// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"testing"
)

func fixture(t *testing.T, key string) []byte {
	t.Helper()
	raw, err := os.ReadFile("testdata/fixtures.json")
	if err != nil {
		t.Fatal(err)
	}
	var v map[string]string
	if json.Unmarshal(raw, &v) != nil {
		t.Fatal("fixture JSON")
	}
	b, err := hex.DecodeString(v[key])
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func TestSyntheticFixtures(t *testing.T) {
	connect := fixture(t, "connect")
	c, err := ParseConnect(connect)
	if err != nil || c.CodePage != 1257 || c.UserDN != "/o=EWSGo/cn=Synthetic" {
		t.Fatalf("connect fixture: %v", err)
	}
	out, err := c.Marshal()
	if err != nil || !bytes.Equal(out, connect) {
		t.Fatal("connect round trip")
	}
	rop := fixture(t, "ropRelease")
	r, err := ParseROP(rop)
	if err != nil {
		t.Fatal(err)
	}
	ops, err := r.DecodeOperations()
	if err != nil || len(ops) != 1 || ops[0].ID != 1 || r.Handles[0] != 111 {
		t.Fatal("ROP fixture")
	}
	out, err = r.Marshal()
	if err != nil || !bytes.Equal(out, rop) {
		t.Fatal("ROP round trip")
	}
	execute := fixture(t, "executeRelease")
	e, err := ParseExecute(execute)
	if err != nil || e.MaxOutput != 4096 || !bytes.Equal(e.ROP, rop) {
		t.Fatalf("execute fixture: %v", err)
	}
	out, err = e.Marshal()
	if err != nil || !bytes.Equal(out, execute) {
		t.Fatal("execute round trip")
	}
	folder, err := ParseROP(fixture(t, "ropOpenFolder"))
	if err != nil {
		t.Fatal(err)
	}
	opened, err := folder.DecodeOperations()
	if err != nil || len(opened) != 1 || opened[0].Folder != 0x0102030405060708 || opened[0].Output != 1 {
		t.Fatal("OpenFolder layout")
	}

	for cut := range len(connect) {
		if _, err := ParseConnect(connect[:cut]); err == nil {
			t.Fatalf("truncated connect accepted: %d", cut)
		}
	}
	for cut := range len(execute) {
		if _, err := ParseExecute(execute[:cut]); err == nil {
			t.Fatalf("truncated execute accepted: %d", cut)
		}
	}
	oversized := bytes.Clone(execute)
	binary.LittleEndian.PutUint32(oversized[4:], 0xffffffff)
	if _, err := ParseExecute(oversized); !errors.Is(err, ErrMalformed) {
		t.Fatal("unbounded length accepted")
	}
	compressed := bytes.Clone(rop)
	binary.LittleEndian.PutUint16(compressed[2:], 5)
	binary.LittleEndian.PutUint16(compressed[6:], 10)
	if _, err := ParseROP(compressed); !errors.Is(err, ErrUnsupported) {
		t.Fatal("valid unsupported framing misclassified")
	}
	invalid := bytes.Clone(rop)
	binary.LittleEndian.PutUint16(invalid[6:], 8)
	if _, err := ParseROP(invalid); !errors.Is(err, ErrMalformed) {
		t.Fatal("size mismatch accepted")
	}
	r.Operations = []byte{1, 0, 1}
	if _, err := r.DecodeOperations(); !errors.Is(err, ErrMalformed) {
		t.Fatal("bad handle index accepted")
	}
	r.Operations = []byte{0xff, 0, 0}
	if _, err := r.DecodeOperations(); !errors.Is(err, ErrUnsupported) {
		t.Fatal("unknown ROP classified as malformed")
	}
}
func TestResponseLayouts(t *testing.T) {
	response, err := ExecuteResponse(NotSupported, nil, nil, 10)
	if err != nil || len(response) != 20 || binary.LittleEndian.Uint32(response[4:]) != NotSupported {
		t.Fatal("execute response")
	}
	if _, err := ExecuteResponse(0, []byte{1, 2}, nil, 1); err == nil {
		t.Fatal("MaxRopOut ignored")
	}
	response, err = ConnectResponse(0, "/o=EWSGo/", "Synthetic")
	if err != nil || binary.LittleEndian.Uint32(response) != 0 || !bytes.Contains(response, []byte{'S', 0, 'y', 0}) {
		t.Fatal("connect Unicode response")
	}
	if len(Failure(NotSupported)) != 8 || binary.LittleEndian.Uint32(Failure(0)) == 0 {
		t.Fatal("failure became success")
	}
}
func FuzzWireReaders(f *testing.F) {
	f.Add([]byte{})
	f.Add([]byte{0, 0, 4, 0, 2, 0, 2, 0, 2, 0})
	f.Fuzz(func(t *testing.T, data []byte) {
		_, _ = ParseConnect(data)
		_, _ = ParseExecute(data)
		if r, err := ParseROP(data); err == nil {
			_, _ = r.DecodeOperations()
		}
	})
}
