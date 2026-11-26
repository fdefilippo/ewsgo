// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"sync"
	"testing"

	"ewsgo/internal/core"
)

func ostSetOperation(logon, input byte, value []byte) []byte {
	out := binary.LittleEndian.AppendUint16([]byte{0x0a, logon, input}, uint16(8+len(value)))
	out = append(out, 1, 0, 2, 1, 4, 0x7c)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(value)))
	return append(out, value...)
}

func TestOSTValueRequestExactFramingAndOptIn(t *testing.T) {
	wire := []byte{0x0a, 4, 1, 10, 0, 1, 0, 2, 1, 4, 0x7c, 2, 0, 0xaa, 0xbb}
	buffer := ROPBuffer{Operations: wire, Handles: []uint32{1, 2}}
	if _, err := buffer.DecodeOperations(); !errors.Is(err, ErrUnsupported) {
		t.Fatal("default decoder accepts SetProperties")
	}
	ops, err := buffer.decodeOperations(true)
	if err != nil || len(ops) != 1 || ops[0].Logon != 4 || ops[0].Input != 1 || !bytes.Equal(ops[0].OSTValue, []byte{0xaa, 0xbb}) {
		t.Fatal("independent request fixture")
	}
	wire[13] = 0
	if ops[0].OSTValue[0] != 0xaa {
		t.Fatal("parsed value aliases caller")
	}
	for n := 1; n < len(wire); n++ {
		if partial, err := (ROPBuffer{Operations: wire[:n], Handles: buffer.Handles}).decodeOperations(true); err == nil || partial != nil {
			t.Fatal("truncation accepted", n)
		}
	}
	for _, alter := range []func([]byte){
		func(b []byte) { b[3]-- }, func(b []byte) { b[3]++ },
		func(b []byte) { b[11]-- }, func(b []byte) { b[11]++ },
		func(b []byte) { b[2] = 2 }, func(b []byte) { b[3], b[4] = 1, 0 },
	} {
		bad := bytes.Clone(wire)
		alter(bad)
		if partial, err := (ROPBuffer{Operations: bad, Handles: buffer.Handles}).decodeOperations(true); err == nil || partial != nil {
			t.Fatal("invalid size or index accepted")
		}
	}
	for _, index := range []int{5, 7, 9, 10} {
		bad := bytes.Clone(wire)
		bad[index] ^= 1
		if _, err := (ROPBuffer{Operations: bad, Handles: buffer.Handles}).decodeOperations(true); !errors.Is(err, ErrUnsupported) {
			t.Fatal("unsupported count, tag or type", index)
		}
	}
	for _, n := range []int{0, maxOSTValueBytes, maxOSTValueBytes + 1} {
		if _, err := (ROPBuffer{Operations: ostSetOperation(4, 1, make([]byte, n)), Handles: buffer.Handles}).decodeOperations(true); err != nil {
			t.Fatal("structurally valid Binary rejected before authorization", n)
		}
	}
}

type ostFixture struct {
	t         *testing.T
	transport *SessionTransport
	session   core.Session
	box       *stateMailbox
	state     *contextState
	identity  MailboxIdentity
	handles   []uint32
	fids      [specialFolderCount]uint64
}

func newOSTFixture(t *testing.T) *ostFixture {
	t.Helper()
	transport, session, box, cookies := storeFixture(t)
	transport.EnableOSTPropertySpike()
	identity, _ := transport.namespace.Mailbox(session.Identity())
	body, _ := storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 4, 0), []uint32{0xffffffff, 0xffffffff}, 4096)
	logon := storeResponse(t, body)
	f := &ostFixture{t: t, transport: transport, session: session, box: box, identity: identity, handles: logon.Handles}
	for role := range f.fids {
		f.fids[role] = binary.LittleEndian.Uint64(logon.Operations[7+role*8:])
	}
	for _, state := range transport.contexts.entries {
		f.state = state
	}
	f.open(int(Inbox))
	return f
}

func (f *ostFixture) execute(ops []byte, maximum uint32) ([]byte, storeResults) {
	f.t.Helper()
	wire, err := (ROPBuffer{Operations: ops, Handles: f.handles}).Marshal()
	if err != nil {
		f.t.Fatal("test request encoding")
	}
	return f.transport.executeStore(context.Background(), f.session, f.state, f.identity, Execute{ROP: wire, MaxOutput: maximum})
}

func (f *ostFixture) run(ops []byte) ([]byte, storeResults) {
	f.t.Helper()
	body, counts := f.execute(ops, 65535)
	buffer := storeResponse(f.t, body)
	f.handles = buffer.Handles
	return buffer.Operations, counts
}

func (f *ostFixture) open(role int) {
	f.t.Helper()
	rop, _ := f.run(folderOperation(f.fids[role], 4, 0, 1))
	if !bytes.Equal(rop, []byte{2, 1, 0, 0, 0, 0, 0, 0}) {
		f.t.Fatal("test Folder open")
	}
}

func TestOSTValueWriteReadWireAndReconnect(t *testing.T) {
	f := newOSTFixture(t)
	set, counts := f.run(ostSetOperation(4, 1, []byte{0xaa, 0xbb}))
	if !bytes.Equal(set, []byte{0x0a, 1, 0, 0, 0, 0, 0, 0}) || counts.errors != 0 || counts.properties.Requests != 0 {
		t.Fatal("successful Set wire or diagnostic schema")
	}
	for _, tc := range []struct {
		tag  uint32
		want []byte
	}{
		{0x7c040102, []byte{7, 1, 0, 0, 0, 0, 0, 2, 0, 0xaa, 0xbb}},
		{0x7c040000, []byte{7, 1, 0, 0, 0, 0, 1, 2, 1, 0, 2, 0, 0xaa, 0xbb}},
	} {
		row, counts := f.run(folderPropertiesOperation(4, 1, 0, tc.tag))
		if !bytes.Equal(row, tc.want) || counts.properties.Values != 1 || counts.properties.Errors != 0 || counts.properties.Other != 1 {
			t.Fatal("readback fixture or diagnostic count")
		}
	}
	// A different handle to the same Folder and a replacement context share
	// the RAM store. Old handles are not reused in the replacement context.
	f.run([]byte{1, 4, 1})
	f.open(int(Inbox))
	row, _ := f.run(folderPropertiesOperation(4, 1, 0, 0x7c040102, 0x67480014, 0x7c040003))
	want := append([]byte{7, 1, 0, 0, 0, 0, 1, 0, 2, 0, 0xaa, 0xbb, 0}, binary.LittleEndian.AppendUint64(nil, f.fids[Inbox])...)
	want = append(want, 0x0a, 2, 1, 4, 0x80)
	if !bytes.Equal(row, want) {
		t.Fatal("mixed row after new handle")
	}
	f.state.mailbox = newMailboxObjects(f.transport.stores)
	f.handles = []uint32{0xffffffff, 0xffffffff}
	f.run(logonOperation(f.identity.LegacyDN, 4, 0))
	f.open(int(Inbox))
	row, _ = f.run(folderPropertiesOperation(4, 1, 0, 0x7c040102))
	if !bytes.Equal(row, []byte{7, 1, 0, 0, 0, 0, 0, 2, 0, 0xaa, 0xbb}) {
		t.Fatal("replacement context lost RAM value")
	}
	f.open(int(SentItems))
	row, _ = f.run(folderPropertiesOperation(4, 1, 0, 0x7c040102))
	if !bytes.Equal(row, []byte{7, 1, 0, 0, 0, 0, 1, 0x0a, 0x0f, 1, 4, 0x80}) {
		t.Fatal("value leaked between Folder FIDs")
	}
	f.run(ostSetOperation(4, 1, nil))
	row, _ = f.run(folderPropertiesOperation(4, 1, 0, 0x7c040102))
	if !bytes.Equal(row, []byte{7, 1, 0, 0, 0, 0, 0, 0, 0}) {
		t.Fatal("empty Binary treated as absent")
	}
}

func TestOSTValueAuthorizationFailureKeepsPreviousValue(t *testing.T) {
	for _, tc := range []struct {
		name  string
		alter func(*ostFixture)
		code  uint32
	}{
		{"read revoked", func(f *ostFixture) { f.box.getErr = core.ErrAccessDenied }, accessDenied},
		{"owner revoked", func(f *ostFixture) { f.box.state.Owner = false }, accessDenied},
		{"owner read error", func(f *ostFixture) { f.box.stateErr = core.ErrAccessDenied }, accessDenied},
		{"owner unsupported", func(f *ostFixture) { f.session = &testSession{f.box.catalogMailbox} }, NotSupported},
		{"foreign scope", func(f *ostFixture) {
			i := f.session.Identity()
			i.Scope.Account = "different"
			f.session = scopedSession{Session: f.session, identity: i}
		}, accessDenied},
		{"deleted Folder", func(f *ostFixture) { f.box.page.Objects = append(f.box.page.Objects[:1], f.box.page.Objects[2:]...) }, PropertyNotFound},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOSTFixture(t)
			f.run(ostSetOperation(4, 1, []byte{1}))
			object, _ := f.state.mailbox.object(f.handles[1], 4)
			tc.alter(f)
			rop, _ := f.run(ostSetOperation(4, 1, []byte{2}))
			value, _ := object.binding.store.ostValue(object.fid)
			if !bytes.Equal(rop, ropError(0x0a, 1, tc.code)) || !bytes.Equal(value, []byte{1}) {
				t.Fatal("authorization failed to preserve old value")
			}
		})
	}
	f := newOSTFixture(t)
	for _, tc := range []struct {
		logon, input byte
		code         uint32
	}{{4, 0, NotSupported}, {5, 1, accessDenied}, {4, 1, invalidObject}} {
		if tc.code == invalidObject {
			f.run([]byte{1, 4, 1})
		}
		rop, _ := f.run(ostSetOperation(tc.logon, tc.input, []byte{1}))
		if !bytes.Equal(rop, ropError(0x0a, tc.input, tc.code)) {
			t.Fatal("object/Logon checks")
		}
	}
}

func TestOSTValueBoundsCopiesAndQuota(t *testing.T) {
	f := newOSTFixture(t)
	value := bytes.Repeat([]byte{0xcc}, maxOSTValueBytes)
	f.run(ostSetOperation(4, 1, value))
	row, _ := f.run(folderPropertiesOperation(4, 1, 0, 0x7c040000))
	if len(row) != 12+len(value) || !bytes.Equal(row[12:], value) {
		t.Fatal("maximum value truncated")
	}
	rop, _ := f.run(ostSetOperation(4, 1, make([]byte, maxOSTValueBytes+1)))
	if !bytes.Equal(rop, []byte{0x0a, 1, 0, 0, 0, 0, 1, 0, 0, 0, 2, 1, 4, 0x7c, 1, 3, 4, 0x80}) {
		t.Fatal("oversize PropertyProblem fixture")
	}
	row, _ = f.run(folderPropertiesOperation(4, 1, 0, 0x7c040102))
	if !bytes.Equal(row[9:], value) {
		t.Fatal("oversize changed previous value")
	}
	for role := 0; role < 7; role++ { // Inbox is role 4; choose eight distinct roles overall.
		if role == int(Inbox) {
			continue
		}
		f.open(role)
		f.run(ostSetOperation(4, 1, []byte{byte(role)}))
	}
	// Fill until eight entries, without relying on the role numeric order.
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	store := object.binding.store
	for role := 0; role < int(specialFolderCount) && len(store.ost) < maxOSTFolders; role++ {
		f.open(role)
		f.run(ostSetOperation(4, 1, []byte{byte(role)}))
	}
	f.open(int(Shortcuts))
	rop, _ = f.run(ostSetOperation(4, 1, []byte{1}))
	if !bytes.Equal(rop, ropError(0x0a, 1, NotSupported)) || len(store.ost) != 8 {
		t.Fatal("ninth Folder quota")
	}
	f.open(int(Inbox))
	f.run(ostSetOperation(4, 1, []byte{0xaa, 0xbb}))
	stored, exists := store.ostValue(f.fids[Inbox])
	stored[0] = 0
	again, _ := store.ostValue(f.fids[Inbox])
	if !exists || !bytes.Equal(again, []byte{0xaa, 0xbb}) {
		t.Fatal("quota blocks existing entry or read aliases state")
	}
}

func TestOSTValueRejectedBatchHasNoPrefixEffects(t *testing.T) {
	for _, tc := range []struct {
		name      string
		suffix    []byte
		maximum   uint32
		enabled   bool
		valueSize int
	}{
		{"default off", nil, 65535, false, 1},
		{"unknown suffix", []byte{8, 4, 1}, 65535, true, 1},
		{"truncated suffix", []byte{2, 4, 1}, 65535, true, 1},
		{"small Set output", nil, 20, true, 1},
		{"large read response", folderPropertiesOperation(4, 1, 0, 0x7c040102, 0x7c040102), 65535, true, maxOSTValueBytes},
		{"small read response", folderPropertiesOperation(4, 1, 0, 0x7c040102), 47, true, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newOSTFixture(t)
			f.transport.ostPropertySpike = tc.enabled
			ops := append(ostSetOperation(4, 1, bytes.Repeat([]byte{1}, tc.valueSize)), 1, 4, 1)
			ops = append(ops, tc.suffix...)
			f.execute(ops, tc.maximum)
			object, code := f.state.mailbox.object(f.handles[1], 4)
			if code != 0 || len(object.binding.store.ost) != 0 || f.box.stateCalls != 1 {
				t.Fatal("rejected batch executed Set, Release or Owner read")
			}
		})
	}
}

func TestOSTValueConcurrentCopiesScopeAndRestart(t *testing.T) {
	f := newOSTFixture(t)
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	store := object.binding.store
	var wg sync.WaitGroup
	for i := 0; i < 16; i++ {
		wg.Go(func() {
			for n := 0; n < 64; n++ {
				v := bytes.Repeat([]byte{byte(n)}, 64)
				if store.setOSTValue(t.Context(), object.fid, v) != 0 {
					t.Error("concurrent write")
				}
				v[0] ^= 0xff
				got, ok := store.ostValue(object.fid)
				if !ok || len(got) != 64 || got[0] != got[1] {
					t.Error("torn value or input alias")
				}
				got[0] ^= 0xff
			}
		})
	}
	wg.Wait()
	catalog, _ := privateFolders(context.Background(), f.session)
	catalog.scope.Account = "other"
	other, _ := f.transport.stores.bind(context.Background(), catalog)
	if _, exists := other.store.ostValue(other.fids[Inbox]); exists {
		t.Fatal("cross-scope value")
	}
	catalog.scope = f.session.Identity().Scope
	restarted, _ := newRAMStores().bind(context.Background(), catalog)
	if _, exists := restarted.store.ostValue(restarted.fids[Inbox]); exists {
		t.Fatal("restart retained value")
	}
}

func TestOSTValueCanceledWriteAndRevokedRead(t *testing.T) {
	f := newOSTFixture(t)
	f.run(ostSetOperation(4, 1, []byte{1}))
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	wire, _ := (ROPBuffer{Operations: ostSetOperation(4, 1, []byte{2}), Handles: f.handles}).Marshal()
	body, _ := f.transport.executeStore(ctx, f.session, f.state, f.identity, Execute{ROP: wire, MaxOutput: 65535})
	rop := storeResponse(t, body).Operations
	value, _ := object.binding.store.ostValue(object.fid)
	if !bytes.Equal(rop, ropError(0x0a, 1, 0x80004005)) || !bytes.Equal(value, []byte{1}) {
		t.Fatal("canceled request changed value")
	}
	f.box.getErr = core.ErrAccessDenied
	rop, _ = f.run(folderPropertiesOperation(4, 1, 0, 0x7c040102))
	if !bytes.Equal(rop, ropError(7, 1, accessDenied)) {
		t.Fatal("stored RAM value bypassed revoked native read")
	}
}

func TestOSTReadAbsentAndShortValueFitsSmallOutput(t *testing.T) {
	f := newOSTFixture(t)
	read := func(want []byte) {
		t.Helper()
		body, counts := f.execute(folderPropertiesOperation(4, 1, 0, 0x7c040102), 4096)
		if !bytes.Equal(storeResponse(t, body).Operations, want) || counts.properties.Requests != 1 {
			t.Fatal("fixed maximum reservation blocks an absent or short OSTID")
		}
	}
	read([]byte{7, 1, 0, 0, 0, 0, 1, 0x0a, 0x0f, 1, 4, 0x80})
	f.run(ostSetOperation(4, 1, []byte{0xaa, 0xbb}))
	read([]byte{7, 1, 0, 0, 0, 0, 0, 2, 0, 0xaa, 0xbb})
	// Two short columns also fit: reserve actual snapshot sizes for duplicates.
	body, _ := f.execute(folderPropertiesOperation(4, 1, 0, 0x7c040102, 0x7c040000), 4096)
	want := []byte{7, 1, 0, 0, 0, 0, 1, 0, 2, 0, 0xaa, 0xbb, 2, 1, 0, 2, 0, 0xaa, 0xbb}
	if !bytes.Equal(storeResponse(t, body).Operations, want) {
		t.Fatal("short duplicate columns blocked or misencoded")
	}
}

func TestOSTBatchViewStableAcrossConcurrentGrowthAndScope(t *testing.T) {
	f := newOSTFixture(t)
	ops, _ := (ROPBuffer{Operations: folderPropertiesOperation(4, 1, 0, 0x7c040102), Handles: f.handles}).decodeOperations(true)
	view := f.transport.ostView(f.session.Identity().Scope, ops)
	bound := specificPropertiesBound(*ops[0].Properties, view.bound())
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	// Simulate a concurrent successful writer after this batch's preflight.
	large := bytes.Repeat([]byte{0xcc}, maxOSTValueBytes)
	object.binding.store.setOSTValue(t.Context(), object.fid, large)
	var diagnostic StorePropertyDiagnostic
	row := f.transport.getStoreProperties(context.Background(), f.session, f.state, ops[0], f.handles, &diagnostic, view, nil)
	if len(row) > bound || !bytes.Equal(row, []byte{7, 1, 0, 0, 0, 0, 1, 0x0a, 0x0f, 1, 4, 0x80}) {
		t.Fatal("concurrent growth escaped immutable snapshot bound")
	}
	// The next batch sees the large value and requires more output, without
	// executing a prefix Release. The whole response later fits a large buffer.
	body, _ := f.execute(append([]byte{1, 4, 1}, folderPropertiesOperation(4, 1, 0, 0x7c040102)...), 4096)
	if storeResponse(t, body).Operations[0] != 0xff {
		t.Fatal("large value did not request a larger buffer")
	}
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
		t.Fatal("Release ran before size check")
	}
	row, _ = f.run(folderPropertiesOperation(4, 1, 0, 0x7c040102))
	if !bytes.Equal(row[9:], large) {
		t.Fatal("large snapshot truncated")
	}
	view = f.transport.ostView(f.session.Identity().Scope, ops)
	object.binding.store.setOSTValue(t.Context(), object.fid, []byte{1})
	got, ok := view.value(object.binding.store, object.fid)
	if !ok || !bytes.Equal(got, large) {
		t.Fatal("snapshot aliases mutable store")
	}
	otherScope := f.session.Identity().Scope
	otherScope.Account = "different"
	other := f.transport.ostView(otherScope, ops)
	if _, ok := other.value(object.binding.store, object.fid); ok || other.bound() != 0 {
		t.Fatal("snapshot reads another scope")
	}
}

func TestOSTBatchReadYourWriteAndFailedWriteKeepsView(t *testing.T) {
	f := newOSTFixture(t)
	read := folderPropertiesOperation(4, 1, 0, 0x7c040102)
	batch := append(ostSetOperation(4, 1, []byte{0xaa, 0xbb}), read...)
	body, counts := f.execute(batch, 4096)
	wantRead := []byte{7, 1, 0, 0, 0, 0, 0, 2, 0, 0xaa, 0xbb}
	want := append([]byte{0x0a, 1, 0, 0, 0, 0, 0, 0}, wantRead...)
	if !bytes.Equal(storeResponse(t, body).Operations, want) || counts.properties.Values != 1 {
		t.Fatal("batch cannot read its successful write")
	}
	oversize := append(ostSetOperation(4, 1, make([]byte, maxOSTValueBytes+1)), read...)
	body, _ = f.execute(oversize, 4096)
	want = append([]byte{0x0a, 1, 0, 0, 0, 0, 1, 0, 0, 0, 2, 1, 4, 0x7c, 1, 3, 4, 0x80}, wantRead...)
	if !bytes.Equal(storeResponse(t, body).Operations, want) {
		t.Fatal("oversize write enlarged view or erased previous value")
	}
	f.box.state.Owner = false
	body, _ = f.execute(append(ostSetOperation(4, 1, []byte{1}), read...), 4096)
	want = append(ropError(0x0a, 1, accessDenied), wantRead...)
	if !bytes.Equal(storeResponse(t, body).Operations, want) {
		t.Fatal("denied write changed batch read view")
	}
}

func TestOSTReadSizingDuringConcurrentRAMUpdates(t *testing.T) {
	f := newOSTFixture(t)
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	var wg sync.WaitGroup
	wg.Go(func() {
		for i := 0; i < 128; i++ {
			size := 2
			if i%2 != 0 {
				size = maxOSTValueBytes
			}
			object.binding.store.setOSTValue(t.Context(), object.fid, bytes.Repeat([]byte{0xcc}, size))
		}
	})
	for i := 0; i < 128; i++ {
		body, _ := f.execute(folderPropertiesOperation(4, 1, 0, 0x7c040102), 4096)
		row := storeResponse(t, body).Operations
		if row[0] != 0xff && (len(row) > 12 || row[0] != 7) {
			t.Fatal("read grew beyond reserved small output")
		}
	}
	wg.Wait()
}

func FuzzOSTReadOutputSizing(f *testing.F) {
	f.Add(uint16(0), uint16(2), uint16(4096), byte(3))
	f.Add(uint16(32768), uint16(1), uint16(256), byte(1))
	f.Add(uint16(2), uint16(32769), uint16(4096), byte(7))
	f.Fuzz(func(t *testing.T, oldSize, writeSize, output uint16, flags byte) {
		fixture := newOSTFixture(t)
		object, _ := fixture.state.mailbox.object(fixture.handles[1], 4)
		if flags&1 != 0 {
			object.binding.store.setOSTValue(t.Context(), object.fid, make([]byte, int(oldSize)%32769))
		}
		before, existed := object.binding.store.ostValue(object.fid)
		var ops []byte
		if flags&2 != 0 {
			ops = ostSetOperation(4, 1, make([]byte, int(writeSize)%32770))
		}
		tags := []uint32{0x7c040102}
		if flags&4 != 0 {
			tags = append(tags, 0x7c040000)
		}
		ops = append(ops, folderPropertiesOperation(4, 1, 0, tags...)...)
		maximum := uint32(32 + int(output)%65503)
		body, _ := fixture.execute(ops, maximum)
		rejected := len(body) == 8
		if !rejected {
			buffer := storeResponse(t, body)
			if len(buffer.Operations)+10+4*len(buffer.Handles) > int(maximum) {
				t.Fatal("response escaped preflight output bound")
			}
			rejected = buffer.Operations[0] == 0xff
		}
		if rejected {
			after, exists := object.binding.store.ostValue(object.fid)
			if exists != existed || !bytes.Equal(before, after) {
				t.Fatal("size rejection executed a write")
			}
		}
	})
}

func FuzzOSTValueDecoder(f *testing.F) {
	f.Add(ostSetOperation(4, 1, []byte{0xaa, 0xbb}))
	f.Add(ostSetOperation(4, 1, nil))
	f.Fuzz(func(t *testing.T, data []byte) {
		ops, err := (ROPBuffer{Operations: data, Handles: []uint32{1, 2}}).decodeOperations(true)
		if err != nil && ops != nil {
			t.Fatal("partial batch")
		}
		for _, op := range ops {
			if op.ID == 0x0a && len(op.OSTValue) > 65535-8 {
				t.Fatal("unbounded Binary")
			}
		}
	})
}
