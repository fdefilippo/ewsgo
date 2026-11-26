// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"strings"
	"testing"

	"ewsgo/internal/core"
)

var ram12LogonTags = []uint32{0x661c001f, 0x661b0102, 0x341d001f, 0x341e0102, 0x341f0102, 0x0e5c000b, 0x346f0003}

func logonNameFixture(t *testing.T) (*ostFixture, *directoryFixture) {
	f := newOSTFixture(t)
	source := &directoryFixture{entries: []core.DirectoryEntry{{Identity: f.session.Identity(), DisplayName: "A€"}}}
	f.session = directorySession{f.session, source}
	return f, source
}

func TestLogonPropertiesRealTagRowAndNativeRename(t *testing.T) {
	f, source := logonNameFixture(t)
	f.transport.EnableUnknownPropertyTrace()
	row, c := f.run(folderPropertiesOperation(4, 0, 1, ram12LogonTags...))
	want := []byte{7, 0, 0, 0, 0, 0, 1, 0, 'A', 0, 0xac, 0x20, 0, 0}
	mapped, _ := f.transport.namespace.Mailbox(f.session.Identity())
	entryID := addressBookEntryFixture(mapped.LegacyDN)
	want = append(want, 0)
	want = binary.LittleEndian.AppendUint16(want, uint16(len(entryID)))
	want = append(want, entryID...)
	for range 3 {
		want = append(want, 0x0a, 2, 1, 4, 0x80)
	}
	want = append(want, 0x0a, 0x0f, 1, 4, 0x80, 0x0a, 2, 1, 4, 0x80)
	if !bytes.Equal(row, want) || c.errors != 0 || c.properties.Values != 2 || c.properties.Errors != 5 || c.properties.Requested != 7 || !c.properties.Logon || c.properties.Folder || source.calls != 1 {
		t.Fatal("independent seven-column fixture or diagnostic")
	}
	if len(c.properties.PropertyNames()) != 0 || len(c.properties.UnknownProperties().Tags) != 7 {
		t.Fatal("diagnostic vocabulary changed")
	}
	source.entries[0].DisplayName = "Renamed"
	row, c = f.run(folderPropertiesOperation(4, 0, 0, 0x661c001e, 0x661c001e))
	if !bytes.Equal(row, append([]byte{7, 0, 0, 0, 0, 0, 0}, []byte("Renamed\x00Renamed\x00")...)) || source.calls != 2 || c.properties.Values != 2 {
		t.Fatal("stale owner name or duplicate directory reads")
	}
	row, _ = f.run(folderPropertiesOperation(4, 1, 1, 0x661c001f))
	if !bytes.Equal(row, []byte{7, 1, 0, 0, 0, 0, 1, 0x0a, 2, 1, 4, 0x80}) || source.calls != 2 {
		t.Fatal("owner name projected on Folder")
	}
}

func TestLogonPropertiesTypesAndCodepage(t *testing.T) {
	f, source := logonNameFixture(t)
	row, _ := f.run(folderPropertiesOperation(4, 0, 1, 0x661c0000, 0x661c0003, 0x67480014))
	want := []byte{7, 0, 0, 0, 0, 0, 1, 0x1f, 0, 0, 'A', 0, 0xac, 0x20, 0, 0, 0x0a, 2, 1, 4, 0x80, 0x0a, 2, 1, 4, 0x80}
	if !bytes.Equal(row, want) {
		t.Fatal("unspecified string type or unsupported columns")
	}
	f.state.codePage = 932
	row, _ = f.run(folderPropertiesOperation(4, 0, 0, 0x661c001e, 0x661c001f))
	want = []byte{7, 0, 0, 0, 0, 0, 1, 0x0a, 0x1e, 1, 4, 0x80, 0, 'A', 0, 0xac, 0x20, 0, 0}
	if !bytes.Equal(row, want) {
		t.Fatal("explicit Unicode affected by String8 codepage")
	}
	f.state.codePage = 1252
	row, _ = f.run(folderPropertiesOperation(4, 0, 0, 0x661c0000))
	if !bytes.Equal(row, []byte{7, 0, 0, 0, 0, 0, 1, 0x1e, 0, 0, 'A', 0x80, 0}) {
		t.Fatal("unspecified String8")
	}
	calls := source.calls
	row, _ = f.run(folderPropertiesOperation(4, 0, 1, 0x661b0003, 0x341e0102, 0x0ffe0003))
	if len(row) != 22 || source.calls != calls {
		t.Fatal("nonimplemented properties query directory")
	}
	row, _ = f.run(folderPropertiesOperation(4, 0, 1))
	if !bytes.Equal(row, []byte{7, 0, 0, 0, 0, 0, 0}) || source.calls != calls {
		t.Fatal("empty projection")
	}
}

func TestLogonPropertiesVisibilityAndAuthorization(t *testing.T) {
	for _, name := range []string{"hidden", "empty", "directory-error", "no-directory", "revoked-root", "revoked-owner", "owner-state-error", "foreign-scope", "wrong-logon", "released", "cancelled"} {
		t.Run(name, func(t *testing.T) {
			f, source := logonNameFixture(t)
			want := uint32(PropertyNotFound)
			ropFailure := false
			logon := byte(4)
			switch name {
			case "hidden":
				source.entries = nil
			case "empty":
				source.entries[0].DisplayName = ""
			case "directory-error":
				source.err = core.ErrAccessDenied
				want = accessDenied
			case "no-directory":
				f.session = f.session.(directorySession).Session
				want = NotSupported
			case "revoked-root":
				f.box.getErr = core.ErrAccessDenied
				want = accessDenied
				ropFailure = true
			case "revoked-owner":
				f.box.state.Owner = false
				want = accessDenied
				ropFailure = true
			case "owner-state-error":
				f.box.stateErr = core.ErrAccessDenied
				want = accessDenied
				ropFailure = true
			case "foreign-scope":
				id := f.session.Identity()
				id.Scope.Account = "foreign"
				f.session = scopedSession{f.session, id}
				want = accessDenied
				ropFailure = true
			case "wrong-logon":
				logon = 5
				want = accessDenied
				ropFailure = true
			case "released":
				f.state.mailbox.release(f.handles[0])
				want = invalidObject
				ropFailure = true
			case "cancelled":
				op := Operation{ID: 7, Logon: 4, Input: 0, Properties: &specificProperties{tags: []uint32{0x661c001f}}}
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				d := StorePropertyDiagnostic{}
				row := f.transport.getStoreProperties(ctx, f.session, f.state, op, f.handles, &d, nil, nil)
				if !bytes.Equal(row, ropError(7, 0, 0x80004005)) {
					t.Fatal("cancelled read returned data")
				}
				return
			}
			row, c := f.run(folderPropertiesOperation(logon, 0, 1, 0x661c001f))
			expected := ropError(7, 0, want)
			if !ropFailure {
				expected = append(ropError(7, 0, 0), 1, 0x0a)
				expected = binary.LittleEndian.AppendUint32(expected, want)
			}
			if !bytes.Equal(row, expected) || c.properties.Values != 0 {
				t.Fatal("authorization/visibility/fallback")
			}
			if ropFailure && source.calls != 0 {
				t.Fatal("directory consulted before authorization")
			}
		})
	}
}

func TestLogonPropertiesPreflightAndSizeLimits(t *testing.T) {
	f, source := logonNameFixture(t)
	op := folderPropertiesOperation(4, 0, 1, ram12LogonTags...)
	needed := 10 + 4*len(f.handles) + specificPropertiesBound(specificProperties{tags: ram12LogonTags}, -1)
	body, _ := f.execute(append([]byte{1, 4, 1}, op...), uint32(needed-1))
	b := storeResponse(t, body)
	if b.Operations[0] != 0xff || source.calls != 0 {
		t.Fatal("preflight read directory")
	}
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
		t.Fatal("preflight released handle")
	}
	body, c := f.execute(op, 4096)
	if len(storeResponse(t, body).Operations) == 0 || c.properties.Values != 2 {
		t.Fatal("real seven-tag request exceeds small output")
	}
	for _, tc := range []struct {
		name string
		kind uint16
		want uint32
	}{
		{strings.Repeat("a", 1023), 0x1e, 0}, {strings.Repeat("a", 1024), 0x1e, 0x8007000e},
		{strings.Repeat("a", 511), 0x1f, 0}, {strings.Repeat("a", 512), 0x1f, 0x8007000e},
		{strings.Repeat("€", 1023), 0x1e, 0},
		{strings.Repeat("€", 1025), 0x1e, 0x8007000e}, {"a\x00b", 0x1f, 0x80070057},
	} {
		v := logonOwnerName(tc.name, 0, tc.kind, true, 1252)
		if v.code != tc.want || len(v.data) > 1024 {
			t.Fatal("encoded name cap or invalid string")
		}
	}
}

func FuzzLogonOwnerNameBound(f *testing.F) {
	f.Add("A€", true, uint32(1252))
	f.Add(strings.Repeat("a", 512), false, uint32(65001))
	f.Fuzz(func(t *testing.T, name string, unicode bool, cp uint32) {
		v := logonOwnerName(name, 0, 0, unicode, cp)
		req := specificProperties{wantUnicode: unicode, tags: []uint32{0x661c0000}}
		row := append(ropError(7, 0, 0), folderPropertyRow(req, []folderPropertyValue{v})...)
		if len(row) > specificPropertiesBound(req, -1) || len(v.data) > 1024 {
			t.Fatal("response exceeded preflight")
		}
	})
}
