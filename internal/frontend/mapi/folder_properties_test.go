// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"testing"

	"ewsgo/internal/core"
)

func folderPropertiesOperation(logon, input byte, unicode uint16, tags ...uint32) []byte {
	out := []byte{7, logon, input, 1, 0} // PropertySizeLimit=1 is deliberately ignored.
	out = binary.LittleEndian.AppendUint16(out, unicode)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(tags)))
	for _, tag := range tags {
		out = binary.LittleEndian.AppendUint32(out, tag)
	}
	return out
}

func TestFolderPropertiesRequestFixtureBoundsAndWholeBatch(t *testing.T) {
	wire := []byte{7, 0x91, 1, 0xff, 0xff, 2, 0, 2, 0, 0x1f, 0, 1, 0x30, 0x14, 0, 0x48, 0x67}
	ops, err := (ROPBuffer{Operations: wire, Handles: []uint32{1, 2}}).DecodeOperations()
	if err != nil || len(ops) != 1 || ops[0].Logon != 0x91 || ops[0].Input != 1 || !ops[0].Properties.wantUnicode || ops[0].Properties.sizeLimit != 65535 || len(ops[0].Properties.tags) != 2 || ops[0].Properties.tags[0] != 0x3001001f {
		t.Fatal("request fixture")
	}
	for n := 1; n < len(wire); n++ {
		if partial, err := (ROPBuffer{Operations: wire[:n], Handles: []uint32{1, 2}}).DecodeOperations(); err == nil || partial != nil {
			t.Fatal("truncation accepted", n)
		}
	}
	for _, tc := range []struct {
		data []byte
		err  error
	}{
		{folderPropertiesOperation(0, 2, 0), ErrMalformed},
		{append(bytes.Clone(wire), ropFastTransferSourceCopyFolder, 0, 0), ErrUnsupported},
		{append(bytes.Clone(wire), 2, 0, 0), ErrMalformed},
	} {
		if ops, err := (ROPBuffer{Operations: tc.data, Handles: []uint32{1, 2}}).DecodeOperations(); !errors.Is(err, tc.err) || ops != nil {
			t.Fatal("partial batch on invalid suffix", err)
		}
	}
	for _, n := range []int{0, 256, 385, (65535 - 2 - 4 - 9) / 4} {
		data := folderPropertiesOperation(0, 0, 0, make([]uint32, n)...)
		if ops, err := (ROPBuffer{Operations: data, Handles: []uint32{1}}).DecodeOperations(); err != nil || len(ops[0].Properties.tags) != n {
			t.Fatal("valid count bound", n)
		}
		if ops, err := (ROPBuffer{Operations: data[:len(data)-1], Handles: []uint32{1}}).DecodeOperations(); !errors.Is(err, ErrMalformed) || ops != nil {
			t.Fatal("truncated property array accepted", n)
		}
	}
}

func TestFolderPropertiesIndependentRowsAndErrors(t *testing.T) {
	folder := storeFolder{fid: 0x0807060504030201, parent: 0x1817161514131211, properties: core.Properties{core.DisplayName: core.Text("A€")}}
	for _, tc := range []struct {
		tags    []uint32
		unicode bool
		cp      uint32
		want    []byte
	}{
		{[]uint32{0x3001001f, 0x67480014, 0x67490014}, false, 1252, []byte{0, 'A', 0, 0xac, 0x20, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8, 0x11, 0x12, 0x13, 0x14, 0x15, 0x16, 0x17, 0x18}},
		{[]uint32{0x3001001e, 0x3001001e}, true, 1252, []byte{0, 'A', 0x80, 0, 'A', 0x80, 0}},
		{[]uint32{0x30010000}, true, 1252, []byte{1, 0x1f, 0, 0, 'A', 0, 0xac, 0x20, 0, 0}},
		{[]uint32{0x30010000}, false, 1252, []byte{1, 0x1e, 0, 0, 'A', 0x80, 0}},
		{[]uint32{0x67480000}, false, 1252, []byte{1, 0x14, 0, 0, 1, 2, 3, 4, 5, 6, 7, 8}},
		{[]uint32{0x0ffe0003, 0x0fff0102}, false, 1252, []byte{1, 0x0a, 2, 1, 4, 0x80, 0x0a, 2, 1, 4, 0x80}},
		{[]uint32{0x0ffe0000}, false, 1252, []byte{1, 0x0a, 0, 0x0a, 2, 1, 4, 0x80}},
		{[]uint32{0x3001001e}, false, 1200, []byte{1, 0x0a, 0x1e, 1, 4, 0x80}},
		{nil, false, 1252, []byte{0}},
	} {
		r := specificProperties{tags: tc.tags, wantUnicode: tc.unicode}
		values := make([]folderPropertyValue, len(tc.tags))
		for i, tag := range tc.tags {
			values[i] = projectFolderProperty(folder, tag, tc.unicode, tc.cp)
		}
		got := folderPropertyRow(r, values)
		if !bytes.Equal(got, tc.want) || len(got)+6 > specificPropertiesBound(r, -1) {
			t.Fatalf("PropertyRow fixture mismatch: %x", got)
		}
	}
	for _, tag := range []uint32{0x67490014, 0x3001001f, 0x36020003, 0x36030003} {
		value := projectFolderProperty(storeFolder{}, tag, false, 1252)
		if value.code != PropertyNotFound {
			t.Fatal("missing field fabricated")
		}
	}
	for _, tag := range []uint32{0x67480003, 0x30010102, 0x8001001f, 0x36020102} {
		if projectFolderProperty(folder, tag, false, 1252).code != NotSupported {
			t.Fatal("unsupported field or type fabricated")
		}
	}
	for _, tc := range []struct {
		name string
		code uint32
	}{
		{strings.Repeat("a", 2047), 0}, {strings.Repeat("a", 2048), 0x8007000e}, {"invalid\x00name", 0x80070057}, {"\xff", 0x80070057},
	} {
		folder.properties[core.DisplayName] = core.Text(tc.name)
		if projectFolderProperty(folder, 0x3001001f, false, 1252).code != tc.code {
			t.Fatal("string limit, validity or truncation")
		}
	}
}

func TestFolderPropertiesBatchFreshReadsAuthorizationAndDiagnostics(t *testing.T) {
	transport, session, box, cookies := storeFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	body, cookies := storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 4, 0), []uint32{0xffffffff, 0xffffffff}, 4096)
	logon := storeResponse(t, body)
	fid := binary.LittleEndian.Uint64(logon.Operations[7+int(Inbox)*8:])
	ops := append(folderOperation(fid, 4, 0, 1), folderPropertiesOperation(4, 1, 0, 0x67480014, 0x0ffe0003)...)
	body, cookies = storeExecute(t, transport, session, cookies, ops, logon.Handles, 4096)
	opened := storeResponse(t, body)
	want := append([]byte{2, 1, 0, 0, 0, 0, 0, 0, 7, 1, 0, 0, 0, 0, 1, 0}, binary.LittleEndian.AppendUint64(nil, fid)...)
	want = append(want, 0x0a, 2, 1, 4, 0x80)
	if !bytes.Equal(opened.Operations, want) || opened.Handles[1] == 0xffffffff {
		t.Fatal("same-batch Folder property read")
	}
	var state *contextState
	for _, s := range transport.contexts.entries {
		state = s
	}
	read := func(logon, input byte, tags ...uint32) ([]byte, storeResults) {
		t.Helper()
		wire, _ := (ROPBuffer{Operations: folderPropertiesOperation(logon, input, 1, tags...), Handles: opened.Handles}).Marshal()
		body, counts := transport.executeStore(context.Background(), session, state, identity, Execute{ROP: wire, MaxOutput: 10000})
		return storeResponse(t, body).Operations, counts
	}
	box.page.Objects[1].Properties[core.DisplayName] = core.Text("Renamed")
	row, counts := read(4, 1, 0x3001001f, 0x67480014, 0x67490014, 0x0ffe0003, 0x70726976)
	if counts.properties.Requests != 1 || counts.properties.Requested != 5 || counts.properties.Values != 3 || counts.properties.Errors != 2 || counts.properties.Other != 1 || !counts.properties.Folder || counts.properties.Logon || !slices.Contains(counts.properties.PropertyNames(), "object_type") || !bytes.Contains(row, []byte{'R', 0, 'e', 0, 'n', 0}) {
		t.Fatal("fresh read or diagnostic counts")
	}
	batch := append(folderPropertiesOperation(4, 1, 1, 0x0fff0102, 0x0fff0000, 0x7ffd0000), folderPropertiesOperation(4, 1, 1, 0x36020003, 0x0fff0102, 0x67480014)...)
	wire, _ := (ROPBuffer{Operations: batch, Handles: opened.Handles}).Marshal()
	_, batchCounts := transport.executeStore(context.Background(), session, state, identity, Execute{ROP: wire, MaxOutput: 10000})
	diagnostic := batchCounts.properties
	if !slices.Equal(diagnostic.PropertyNames(), []string{"folder_id", "entry_id", "content_count"}) || diagnostic.Requests != 2 || diagnostic.Requested != 6 || diagnostic.Values != 4 || diagnostic.Errors != 2 || diagnostic.Other != 1 || !diagnostic.Folder || diagnostic.Logon || batchCounts.errors != 0 {
		t.Fatal("executed batch names, duplicates or value/error counts")
	}
	row, counts = read(4, 0, 0x3001001f)
	if !bytes.Equal(row, []byte{7, 0, 0, 0, 0, 0, 1, 0x0a, 2, 1, 4, 0x80}) || !counts.properties.Logon || counts.properties.Folder || counts.properties.Values != 0 || counts.properties.Errors != 1 || counts.errors != 0 {
		t.Fatal("Logon converted to Folder")
	}
	row, counts = read(5, 1, 0x67480014)
	if binary.LittleEndian.Uint32(row[2:]) != accessDenied || counts.properties.Folder || counts.properties.Logon {
		t.Fatal("foreign Logon accepted or claimed verified kind")
	}
	box.getErr = core.ErrAccessDenied
	row, _ = read(4, 1, 0x67480014)
	if binary.LittleEndian.Uint32(row[2:]) != accessDenied {
		t.Fatal("cached Folder bypassed revoked access")
	}
	box.getErr = nil
	box.page.Objects = append(box.page.Objects[:1], box.page.Objects[2:]...)
	row, _ = read(4, 1, 0x67480014)
	if binary.LittleEndian.Uint32(row[2:]) != PropertyNotFound {
		t.Fatal("deleted Folder remained readable")
	}
}

func TestFolderPropertiesPreflightRejectsWithoutHandleEffects(t *testing.T) {
	transport, session, _, cookies := storeFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	body, cookies := storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 0, 0), []uint32{0xffffffff, 0xffffffff}, 4096)
	logon := storeResponse(t, body)
	fid := binary.LittleEndian.Uint64(logon.Operations[7+int(Outbox)*8:])
	ops := append(folderOperation(fid, 0, 0, 1), folderPropertiesOperation(0, 1, 0, 0x67480014)...)
	body, cookies = storeExecute(t, transport, session, cookies, ops, logon.Handles, 41)
	row := storeResponse(t, body)
	if row.Operations[0] != 0xff || binary.LittleEndian.Uint16(row.Operations[1:]) != 42 || row.Handles[1] != 0xffffffff {
		t.Fatal("Folder property output reservation")
	}
	body, cookies = storeExecute(t, transport, session, cookies, ops, logon.Handles, 42)
	opened := storeResponse(t, body)
	if opened.Handles[1] == 0xffffffff || len(opened.Operations) != 23 {
		t.Fatal("reserved output exact fit")
	}
	for _, suffix := range [][]byte{[]byte{8, 0, 0}, []byte{7, 0, 0}} {
		body, cookies = storeExecute(t, transport, session, cookies, append([]byte{1, 0, 1}, suffix...), opened.Handles, 4096)
		if binary.LittleEndian.Uint32(body[4:]) == 0 {
			t.Fatal("bad suffix accepted")
		}
		for _, state := range transport.contexts.entries {
			if _, code := state.mailbox.object(opened.Handles[1], 0); code != 0 {
				t.Fatal("invalid suffix executed Release")
			}
		}
	}
	large := folderPropertiesOperation(0, 1, 1, makeRepeatedTags(256, 0x3001001f)...)
	body, _ = storeExecute(t, transport, session, cookies, append([]byte{1, 0, 1}, large...), opened.Handles, 65535)
	if binary.LittleEndian.Uint32(body) != 0x47d {
		t.Fatal("single-buffer upper bound bypassed")
	}
	for _, state := range transport.contexts.entries {
		if _, code := state.mailbox.object(opened.Handles[1], 0); code != 0 {
			t.Fatal("oversized response executed Release")
		}
	}
}

func TestFolderAccessAndOptionalBootstrapProperties(t *testing.T) {
	transport, session, box, cookies := storeFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	body, cookies := storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 0, 0), []uint32{0xffffffff, 0xffffffff}, 4096)
	logon := storeResponse(t, body)
	fid := binary.LittleEndian.Uint64(logon.Operations[7+int(Inbox)*8:])
	tags := folderPropertiesOperation(0, 1, 1, 0x0ff40003, 0x66720102, 0x0ff40000, 0x35ff0102, 0x0ff40102)
	ops := append(folderOperation(fid, 0, 0, 1), tags...)
	ops = append(ops, folderPropertiesOperation(0, 0, 1, 0x0ea00048)...)
	body, cookies = storeExecute(t, transport, session, cookies, ops, logon.Handles, 4096)
	opened := storeResponse(t, body)
	want := []byte{7, 1, 0, 0, 0, 0, 1,
		0, 2, 0, 0, 0,
		0x0a, 0x0f, 1, 4, 0x80,
		3, 0, 0, 2, 0, 0, 0,
		0x0a, 0x0f, 1, 4, 0x80,
		0x0a, 2, 1, 4, 0x80,
		7, 0, 0, 0, 0, 0, 1, 0x0a, 0x0f, 1, 4, 0x80}
	if !bytes.Equal(opened.Operations[8:], want) {
		t.Fatalf("property rows differ: %x", opened.Operations[8:])
	}

	transport.EnableCreateFolderSpike()
	transport.EnableFolderMetadataSpike()
	transport.EnableFAICollectorSpike()
	access := folderPropertiesOperation(0, 1, 1, 0x0ff40003)
	body, cookies = storeExecute(t, transport, session, cookies, access, opened.Handles, 4096)
	if got := storeResponse(t, body).Operations; !bytes.Equal(got, []byte{7, 1, 0, 0, 0, 0, 0, 0x2b, 0, 0, 0}) {
		t.Fatalf("folder access differs with writes enabled: %x", got)
	}
	box.state.Owner = false
	body, _ = storeExecute(t, transport, session, cookies, access, opened.Handles, 4096)
	if got := storeResponse(t, body).Operations; !bytes.Equal(got, []byte{7, 1, 0, 0, 0, 0, 1, 0x0a, 5, 0, 7, 0x80}) {
		t.Fatal("existing Folder handle retained the owner's access mask")
	}
}

func makeRepeatedTags(n int, tag uint32) []uint32 {
	out := make([]uint32, n)
	for i := range out {
		out[i] = tag
	}
	return out
}

func FuzzFolderPropertiesOperation(f *testing.F) {
	f.Add(folderPropertiesOperation(0, 0, 1, 0x3001001f, 0x67480014))
	f.Add(folderPropertiesOperation(0, 0, 0))
	f.Fuzz(func(t *testing.T, data []byte) {
		ops, err := (ROPBuffer{Operations: data, Handles: []uint32{1, 2}}).DecodeOperations()
		if err != nil && ops != nil {
			t.Fatal("partial batch on error")
		}
		for _, op := range ops {
			if op.Properties != nil && len(op.Properties.tags) > 256 {
				t.Fatal("property count bound")
			}
		}
	})
}
