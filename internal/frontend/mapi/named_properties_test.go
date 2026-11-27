// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"slices"
	"sync"
	"testing"
	"unicode/utf16"

	"ewsgo/internal/core"
)

// Synthetic requests, independently encoded from the decoder's implementation.
func propertyIDsOperation(logon, input, flags byte, names ...propertyName) []byte {
	wire := binary.LittleEndian.AppendUint16([]byte{0x56, logon, input, flags}, uint16(len(names)))
	for _, name := range names {
		wire = append(wire, name.Kind)
		wire = append(wire, name.Set[:]...)
		switch name.Kind {
		case 0:
			wire = binary.LittleEndian.AppendUint32(wire, name.LID)
		case 1:
			units := utf16.Encode([]rune(name.Name))
			wire = append(wire, byte(2*(len(units)+1)))
			for _, u := range units {
				wire = binary.LittleEndian.AppendUint16(wire, u)
			}
			wire = append(wire, 0, 0)
		}
	}
	return wire
}

func propertyResponseIDs(t *testing.T, response ROPBuffer) []uint16 {
	t.Helper()
	p := response.Operations
	if len(p) < 8 || p[0] != 0x56 || binary.LittleEndian.Uint32(p[2:]) != 0 {
		t.Fatal("named-property ROP failed")
	}
	n := int(binary.LittleEndian.Uint16(p[6:]))
	if len(p) != 8+2*n {
		t.Fatal("named-property response length")
	}
	ids := make([]uint16, n)
	for i := range ids {
		ids[i] = binary.LittleEndian.Uint16(p[8+2*i:])
	}
	return ids
}

func TestPropertyNameCodecLiteralUTF16AndHostileFrames(t *testing.T) {
	// ROP, logon, input, flags, count; KindString, GUID, NameSize, UTF-16LE.
	wire := append([]byte{0x56, 7, 0, 2, 1, 0, 1}, make([]byte, 16)...)
	wire = append(wire, 8, 0xe9, 0, 0x3d, 0xd8, 0x00, 0xde, 0, 0) // é😀 and NUL.
	decode := func(data []byte) ([]Operation, error) {
		return (ROPBuffer{Operations: data, Handles: []uint32{1}}).DecodeOperations()
	}
	ops, err := decode(wire)
	if err != nil || len(ops) != 1 || ops[0].Flags != 2 || ops[0].PropertyNames[0].Name != "é😀" {
		t.Fatal("literal UTF-16 request was not decoded")
	}
	for n := 1; n < len(wire); n++ {
		if ops, err := decode(wire[:n]); err == nil || ops != nil {
			t.Fatal("truncated request accepted", n)
		}
	}
	for _, change := range []func([]byte){
		func(b []byte) { b[4], b[5] = 0xff, 0xff },
		func(b []byte) { b[6] = 2 },
		func(b []byte) { b[23] = 7 },
		func(b []byte) { b[23] = 0 },
		func(b []byte) { b[24], b[25] = 0, 0 },
		func(b []byte) { b[26], b[27] = 0, 0xdc },
		func(b []byte) { b[28], b[29] = 1, 0 },
		func(b []byte) { b[30] = 1 },
	} {
		bad := bytes.Clone(wire)
		change(bad)
		if ops, err := decode(bad); err == nil || ops != nil {
			t.Fatal("malformed name accepted")
		}
	}
	for _, name := range []propertyName{{Kind: 0, LID: 0x12345678}, {Kind: 1}, {Kind: 0xff}} {
		ops, err := decode(propertyIDsOperation(1, 0, 0, name))
		if err != nil || !reflect.DeepEqual(ops[0].PropertyNames, []propertyName{name}) {
			t.Fatal("numeric/empty/no-associated name layout", err)
		}
	}
	if ops, err := decode(append(wire, 2, 0, 0)); err == nil || ops != nil {
		t.Fatal("partial batch returned before malformed suffix")
	}
}

func TestNamedRegistryLookupCreationSpecialSetsQuotaAndConcurrency(t *testing.T) {
	var registry namedPropertyRegistry
	name := propertyName{Set: [16]byte{1}, LID: 42}
	header := propertyName{Set: psInternetHeaders, Kind: 1, Name: "X-EWSGo-Test"}
	lower := header
	lower.Name = "x-ewsgo-test"
	standard := propertyName{Set: psMAPI, LID: 0x3001}
	unmapped := propertyName{Set: psMAPI, Kind: 1, Name: "no-such-standard"}
	ids, code := registry.ids(context.Background(), []propertyName{name, standard, unmapped}, false)
	if code != 0 || !reflect.DeepEqual(ids, []uint16{0, 0x3001, 0}) || len(registry.names) != 0 {
		t.Fatal("lookup registered names or PS_MAPI mapping wrong")
	}
	ids, code = registry.ids(context.Background(), []propertyName{name, header, lower, name, unmapped, {Kind: 0xff}}, true)
	if code != 0 || !reflect.DeepEqual(ids, []uint16{0x8001, 0x8002, 0x8002, 0x8001, 0, 0}) {
		t.Fatal("registration/duplicates/header normalization")
	}
	ids, code = registry.ids(context.Background(), []propertyName{
		{Set: psMAPI, LID: 0x7fff}, {Set: psMAPI, LID: 0x8000}, {Set: psMAPI, LID: 0x8001},
		{Set: psMAPI, LID: 0xfffe}, {Set: psMAPI, LID: 0xffff}, name,
	}, true)
	if code != 0 || !reflect.DeepEqual(ids, []uint16{0x7fff, 0, 0, 0, 0, 0x8001}) {
		t.Fatalf("PS_MAPI boundary returned %x, %x", ids, code)
	}

	// Different property sets, kinds and ordinary-name case remain distinct.
	a := propertyName{Set: [16]byte{2}, Kind: 1, Name: "Case"}
	b := a
	b.Name = "case"
	c := a
	c.Set[0] = 3
	d := propertyName{Set: a.Set, LID: 42}
	more, _ := registry.ids(context.Background(), []propertyName{a, b, c, d}, true)
	if !reflect.DeepEqual(more, []uint16{0x8003, 0x8004, 0x8005, 0x8006}) {
		t.Fatal("namespace/kind/name distinctions collapsed")
	}
	var wg sync.WaitGroup
	for range 32 {
		wg.Go(func() {
			got, code := registry.ids(context.Background(), []propertyName{name, lower}, true)
			if code != 0 || !reflect.DeepEqual(got, []uint16{0x8001, 0x8002}) {
				t.Error("concurrent registration changed IDs")
			}
		})
	}
	wg.Wait()
	enum, _ := registry.ids(context.Background(), nil, false)
	if !reflect.DeepEqual(enum, []uint16{0x8001, 0x8002, 0x8003, 0x8004, 0x8005, 0x8006}) {
		t.Fatal("enumeration omitted/reordered registered IDs")
	}
	fill := make([]propertyName, maxNamedMappings-6)
	for i := range fill {
		fill[i] = propertyName{Set: [16]byte{9}, LID: uint32(i)}
	}
	if _, code := registry.ids(context.Background(), fill, true); code != 0 {
		t.Fatal("could not reach quota")
	}
	newName := propertyName{Set: [16]byte{10}}
	if _, code := registry.ids(context.Background(), []propertyName{name, newName}, true); code != 0x8007000e || len(registry.names) != maxNamedMappings {
		t.Fatal("quota not enforced before registration")
	}
	got, code := registry.ids(context.Background(), []propertyName{name, newName}, false)
	if code != 0 || !reflect.DeepEqual(got, []uint16{0x8001, 0}) {
		t.Fatal("quota prevented lookup or created missing entry")
	}
	var almostFull namedPropertyRegistry
	fill = append(fill, a, b, c, d, name) // 4095 unique entries.
	almostFull.ids(context.Background(), fill, true)
	if _, code := almostFull.ids(context.Background(), []propertyName{newName, header}, true); code != 0x8007000e || len(almostFull.names) != maxNamedMappings-1 {
		t.Fatal("quota failure partially registered this ROP")
	}
}

func TestStoreNamedPropertiesBatchFolderPermissionsAndReconnect(t *testing.T) {
	transport, session, box, cookies := storeFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	name := propertyName{Set: [16]byte{3}, Kind: 1, Name: "Synthetic"}
	// Logon and name registration share a valid batch and output handle table.
	ops := append(logonOperation(identity.LegacyDN, 3, 0), propertyIDsOperation(3, 0, 2, name)...)
	body, cookies := storeExecute(t, transport, session, cookies, ops, []uint32{0xffffffff, 0xffffffff}, 4096)
	response := storeResponse(t, body)
	first := bytes.Clone(response.Operations[:166])
	response.Operations = response.Operations[166:]
	if !reflect.DeepEqual(propertyResponseIDs(t, response), []uint16{0x8001}) {
		t.Fatal("batch registration")
	}
	handle := response.Handles[0]
	fid := binary.LittleEndian.Uint64(first[7+4*8:])
	body, cookies = storeExecute(t, transport, session, cookies, folderOperation(fid, 3, 0, 1), response.Handles, 4096)
	folderHandles := storeResponse(t, body).Handles
	body, cookies = storeExecute(t, transport, session, cookies, propertyIDsOperation(3, 1, 0, name), folderHandles, 4096)
	if !reflect.DeepEqual(propertyResponseIDs(t, storeResponse(t, body)), []uint16{0x8001}) {
		t.Fatal("folder did not share store mapping")
	}
	for _, tc := range []struct {
		logon, input, flags byte
		names               []propertyName
		want                uint32
	}{
		{4, 0, 0, []propertyName{name}, accessDenied},
		{3, 0, 1, []propertyName{name}, 0x80070057},
		{3, 1, 0, nil, 0},
	} {
		body, cookies = storeExecute(t, transport, session, cookies, propertyIDsOperation(tc.logon, tc.input, tc.flags, tc.names...), folderHandles, 10000)
		response := storeResponse(t, body)
		if binary.LittleEndian.Uint32(response.Operations[2:]) != tc.want ||
			tc.want == 0 && len(propertyResponseIDs(t, response)) != 0 {
			t.Fatal("handle/flags/enumeration boundary")
		}
	}
	// Losing creation permission preserves lookup and maps missing rows to zero.
	box.state.Owner = false
	fresh := propertyName{Set: [16]byte{4}}
	body, cookies = storeExecute(t, transport, session, cookies, propertyIDsOperation(3, 0, 2, name, fresh), []uint32{handle}, 4096)
	if !reflect.DeepEqual(propertyResponseIDs(t, storeResponse(t, body)), []uint16{0x8001, 0}) {
		t.Fatal("creation denied did not preserve lookup")
	}
	box.state.Owner = true
	// Current native access is checked even though the mapping is cached.
	box.getErr = core.ErrAccessDenied
	body, cookies = storeExecute(t, transport, session, cookies, propertyIDsOperation(3, 0, 0, name), []uint32{handle}, 4096)
	if binary.LittleEndian.Uint32(storeResponse(t, body).Operations[2:]) != accessDenied {
		t.Fatal("cached mapping bypassed native access")
	}
	box.getErr = nil
	box.stateErr = core.ErrAuthentication
	body, cookies = storeExecute(t, transport, session, cookies, propertyIDsOperation(3, 0, 2, fresh), []uint32{handle}, 4096)
	if binary.LittleEndian.Uint32(storeResponse(t, body).Operations[2:]) != 0x80040111 {
		t.Fatal("creation state failure ignored")
	}
	box.stateErr = nil
	connect, _ := (Connect{UserDN: identity.LegacyDN}).Marshal()
	w := transportRequest(t, transport, session, "Connect", connect, cookies)
	cookies = w.Result().Cookies()
	body, cookies = storeExecute(t, transport, session, cookies, propertyIDsOperation(3, 0, 0, name), []uint32{handle}, 4096)
	if binary.LittleEndian.Uint32(storeResponse(t, body).Operations[2:]) != nullObject {
		t.Fatal("old context handle survived reconnect")
	}
	body, cookies = storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 3, 0), []uint32{0xffffffff}, 4096)
	handles := storeResponse(t, body).Handles
	body, _ = storeExecute(t, transport, session, cookies, propertyIDsOperation(3, 0, 0, name, fresh), handles, 4096)
	if !reflect.DeepEqual(propertyResponseIDs(t, storeResponse(t, body)), []uint16{0x8001, 0}) {
		t.Fatal("reconnect lost mappings or registered failed request")
	}
}

func TestNamedPropertyIDsOnMessageAndAttachment(t *testing.T) {
	f, base := newContentsDownloadFixture(t)
	box := &attachmentFixtureMailbox{openMessageFixtureMailbox: &openMessageFixtureMailbox{messageFixtureMailbox: base}}
	f.session = &testSession{box}
	box.objects[0].MIME = []byte(attachmentFixtureMIME)
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	fid := f.fids[Inbox]
	snapshot, err := store.observeMailMessages(t.Context(), f.session, fid)
	if err != nil || len(snapshot.messages) != 1 {
		t.Fatal("message fixture unavailable", err)
	}
	faiRunOK(t, f, openMessageOperation(4, 1, 2, fid, snapshot.messages[0].mid, 0x0fff, 0))
	faiRunOK(t, f, attachmentOpenWire(4, 2, 3, 0, 0))
	name := propertyName{Set: [16]byte{0xf1}, Kind: 1, Name: "synthetic"}
	standard := propertyName{Set: psMAPI, LID: 0x1000}
	check := func(slot, flags byte, expected []uint16, names ...propertyName) {
		t.Helper()
		got := faiRunOK(t, f, propertyIDsOperation(4, slot, flags, names...))
		if !slices.Equal(propertyResponseIDs(t, ROPBuffer{Operations: got}), expected) {
			t.Fatalf("unexpected mapping on slot %d", slot)
		}
	}
	check(2, 0, []uint16{0, 0x1000}, name, standard)
	check(2, 2, []uint16{0x8001}, name)
	check(3, 0, []uint16{0x8001, 0x1000}, name, standard)
	check(2, 0, nil)
	check(3, 0, nil)
	check(0, 0, []uint16{0x8001})
}

func TestStoreNamedPropertiesPreflightAndScopeEpoch(t *testing.T) {
	for _, tail := range [][]byte{nil, {2, 0, 0}, {0xfd, 0, 0}} {
		transport, session, _, cookies := storeFixture(t)
		identity, _ := transport.namespace.Mailbox(session.Identity())
		body, cookies := storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 0, 0), []uint32{0xffffffff}, 4096)
		handles := storeResponse(t, body).Handles
		name := propertyName{Set: [16]byte{5}}
		maximum := uint32(4096)
		if tail == nil {
			maximum = 20
		}
		body, cookies = storeExecute(t, transport, session, cookies, append(propertyIDsOperation(0, 0, 2, name), tail...), handles, maximum)
		if tail == nil && storeResponse(t, body).Operations[0] != 0xff {
			t.Fatal("missing BufferTooSmall")
		}
		body, _ = storeExecute(t, transport, session, cookies, propertyIDsOperation(0, 0, 0, name), handles, 4096)
		if !reflect.DeepEqual(propertyResponseIDs(t, storeResponse(t, body)), []uint16{0}) {
			t.Fatal("preflight failure registered a name")
		}
	}
	stores, binding, session := ramFixture(t)
	name := propertyName{Set: [16]byte{5}}
	binding.store.named.ids(context.Background(), []propertyName{name}, true)
	catalog, _ := privateFolders(context.Background(), session)
	other := catalog
	other.scope.Account += "/another"
	otherBinding, _ := stores.bind(t.Context(), other)
	backend := catalog
	backend.scope.Backend += "/another"
	backendBinding, _ := stores.bind(t.Context(), backend)
	fresh, _ := newRAMStores().bind(context.Background(), catalog)
	for _, store := range []*ramStore{otherBinding.store, backendBinding.store, fresh.store} {
		ids, _ := store.named.ids(context.Background(), []propertyName{name}, false)
		if !reflect.DeepEqual(ids, []uint16{0}) {
			t.Fatal("named-property namespace leaked across scope/epoch")
		}
	}
	// A forged cross-scope binding and cancelled request cannot register names.
	objects := newMailboxObjects(stores)
	handle, _ := objects.addLogon(0, binding)
	op := Operation{ID: 0x56, Flags: 2, PropertyNames: []propertyName{{Set: [16]byte{7}}}}
	foreign := scopedSession{Session: session, identity: core.Identity{Scope: other.scope}}
	if response := getStorePropertyIDs(context.Background(), foreign, objects, op, []uint32{handle}); binary.LittleEndian.Uint32(response[2:]) != accessDenied {
		t.Fatal("foreign native scope accepted for named properties")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// Lookup avoids requiring a MailboxStateReader on this catalog fixture.
	op.Flags = 0
	if response := getStorePropertyIDs(ctx, session, objects, op, []uint32{handle}); binary.LittleEndian.Uint32(response[2:]) == 0 {
		t.Fatal("cancelled lookup succeeded")
	}
}

func FuzzNamedPropertyOperation(f *testing.F) {
	f.Add(propertyIDsOperation(0, 0, 2, propertyName{Set: [16]byte{1}, Kind: 1, Name: "Synthetic😀"}))
	f.Add(propertyIDsOperation(0, 0, 0))
	f.Fuzz(func(t *testing.T, data []byte) {
		ops, err := (ROPBuffer{Operations: data, Handles: []uint32{1}}).DecodeOperations()
		if err != nil && ops != nil {
			t.Fatal("partial operations on error")
		}
		if len(ops) > 1024 {
			t.Fatal("operation bound")
		}
	})
}

func TestNamedBatchReportsLaterUnsupportedOpcodeWithoutExecutingPrefix(t *testing.T) {
	transport, session, _, cookies := storeFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	name := propertyName{Set: [16]byte{7}, Kind: 1, Name: "private-test-name"}
	ops := append(logonOperation(identity.LegacyDN, 0, 0), propertyIDsOperation(0, 0, 2, name)...)
	ops = append(ops, ropFastTransferSourceCopyFolder, 0, 0) // FastTransfer is disabled in this profile.
	buffer := ROPBuffer{Operations: ops, Handles: []uint32{0xffffffff}}
	decoded, err := buffer.DecodeOperations()
	var unsupported unsupportedOperation
	if decoded != nil || !errors.Is(err, ErrUnsupported) || !errors.As(err, &unsupported) || unsupported.label != "RopFastTransferSourceCopyFolder" || bytes.Contains([]byte(err.Error()), []byte("private-")) {
		t.Fatal("later unsupported opcode not classified safely")
	}
	var state *contextState
	for _, existing := range transport.contexts.entries {
		state = existing
	}
	wire, _ := buffer.Marshal()
	_, counts := transport.executeStore(context.Background(), session, state, identity, Execute{ROP: wire, MaxOutput: 4096})
	if counts.unsupported != "RopFastTransferSourceCopyFolder" || len(transport.stores.entries) != 0 || len(state.mailbox.objects) != 0 {
		t.Fatal("unsupported suffix executed the prefix or lost its label")
	}
	// Transport still returns NotSupported; no successful prefix was fabricated.
	body, _ := storeExecute(t, transport, session, cookies, ops, buffer.Handles, 4096)
	if binary.LittleEndian.Uint32(body[4:]) != NotSupported {
		t.Fatal("method error changed")
	}
}
