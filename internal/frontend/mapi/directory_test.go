// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"ewsgo/internal/core"
)

type directoryFixture struct {
	entries []core.DirectoryEntry
	err     error
	calls   int
}

func (d *directoryFixture) Entries(context.Context) ([]core.DirectoryEntry, error) {
	d.calls++
	return d.entries, d.err
}

type directorySession struct {
	core.Session
	source core.DirectoryReader
}

func (s directorySession) Directory() core.DirectoryReader { return s.source }

func TestDNToMIdCodecBoundsAndResponse(t *testing.T) {
	v := DNToMId{Names: []string{"/o=Synthetic/cn=One", ""}, Auxiliary: []byte{0x80, 0xff}}
	data, err := v.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	parsed, err := ParseDNToMId(data)
	if err != nil || !reflect.DeepEqual(parsed, v) {
		t.Fatal("DN array wire layout", err)
	}
	for i := range data {
		if _, err := ParseDNToMId(data[:i]); err == nil {
			t.Fatal("truncated request accepted", i)
		}
	}
	for _, flag := range []byte{1, 2, 255} {
		other := bytes.Clone(data)
		binary.LittleEndian.PutUint32(other, 0xffffffff) // Reserved MUST be ignored.
		other[4] = flag
		if _, err := ParseDNToMId(other); err != nil {
			t.Fatal("reserved field or nonzero Boolean rejected")
		}
	}
	for _, invalid := range [][]byte{append(bytes.Clone(data), 0), make([]byte, MaxBody+1), append32(append32([]byte{0, 0, 0, 0, 1}, MaxDirectoryNames+1), 0), append32(append32([]byte{0, 0, 0, 0, 1}, 1), 0x80808080)} {
		if _, err := ParseDNToMId(invalid); err == nil {
			t.Fatal("invalid DN buffer accepted")
		}
	}
	for _, request := range []DNToMId{{}, {Names: []string{}}, {Names: []string{strings.Repeat("x", 4096)}}} {
		data, err := request.Marshal()
		if err != nil {
			t.Fatal(err)
		}
		got, err := ParseDNToMId(data)
		if err != nil || (!reflect.DeepEqual(request.Names, got.Names) || !bytes.Equal(request.Auxiliary, got.Auxiliary)) {
			t.Fatal("absent/empty/bounded names not preserved")
		}
	}
	oversizedNames := make([]string, MaxDirectoryNames)
	for i := range oversizedNames {
		oversizedNames[i] = strings.Repeat("x", 4096)
	}
	for _, request := range []DNToMId{{Names: []string{"x\x00y"}}, {Names: []string{"\u00e0"}}, {Names: []string{strings.Repeat("x", 4097)}}, {Names: make([]string, MaxDirectoryNames+1)}, {Names: oversizedNames}, {Auxiliary: make([]byte, MaxAuxiliary+1)}} {
		if _, err := request.Marshal(); err == nil {
			t.Fatal("invalid names serialized")
		}
	}
	data, _ = (DNToMId{}).Marshal()
	binary.LittleEndian.PutUint32(data[5:], MaxAuxiliary+1)
	if _, err := ParseDNToMId(data); err == nil {
		t.Fatal("oversized auxiliary accepted")
	}
	response := DNToMIdResponse(0, []uint32{16, 0, 17})
	expected := []byte{0, 0, 0, 0, 0, 0, 0, 0, 1, 3, 0, 0, 0, 16, 0, 0, 0, 0, 0, 0, 0, 17, 0, 0, 0, 0, 0, 0, 0}
	if !bytes.Equal(response, expected) || !bytes.Equal(DNToMIdResponse(0x80004005, []uint32{16}), []byte{0, 0, 0, 0, 5, 64, 0, 128, 0, 0, 0, 0, 0}) {
		t.Fatal("success/failure response framing")
	}
}

func TestNativeDirectoryContextVisibilityAndMIDs(t *testing.T) {
	transport, base, _ := transportFixture(t)
	transport.EnableNativeDirectory()
	self := base.Identity()
	other := self
	other.Scope.Account = "two"
	other.Name = "other@example.invalid"
	source := &directoryFixture{entries: []core.DirectoryEntry{{Identity: self}, {Identity: other}}}
	session := directorySession{base, source}
	bind, _ := (Bind{State: &STAT{CodePage: 1252}}).Marshal()
	w := directoryRequest(t, transport, session, "Bind", bind, nil)
	body := requireTransportCode(t, w, 0)
	guid := bytes.Clone(body[8:24])
	if bytes.Equal(guid, make([]byte, 16)) || bytes.Equal(guid, transport.namespace.uuid[:]) || source.calls != 0 {
		t.Fatal("Bind GUID or unexpected GAL query")
	}
	current := w.Result().Cookies()
	id, _ := transport.namespace.Mailbox(self)
	otherID, _ := transport.namespace.Mailbox(other)
	names := []string{otherID.LegacyDN, "/o=Unknown/cn=Absent", strings.ToUpper(id.LegacyDN), otherID.LegacyDN, ""}
	call := func(names []string, want uint32) []byte {
		t.Helper()
		data, _ := (DNToMId{Names: names}).Marshal()
		w := directoryRequest(t, transport, session, "DNToMId", data, current)
		body := requireTransportCode(t, w, 0)
		if binary.LittleEndian.Uint32(body[4:]) != want {
			t.Fatalf("directory method result: %x", body[4:8])
		}
		current = w.Result().Cookies()
		return body
	}
	body = call(names, 0)
	if !bytes.Equal(body, DNToMIdResponse(0, []uint32{16, 0, 17, 16, 0})) {
		t.Fatal("order, duplicates, own/foreign native mapping")
	}
	body = call([]string{id.LegacyDN, otherID.LegacyDN}, 0)
	if !bytes.Equal(body, DNToMIdResponse(0, []uint32{17, 16})) || source.calls != 2 {
		t.Fatal("MID unstable or native visibility not revalidated")
	}
	data, _ := (DNToMId{Names: names}).Marshal()
	foreign := directorySession{scopedSession{base, core.Identity{Scope: core.Scope{Backend: "synthetic", Account: "foreign"}, Name: "foreign@example.invalid"}}, source}
	requireTransportCode(t, directoryRequest(t, transport, foreign, "DNToMId", data, current), 10)
	requireTransportCode(t, directoryRequest(t, transport, session, "DNToMId", data[:5], current), 12)
	if source.calls != 2 {
		t.Fatal("rejected context/request accessed GAL")
	}
	source.entries = source.entries[:1] // Native hide/delete is immediately respected.
	body = call(names, 0)
	if !bytes.Equal(body, DNToMIdResponse(0, []uint32{0, 0, 17, 0, 0})) {
		t.Fatal("cached identity bypassed native visibility")
	}
	for _, failure := range []struct {
		err  error
		code uint32
	}{{core.ErrAccessDenied, 0x80070005}, {core.ErrUnsupported, NotSupported}, {fmt.Errorf("private failure details"), 0x80004005}} {
		source.err = failure.err
		if body = call(names, failure.code); len(body) != 13 || body[8] != 0 {
			t.Fatal("backend error returned partial MIDs")
		}
	}
	source.err = nil
	call(nil, 0x80070057)
	call([]string{}, 0)
	old := current
	w = directoryRequest(t, transport, session, "Bind", bind, current)
	body = requireTransportCode(t, w, 0)
	if bytes.Equal(guid, body[8:24]) {
		t.Fatal("new context reused ephemeral MID namespace")
	}
	requireTransportCode(t, directoryRequest(t, transport, session, "DNToMId", data, old), 10)
	current = w.Result().Cookies()
	requireTransportCode(t, directoryRequest(t, transport, session, "Unbind", make([]byte, 8), current), 0)
	requireTransportCode(t, directoryRequest(t, transport, session, "DNToMId", data, current), 10)
}

func TestNativeDirectoryFailsClosedAtCapabilityAndAllocationBounds(t *testing.T) {
	transport, session, _ := transportFixture(t)
	request := DNToMId{Names: []string{"unknown"}}
	state := &contextState{directory: newDirectoryState()}
	if !bytes.Equal(transport.directoryIDs(context.Background(), session, state, request), DNToMIdResponse(NotSupported, nil)) {
		t.Fatal("missing native capability succeeded")
	}
	self := session.Identity()
	id, _ := transport.namespace.Mailbox(self)
	source := &directoryFixture{entries: []core.DirectoryEntry{{Identity: self}}}
	native := directorySession{session, source}
	for i := 0; i < maxDirectoryMIDs; i++ {
		state.directory.ids[core.Scope{Backend: "synthetic", Account: fmt.Sprint(i)}] = uint32(i) + 16
	}
	body := transport.directoryIDs(context.Background(), native, state, DNToMId{Names: []string{id.LegacyDN}})
	if !bytes.Equal(body, DNToMIdResponse(0x8007000e, nil)) || len(state.directory.ids) != maxDirectoryMIDs {
		t.Fatal("MID quota was not atomic")
	}
	for _, entries := range [][]core.DirectoryEntry{make([]core.DirectoryEntry, core.MaxDirectoryEntries+1), {{Identity: self}, {Identity: self}}, {{Identity: core.Identity{Scope: core.Scope{Backend: "other", Account: "foreign"}, Name: "other@example.invalid"}}}} {
		source.entries = entries
		if body = transport.directoryIDs(context.Background(), native, state, request); !bytes.Equal(body, DNToMIdResponse(0x80004005, nil)) {
			t.Fatal("invalid native catalog yielded false unknown DN")
		}
	}
}

func FuzzDNToMId(f *testing.F) {
	seed, _ := (DNToMId{Names: []string{"/o=EWSGo/cn=Synthetic"}}).Marshal()
	f.Add(seed)
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		if v, err := ParseDNToMId(data); err == nil {
			reencoded, err := v.Marshal()
			if err != nil {
				t.Fatal("accepted request cannot serialize")
			}
			if _, err := ParseDNToMId(reencoded); err != nil {
				t.Fatal("serialized request invalid")
			}
		}
	})
}

func TestNativeDirectoryExpiresBeforeAnyGALRead(t *testing.T) {
	transport, base, _ := transportFixture(t)
	transport.EnableNativeDirectory()
	source := &directoryFixture{entries: []core.DirectoryEntry{{Identity: base.Identity()}}}
	session := directorySession{base, source}
	bind, _ := (Bind{State: &STAT{CodePage: 1252}}).Marshal()
	w := directoryRequest(t, transport, session, "Bind", bind, nil)
	requireTransportCode(t, w, 0)
	current := w.Result().Cookies()
	now := time.Now().Add(2 * sessionTTL)
	transport.contexts.now = func() time.Time { return now }
	data, _ := (DNToMId{Names: []string{"unknown"}}).Marshal()
	requireTransportCode(t, directoryRequest(t, transport, session, "DNToMId", data, current), 10)
	if source.calls != 0 {
		t.Fatal("expired context accessed native GAL")
	}
}

type directorySummaryWriter struct {
	*httptest.ResponseRecorder
	names, resolved, calls int
}

func (w *directorySummaryWriter) RecordDirectoryResult(names, resolved int) {
	w.names, w.resolved = names, resolved
	w.calls++
}

func TestDirectorySummaryDoesNotReportMethodFailureAsUnknownDNs(t *testing.T) {
	w := &directorySummaryWriter{ResponseRecorder: httptest.NewRecorder()}
	recordDirectoryResult(w, DNToMIdResponse(0, []uint32{16, 0, 16}))
	if w.calls != 1 || w.names != 3 || w.resolved != 2 {
		t.Fatal("incorrect resolution summary")
	}
	for _, body := range [][]byte{DNToMIdResponse(0x80004005, nil), DNToMIdResponse(0, nil), {0}, DNToMIdResponse(0, make([]uint32, MaxDirectoryNames+1))} {
		recordDirectoryResult(w, body)
		if w.calls != 1 {
			t.Fatal("failed/incomplete response produced false lookup counts")
		}
	}
}
