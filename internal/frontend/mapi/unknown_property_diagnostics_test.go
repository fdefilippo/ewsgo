// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/binary"
	"reflect"
	"testing"
)

func TestUnknownPropertyTraceDefaultsOffAndFiltersIDSpace(t *testing.T) {
	for id := 0; id <= 0xffff; id++ {
		tag := uint32(id)<<16 | 0x0102
		var off StorePropertyDiagnostic
		off.request([]uint32{tag})
		if off.unknown.count != 0 || off.unknown.tags != [16]uint32{} || off.unknown.overflow != 0 || off.unknown.highIDs != 0 || off.unknown.reservedIDs != 0 {
			t.Fatal("default diagnostic retained request tag", id)
		}
		var on StorePropertyDiagnostic
		on.unknown.enabled = true
		on.request([]uint32{tag})
		snapshot := on.UnknownProperties()
		if publicStorePropertyIndex(uint16(id)) >= 0 {
			if on.Other != 0 || len(snapshot.Tags) != 0 || snapshot.HighIDs != 0 || snapshot.ReservedIDs != 0 {
				t.Fatal("known property treated as unknown", id)
			}
		} else if id == 0 || id == 0xffff {
			if on.Other != 1 || snapshot.ReservedIDs != 1 || len(snapshot.Tags) != 0 || snapshot.HighIDs != 0 {
				t.Fatal("reserved ID tag exposed", id)
			}
		} else if id >= 0x8000 {
			if on.Other != 1 || snapshot.HighIDs != 1 || len(snapshot.Tags) != 0 || snapshot.ReservedIDs != 0 {
				t.Fatal("high ID tag exposed", id)
			}
		} else if on.Other != 1 || len(snapshot.Tags) != 1 || snapshot.HighIDs != 0 || snapshot.ReservedIDs != 0 {
			t.Fatal("low unknown property omitted", id)
		}
		if snapshot.Overflow != 0 {
			t.Fatal("single column overflow")
		}
	}
}

func TestUnknownPropertyTraceIndependentTagsAndDetachedSnapshot(t *testing.T) {
	var diagnostic StorePropertyDiagnostic
	diagnostic.unknown.enabled = true
	diagnostic.request([]uint32{0x0001001f, 0x7fff0000, 0x0001001f, 0x00010102, 0x0001ffff, 0x3001ffff, 0x00000003, 0xffff001f, 0x80000003, 0xfffe001f})
	snapshot := diagnostic.UnknownProperties()
	if !reflect.DeepEqual(snapshot.Tags, []string{"0x0001001f", "0x7fff0000", "0x00010102", "0x0001ffff"}) || snapshot.HighIDs != 2 || snapshot.ReservedIDs != 2 || snapshot.Overflow != 0 || diagnostic.Other != 9 {
		t.Fatal("ID/type, boundaries, known names or deduplication")
	}
	snapshot.Tags[0] = "private-request-string"
	if diagnostic.UnknownProperties().Tags[0] != "0x0001001f" {
		t.Fatal("snapshot aliases diagnostic")
	}
	if !reflect.DeepEqual(diagnostic.PropertyNames(), []string{"display_name"}) {
		t.Fatal("existing vocabulary changed")
	}
}

func TestUnknownPropertyTraceBoundAcrossROPsAndOverflowOccurrences(t *testing.T) {
	var diagnostic StorePropertyDiagnostic
	diagnostic.unknown.enabled = true
	var first, second []uint32
	for i := range 16 {
		tag := uint32(0x4000+i)<<16 | 3
		if i < 10 {
			first = append(first, tag)
		} else {
			second = append(second, tag)
		}
	}
	second = append(second, 0x40000003, 0x40100003, 0x40100003, 0x80000003, 0xfffe0003, 0x00000003, 0xffff0003, 0x3001001f, 0x67480014)
	diagnostic.request(first)
	diagnostic.request(second)
	snapshot := diagnostic.UnknownProperties()
	if len(snapshot.Tags) != 16 || snapshot.Tags[0] != "0x40000003" || snapshot.Tags[15] != "0x400f0003" || snapshot.Overflow != 2 || snapshot.HighIDs != 2 || snapshot.ReservedIDs != 2 || diagnostic.Requests != 2 || diagnostic.Requested != 25 || diagnostic.Other != 23 {
		t.Fatal("batch limit or occurrence accounting")
	}
	var maximum StorePropertyDiagnostic
	maximum.unknown.enabled = true
	columns := make([]uint32, 256)
	for i := range columns {
		columns[i] = uint32(0x4000+i)<<16 | 0x0102
	}
	for range 1024 {
		maximum.request(columns)
	}
	bounded := maximum.UnknownProperties()
	if len(bounded.Tags) != 16 || bounded.Overflow != 245760 || maximum.Other != 262144 {
		t.Fatal("maximum occurrence accounting")
	}
}

func TestUnknownPropertyTraceDoesNotChangeExecutedBatchOrOutputPreflight(t *testing.T) {
	transport, session, _, cookies := storeFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	body, cookies := storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 0, 0), []uint32{0xffffffff, 0xffffffff}, 4096)
	logon := storeResponse(t, body)
	fid := binary.LittleEndian.Uint64(logon.Operations[7+int(Inbox)*8:])
	body, _ = storeExecute(t, transport, session, cookies, folderOperation(fid, 0, 0, 1), logon.Handles, 4096)
	opened := storeResponse(t, body)
	var state *contextState
	for _, s := range transport.contexts.entries {
		state = s
	}
	ops := folderPropertiesOperation(0, 1, 1, 0x0001001f, 0x80000003, 0x00000003)
	ops = append(ops, folderPropertiesOperation(0, 1, 1, 0x0001001f, 0x00010102, 0x67480014)...)
	wire, _ := (ROPBuffer{Operations: ops, Handles: opened.Handles}).Marshal()
	request := Execute{ROP: wire, MaxOutput: 4096}
	off, offCounts := transport.executeStore(t.Context(), session, state, identity, request)
	transport.EnableUnknownPropertyTrace()
	on, onCounts := transport.executeStore(t.Context(), session, state, identity, request)
	snapshot := onCounts.properties.UnknownProperties()
	if !reflect.DeepEqual(off, on) || len(offCounts.properties.UnknownProperties().Tags) != 0 || onCounts.properties.Requests != 2 || onCounts.properties.Requested != 6 || onCounts.properties.Values != 1 || onCounts.properties.Errors != 5 || onCounts.properties.Other != 5 || !reflect.DeepEqual(snapshot.Tags, []string{"0x0001001f", "0x00010102"}) || snapshot.HighIDs != 1 || snapshot.ReservedIDs != 1 {
		t.Fatal("executed ROP batch projection or diagnostics")
	}
	request.MaxOutput = 1
	_, counts := transport.executeStore(t.Context(), session, state, identity, request)
	if counts.properties.Requests != 0 || len(counts.properties.UnknownProperties().Tags) != 0 {
		t.Fatal("preflight failure acquired tags")
	}
	broken, _ := (ROPBuffer{Operations: append(ops, 0xff, 0, 0), Handles: opened.Handles}).Marshal()
	request = Execute{ROP: broken, MaxOutput: 4096}
	_, counts = transport.executeStore(t.Context(), session, state, identity, request)
	if counts.properties.Requests != 0 || len(counts.properties.UnknownProperties().Tags) != 0 {
		t.Fatal("invalid batch acquired tags")
	}
}
