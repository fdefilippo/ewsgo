// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestGetPropsCodecAndWireLimits(t *testing.T) {
	v := GetProps{Flags: 0xffffffff, State: &STAT{CurrentRec: 17, CodePage: 1252, Delta: -1}, Tags: []uint32{0x3001001f, 0x39fe001e}, Auxiliary: []byte{0xff, 0x80}}
	data, err := v.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	got, err := ParseGetProps(data)
	if err != nil || !reflect.DeepEqual(got, v) {
		t.Fatal("GetProps roundtrip", err)
	}
	for i := range data {
		if _, err := ParseGetProps(data[:i]); err == nil {
			t.Fatal("truncated request accepted", i)
		}
	}
	for _, input := range [][]byte{append(bytes.Clone(data), 0), make([]byte, MaxBody+1)} {
		if _, err := ParseGetProps(input); err == nil {
			t.Fatal("invalid buffer accepted")
		}
	}
	for _, tags := range [][]uint32{nil, {}, make([]uint32, MaxPropertyTags)} {
		v := GetProps{Tags: tags}
		wire, _ := v.Marshal()
		parsed, err := ParseGetProps(wire)
		if err != nil || !reflect.DeepEqual(parsed.Tags, tags) {
			t.Fatal("null, empty or local tag bound")
		}
	}
	for _, n := range []int{MaxPropertyTags + 1, maxWirePropertyTags, maxWirePropertyTags + 1} {
		wire := append32(append([]byte{0, 0, 0, 0, 0}, 1), uint32(n))
		wire = append32(append(wire, make([]byte, 4*n)...), 0)
		_, err := ParseGetProps(wire)
		want := ErrTooManyProperties
		if n > maxWirePropertyTags {
			want = ErrMalformed
		}
		if !errors.Is(err, want) {
			t.Fatal("wire limit classified incorrectly", n, err)
		}
		if _, err := ParseGetProps(wire[:len(wire)-1]); err != ErrMalformed {
			t.Fatal("truncation must precede local table limit")
		}
	}
	if _, err := (GetProps{Tags: make([]uint32, 257)}).Marshal(); err == nil {
		t.Fatal("local encode bound")
	}
	if _, err := (GetProps{Auxiliary: make([]byte, MaxAuxiliary+1)}).Marshal(); err == nil {
		t.Fatal("auxiliary encode bound")
	}
	data, _ = (GetProps{}).Marshal()
	binary.LittleEndian.PutUint32(data[len(data)-4:], MaxAuxiliary+1)
	if _, err := ParseGetProps(data); err == nil {
		t.Fatal("auxiliary decode bound")
	}
}

func TestGetPropsTaggedWireIsNotROPPropertyRow(t *testing.T) {
	value, ok := addressBookString("A", 0x001f, 1252)
	if !ok {
		t.Fatal("UTF16 value")
	}
	got := getPropsResponse(ErrorsReturned, 1252, []addressBookValue{{tag: 0x3001001f, data: value}, missingProperty(0x8006001e)})
	want := []byte{
		0, 0, 0, 0, 0x80, 3, 4, 0, 0xe4, 4, 0, 0, 1, 2, 0, 0, 0,
		0x1f, 0, 1, 0x30, 0xff, 'A', 0, 0, 0,
		0x0a, 0, 6, 0x80, 0x0f, 1, 4, 0x80, 0, 0, 0, 0,
	}
	if !bytes.Equal(got, want) {
		t.Fatal("AddressBook tagged list, HasValue or per-property error wire")
	}
	if got := getPropsResponse(0x80070005, 1252, []addressBookValue{missingProperty(0)}); len(got) != 17 || got[12] != 0 {
		t.Fatal("method failure exposed a row")
	}
	if got := getPropsResponse(0, 1252, []addressBookValue{}); len(got) != 21 || got[12] != 1 {
		t.Fatal("present empty row was null")
	}
}

func TestGetPropsStringConversions(t *testing.T) {
	for _, tc := range []struct {
		value string
		kind  uint16
		cp    uint32
		want  []byte
	}{
		{"é€漢", 0x001e, 1252, []byte{0xff, 0xe9, 0x80, '?', 0}},
		{"€", 0x001e, 65001, []byte{0xff, 0xe2, 0x82, 0xac, 0}},
		{"😀", 0x001f, 1252, []byte{0xff, 0x3d, 0xd8, 0, 0xde, 0, 0}},
	} {
		got, ok := addressBookString(tc.value, tc.kind, tc.cp)
		if !ok || !bytes.Equal(got, tc.want) {
			t.Fatal("code page conversion")
		}
	}
	for _, text := range []string{"x\x00y", string([]byte{0xff}), strings.Repeat("x", 8193)} {
		if _, ok := addressBookString(text, 0x001f, 1252); ok {
			t.Fatal("invalid/oversized string encoded")
		}
	}
	if _, ok := addressBookString("x", 0x001e, 1200); ok {
		t.Fatal("unsupported code page")
	}
}

func TestGetPropsNativeValuesVisibilityAndErrors(t *testing.T) {
	transport, base, _ := transportFixture(t)
	self := base.Identity()
	entry := core.DirectoryEntry{Identity: self, DisplayName: "Native Test €"}
	source := &directoryFixture{entries: []core.DirectoryEntry{entry}}
	session := directorySession{base, source}
	state := &contextState{codePage: 1252, directory: newDirectoryState()}
	state.directory.ids[self.Scope] = 16
	request := GetProps{Flags: 0xffffffff, State: &STAT{CurrentRec: 16, CodePage: 1252}, Tags: []uint32{0x3001001f, 0x39fe001e, 0x3001001f, 0x8006001e, 0x300b0102, 0x30020000}}
	got, values, missing := transport.directoryProperties(context.Background(), session, state, request)
	if values != 6 || missing != 1 || binary.LittleEndian.Uint32(got[4:]) != ErrorsReturned || source.calls != 1 {
		t.Fatal("native properties and per-field errors")
	}
	name, _ := addressBookString(entry.DisplayName, 0x001f, 1252)
	smtp, _ := addressBookString(self.Name, 0x001e, 1252)
	typeEX, _ := addressBookString("EX", 0x001f, 1252)
	mapped, _ := transport.namespace.Mailbox(self)
	search := appendBlob([]byte{0xff}, append([]byte("EX:"+strings.ToUpper(mapped.LegacyDN)), 0))
	want := getPropsResponse(ErrorsReturned, 1252, []addressBookValue{{0x3001001f, name}, {0x39fe001e, smtp}, {0x3001001f, name}, missingProperty(0x8006001e), {0x300b0102, search}, {0x3002001f, typeEX}})
	if !bytes.Equal(got, want) {
		t.Fatal("order, duplicate tags, native values, unspecified or binary DWORD length")
	}
	// Native rename preserves the account's MID; output reflects the fresh GAL.
	source.entries[0].Identity.Name = "renamed@example.invalid"
	got, _, _ = transport.directoryProperties(context.Background(), session, state, request)
	if !bytes.Contains(got, []byte("renamed@example.invalid")) || bytes.Contains(got, []byte(self.Name)) {
		t.Fatal("stale GAL values")
	}
	source.entries = nil
	got, _, missing = transport.directoryProperties(context.Background(), session, state, request)
	if missing != len(request.Tags) || binary.LittleEndian.Uint32(got[4:]) != ErrorsReturned {
		t.Fatal("hidden entry leaked through allocated MID")
	}
	source.entries = []core.DirectoryEntry{entry}
	request.State.CurrentRec = 99
	got, _, missing = transport.directoryProperties(context.Background(), session, state, request)
	if missing != len(request.Tags) || binary.LittleEndian.Uint32(got[4:]) != ErrorsReturned {
		t.Fatal("unknown MID must return per-property errors")
	}
	request.State.CurrentRec = 16
	request.Tags = []uint32{}
	got, values, missing = transport.directoryProperties(context.Background(), session, state, request)
	if values != 0 || missing != 0 || !bytes.Equal(got, getPropsResponse(0, 1252, []addressBookValue{})) {
		t.Fatal("empty request")
	}
	request.Tags = nil
	got, values, missing = transport.directoryProperties(context.Background(), session, state, request)
	if values != 8 || missing != 0 || binary.LittleEndian.Uint32(got[4:]) != 0 {
		t.Fatal("default properties")
	}
	for _, tc := range []struct {
		err  error
		want uint32
	}{{core.ErrAccessDenied, 0x80070005}, {core.ErrUnsupported, NotSupported}, {errors.New("private-backend-details"), 0x80004005}} {
		source.err = tc.err
		got, _, _ = transport.directoryProperties(context.Background(), session, state, request)
		if len(got) != 17 || got[12] != 0 || binary.LittleEndian.Uint32(got[4:]) != tc.want {
			t.Fatal("native failure returned row or wrong error")
		}
	}
	source.err = nil
	request.Tags = []uint32{0x3001001e}
	for _, stat := range []*STAT{nil, {CodePage: 1200}, {CodePage: 1252, ContainerID: 16}} {
		request.State = stat
		before := source.calls
		got, _, _ = transport.directoryProperties(context.Background(), session, state, request)
		if len(got) != 17 || source.calls != before || got[12] != 0 {
			t.Fatal("invalid state accessed GAL or returned values")
		}
	}
}

func TestGetPropsContextIsolationExpiryAndRebind(t *testing.T) {
	transport, base, _ := transportFixture(t)
	transport.EnableNativeDirectory()
	source := &directoryFixture{entries: []core.DirectoryEntry{{Identity: base.Identity(), DisplayName: "Native"}}}
	session := directorySession{base, source}
	bind, _ := (Bind{State: &STAT{CodePage: 1252}}).Marshal()
	w := directoryRequest(t, transport, session, "Bind", bind, nil)
	current := w.Result().Cookies()
	mapped, _ := transport.namespace.Mailbox(base.Identity())
	names, _ := (DNToMId{Names: []string{mapped.LegacyDN}}).Marshal()
	w = directoryRequest(t, transport, session, "DNToMId", names, current)
	resolved := requireTransportCode(t, w, 0)
	mid := binary.LittleEndian.Uint32(resolved[13:])
	current = w.Result().Cookies()
	data, _ := (GetProps{State: &STAT{CurrentRec: mid, CodePage: 1252}, Tags: []uint32{0x3001001f}}).Marshal()
	w = directoryRequest(t, transport, session, "GetProps", data, current)
	body := requireTransportCode(t, w, 0)
	if binary.LittleEndian.Uint32(body[4:]) != 0 || source.calls != 2 {
		t.Fatal("GetProps transport or visibility read")
	}
	requireTransportCode(t, directoryRequest(t, transport, session, "GetProps", data, current), 15)
	current = w.Result().Cookies()
	foreign := directorySession{scopedSession{base, core.Identity{Scope: core.Scope{Backend: "synthetic", Account: "foreign"}, Name: "foreign@example.invalid"}}, source}
	requireTransportCode(t, directoryRequest(t, transport, foreign, "GetProps", data, current), 10)
	requireTransportCode(t, directoryRequest(t, transport, session, "GetProps", data[:6], current), 12)
	if source.calls != 2 {
		t.Fatal("invalid context accessed GAL")
	}
	old := current
	w = directoryRequest(t, transport, session, "Bind", bind, current)
	current = w.Result().Cookies()
	requireTransportCode(t, directoryRequest(t, transport, session, "GetProps", data, old), 10)
	w = directoryRequest(t, transport, session, "GetProps", data, current)
	body = requireTransportCode(t, w, 0)
	if binary.LittleEndian.Uint32(body[4:]) != ErrorsReturned {
		t.Fatal("MID survived a new Bind")
	}
	current = w.Result().Cookies()
	before := source.calls
	transport.contexts.now = func() time.Time { return time.Now().Add(2 * sessionTTL) }
	requireTransportCode(t, directoryRequest(t, transport, session, "GetProps", data, current), 10)
	if source.calls != before {
		t.Fatal("expired context accessed GAL")
	}
}

// A valid but large array produces TableTooBig and rotates the sequence. It
// must not be confused with malformed framing (transport code 12).
func TestGetPropsLargeListAndFeatureGate(t *testing.T) {
	transport, base, _ := transportFixture(t)
	data, _ := (GetProps{}).Marshal()
	requireTransportCode(t, directoryRequest(t, transport, base, "GetProps", data, nil), 5)
	transport.EnableNativeDirectory()
	bind, _ := (Bind{State: &STAT{CodePage: 1252}}).Marshal()
	w := directoryRequest(t, transport, base, "Bind", bind, nil)
	current := w.Result().Cookies()
	data = appendSTAT([]byte{0, 0, 0, 0, 1}, &STAT{CodePage: 1252})
	data = append32(append(data, 1), 257)
	data = append32(append(data, make([]byte, 257*4)...), 0)
	w = directoryRequest(t, transport, base, "GetProps", data, current)
	body := requireTransportCode(t, w, 0)
	if !bytes.Equal(body, getPropsResponse(TableTooBig, 1252, nil)) {
		t.Fatal("large valid array error")
	}
}

func TestGetPropsTotalResponseBoundAndDefaultAbsence(t *testing.T) {
	transport, base, _ := transportFixture(t)
	self := base.Identity()
	source := &directoryFixture{entries: []core.DirectoryEntry{{Identity: self, DisplayName: strings.Repeat("x", 8192)}}}
	session := directorySession{base, source}
	state := &contextState{codePage: 1252, directory: newDirectoryState()}
	state.directory.ids[self.Scope] = 16
	tags := make([]uint32, MaxPropertyTags)
	for i := range tags {
		tags[i] = 0x3001001f
	}
	request := GetProps{State: &STAT{CodePage: 1252, CurrentRec: 16}, Tags: tags}
	body, values, missing := transport.directoryProperties(context.Background(), session, state, request)
	if !bytes.Equal(body, getPropsResponse(TableTooBig, 1252, nil)) || values != 0 || missing != 0 {
		t.Fatal("response limit returned partial/oversized values")
	}
	source.entries[0].DisplayName = ""
	request.Tags = nil
	body, values, missing = transport.directoryProperties(context.Background(), session, state, request)
	if values != 7 || missing != 0 || binary.LittleEndian.Uint32(body[4:]) != 0 {
		t.Fatal("default list includes absent native name")
	}
	request.State.CurrentRec = 99
	body, values, missing = transport.directoryProperties(context.Background(), session, state, request)
	if !bytes.Equal(body, getPropsResponse(0, 1252, []addressBookValue{})) || values != 0 || missing != 0 {
		t.Fatal("defaults for unknown MID invented values")
	}
}

func FuzzGetProps(f *testing.F) {
	data, _ := (GetProps{State: &STAT{CodePage: 1252}, Tags: []uint32{0x3001001f}}).Marshal()
	f.Add(data)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		v, err := ParseGetProps(data)
		if err == nil {
			if _, err := v.Marshal(); err != nil {
				t.Fatal("parsed request cannot be encoded")
			}
		}
	})
}

func TestGetPropsAddressBookGUIDAndCodepageRelevance(t *testing.T) {
	transport, base, _ := transportFixture(t)
	self := base.Identity()
	source := &directoryFixture{entries: []core.DirectoryEntry{{Identity: self, DisplayName: "Native"}}}
	session := directorySession{base, source}
	state := &contextState{codePage: 1252, directory: newDirectoryState()}
	state.directory.ids[self.Scope] = 16
	guid, _ := transport.namespace.AddressBookGUID(self.Scope)
	wantValue := addressBookValue{0x8c6d0102, appendBlob([]byte{0xff}, guid[:])}
	for _, cp := range []uint32{0, 1200, 1252, 65001, 0xffffffff} {
		request := GetProps{State: &STAT{CurrentRec: 16, CodePage: cp}, Tags: []uint32{0x8c6d0102, 0x8c6d0000}}
		body, count, missing := transport.directoryProperties(context.Background(), session, state, request)
		if count != 2 || missing != 0 || !bytes.Equal(body, getPropsResponse(0, cp, []addressBookValue{wantValue, wantValue})) {
			t.Fatal("binary GUID incorrectly depends on String8 codepage", cp)
		}
		request.Tags = []uint32{0x3001001f}
		body, count, missing = transport.directoryProperties(context.Background(), session, state, request)
		if count != 1 || missing != 0 || binary.LittleEndian.Uint32(body[4:]) != 0 {
			t.Fatal("explicit Unicode value rejected", cp)
		}
		if cp != 1252 && cp != 65001 {
			request.Tags = []uint32{0x8c6d0102, 0x3001001e}
			before := source.calls
			body, _, _ = transport.directoryProperties(context.Background(), session, state, request)
			if !bytes.Equal(body, getPropsResponse(0x8004011e, cp, nil)) || source.calls != before {
				t.Fatal("String8 codepage validation lost")
			}
		}
	}
	request := GetProps{State: &STAT{CurrentRec: 16, CodePage: 1200}, Tags: []uint32{0x8c6d0102}}
	source.entries[0].Identity.Name = "renamed@example.invalid"
	body, _, missing := transport.directoryProperties(context.Background(), session, state, request)
	if missing != 0 || !bytes.Equal(body, getPropsResponse(0, 1200, []addressBookValue{wantValue})) {
		t.Fatal("SMTP rename changed directory GUID")
	}
	state.directory = newDirectoryState()
	state.directory.ids[self.Scope] = 17
	request.State.CurrentRec = 17
	body, _, missing = transport.directoryProperties(context.Background(), session, state, request)
	if missing != 0 || !bytes.Equal(body, getPropsResponse(0, 1200, []addressBookValue{wantValue})) {
		t.Fatal("rebind changed directory GUID")
	}
	source.entries = nil
	body, _, missing = transport.directoryProperties(context.Background(), session, state, request)
	if missing != 1 || !bytes.Equal(body, getPropsResponse(ErrorsReturned, 1200, []addressBookValue{missingProperty(0x8c6d0102)})) {
		t.Fatal("hidden native entry exposed deterministic GUID")
	}
}
