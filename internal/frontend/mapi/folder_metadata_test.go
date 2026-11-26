// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ewsgo/internal/core"
)

// Independent synthetic fixture. No captured client values are used.
func metadataOperation(logon, input byte) []byte {
	values := append32(nil, 0x3613001e)
	values = append(values, []byte("IPF.Note\x00")...)
	values = append32(values, 0x36da0102)
	values = append(values, 3, 0, 0xa1, 0xb2, 0xc3)
	values = append32(values, 0x36de0003)
	values = append(values, 0x11, 0x22, 0x33, 0x44)
	h := binary.LittleEndian.AppendUint16([]byte{0xa, logon, input}, uint16(len(values)+2))
	return append(append(h, 3, 0), values...)
}

func metadataExpectedRow(input byte) []byte {
	out := []byte{7, input, 0, 0, 0, 0, 0}
	for _, b := range []byte("IPF.Note") {
		out = append(out, b, 0)
	}
	return append(out, 0, 0, 3, 0, 0xa1, 0xb2, 0xc3, 0x11, 0x22, 0x33, 0x44)
}

func TestSetPropertiesNoReplicateKeepsOpcodeAndBatchState(t *testing.T) {
	f := newOSTFixture(t)
	f.transport.EnableFolderMetadataSpike()
	metadata := metadataOperation(4, 1)
	metadata[0] = 0x79
	ost := ostSetOperation(4, 1, []byte{0xde, 0xad})
	ost[0] = 0x79
	readMetadata := folderPropertiesOperation(4, 1, 1, 0x3613001f, 0x36da0102, 0x36de0003)
	readOST := folderPropertiesOperation(4, 1, 1, ostPropertyTag)
	operations := append(append(append(metadata, ost...), readMetadata...), readOST...)
	got, counts := f.run(operations)
	want := append(propertyProblemsResponse(0x79, 1, nil), propertyProblemsResponse(0x79, 1, nil)...)
	want = append(want, metadataExpectedRow(1)...)
	want = append(want, 7, 1, 0, 0, 0, 0, 0, 2, 0, 0xde, 0xad)
	if !bytes.Equal(got, want) || counts.errors != 0 {
		t.Fatalf("SetPropertiesNoReplicate batch = %x, want %x", got, want)
	}

	bad := ostSetOperation(4, 1, make([]byte, maxOSTValueBytes+1))
	bad[0] = 0x79
	got, _ = f.run(bad)
	want = propertyProblemsResponse(0x79, 1, []faiPropertyProblem{{tag: ostPropertyTag, code: propertyBadValue}})
	if !bytes.Equal(got, want) {
		t.Fatalf("SetPropertiesNoReplicate property problem = %x, want %x", got, want)
	}
}

func TestMetadataDecoderClosedBoundary(t *testing.T) {
	raw := metadataOperation(4, 1)
	b := ROPBuffer{Operations: raw, Handles: []uint32{1, 2}}
	if _, err := b.decodeLaboratoryOperations(true, true, true); err == nil {
		t.Fatal("default metadata enabled")
	}
	ops, err := b.decodeFolderOperations(false, true, true, true)
	if err != nil || len(ops) != 1 || string(ops[0].Metadata[0x3613]) != "IPF.Note" || !bytes.Equal(ops[0].Metadata[0x36da], []byte{0xa1, 0xb2, 0xc3}) {
		t.Fatal("independent fixture")
	}
	if strings.Contains(fmt.Sprintf("%v %#v", ops[0].Metadata, ops[0].Metadata), "IPF.Note") {
		t.Fatal("format leaked")
	}
	for i := range raw {
		raw[i] = 0
	}
	if string(ops[0].Metadata[0x3613]) != "IPF.Note" {
		t.Fatal("aliased input")
	}
	raw = metadataOperation(4, 1)
	for i := 1; i < len(raw); i++ {
		if _, err := (ROPBuffer{Operations: raw[:i], Handles: b.Handles}).decodeFolderOperations(false, true, true, true); err == nil {
			t.Fatal("truncation")
		}
	}
	for _, alter := range []func([]byte){
		func(b []byte) { b[7] = 0x0b },  // wrong class type
		func(b []byte) { b[9] = 0x14 },  // unknown ID
		func(b []byte) { b[11] = 0xff }, // non-ASCII class
		func(b []byte) { b[3]-- }, func(b []byte) { b[3]++ },
	} {
		bad := bytes.Clone(raw)
		alter(bad)
		if _, err := (ROPBuffer{Operations: bad, Handles: b.Handles}).decodeFolderOperations(false, true, true, true); err == nil {
			t.Fatal("bad input")
		}
	}
	dup := append32([]byte{0xa, 4, 1, 0, 0, 2, 0}, 0x36de0003)
	dup = append32(dup, 1)
	dup = append32(dup, 0x36de0003)
	dup = append32(dup, 2)
	binary.LittleEndian.PutUint16(dup[3:], uint16(len(dup)-5))
	if _, err := (ROPBuffer{Operations: dup, Handles: b.Handles}).decodeFolderOperations(false, true, true, true); err == nil {
		t.Fatal("duplicate ID")
	}
	unicode := append32([]byte{0xa, 4, 1, 0, 0, 1, 0}, 0x3613001f)
	unicode = append(unicode, 'A', 0, 'B', 0, 0, 0)
	binary.LittleEndian.PutUint16(unicode[3:], uint16(len(unicode)-5))
	parsed, err := (ROPBuffer{Operations: unicode, Handles: b.Handles}).decodeFolderOperations(false, true, true, true)
	if err != nil || string(parsed[0].Metadata[0x3613]) != "AB" {
		t.Fatal("Unicode class normalization")
	}
}

func TestMetadataExecuteReadReconnectAndGates(t *testing.T) {
	f := newOSTFixture(t)
	body, _ := f.execute(metadataOperation(4, 1), 4096)
	if len(body) < 8 || binary.LittleEndian.Uint32(body[4:]) != NotSupported {
		t.Fatal("default enabled")
	}
	f.transport.EnableFolderMetadataSpike()
	read := folderPropertiesOperation(4, 1, 1, 0x3613001f, 0x36da0102, 0x36de0003)
	rop, counts := f.run(append(metadataOperation(4, 1), read...))
	want := []byte{0xa, 1, 0, 0, 0, 0, 0, 0, 7, 1, 0, 0, 0, 0, 0}
	for _, b := range []byte("IPF.Note") {
		want = append(want, b, 0)
	}
	want = append(want, 0, 0, 3, 0, 0xa1, 0xb2, 0xc3, 0x11, 0x22, 0x33, 0x44)
	if !bytes.Equal(rop, want) || counts.properties.Values != 3 || counts.errors != 0 {
		t.Fatal("wire/read-your-write")
	}
	f.state.mailbox = newMailboxObjects(f.transport.stores)
	f.handles = []uint32{0xffffffff, 0xffffffff}
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	f.open(int(Inbox))
	got, _ := f.run(read)
	if !bytes.Equal(got, want[8:]) {
		t.Fatal("reconnect lost metadata")
	}
	f.open(int(SentItems))
	missing, _ := f.run(read)
	if binary.LittleEndian.Uint32(missing[2:]) != 0 || missing[6] != 1 {
		t.Fatal("missing folder metadata")
	}
	f.open(int(Inbox))
	f.transport.folderMetadataSpike = false
	_, cnt := f.run(read)
	if cnt.properties.Values != 0 {
		t.Fatal("read gate")
	}
	f.transport.EnableFolderMetadataSpike()
	opaque, _ := f.run(folderPropertiesOperation(4, 1, 1, 0x36de0000))
	if !bytes.Equal(opaque, []byte{7, 1, 0, 0, 0, 0, 1, 3, 0, 0, 0x11, 0x22, 0x33, 0x44}) {
		t.Fatal("unspecified integer resolution")
	}
	obj, _ := f.state.mailbox.object(f.handles[1], 4)
	if _, code := obj.binding.store.setMetadata(context.Background(), f.session, obj.fid, folderMetadata{0x36da: nil}); code != 0 {
		t.Fatal("empty binary set")
	}
	empty, _ := f.run(folderPropertiesOperation(4, 1, 0, 0x36da0102))
	if !bytes.Equal(empty, []byte{7, 1, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatal("empty binary is not absence")
	}
}

func TestAdditionalEntryIDsExSharedBetweenRootAndInbox(t *testing.T) {
	f := newOSTFixture(t)
	f.transport.EnableFolderMetadataSpike()
	f.handles = append(f.handles, 0xffffffff)
	f.run(folderOperation(f.fids[MailboxRoot], 4, 0, 2))
	tag := uint32(0x36d90102)
	// PersistData with an unknown ID, preserved verbatim.
	value := []byte{0xfe, 0x8f, 3, 0, 1, 2, 3}
	write := func(input byte, value []byte, withClass bool) []byte {
		props := binary.LittleEndian.AppendUint16(append32(nil, tag), uint16(len(value)))
		props = append(props, value...)
		count := byte(1)
		if withClass {
			props = append(append32(props, 0x3613001e), []byte("IPF.Note\x00")...)
			count++
		}
		op := binary.LittleEndian.AppendUint16([]byte{0xa, 4, input}, uint16(len(props)+2))
		return append(append(op, count, 0), props...)
	}
	check := func(out []byte, slot byte, value []byte) {
		t.Helper()
		want := binary.LittleEndian.AppendUint16(append(ropError(7, slot, 0), 0), uint16(len(value)))
		want = append(want, value...)
		if !bytes.Equal(out, want) {
			t.Fatal("unexpected binary property response")
		}
	}
	// The same batch writes to the root and immediately reads from Inbox.
	out, _ := f.run(append(write(2, value, true), folderPropertiesOperation(4, 1, 0, tag)...))
	if !bytes.Equal(out[:8], append(ropError(0xa, 2, 0), 0, 0)) {
		t.Fatal("mixed root write failed")
	}
	check(out[8:], 1, value)
	root, _ := f.state.mailbox.object(f.handles[2], 4)
	inbox, _ := f.state.mailbox.object(f.handles[1], 4)
	if string(root.binding.store.metadata[root.fid][0x3613]) != "IPF.Note" ||
		len(root.binding.store.metadata[inbox.fid][0x3613]) != 0 {
		t.Fatal("unrelated properties moved to Inbox")
	}
	out, _ = f.run(append(write(1, nil, false), folderPropertiesOperation(4, 2, 0, tag)...))
	check(out[8:], 2, nil)
	out, _ = f.run(write(2, make([]byte, maxMetadataBytes), true))
	if !bytes.Equal(out, ropError(0xa, 2, 0x8007000e)) {
		t.Fatal("oversized binary accepted")
	}
	out, _ = f.run(folderPropertiesOperation(4, 1, 0, tag))
	check(out, 1, nil)
}

func TestMetadataPreflightAuthorizationAndQuota(t *testing.T) {
	f := newOSTFixture(t)
	f.transport.EnableFolderMetadataSpike()
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	s := object.binding.store
	for _, ops := range [][]byte{append([]byte{1, 4, 1}, metadataOperation(4, 1)[:15]...), append([]byte{1, 4, 1}, setListOperation(4, 1)...), append(metadataOperation(4, 1), 0xff, 4, 1)} {
		body, _ := f.execute(ops, 65535)
		if len(body) < 8 || binary.LittleEndian.Uint32(body[4:]) == 0 || len(s.metadata) != 0 {
			t.Fatal("invalid batch effects")
		}
		if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
			t.Fatal("prefix Release executed")
		}
	}
	body, _ := f.execute(append([]byte{1, 4, 1}, metadataOperation(4, 1)...), 1)
	if len(s.metadata) != 0 || len(body) == 0 {
		t.Fatal("small output effects")
	}
	f.run(metadataOperation(4, 1))
	before := cloneMetadata(s.metadata[object.fid])
	for _, tc := range []struct {
		name    string
		alter   func()
		restore func()
		code    uint32
	}{
		{"owner", func() { f.box.state.Owner = false }, func() { f.box.state.Owner = true }, accessDenied},
		{"access", func() { f.box.getErr = core.ErrAccessDenied }, func() { f.box.getErr = nil }, accessDenied},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.alter()
			rop, _ := f.run(metadataOperation(4, 1))
			tc.restore()
			if !bytes.Equal(rop, ropError(0xa, 1, tc.code)) || !reflect.DeepEqual(s.metadata[object.fid], before) {
				t.Fatal("authorization")
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, code := s.setMetadata(ctx, f.session, object.fid, folderMetadata{0x36de: []byte{9, 9, 9, 9}}); code == 0 || !reflect.DeepEqual(s.metadata[object.fid], before) {
		t.Fatal("canceled commit")
	}
	if _, code := s.setMetadata(context.Background(), f.session, object.fid, folderMetadata{0x36da: make([]byte, maxMetadataBytes)}); code != 0x8007000e || !reflect.DeepEqual(s.metadata[object.fid], before) {
		t.Fatal("byte quota atomicity")
	}
	large := folderMetadata{0x36da: make([]byte, maxMetadataBytes-8)}
	for _, role := range []SpecialFolderRole{MailboxRoot, SentItems, DeletedItems, Outbox, Views, CommonViews, Search} {
		if _, code := s.setMetadata(context.Background(), f.session, f.fids[role], large); code != 0 {
			t.Fatal("quota setup")
		}
	}
	used := s.metadataBytes
	if _, code := s.setMetadata(context.Background(), f.session, f.fids[Shortcuts], large); code != 0x8007000e || s.metadataBytes != used || s.metadata[f.fids[Shortcuts]] != nil {
		t.Fatal("store quota atomicity")
	}
	i := f.session.Identity()
	i.Scope.Account = "foreign"
	if _, code := s.setMetadata(context.Background(), scopedSession{Session: f.session, identity: i}, object.fid, before); code != accessDenied {
		t.Fatal("scope")
	}
	rop, _ := f.run(metadataOperation(4, 0))
	if !bytes.Equal(rop, ropError(0xa, 0, NotSupported)) {
		t.Fatal("Logon accepted metadata")
	}
	rop, _ = f.run(metadataOperation(5, 1))
	if !bytes.Equal(rop, ropError(0xa, 1, accessDenied)) {
		t.Fatal("wrong Logon")
	}
	f.run([]byte{1, 4, 1})
	rop, _ = f.run(metadataOperation(4, 1))
	if !bytes.Equal(rop, ropError(0xa, 1, invalidObject)) {
		t.Fatal("released handle")
	}
}

func TestMetadataSnapshotSizingAndHierarchy(t *testing.T) {
	f := newOSTFixture(t)
	f.transport.EnableFolderMetadataSpike()
	obj, _ := f.state.mailbox.object(f.handles[1], 4)
	s := obj.binding.store
	_, code := s.setMetadata(context.Background(), f.session, obj.fid, folderMetadata{0x36da: []byte{1}})
	if code != 0 {
		t.Fatal("set")
	}
	ops := []Operation{{Properties: &specificProperties{tags: []uint32{0x36da0102}}}}
	v := f.transport.metadataView(f.session.Identity().Scope, ops)
	_, code = s.setMetadata(context.Background(), f.session, obj.fid, folderMetadata{0x36da: make([]byte, 32000)})
	if code != 0 {
		t.Fatal("grow")
	}
	if len(v.property(obj.fid, 0x36da0102, false, 1252).data) != 3 || v.bound(0x36da0102) != 4 {
		t.Fatal("snapshot grew")
	}
	tooLarge := append([]byte{1, 4, 1}, folderPropertiesOperation(4, 1, 0, 0x36da0102, 0x36da0102, 0x36da0102)...)
	f.execute(tooLarge, 65535)
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
		t.Fatal("oversize output executed Release")
	}
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; i < 40; i++ {
			s.setMetadata(context.Background(), f.session, obj.fid, folderMetadata{0x36da: make([]byte, 1+(i%2)*31000)})
		}
	})
	for range 40 {
		body, _ := f.execute(folderPropertiesOperation(4, 1, 0, 0x36da0102), 4096)
		if len(body) > 8 {
			r := storeResponse(t, body)
			if len(r.Operations) > 4096 {
				t.Fatal("read escaped output")
			}
		}
	}
	wg.Wait()
	h := newHierarchyFixture(t)
	fid := h.binding.fids[Inbox]
	a := h.snapshot(t, MailboxRoot, hierarchyTestTime)
	if _, code := h.store.setMetadata(context.Background(), h.session, fid, folderMetadata{0x36de: []byte{1, 2, 3, 4}}); code != 0 {
		t.Fatal("hierarchy metadata")
	}
	b := h.snapshot(t, MailboxRoot, hierarchyTestTime.Add(time.Second))
	if hierarchyRow(t, a, fid).cn == hierarchyRow(t, b, fid).cn {
		t.Fatal("metadata did not advance CN")
	}
	h.store.setMetadata(context.Background(), h.session, fid, folderMetadata{0x36de: []byte{1, 2, 3, 4}})
	c := h.snapshot(t, MailboxRoot, hierarchyTestTime.Add(2*time.Second))
	if !reflect.DeepEqual(b, c) {
		t.Fatal("identical metadata changed hierarchy")
	}
}

func FuzzFolderMetadataDecoder(f *testing.F) {
	f.Add(metadataOperation(4, 1))
	f.Add(additionalEntryIDsOperation(4, 1, syntheticAdditionalEntryIDs()))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, raw []byte) {
		ops, err := (ROPBuffer{Operations: raw, Handles: []uint32{1, 2, 3, 4}}).decodeFolderOperations(true, true, true, true)
		if err != nil {
			return
		}
		for _, op := range ops {
			if len(op.Metadata) > 4 {
				t.Fatal("unbounded bag")
			}
			for id, data := range op.Metadata {
				if !metadataID(id) || len(data) > 65535 {
					t.Fatal("unexpected metadata")
				}
			}
		}
	})
}
