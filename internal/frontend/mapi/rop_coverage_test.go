// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"
)

func TestExecuteIndependentROPsAroundStoreState(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	f.transport.synchronizationOptionsTrace = true
	list := []byte{0x09, 4, 1}
	state := []byte{ropGetStoreState, 4, 0}
	columns := []byte{0x37, 4, 2}
	faiRunOK(t, f, []byte{0x05, 4, 1, 2, 0x10})
	faiRunOK(t, f, tableColumnsWire(2, 0x674a0014))
	table := f.state.mailbox.objects[f.handles[2]].table
	before := slices.Clone(f.handles)

	wire := slices.Concat(list, state, columns)
	out, counts := f.run(wire)
	tags, rest := listedPropertyTags(t, out, 0x09, 1)
	if !slices.Contains(tags, uint32(0x67480014)) || !slices.Contains(tags, uint32(0x3001001f)) || slices.Contains(tags, ostPropertyTag) {
		t.Fatal("Inbox list does not match projected properties", tags)
	}
	if !bytes.HasPrefix(rest, append32(ropError(ropGetStoreState, 0, 0), 0)) || counts.errors != 0 {
		t.Fatal("missing StoreState in mixed batch")
	}
	tags, rest = listedPropertyTags(t, rest[10:], 0x37, 2)
	if !slices.Contains(tags, uint32(0x674a0014)) || !slices.Contains(tags, uint32(0x67480014)) || len(rest) != 0 {
		t.Fatal("QueryColumnsAll omitted unselected columns", tags)
	}
	if !slices.Equal(before, f.handles) || table.cursor != 0 || !slices.Equal(table.columns, []uint32{0x674a0014}) {
		t.Fatal("enumeration altered handle table, position or selected columns")
	}
	_, _, first, _, _ := counts.execution.Fields()
	if first != "none" {
		t.Fatal("wrong diagnostics", first)
	}
	for _, test := range []struct {
		wire []byte
		code uint32
	}{
		{[]byte{ropGetStoreState, 5, 0}, accessDenied},
		{[]byte{ropGetStoreState, 4, 3}, nullObject},
		{[]byte{ropGetStoreState, 4, 1}, NotSupported},
		{[]byte{0x37, 4, 1}, NotSupported},
		{[]byte{0x09, 4, 2}, NotSupported},
	} {
		out, _ := f.run(test.wire)
		if !bytes.Equal(out, ropError(test.wire[0], test.wire[2], test.code)) {
			t.Fatalf("unexpected object validation: %x", out)
		}
	}
	body, _ := f.execute(slices.Concat([]byte{1, 4, 1}, state, []byte{0x37, 4}), 65535)
	if !bytes.Equal(body, executeError(0x80070057, 65535)) {
		t.Fatalf("malformed suffix was accepted: %x", body)
	}
	if _, code := f.state.mailbox.lookup(f.handles[1], 4); code != 0 {
		t.Fatal("malformed batch released handle")
	}
}

func TestImportMessageMoveStubPreservesBatchFraming(t *testing.T) {
	f, _, _ := newFAIExecuteFixture(t)
	faiRunOK(t, f, []byte{0x7e, 4, 1, 2, 1})
	collector := f.state.mailbox.objects[f.handles[2]].download.collector
	if collector == nil {
		t.Fatal("missing Contents collector")
	}
	replica := [16]byte{1}
	folder, source, target := hierarchyXID(replica, 1), hierarchyXID(replica, 2), hierarchyXID(replica, 3)
	change := hierarchyXID([16]byte{2}, 4)
	op := appendBlob([]byte{0x78, 4, 2}, folder[:])
	op = appendBlob(op, source[:])
	op = appendBlob(op, append([]byte{22}, change[:]...))
	op = appendBlob(op, target[:])
	op = appendBlob(op, change[:])
	properties := []byte{0x09, 4, 1}
	before := slices.Clone(f.handles)
	wire, counts := f.run(slices.Concat(op, properties))
	if !bytes.HasPrefix(wire, ropError(0x78, 2, ropNotImplemented)) || counts.errors != 1 || !slices.Equal(before, f.handles) {
		t.Fatal("move stub displaced the following operation or altered handles")
	}
	if tags, rest := listedPropertyTags(t, wire[6:], 0x09, 1); len(tags) == 0 || len(rest) != 0 {
		t.Fatal("property list after move was not executed")
	}
	op[2] = 1
	if wire, _ := f.run(op); !bytes.Equal(wire, ropError(0x78, 1, NotSupported)) {
		t.Fatal("folder handle accepted in place of the collector")
	}
	if body, _ := f.execute(slices.Concat([]byte{1, 4, 1}, op[:len(op)-1]), 65535); !bytes.Equal(body, executeError(0x80070057, 65535)) {
		t.Fatal("truncated move request did not reject the batch")
	}
	if _, code := f.state.mailbox.lookup(f.handles[1], 4); code != 0 {
		t.Fatal("malformed move executed its prefix")
	}
}

func TestRecipientWriteStubsKeepErrorsLocalToExecuteBatch(t *testing.T) {
	f, _, _ := newFAIExecuteFixture(t)
	f.transport.EnableContentsTableSpike()
	faiRunOK(t, f, createAssociatedWire(1, 3, f.fids[Inbox], 1))
	before := slices.Clone(f.handles)
	remove := []byte{0x0d, 4, 3, 0, 0, 0, 0}
	modify := append32([]byte{0x0e, 4, 3, 1, 0}, 0x39fe001f)
	modify = append(modify, 2, 0)
	modify = append(append32(modify, 7), 1, 0, 0) // Delete one RowId.
	row := append([]byte{0x0b, 0}, []byte("draft@reference.invalid\x00")...)
	row = append(row, 0, 0, 0)
	modify = binary.LittleEndian.AppendUint16(append(append32(modify, 8), 1), uint16(len(row)))
	modify = append(modify, row...)
	list := []byte{0x09, 4, 1}

	out, counts := f.run(slices.Concat(remove, modify, list))
	expect := slices.Concat(ropError(0x0d, 3, ropNotImplemented), ropError(0x0e, 3, ropNotImplemented))
	if !bytes.HasPrefix(out, expect) || counts.errors != 2 {
		t.Fatalf("wrong per-ROP errors: %x", out)
	}
	_, rest := listedPropertyTags(t, out[len(expect):], 0x09, 1)
	if len(rest) != 0 || !slices.Equal(before, f.handles) {
		t.Fatal("recipient errors affected the next response or changed handles")
	}

	for _, wire := range [][]byte{remove, modify} {
		for n := 3; n < len(wire); n++ {
			body, _ := f.execute(slices.Concat([]byte{1, 4, 1}, wire[:n]), 65535)
			if !bytes.Equal(body, executeError(0x80070057, 65535)) {
				t.Fatalf("truncated ROP %#x at %d was accepted: %x", wire[0], n, body)
			}
			if _, code := f.state.mailbox.lookup(f.handles[1], 4); code != 0 {
				t.Fatal("truncated batch released handle")
			}
		}
	}
}

func TestFastTransferDestinationRejectionsPreserveBatch(t *testing.T) {
	f, _, _ := newFAIExecuteFixture(t)
	configure := []byte{ropFastTransferDestinationConfigure, 4, 1, 3, 1, 0}
	put := []byte{ropFastTransferDestinationPutBuffer, 4, 3, 4, 0, 1, 4, 1, 0xff}
	suffix := []byte{0x09, 4, 1}
	before := slices.Clone(f.handles)
	memory, transfers := f.transport.stores.contents.used, f.transport.stores.transfers.Load()
	out, counts := f.run(slices.Concat(configure, put, suffix))
	want := slices.Concat(ropError(ropFastTransferDestinationConfigure, 3, ropNotImplemented),
		ropError(ropFastTransferDestinationPutBuffer, 3, nullObject), []byte{0, 0, 0, 0, 1, 0, 0, 0, 0})
	if !bytes.HasPrefix(out, want) || counts.errors != 2 || !slices.Equal(before, f.handles) ||
		f.transport.stores.contents.used != memory || f.transport.stores.transfers.Load() != transfers {
		t.Fatalf("unexpected upload stub result: %x", out)
	}
	_, rest := listedPropertyTags(t, out[len(want):], 0x09, 1)
	if len(rest) != 0 {
		t.Fatal("upload frame did not preserve the following ROP")
	}
	for _, wire := range [][]byte{configure, put} {
		for n := 1; n < len(wire); n++ {
			body, _ := f.execute(slices.Concat([]byte{1, 4, 1}, wire[:n]), 65535)
			if !bytes.Equal(body, executeError(0x80070057, 65535)) {
				t.Fatalf("accepted truncated upload ROP %#x at %d: %x", wire[0], n, body)
			}
			if _, code := f.state.mailbox.lookup(f.handles[1], 4); code != 0 {
				t.Fatal("malformed frame released the folder")
			}
		}
	}
	configure[3] = byte(len(f.handles))
	if body, _ := f.execute(configure, 65535); !bytes.Equal(body, executeError(0x80070057, 65535)) {
		t.Fatal("accepted an out-of-range output handle")
	}
}

func TestGetRulesTableKeepsErrorsLocalToExecuteBatch(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	f.transport.synchronizationOptionsTrace = true
	list := []byte{0x09, 4, 1}
	for _, tc := range []struct {
		name                string
		logon, input, flags byte
		code                uint32
	}{
		{"plain", 4, 1, 0, ropNotImplemented},
		{"unicode", 4, 1, 0x40, ropNotImplemented},
		{"reserved flag", 4, 1, 0x10, 0x80070057},
		{"wrong logon", 5, 1, 0, accessDenied},
		{"not a folder", 4, 0, 0, NotSupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			before := slices.Clone(f.handles)
			operation := []byte{0x3f, tc.logon, tc.input, 1, tc.flags}
			out, counts := f.run(slices.Concat(operation, list))
			if !bytes.HasPrefix(out, ropError(0x3f, 1, tc.code)) || counts.errors != 1 {
				t.Fatalf("wrong per-ROP error: %x", out)
			}
			_, rest := listedPropertyTags(t, out[6:], 0x09, 1)
			if len(rest) != 0 || !slices.Equal(before, f.handles) {
				t.Fatal("rules error replaced the existing output handle or altered the following response")
			}
			_, ops, _, _, _ := counts.execution.Fields()
			if !slices.Contains(ops, "get_rules_table") {
				t.Fatal("missing execution diagnostic", ops)
			}
		})
	}
	for _, invalid := range [][]byte{{0x3f, 4, 1, 2}, {0x3f, 4, 1, 0xff, 0}} {
		body, _ := f.execute(slices.Concat([]byte{1, 4, 1}, invalid), 65535)
		if !bytes.Equal(body, executeError(0x80070057, 65535)) {
			t.Fatalf("invalid batch did not fail preflight: %x", body)
		}
		if _, code := f.state.mailbox.lookup(f.handles[1], 4); code != 0 {
			t.Fatal("invalid batch released the earlier folder handle")
		}
	}
}

func listedPropertyTags(t *testing.T, out []byte, id, slot byte) ([]uint32, []byte) {
	t.Helper()
	if len(out) < 8 || !bytes.HasPrefix(out, ropError(id, slot, 0)) {
		t.Fatalf("expected property tag list for %02x, got %x", id, out)
	}
	count := int(binary.LittleEndian.Uint16(out[6:]))
	size := 8 + count*4
	if size > len(out) {
		t.Fatal("truncated property tag list")
	}
	tags := make([]uint32, count)
	for i := range tags {
		tags[i] = binary.LittleEndian.Uint32(out[8+i*4:])
	}
	if !slices.Equal(tags, slices.Compact(slices.Clone(tags))) || !slices.IsSorted(tags) {
		t.Fatal("unsorted or duplicate property tags")
	}
	return tags, out[size:]
}

func TestReceiveFolderTableUsesCurrentProjection(t *testing.T) {
	f := newOSTFixture(t)
	op := []byte{0x68, 4, 0}
	type route struct{ fid, modified uint64 }
	read := func() map[string]route {
		out := faiRunOK(t, f, op)
		if len(out) != receiveTableResponseSize || !bytes.HasPrefix(out, ropError(0x68, 0, 0)) || binary.LittleEndian.Uint32(out[6:]) != 4 {
			t.Fatalf("wrong receive folder table header: %x", out)
		}
		routes := make(map[string]route, 4)
		out = out[10:]
		for len(out) > 0 {
			if len(out) < 18 || out[0] != 0 {
				t.Fatal("invalid receive folder row")
			}
			end := 9 + bytes.IndexByte(out[9:], 0)
			if end < 9 || end+9 > len(out) {
				t.Fatal("invalid receive folder class")
			}
			routes[string(out[9:end])] = route{binary.LittleEndian.Uint64(out[1:]), binary.LittleEndian.Uint64(out[end+1:])}
			out = out[end+9:]
		}
		return routes
	}
	first, repeat := read(), read()
	for class, role := range map[string]SpecialFolderRole{"": Inbox, "IPM": Inbox, "Report.IPM": Inbox, "IPC": MailboxRoot} {
		got := first[class]
		if got.fid != f.fids[role] || got.modified == 0 || got != repeat[class] {
			t.Fatal("unbound or unstable route", class)
		}
	}
	f.box.page.Objects[1].ID = "replacement-inbox"
	second := read()
	if second[""] == first[""] || second[""].modified <= first[""].modified || second["IPC"] != first["IPC"] {
		t.Fatal("remapping did not update only the Inbox associations")
	}
	f.box.state.Owner = false
	if out, _ := f.run(op); !bytes.Equal(out, ropError(0x68, 0, accessDenied)) {
		t.Fatal("non-owner read the receive folder table")
	}
}
