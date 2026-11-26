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
	"unicode/utf16"

	"ewsgo/internal/core"
)

// Independent request fixture; no production request marshaler is used.
func createOperation(logon, input, output byte, name, comment string, existing bool) []byte {
	var open byte
	if existing {
		open = 1
	}
	out := []byte{0x1c, logon, input, output, 1, 1, open, 0}
	for _, text := range []string{name, comment} {
		for _, unit := range utf16.Encode([]rune(text)) {
			out = binary.LittleEndian.AppendUint16(out, unit)
		}
		out = append(out, 0, 0)
	}
	return out
}

func newCreateFixture(t *testing.T) *ostFixture {
	f := newOSTFixture(t)
	f.transport.EnableHierarchyDownloadSpike()
	f.transport.EnableCreateFolderSpike()
	f.box.page.Objects[0].Properties = core.Properties{core.DisplayName: core.Text("Native root")}
	f.handles = append(f.handles, 0xffffffff, 0xffffffff)
	f.open(int(IPMSubtree))
	return f
}

func TestCreateFolderCodecAndMalformedBatch(t *testing.T) {
	raw := createOperation(4, 1, 2, "Private 📨", "Private comment", true)
	buffer := ROPBuffer{Operations: raw, Handles: []uint32{1, 2, 3}}
	if _, err := buffer.DecodeOperations(); err == nil {
		t.Fatal("default gate open")
	}
	ops, err := buffer.decodeLaboratoryOperations(false, true, true)
	if err != nil || len(ops) != 1 || ops[0].CreateFolder.name != "Private 📨" || !ops[0].CreateFolder.existing {
		t.Fatal("Unicode fixture")
	}
	if strings.Contains(fmt.Sprintf("%v %#v", ops[0].CreateFolder, ops[0].CreateFolder), "Private") {
		t.Fatal("formatting leak")
	}
	for n := 0; n < len(raw); n++ {
		if n == 0 {
			continue
		}
		if _, err := (ROPBuffer{Operations: raw[:n], Handles: buffer.Handles}).decodeLaboratoryOperations(false, true, true); err == nil {
			t.Fatal("truncated request accepted", n)
		}
	}
	for _, name := range []string{strings.Repeat("x", 256), strings.Repeat("📨", 128)} {
		buffer.Operations = createOperation(4, 1, 2, name, "", false)
		if _, err := buffer.decodeLaboratoryOperations(false, true, true); err == nil {
			t.Fatal("name quota")
		}
	}
	buffer.Operations = createOperation(4, 1, 2, "x", strings.Repeat("x", 1025), false)
	if _, err := buffer.decodeLaboratoryOperations(false, true, true); err == nil {
		t.Fatal("comment quota")
	}
	for _, unit := range []uint16{0xd800, 0xdc00} {
		bad := append([]byte{0x1c, 4, 1, 2, 1, 1, 0, 0}, byte(unit), byte(unit>>8), 0, 0, 0, 0)
		if _, err := (ROPBuffer{Operations: bad, Handles: buffer.Handles}).decodeLaboratoryOperations(false, true, true); err == nil {
			t.Fatal("invalid surrogate")
		}
	}
}

func TestCreateFolderExecuteAndProjection(t *testing.T) {
	f := newCreateFixture(t)
	op := createOperation(4, 1, 2, "RAM 📨", "comment", false)
	result, counts := f.run(op)
	if len(result) != 15 || !bytes.Equal(result[:6], []byte{0x1c, 2, 0, 0, 0, 0}) || result[14] != 0 || counts.errors != 0 || counts.folders != 1 {
		t.Fatal("private response wire")
	}
	fid := binary.LittleEndian.Uint64(result[6:])
	createdHandle := f.handles[2]
	result, _ = f.run(op)
	if binary.LittleEndian.Uint32(result[2:]) != duplicateFolderName || f.handles[2] != createdHandle {
		t.Fatal("duplicate mutated handle")
	}
	result, _ = f.run(createOperation(4, 1, 2, "ram 📨", "changed", true))
	if len(result) != 15 || binary.LittleEndian.Uint32(result[2:]) != 0 || binary.LittleEndian.Uint64(result[6:]) != fid || result[14] != 0 {
		t.Fatal("reopen identity/private wire")
	}
	result, _ = f.run(folderOperation(fid, 4, 0, 2))
	if binary.LittleEndian.Uint32(result[2:]) != 0 {
		t.Fatal("OpenFolder dynamic")
	}
	result, counts = f.run(folderPropertiesOperation(4, 2, 1, 0x3001001f, 0x3004001f, 0x67480014, 0x67490014, 0x66380003, 0x360a000b))
	if counts.properties.Values != 6 || counts.properties.Errors != 0 || !bytes.Contains(result, []byte{'c', 0, 'o', 0, 'm', 0, 'm', 0, 'e', 0, 'n', 0, 't', 0, 0, 0}) {
		t.Fatal("Folder properties/comment preserved")
	}
	child, _ := f.run(createOperation(4, 2, 3, "Child", "", false))
	childFID := binary.LittleEndian.Uint64(child[6:])
	if binary.LittleEndian.Uint32(child[2:]) != 0 {
		t.Fatal("child creation")
	}
	result, counts = f.run(folderPropertiesOperation(4, 2, 1, 0x66380003, 0x360a000b))
	if counts.properties.Values != 2 || !bytes.Equal(result, []byte{7, 2, 0, 0, 0, 0, 0, 1, 0, 0, 0, 1}) {
		t.Fatal("child count and subfolders")
	}
	binding := f.state.mailbox.objects[f.handles[0]].binding
	snap, err := binding.store.observeHierarchy(context.Background(), f.session, binding.fids[MailboxRoot], hierarchyTestTime)
	if err != nil {
		t.Fatal(err)
	}
	row := hierarchyRow(t, snap, fid)
	childRow := hierarchyRow(t, snap, childFID)
	if childRow.parent != fid || row.name != "RAM 📨" || !bytes.Equal(childRow.parentSourceKey, row.sourceKey[:]) {
		t.Fatal("ICS parent linkage")
	}
	next, err := binding.store.observeHierarchy(context.Background(), f.session, binding.fids[MailboxRoot], hierarchyTestTime.Add(time.Hour))
	if err != nil || !reflect.DeepEqual(snap, next) {
		t.Fatal("identical observations changed IDs/CN/date")
	}
	state, err := newHierarchyState(context.Background(), f.session, binding.store, fid)
	if err != nil {
		t.Fatal("dynamic ICS anchor", err)
	}
	anchored, err := binding.store.observeHierarchy(context.Background(), f.session, fid, hierarchyTestTime.Add(time.Hour))
	if err != nil || len(anchored.folders) != 1 || anchored.folders[0].fid != childFID {
		t.Fatal("dynamic subtree")
	}
	delta, err := state.planHierarchy(context.Background(), f.session, anchored)
	if err != nil || len(delta.changes) != 1 {
		t.Fatal("initial dynamic delta")
	}
	state, err = newHierarchyState(context.Background(), f.session, binding.store, fid)
	if err != nil {
		t.Fatal(err)
	}
	uploadHierarchySet(t, state, f.session, idsetGivenTag, delta.finalGiven)
	uploadHierarchySet(t, state, f.session, cnsetSeenTag, delta.finalSeen)
	stable, err := state.planHierarchy(context.Background(), f.session, anchored)
	if err != nil || len(stable.changes) != 0 || len(stable.removed) != 0 {
		t.Fatal("dynamic no-change state")
	}
	// Rebinding a context keeps process-store metadata; restarting creates a new epoch.
	catalog, _ := privateFolders(context.Background(), f.session)
	rebound, err := f.transport.stores.bind(context.Background(), catalog)
	if err != nil || rebound.store != binding.store {
		t.Fatal("reconnect lost store")
	}
	fresh, err := newRAMStores().bind(context.Background(), catalog)
	if err != nil || fresh.store.replica == binding.store.replica || len(fresh.store.dynamic) != 0 {
		t.Fatal("restart retained objects/epoch")
	}
	f.box.getErr = core.ErrAccessDenied
	if _, err := binding.store.folder(context.Background(), f.session, childFID); err != core.ErrAccessDenied {
		t.Fatal("native revoke bypassed")
	}
}

func TestCreateFolderRejectsBeforeEffects(t *testing.T) {
	for _, kind := range []string{"unknown-tail", "malformed-tail", "small-output", "handle-quota", "wrong-logon", "logon-parent", "invalid-type", "reserved", "empty"} {
		t.Run(kind, func(t *testing.T) {
			f := newCreateFixture(t)
			op := createOperation(4, 1, 2, "new", "", false)
			max := uint32(4096)
			switch kind {
			case "unknown-tail":
				op = append(append([]byte{1, 4, 1}, op...), 0xaa, 4, 1)
			case "malformed-tail":
				op = append([]byte{1, 4, 1}, op[:len(op)-1]...)
			case "small-output":
				max = 30
			case "handle-quota":
				f.state.mailbox.allocated = maxContextHandleAllocations
			case "wrong-logon":
				op[1] = 5
			case "logon-parent":
				op[2] = 0
			case "invalid-type":
				op[4] = 3
			case "reserved":
				op[7] = 1
			case "empty":
				op = createOperation(4, 1, 2, "", "", false)
			}
			before := append([]uint32(nil), f.handles...)
			body, _ := f.execute(op, max)
			store := f.state.mailbox.objects[f.handles[0]].binding.store
			if len(store.dynamic) != 0 || !reflect.DeepEqual(f.handles, before) {
				t.Fatal("failure left RAM folder/handle")
			}
			if _, code := f.state.mailbox.object(before[1], 4); code != 0 {
				t.Fatal("prefix released parent on preflight failure")
			}
			if kind == "small-output" && storeResponse(t, body).Operations[0] != 0xff {
				t.Fatal("output preflight")
			}
		})
	}
	f := newCreateFixture(t)
	f.transport.createFolderSpike = false
	body, _ := f.execute(createOperation(4, 1, 2, "x", "", false), 4096)
	if binary.LittleEndian.Uint32(body[4:]) != NotSupported {
		t.Fatal("default behavior changed")
	}
}

func TestCreateFolderBoundsNativeCollisionAndRevocation(t *testing.T) {
	ctx := context.Background()
	f := newHierarchyFixture(t)
	root := f.binding.fids[MailboxRoot]
	create := func(parent uint64, name string, open bool) (uint64, uint32) {
		return f.store.createFolder(ctx, f.session, parent, createFolderRequest{kind: 1, name: name, existing: open})
	}

	// Fixed service sibling and native sibling collisions use the same rule.
	if _, code := create(root, "Views", false); code != duplicateFolderName {
		t.Fatal("service collision")
	}
	if id, code := create(root, "Views", true); code != 0 || id != f.binding.fids[Views] {
		t.Fatal("service reopen")
	}
	if _, code := create(f.binding.fids[IPMSubtree], "arbitrary locale/name", true); code != 0x80004005 {
		t.Fatal("ambiguous siblings selected arbitrarily")
	}
	parent, _ := create(f.binding.fids[IPMSubtree], "Parent 📨", true)
	native, err := f.store.folder(ctx, f.session, parent)
	if err != nil || native.native != "parent/opaque" {
		t.Fatal("native reopen")
	}
	leaf := parent
	for i := 0; i < 8; i++ {
		var code uint32
		leaf, code = create(leaf, fmt.Sprint("level", i), false)
		if code != 0 {
			t.Fatal("depth before limit")
		}
	}
	if _, code := create(leaf, "too deep", false); code != 0x8007000e {
		t.Fatal("depth quota")
	}
	other := scopedSession{Session: f.session, identity: core.Identity{Scope: core.Scope{Backend: "other", Account: "other"}}}
	if _, code := f.store.createFolder(ctx, other, root, createFolderRequest{name: "x"}); code != accessDenied {
		t.Fatal("cross scope")
	}
	// Removing the non-role native ancestor makes the retained RAM branch inaccessible.
	f.box.page.Objects = append(f.box.page.Objects[:4], f.box.page.Objects[6:]...)
	if _, err := f.store.folder(ctx, f.session, leaf); err == nil {
		t.Fatal("removed ancestor")
	}
	snap := f.snapshot(t, MailboxRoot, hierarchyTestTime)
	for _, row := range snap.folders {
		if row.fid == leaf {
			t.Fatal("orphan RAM branch exported")
		}
	}
}

func TestCreateFolderQuotaAndConcurrentUniqueness(t *testing.T) {
	f := newHierarchyFixture(t)
	ctx := context.Background()
	parent := f.binding.fids[MailboxRoot]
	var wg sync.WaitGroup
	codes := make(chan uint32, 12)
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, code := f.store.createFolder(ctx, f.session, parent, createFolderRequest{name: "same"})
			codes <- code
		}()
	}
	wg.Wait()
	close(codes)
	success := 0
	for code := range codes {
		if code == 0 {
			success++
		} else if code != duplicateFolderName {
			t.Fatal("unexpected concurrent error")
		}
	}
	if success != 1 {
		t.Fatal("concurrent duplicate")
	}
	for i := 1; i < 32; i++ {
		if _, code := f.store.createFolder(ctx, f.session, parent, createFolderRequest{name: fmt.Sprint(i)}); code != 0 {
			t.Fatal("quota before limit")
		}
	}
	if _, code := f.store.createFolder(ctx, f.session, parent, createFolderRequest{name: "over"}); code != 0x8007000e {
		t.Fatal("count quota")
	}
	fresh := newHierarchyFixture(t)
	fresh.store.dynamicBytes = maxDynamicBytes
	if _, code := fresh.store.createFolder(ctx, fresh.session, fresh.binding.fids[MailboxRoot], createFolderRequest{name: "over"}); code != 0x8007000e || len(fresh.store.dynamic) != 0 {
		t.Fatal("metadata quota")
	}
}

func FuzzCreateFolderParser(f *testing.F) {
	f.Add(createOperation(4, 1, 2, "name 📨", "comment", false))
	f.Add([]byte{0x1c, 4, 1, 2, 1, 1, 0, 0})
	f.Fuzz(func(t *testing.T, raw []byte) {
		ops, err := (ROPBuffer{Operations: raw, Handles: []uint32{1, 2, 3, 4}}).decodeLaboratoryOperations(true, true, true)
		if err != nil {
			return
		}
		for _, op := range ops {
			if op.CreateFolder != nil {
				request := op.CreateFolder
				if len(utf16.Encode([]rune(request.name))) > 255 || len(utf16.Encode([]rune(request.comment))) > 1024 || int(op.Output) >= 4 {
					t.Fatal("parser escaped limits")
				}
			}
		}
	})
}

func TestCreateFolderResourceAndAuthorizationFailures(t *testing.T) {
	for _, mode := range []string{"mapping", "counter", "canceled", "owner", "query", "allocator"} {
		t.Run(mode, func(t *testing.T) {
			f := newCreateFixture(t)
			store := f.state.mailbox.objects[f.handles[0]].binding.store
			switch mode {
			case "mapping":
				for n := len(store.byKey); n < maxRAMMappings; n++ {
					store.byKey[folderKey{dynamic: uint64(n + 10000)}] = folderID(uint64(n + 10000))
				}
			case "counter":
				store.next = maxGlobalCount
			case "owner":
				f.box.state.Owner = false
			case "query":
				f.box.err = core.ErrAccessDenied
			case "allocator":
				f.transport.stores.nextHandle.Store(0xfffffffe)
			}
			if mode == "canceled" {
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				if _, code := store.createFolder(ctx, f.session, f.fids[MailboxRoot], createFolderRequest{name: "no"}); code == 0 {
					t.Fatal("canceled creation")
				}
			} else {
				result, _ := f.run(createOperation(4, 1, 2, "no", "", false))
				if binary.LittleEndian.Uint32(result[2:]) == 0 {
					t.Fatal("failed resource/authorization succeeded")
				}
			}
			if len(store.dynamic) != 0 {
				t.Fatal("partial folder retained")
			}
		})
	}
}
