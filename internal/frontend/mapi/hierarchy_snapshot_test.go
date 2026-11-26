// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"ewsgo/internal/core"
)

var hierarchyTestTime = time.Date(2026, 11, 16, 4, 30, 0, 0, time.UTC)

type hierarchyFixture struct {
	store   *ramStore
	stores  *ramStores
	binding storeBinding
	session core.Session
	box     *stateMailbox
}

func newHierarchyFixture(t *testing.T) *hierarchyFixture {
	t.Helper()
	box := &stateMailbox{catalogMailbox: catalogFixture(), state: core.MailboxState{Owner: true}}
	box.page.Objects[0].Properties = core.Properties{core.DisplayName: core.Text("Native root")}
	box.page.Objects = append(box.page.Objects,
		core.Object{ID: "parent/opaque", Parent: box.Root(), Kind: core.Folder, Properties: core.Properties{core.DisplayName: core.Text("Parent 📨")}},
		core.Object{ID: "child:opaque", Parent: "parent/opaque", Kind: core.Folder, Properties: core.Properties{core.DisplayName: core.Text("Child")}},
	)
	session := &testSession{box}
	catalog, err := privateFolders(context.Background(), session)
	if err != nil {
		t.Fatal("test catalog")
	}
	stores := newRAMStores()
	binding, err := stores.bind(context.Background(), catalog)
	if err != nil {
		t.Fatal("test binding")
	}
	return &hierarchyFixture{store: binding.store, stores: stores, binding: binding, session: session, box: box}
}

func (f *hierarchyFixture) snapshot(t *testing.T, root SpecialFolderRole, at time.Time) hierarchySnapshot {
	t.Helper()
	snapshot, err := f.store.observeHierarchy(context.Background(), f.session, f.binding.fids[root], at)
	if err != nil {
		t.Fatal("snapshot failed", err)
	}
	return snapshot
}

func hierarchyRow(t *testing.T, snapshot hierarchySnapshot, fid uint64) hierarchyFolder {
	t.Helper()
	for _, row := range snapshot.folders {
		if row.fid == fid {
			return row
		}
	}
	t.Fatal("test row missing")
	return hierarchyFolder{}
}

func TestHierarchyObservationRetainsImportedPredecessors(t *testing.T) {
	f := newHierarchyFixture(t)
	first := f.snapshot(t, MailboxRoot, hierarchyTestTime)
	key := folderKey{native: "child:opaque"}
	fid := f.store.byKey[key]
	old := f.store.hierarchy[key]
	foreign := [16]byte{0xff}
	imported := changeXID(string(foreign[:]) + "\x01\x02\x03\x04\x05\x06\x07\x08")
	history := faiPCL{first.replica: old.change.local(), foreign: imported.local()}
	old.change, old.history = imported, string(history.marshal())
	f.store.hierarchy[key] = old

	current := f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(time.Minute))
	row := hierarchyRow(t, current, fid)
	if row.changeKey != imported || !bytes.Equal(row.predecessors, history.marshal()) || row.cn != old.cn {
		t.Fatal("unchanged observation modified the imported version")
	}
	stream, err := hierarchyFastTransfer(current.replica, [16]byte{}, synchronizationConfigure{}, hierarchyDelta{changes: []hierarchyFolder{row}})
	wire := binary.LittleEndian.AppendUint32(nil, 0x65e20102)
	wire = binary.LittleEndian.AppendUint32(wire, uint32(len(imported)))
	wire = append(wire, []byte(imported)...)
	wire = binary.LittleEndian.AppendUint32(wire, 0x65e30102)
	wire = binary.LittleEndian.AppendUint32(wire, uint32(len(row.predecessors)))
	wire = append(wire, row.predecessors...)
	if err != nil || !bytes.Contains(stream.data, wire) {
		t.Fatal("hierarchy FastTransfer lost the imported XID or its PCL")
	}

	// Removing and reobserving the folder preserves its identity and history.
	child := f.box.page.Objects[len(f.box.page.Objects)-1]
	f.box.page.Objects = f.box.page.Objects[:len(f.box.page.Objects)-1]
	f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(time.Hour))
	child.Properties[core.DisplayName] = core.Text("Native rename")
	f.box.page.Objects = append(f.box.page.Objects, child)
	current = f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(2*time.Hour))
	row = hierarchyRow(t, current, fid)
	next, err := parseFAIPCL(row.predecessors)
	if err != nil || row.changeKey.namespace() != current.replica || row.cn <= old.cn ||
		!next.includes(history) || !next.includesXID(row.changeKey) {
		t.Fatal("native change dropped a predecessor or failed to advance the local version", err)
	}
}

func TestHierarchySnapshotScopeOrderAndIdentifiers(t *testing.T) {
	f := newHierarchyFixture(t)
	snapshot := f.snapshot(t, MailboxRoot, hierarchyTestTime)
	if len(snapshot.folders) != 16 || len(snapshot.known) != 17 {
		t.Fatal("six native + eleven service folders, excluding anchor")
	}
	seen := map[uint64][22]byte{snapshot.root: {}}
	for _, row := range snapshot.folders {
		parent, ok := seen[row.parent]
		if !ok || row.fid == snapshot.root {
			t.Fatal("root included or parent comes after child")
		}
		if row.parent == snapshot.root {
			if len(row.parentSourceKey) != 0 {
				t.Fatal("root children need empty parent source key")
			}
		} else if !bytes.Equal(row.parentSourceKey, parent[:]) {
			t.Fatal("incorrect parent source key")
		}
		wire := binary.LittleEndian.AppendUint64(nil, row.fid)
		if !bytes.Equal(row.sourceKey[:16], snapshot.replica[:]) || !bytes.Equal(row.sourceKey[16:], wire[2:]) {
			t.Fatal("source key is not replica GUID plus FID's GLOBCNT")
		}
		count := globalCount(row.cn)
		if row.changeKey.namespace() != snapshot.replica || row.changeKey.local() != xidLocal(count[:]) || row.changeKey == gidChangeXID(row.sourceKey) || row.cn == 0 {
			t.Fatal("change number fixture")
		}
		if row.predecessors[0] != 22 || !bytes.Equal(row.predecessors[1:], []byte(row.changeKey)) {
			t.Fatal("single-replica PCL must contain latest CN as SizedXid")
		}
		if !row.modified.Equal(hierarchyTestTime) {
			t.Fatal("observation date not preserved")
		}
		seen[row.fid] = row.sourceKey
	}
	ipm := f.snapshot(t, IPMSubtree, hierarchyTestTime.Add(time.Hour))
	if len(ipm.folders) != 8 {
		t.Fatal("IPM descendants only, including virtual Outbox, Journal and Notes")
	}
	for _, row := range ipm.folders {
		if row.fid == f.binding.fids[IPMSubtree] {
			t.Fatal("native anchor included")
		}
		if row.parent == ipm.root && len(row.parentSourceKey) != 0 {
			t.Fatal("anchor-relative parent key")
		}
	}
	if len(f.snapshot(t, Outbox, hierarchyTestTime).folders) != 0 {
		t.Fatal("virtual leaf has descendants")
	}
	// Returned data never aliases the registry or another detached snapshot.
	ipm.folders[0].name = "caller edit"
	ipm.known[0] = 0
	for i := range snapshot.folders {
		if len(snapshot.folders[i].parentSourceKey) > 0 {
			snapshot.folders[i].parentSourceKey[0] ^= 1
		}
	}
	again := f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(2*time.Hour))
	if !reflect.DeepEqual(again, f.snapshot(t, MailboxRoot, hierarchyTestTime)) {
		t.Fatal("caller mutation or scans changed registry")
	}
}

func TestHierarchySnapshotStableReconnectAndChangeVersions(t *testing.T) {
	f := newHierarchyFixture(t)
	first := f.snapshot(t, MailboxRoot, hierarchyTestTime)
	// Backend order, observation clock, and unrelated folder counts do not
	// create changes. A replacement session binds to the same process store.
	for i, j := 0, len(f.box.page.Objects)-1; i < j; i, j = i+1, j-1 {
		f.box.page.Objects[i], f.box.page.Objects[j] = f.box.page.Objects[j], f.box.page.Objects[i]
	}
	f.box.page.Objects[0].Properties[core.Key{ID: 0x3602}] = core.Integer(120)
	catalog, _ := privateFolders(context.Background(), &testSession{f.box})
	bound, err := f.stores.bind(context.Background(), catalog)
	if err != nil || bound.store != f.store {
		t.Fatal("reconnect did not reuse replica")
	}
	nextCounts := f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(time.Hour))
	if nextCounts.highWater != first.highWater {
		t.Fatal("message counts advanced folder CN")
	}
	for i := range nextCounts.folders {
		nextCounts.folders[i].counts = first.folders[i].counts
	}
	if !reflect.DeepEqual(nextCounts, first) {
		t.Fatal("counts-only scan/reconnect changed folder versions")
	}
	fid := f.store.byKey[folderKey{native: "child:opaque"}]
	previous := hierarchyRow(t, first, fid)
	for i, alter := range []func(*core.Object){
		func(o *core.Object) { o.Properties[core.DisplayName] = core.Text("Renamed") },
		func(o *core.Object) { o.Parent = f.box.Root() },
		func(o *core.Object) { o.Revision = 91; o.Properties[core.MetadataRevision] = core.Integer(91) },
		func(o *core.Object) { o.Properties[core.LastModifiedAt] = core.Timestamp(time.Unix(123456, 0)) },
	} {
		for j := range f.box.page.Objects {
			if f.box.page.Objects[j].ID == "child:opaque" {
				alter(&f.box.page.Objects[j])
			}
		}
		at := hierarchyTestTime.Add(time.Duration(i+2) * time.Hour)
		next := f.snapshot(t, MailboxRoot, at)
		row := hierarchyRow(t, next, fid)
		if row.sourceKey != previous.sourceKey || row.cn <= previous.cn || row.changeKey == previous.changeKey || row.modified != at {
			t.Fatal("rename/move/revision/date failed to version one stable object", i)
		}
		for _, unchanged := range next.folders {
			if unchanged.fid == fid {
				continue
			}
			old := hierarchyRow(t, first, unchanged.fid)
			if unchanged.cn != old.cn || unchanged.modified != old.modified {
				t.Fatal("unrelated object changed")
			}
		}
		if same := f.snapshot(t, MailboxRoot, at.Add(time.Minute)); !reflect.DeepEqual(same, next) {
			t.Fatal("repeated changed scan creates another version")
		}
		previous = row
	}
	// A new process namespace is a new epoch; it cannot reuse old source keys.
	fresh, err := newRAMStores().bind(context.Background(), catalog)
	if err != nil || fresh.store.replica == f.store.replica {
		t.Fatal("restart reused RAM namespace")
	}
}

func TestHierarchySnapshotRemovalReappearanceAndAnchor(t *testing.T) {
	f := newHierarchyFixture(t)
	first := f.snapshot(t, MailboxRoot, hierarchyTestTime)
	key := folderKey{native: "child:opaque"}
	fid := f.store.byKey[key]
	old := hierarchyRow(t, first, fid)
	child := f.box.page.Objects[len(f.box.page.Objects)-1]
	f.box.page.Objects = f.box.page.Objects[:len(f.box.page.Objects)-1]
	next := f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(time.Hour))
	if len(next.folders) != 15 || len(next.known) != 17 || f.store.hierarchy[key].present || f.store.hierarchy[key].cn <= old.cn || f.store.byKey[key] != fid {
		t.Fatal("removed projection lost identity or retained presence")
	}
	if !reflect.DeepEqual(next, f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(2*time.Hour))) {
		t.Fatal("repeated absence creates deletions")
	}
	removedCN := f.store.hierarchy[key].cn
	before := f.store.next
	if partial, err := f.store.observeHierarchy(context.Background(), f.session, fid, hierarchyTestTime); !errors.Is(err, core.ErrNotFound) || !reflect.DeepEqual(partial, hierarchySnapshot{}) || f.store.next != before {
		t.Fatal("missing anchor became empty successful hierarchy")
	}
	f.box.page.Objects = append(f.box.page.Objects, child)
	row := hierarchyRow(t, f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(3*time.Hour)), fid)
	if row.sourceKey != old.sourceKey || row.cn <= removedCN {
		t.Fatal("reappearance identity/version")
	}
	// Move the same child outside the selected subtree: it remains known/live
	// globally, but must disappear from that root's projected descendants.
	parent := f.store.byKey[folderKey{native: "parent/opaque"}]
	sub, err := f.store.observeHierarchy(context.Background(), f.session, parent, hierarchyTestTime)
	if err != nil || len(sub.folders) != 1 || len(sub.folders[0].parentSourceKey) != 0 {
		t.Fatal("nested scope")
	}
	f.box.page.Objects[len(f.box.page.Objects)-1].Parent = f.box.Root()
	sub, err = f.store.observeHierarchy(context.Background(), f.session, parent, hierarchyTestTime.Add(4*time.Hour))
	if err != nil || len(sub.folders) != 0 || !f.store.hierarchy[key].present {
		t.Fatal("move outside subtree treated as global delete")
	}
}

// Frozen state is test-only and includes all structures touched by snapshots.
func hierarchyRegistryState(s *ramStore) string {
	return fmt.Sprintf("%d/%#v/%#v/%#v", s.next, s.byKey, s.byFID, s.hierarchy)
}

func TestHierarchySnapshotFailureAtomicity(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*hierarchyFixture)
		want   error
	}{
		{"query denied", func(f *hierarchyFixture) { f.box.err = core.ErrAccessDenied }, core.ErrAccessDenied},
		{"read revoked", func(f *hierarchyFixture) { f.box.getErr = core.ErrAccessDenied }, core.ErrAccessDenied},
		{"owner revoked", func(f *hierarchyFixture) { f.box.state.Owner = false }, core.ErrAccessDenied},
		{"owner read failed", func(f *hierarchyFixture) { f.box.stateErr = core.ErrAccessDenied }, core.ErrAccessDenied},
		{"owner unsupported", func(f *hierarchyFixture) { f.session = &testSession{f.box.catalogMailbox} }, core.ErrUnsupported},
		{"foreign scope", func(f *hierarchyFixture) {
			id := f.session.Identity()
			id.Scope.Account = "foreign"
			f.session = scopedSession{Session: f.session, identity: id}
		}, core.ErrAccessDenied},
		{"truncated", func(f *hierarchyFixture) { f.box.page.More = true }, core.ErrUnsupported},
		{"duplicate", func(f *hierarchyFixture) { f.box.page.Objects[5].ID = f.box.page.Objects[4].ID }, core.ErrConflict},
		{"cycle", func(f *hierarchyFixture) { f.box.page.Objects[4].Parent = "child:opaque" }, core.ErrInvalid},
		{"missing parent", func(f *hierarchyFixture) { f.box.page.Objects[5].Parent = "unknown" }, core.ErrInvalid},
		{"absent name", func(f *hierarchyFixture) { delete(f.box.page.Objects[5].Properties, core.DisplayName) }, core.ErrInvalid},
		{"invalid name", func(f *hierarchyFixture) {
			f.box.page.Objects[5].Properties[core.DisplayName] = core.Text("bad\x00name")
		}, core.ErrInvalid},
		{"invalid UTF8", func(f *hierarchyFixture) { f.box.page.Objects[5].Properties[core.DisplayName] = core.Text("\xff") }, core.ErrInvalid},
		{"long name", func(f *hierarchyFixture) {
			f.box.page.Objects[5].Properties[core.DisplayName] = core.Text(strings.Repeat("x", maxHierarchyText+1))
		}, core.ErrUnsupported},
		{"wrong revision type", func(f *hierarchyFixture) { f.box.page.Objects[5].Properties[core.MetadataRevision] = core.Text("1") }, core.ErrInvalid},
		{"inconsistent revision", func(f *hierarchyFixture) { f.box.page.Objects[5].Properties[core.MetadataRevision] = core.Integer(1) }, core.ErrInvalid},
		{"wrong date type", func(f *hierarchyFixture) { f.box.page.Objects[5].Properties[core.LastModifiedAt] = core.Integer(1) }, core.ErrInvalid},
		{"bad date", func(f *hierarchyFixture) {
			f.box.page.Objects[5].Properties[core.LastModifiedAt] = core.Timestamp(time.Time{})
		}, core.ErrInvalid},
		{"counter exhausted", func(f *hierarchyFixture) {
			f.store.next = maxGlobalCount
			f.box.page.Objects[5].Properties[core.DisplayName] = core.Text("changed")
		}, core.ErrUnsupported},
		{"new mapping needs both FID and CN", func(f *hierarchyFixture) {
			f.store.next = maxGlobalCount - 1
			f.box.page.Objects = append(f.box.page.Objects, core.Object{ID: "new-folder", Kind: core.Folder, Parent: f.box.Root(), Properties: core.Properties{core.DisplayName: core.Text("new")}})
		}, core.ErrUnsupported},
		{"mapping exhausted", func(f *hierarchyFixture) {
			for len(f.store.byKey) < maxRAMMappings {
				n := len(f.store.byKey)
				key := folderKey{native: core.ID(fmt.Sprintf("reserved-%d", n))}
				fid := folderID(uint64(n + 100))
				f.store.byKey[key], f.store.byFID[fid] = fid, key
			}
			f.box.page.Objects = append(f.box.page.Objects, core.Object{ID: "new-folder", Kind: core.Folder, Parent: f.box.Root(), Properties: core.Properties{core.DisplayName: core.Text("new")}})
		}, core.ErrUnsupported},
		{"byte budget", func(f *hierarchyFixture) {
			for i := 0; i < 100; i++ {
				f.box.page.Objects = append(f.box.page.Objects, core.Object{ID: core.ID(fmt.Sprintf("large-%d", i)), Kind: core.Folder, Parent: f.box.Root(), Properties: core.Properties{core.DisplayName: core.Text(strings.Repeat("x", maxHierarchyText))}})
			}
		}, core.ErrUnsupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newHierarchyFixture(t)
			f.snapshot(t, MailboxRoot, hierarchyTestTime)
			tc.change(f)
			before := hierarchyRegistryState(f.store)
			partial, err := f.store.observeHierarchy(context.Background(), f.session, f.binding.fids[MailboxRoot], hierarchyTestTime.Add(time.Hour))
			if !errors.Is(err, tc.want) || !reflect.DeepEqual(partial, hierarchySnapshot{}) || hierarchyRegistryState(f.store) != before {
				t.Fatal("failed snapshot committed partial data", err)
			}
		})
	}
}

type hierarchyHookMailbox struct {
	*stateMailbox
	queryHook func()
	ownerHook func()
}

func (b *hierarchyHookMailbox) Query(ctx context.Context, q core.Query) (core.Page, error) {
	if b.queryHook != nil {
		b.queryHook()
	}
	return b.stateMailbox.Query(ctx, q)
}
func (b *hierarchyHookMailbox) MailboxState(ctx context.Context) (core.MailboxState, error) {
	if b.ownerHook != nil {
		b.ownerHook()
	}
	return b.stateMailbox.MailboxState(ctx)
}

func TestHierarchySnapshotRevocationCancellationAndOverflowBoundary(t *testing.T) {
	for _, mode := range []string{"owner changes during query", "cancel during query", "cancel at commit", "unknown anchor", "invalid clock"} {
		t.Run(mode, func(t *testing.T) {
			f := newHierarchyFixture(t)
			f.snapshot(t, MailboxRoot, hierarchyTestTime)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			box := &hierarchyHookMailbox{stateMailbox: f.box}
			f.session = &testSession{box}
			anchor, at := f.binding.fids[MailboxRoot], hierarchyTestTime
			switch mode {
			case "owner changes during query":
				box.queryHook = func() { f.box.state.Owner = false }
			case "cancel during query":
				box.queryHook = cancel
			case "cancel at commit":
				calls := 0
				box.ownerHook = func() {
					calls++
					if calls == 2 {
						cancel()
					}
				}
			case "unknown anchor":
				anchor = 0
			case "invalid clock":
				at = time.Time{}
			}
			before := hierarchyRegistryState(f.store)
			if partial, err := f.store.observeHierarchy(ctx, f.session, anchor, at); err == nil || !reflect.DeepEqual(partial, hierarchySnapshot{}) || hierarchyRegistryState(f.store) != before {
				t.Fatal("failed access/cancellation changed RAM")
			}
		})
	}
	f := newHierarchyFixture(t)
	f.snapshot(t, MailboxRoot, hierarchyTestTime)
	f.store.next = maxGlobalCount - 1
	f.box.page.Objects[5].Properties[core.DisplayName] = core.Text("last version")
	last := f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(time.Hour))
	if f.store.next != maxGlobalCount || !reflect.DeepEqual(last, f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(2*time.Hour))) {
		t.Fatal("last valid CN or unchanged scan at exhaustion")
	}
}

func TestHierarchySnapshotConcurrentReservationsAndIdenticalScans(t *testing.T) {
	f := newHierarchyFixture(t)
	var wg sync.WaitGroup
	errs := make(chan error, 16)
	snapshots := make(chan hierarchySnapshot, 8)
	for i := 0; i < 8; i++ {
		wg.Add(2)
		go func() {
			defer wg.Done()
			s, err := f.store.observeHierarchy(context.Background(), f.session, f.binding.fids[MailboxRoot], hierarchyTestTime)
			if err == nil {
				snapshots <- s
			}
			errs <- err
		}()
		go func() {
			defer wg.Done()
			_, code := f.store.reserveLocalIDs(context.Background(), 100)
			if code != 0 {
				errs <- core.ErrUnsupported
			} else {
				errs <- nil
			}
		}()
	}
	wg.Wait()
	close(errs)
	close(snapshots)
	for err := range errs {
		if err != nil {
			t.Fatal("concurrent snapshot/reservation")
		}
	}
	var first []hierarchyFolder
	for snapshot := range snapshots {
		if first == nil {
			first = snapshot.folders
		} else if !reflect.DeepEqual(first, snapshot.folders) {
			t.Fatal("identical concurrent scans create versions")
		}
	}
	seen := map[uint64]bool{}
	for fid := range f.store.byFID {
		wire := binary.LittleEndian.AppendUint64(nil, fid)
		var count uint64
		for _, b := range wire[2:] {
			count = count<<8 | uint64(b)
		}
		seen[count] = true
	}
	for _, v := range f.store.hierarchy {
		if seen[v.cn] {
			t.Fatal("FID and CN share counter value")
		}
		seen[v.cn] = true
	}
	if f.store.next != 13+4+17+800 {
		t.Fatal("counter reservations or duplicate version allocation")
	}
}

func TestHierarchySnapshotRedactionAndClockCorrection(t *testing.T) {
	f := newHierarchyFixture(t)
	first := f.snapshot(t, MailboxRoot, hierarchyTestTime)
	f.box.page.Objects[1].Properties[core.DisplayName] = core.Text("renamed during clock correction")
	next := f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(-time.Hour))
	row := hierarchyRow(t, next, f.binding.fids[Inbox])
	old := hierarchyRow(t, first, f.binding.fids[Inbox])
	if row.cn <= old.cn || row.modified != old.modified {
		t.Fatal("clock correction must not decrease existing date")
	}
	for _, v := range []any{next, row, hierarchyNode{name: "secret"}} {
		for _, format := range []string{"%v", "%+v", "%#v"} {
			if strings.Contains(fmt.Sprintf(format, v), "secret") || !strings.Contains(fmt.Sprintf(format, v), "redacted") {
				t.Fatal("diagnostic formatting leaks snapshot")
			}
		}
	}
}

func TestHierarchySnapshotOptionalZeroRevisionAndMetadataAbsence(t *testing.T) {
	f := newHierarchyFixture(t)
	first := f.snapshot(t, MailboxRoot, hierarchyTestTime)
	fid := f.binding.fids[Inbox]
	old := hierarchyRow(t, first, fid)
	object := &f.box.page.Objects[1]
	object.Properties[core.MetadataRevision] = core.Integer(0)
	present := f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(time.Hour))
	row := hierarchyRow(t, present, fid)
	if row.cn <= old.cn || row.sourceKey != old.sourceKey || object.Properties[core.LastModifiedAt] != nil {
		t.Fatal("optional zero sequence lost presence or native timestamp invented")
	}
	delete(object.Properties, core.MetadataRevision)
	absent := f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(2*time.Hour))
	if hierarchyRow(t, absent, fid).cn <= row.cn || !reflect.DeepEqual(absent, f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(3*time.Hour))) {
		t.Fatal("removing optional metadata did not produce exactly one version")
	}
}
