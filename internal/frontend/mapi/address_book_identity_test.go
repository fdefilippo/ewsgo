// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"ewsgo/internal/core"
)

// Independent MS-OXNSPI 2.2.9.3 fixture: bytes, not a GUID string converted
// with mailbox GUID byte order. The existing LegacyDN is the identity input.
func addressBookEntryFixture(dn string) []byte {
	out := []byte{0, 0, 0, 0, 0xdc, 0xa7, 0x40, 0xc8, 0xc0, 0x42, 0x10, 0x1a, 0xb4, 0xb9, 8, 0, 0x2b, 0x2f, 0xe1, 0x82, 1, 0, 0, 0, 0, 0, 0, 0}
	return append(append(out, []byte(dn)...), 0)
}

func TestOwnAddressBookIdentityAcrossStoreAndNSPI(t *testing.T) {
	f, source := logonNameFixture(t)
	self := f.session.Identity()
	mapped, _ := f.transport.namespace.Mailbox(self)
	want := addressBookEntryFixture(mapped.LegacyDN)
	row, counts := f.run(folderPropertiesOperation(4, 0, 1, 0x661b0102))
	wire := append([]byte{7, 0, 0, 0, 0, 0, 0}, binary.LittleEndian.AppendUint16(nil, uint16(len(want)))...)
	wire = append(wire, want...)
	if !bytes.Equal(row, wire) || counts.properties.Values != 1 || source.calls != 1 {
		t.Fatal("owner EntryID missing or incorrect ROP binary count")
	}
	both, counts := f.run(folderPropertiesOperation(4, 0, 1, 0x66190102, 0x661b0102))
	if !bytes.Equal(both, append(bytes.Clone(wire), wire[7:]...)) || counts.properties.Values != 2 || source.calls != 2 {
		t.Fatal("the authenticated user and mailbox owner must resolve to the same EntryID")
	}
	state := &contextState{codePage: 1252, directory: newDirectoryState()}
	state.directory.ids[self.Scope] = 16
	req := GetProps{State: &STAT{CurrentRec: 16, CodePage: 1252}, Tags: []uint32{0x0fff0102, 0x800f101f}}
	body, values, missing := f.transport.directoryProperties(context.Background(), f.session, state, req)
	proxy := []byte{0xff, 1, 0, 0, 0, 0xff}
	for _, c := range "SMTP:" + self.Name {
		proxy = binary.LittleEndian.AppendUint16(proxy, uint16(c))
	}
	proxy = append(proxy, 0, 0)
	expected := getPropsResponse(0, 1252, []addressBookValue{{0x0fff0102, appendBlob([]byte{0xff}, want)}, {0x800f101f, proxy}})
	if !bytes.Equal(body, expected) || values != 2 || missing != 0 {
		t.Fatal("own NSPI identity or multi-string wire missing")
	}
	// The DN identifies the same currently visible object through the existing lookup.
	dn := string(want[28 : len(want)-1])
	ids := f.transport.directoryIDs(context.Background(), f.session, state, DNToMId{Names: []string{dn}})
	if binary.LittleEndian.Uint32(ids[13:]) != 16 {
		t.Fatal("EntryID DN does not roundtrip")
	}
	source.entries[0].Identity.Name = "renamed@example.invalid"
	body, _, missing = f.transport.directoryProperties(context.Background(), f.session, state, req)
	if missing != 0 || !bytes.Contains(body, want) || bytes.Equal(body, expected) {
		t.Fatal("rename changed identity or left primary SMTP stale")
	}
	row, _ = f.run(folderPropertiesOperation(4, 0, 1, 0x661b0102))
	if !bytes.Equal(row, wire) {
		t.Fatal("GAL rename changed store owner identity")
	}
	// A visible unrelated GAL object has no asserted MAILUSER type.
	other := self
	other.Scope.Account = "other"
	other.Name = "other@example.invalid"
	source.entries = append(source.entries, core.DirectoryEntry{Identity: other, DisplayName: "Other"})
	state.directory.ids[other.Scope] = 17
	req.State.CurrentRec = 17
	body, _, missing = f.transport.directoryProperties(context.Background(), f.session, state, req)
	if missing != 1 || !bytes.Equal(body[17:25], []byte{0xa, 0, 0xff, 0xf, 0xf, 1, 4, 0x80}) {
		t.Fatal("foreign GAL object assigned own EntryID")
	}
	req.State.CurrentRec = 16
	source.entries = nil
	body, _, missing = f.transport.directoryProperties(context.Background(), f.session, state, req)
	if missing != 2 || bytes.Contains(body, want) {
		t.Fatal("hidden own object leaked through MID")
	}
	row, counts = f.run(folderPropertiesOperation(4, 0, 1, 0x66190102, 0x661b0102))
	if counts.properties.Values != 0 || !bytes.Equal(row, []byte{7, 0, 0, 0, 0, 0, 1, 0xa, 0xf, 1, 4, 0x80, 0xa, 0xf, 1, 4, 0x80}) {
		t.Fatal("hidden EntryID leaked")
	}
}

func TestOwnAddressBookEntryIDTypesAuthorizationAndPreflight(t *testing.T) {
	for _, name := range []string{"binary", "unspecified", "wrong-type", "hidden", "no-display-name", "directory-error", "root-revoked", "owner-revoked", "folder", "output-too-small"} {
		t.Run(name, func(t *testing.T) {
			f, source := logonNameFixture(t)
			mapped, _ := f.transport.namespace.Mailbox(f.session.Identity())
			entryID := addressBookEntryFixture(mapped.LegacyDN)
			tag := uint32(0x661b0102)
			input := byte(0)
			wantError := uint32(0)
			ropFailure := false
			switch name {
			case "unspecified":
				tag = 0x661b0000
			case "wrong-type":
				tag = 0x661b001f
				wantError = NotSupported
			case "hidden":
				source.entries = nil
				wantError = PropertyNotFound
			case "no-display-name":
				source.entries[0].DisplayName = ""
			case "directory-error":
				source.err = core.ErrAccessDenied
				wantError = accessDenied
			case "root-revoked":
				f.box.getErr = core.ErrAccessDenied
				wantError = accessDenied
				ropFailure = true
			case "owner-revoked":
				f.box.state.Owner = false
				wantError = accessDenied
				ropFailure = true
			case "folder":
				input = 1
				wantError = NotSupported
			case "output-too-small":
				op := folderPropertiesOperation(4, 0, 1, tag)
				size := 10 + 4*len(f.handles) + specificPropertiesBound(specificProperties{tags: []uint32{tag}}, -1)
				body, _ := f.execute(append([]byte{1, 4, 1}, op...), uint32(size-1))
				if storeResponse(t, body).Operations[0] != 0xff || source.calls != 0 {
					t.Fatal("EntryID read before output reservation")
				}
				if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
					t.Fatal("preflight released handle")
				}
				return
			}
			f.state.codePage = 932 // Binary owner identity has no String8 encoding.
			row, c := f.run(folderPropertiesOperation(4, input, 1, tag))
			if wantError != 0 {
				expected := ropError(7, input, wantError)
				if !ropFailure {
					expected = append32(append(ropError(7, input, 0), 1, 0xa), wantError)
				}
				if !bytes.Equal(row, expected) || c.properties.Values != 0 {
					t.Fatal("owner EntryID authorization/type error")
				}
				if (ropFailure || name == "wrong-type" || name == "folder") && source.calls != 0 {
					t.Fatal("directory accessed without supported authorized request")
				}
				return
			}
			head := []byte{7, 0, 0, 0, 0, 0, 0}
			if tag&0xffff == 0 {
				head = []byte{7, 0, 0, 0, 0, 0, 1, 2, 1, 0}
			}
			expected := append(binary.LittleEndian.AppendUint16(head, uint16(len(entryID))), entryID...)
			if !bytes.Equal(row, expected) || len(row) > specificPropertiesBound(specificProperties{tags: []uint32{tag}}, -1) {
				t.Fatal("EntryID type, binary wire or bound")
			}
		})
	}
}

func TestOwnAddressBookNSPITypesAndRebind(t *testing.T) {
	transport, base, _ := transportFixture(t)
	transport.EnableNativeDirectory()
	source := &directoryFixture{entries: []core.DirectoryEntry{{Identity: base.Identity()}}}
	session := directorySession{base, source}
	mapped, _ := transport.namespace.Mailbox(base.Identity())
	want := addressBookEntryFixture(mapped.LegacyDN)
	bind, _ := (Bind{State: &STAT{CodePage: 1252}}).Marshal()
	for range 2 {
		w := directoryRequest(t, transport, session, "Bind", bind, nil)
		current := w.Result().Cookies()
		names, _ := (DNToMId{Names: []string{mapped.LegacyDN}}).Marshal()
		w = directoryRequest(t, transport, session, "DNToMId", names, current)
		resolved := requireTransportCode(t, w, 0)
		mid := binary.LittleEndian.Uint32(resolved[13:])
		current = w.Result().Cookies()
		for _, tc := range []struct {
			tag, cp uint32
			kind    uint16
			missing bool
		}{
			{0x0fff0102, 932, 0x0102, false}, {0x0fff0000, 932, 0x0102, false}, {0x0fff001f, 1252, 0, true},
			{0x800f101f, 932, 0x101f, false}, {0x800f101e, 65001, 0x101e, false}, {0x800f0000, 932, 0x101f, false}, {0x800f001f, 1252, 0, true},
		} {
			data, _ := (GetProps{State: &STAT{CurrentRec: mid, CodePage: tc.cp}, Tags: []uint32{tc.tag}}).Marshal()
			w = directoryRequest(t, transport, session, "GetProps", data, current)
			body := requireTransportCode(t, w, 0)
			current = w.Result().Cookies()
			tag := binary.LittleEndian.Uint32(body[17:])
			code := binary.LittleEndian.Uint32(body[4:])
			if tc.missing {
				if tag&0xffff != 0xa || code != ErrorsReturned {
					t.Fatal("unsupported NSPI type succeeded")
				}
				continue
			}
			if uint16(tag) != tc.kind || code != 0 {
				t.Fatal("explicit binary/Unicode affected by unrelated codepage")
			}
			if tc.kind == 0x102 && !bytes.Equal(body[26:len(body)-4], want) {
				t.Fatal("EntryID changed across NSPI bind")
			}
		}
	}
}

func TestOwnAddressBookNSPIEphemeralFlag(t *testing.T) {
	transport, base, _ := transportFixture(t)
	source := &directoryFixture{entries: []core.DirectoryEntry{{Identity: base.Identity()}}}
	session := directorySession{base, source}
	var prior []byte
	for range 2 {
		state := &contextState{codePage: 1252, directory: newDirectoryState()}
		state.directory.ids[base.Identity().Scope] = 16
		for _, flags := range []uint32{2, 0xffffffff, 2} {
			req := GetProps{Flags: flags, State: &STAT{CurrentRec: 16, CodePage: 1252}, Tags: []uint32{0x0fff0102}}
			body, _, missing := transport.directoryProperties(context.Background(), session, state, req)
			want := append([]byte{0x87, 0, 0, 0}, state.directory.guid[:]...)
			want = append(want, 1, 0, 0, 0, 0, 0, 0, 0, 16, 0, 0, 0)
			if missing != 0 || !bytes.Equal(body[26:len(body)-4], want) {
				t.Fatal("fEphID ignored or context ID malformed")
			}
			if prior != nil && bytes.Equal(prior, want) {
				t.Fatal("ephemeral ID survived new directory context")
			}
		}
		req := GetProps{State: &STAT{CurrentRec: 16, CodePage: 1252}, Tags: []uint32{0x0fff0102}}
		body, _, _ := transport.directoryProperties(context.Background(), session, state, req)
		if body[26] != 0 {
			t.Fatal("permanent form affected by previous ephemeral request")
		}
		prior = append([]byte{0x87, 0, 0, 0}, state.directory.guid[:]...)
		prior = append(prior, 1, 0, 0, 0, 0, 0, 0, 0, 16, 0, 0, 0)
	}
}
