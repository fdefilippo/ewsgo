// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"
	"unicode/utf16"

	"ewsgo/internal/core"
)

func TestHierarchyImportExecuteReportsRefusalWithoutChangingWire(t *testing.T) {
	f := newHierarchyCollectorFixture(t)
	f.transport.EnableHierarchyImport()
	box := &placementTestMailbox{stateMailbox: f.box}
	f.session = &testSession{box}
	f.box.page.Objects[0].Properties = core.Properties{core.DisplayName: core.Text("Root")}
	faiRunOK(t, f, folderOperation(f.fids[IPMSubtree], 4, 0, 1))
	faiRunOK(t, f, []byte{1, 4, 2, 0x7e, 4, 1, 2, 0})
	collector, _ := f.state.mailbox.object(f.handles[2], 4)
	store := collector.binding.store
	f.box.page.Objects = append(f.box.page.Objects, core.Object{ID: "source", Parent: f.box.Root(), Kind: core.Folder, Revision: 1,
		Properties: core.Properties{core.DisplayName: core.Text("Source"), core.MetadataRevision: core.Integer(1)}})
	if _, err := store.observeHierarchy(context.Background(), f.session, collector.download.state.root, time.Now()); err != nil {
		t.Fatal(err)
	}
	request := hierarchyImport{
		source:   store.hierarchyXID(f.fids[IPMSubtree]),
		change:   gidChangeXID(hierarchyXID([16]byte{0xcf}, 1)),
		modified: core.Timestamp(hierarchyTestTime),
		name:     "private folder name",
	}
	source := placementImportWire(request)
	collectorWire := bytes.Clone(source)
	collectorWire[2] = 1
	request.source = hierarchyXID([16]byte{0xcf}, 2)
	foreign := placementImportWire(request)
	request.source = hierarchyXID(store.replica, maxGlobalCount-1)
	unreserved := placementImportWire(request)
	request.source = store.hierarchyXID(store.byKey[folderKey{native: "source"}])
	class := placementImportWire(request)
	copy(class[len(class)-9:], "IPF.Task\x00")
	property := placementImportWire(request)
	binary.LittleEndian.PutUint32(property[len(property)-13:], 0x3004001e)
	for _, tc := range []struct {
		wire   []byte
		reason string
	}{
		{collectorWire, "hierarchy_import_collector"},
		{source, "hierarchy_import_virtual_source"},
		{unreserved, "hierarchy_import_unreserved_source"},
		{foreign, "hierarchy_import_replica"},
		{class, "hierarchy_import_class"},
		{property, "hierarchy_import_property"},
	} {
		t.Run(tc.reason, func(t *testing.T) {
			before := box.pointReads
			f.transport.EnableUnknownPropertyTrace()
			f.transport.synchronizationOptionsTrace = false
			withoutTrace, counts := f.run(tc.wire)
			if len(counts.execution.HierarchyImportProperties().Tags) != 0 {
				t.Fatal("hierarchy property labels collected without both traces")
			}
			requireImportDiagnostic(t, counts.synchronizationOptions, "none")
			f.transport.synchronizationOptionsTrace = true
			withTrace, counts := f.run(tc.wire)
			if !bytes.Equal(withoutTrace, ropError(0x73, tc.wire[2], NotSupported)) ||
				!bytes.Equal(withoutTrace, withTrace) || counts.errors != 1 || box.updates != 0 {
				t.Fatal("diagnostic changed the refusal or performed a write")
			}
			requireImportDiagnostic(t, counts.synchronizationOptions, tc.reason)
			labels := counts.execution.HierarchyImportProperties().Tags
			if tc.reason == "hierarchy_import_property" {
				if box.pointReads != before+2 { // One root check per legacy catalogue read.
					t.Fatal("unsupported hierarchy properties caused a redundant source read")
				}
				if len(labels) != 1 || labels[0] != "0x3004001e" {
					t.Fatalf("unexpected hierarchy property labels %v", labels)
				}
			} else if len(labels) != 0 {
				t.Fatal("unrelated refusal collected hierarchy property labels")
			}
		})
	}
}

func TestHierarchyImportIgnoresReplayedProtectedDescendants(t *testing.T) {
	f := newHierarchyFixture(t)
	box := &placementTestMailbox{stateMailbox: f.box}
	f.session = &testSession{box}
	snapshot := f.snapshot(t, IPMSubtree, hierarchyTestTime)
	state := &hierarchyState{store: f.store, root: snapshot.root}
	for _, tc := range []struct {
		role    SpecialFolderRole
		refusal hierarchyImportRefusal
	}{
		{Inbox, hierarchyImportDistinguished},
		{Outbox, hierarchyImportVirtualSource},
	} {
		row := hierarchyRow(t, snapshot, f.binding.fids[tc.role])
		request := hierarchyImport{
			source: row.sourceKey, change: row.changeKey, modified: core.Timestamp(row.modified),
			name: "client name", predecessors: row.predecessors,
		}
		ctx := context.Background()
		if err := state.importMailFolderPlacement(ctx, f.session, request); err != errFAIIgnore || box.updates != 0 {
			t.Fatalf("replay of role %d: %v, writes %d", tc.role, err, box.updates)
		}
		request.change = gidChangeXID(hierarchyXID([16]byte{0xcf}, 9))
		if err := state.importMailFolderPlacement(ctx, f.session, request); err != tc.refusal || box.updates != 0 {
			t.Fatalf("new change to role %d: %v, writes %d", tc.role, err, box.updates)
		}
	}
}

type placementTestMailbox struct {
	*stateMailbox
	updates    int
	pointReads int
	fail       bool
}

func (b *placementTestMailbox) Get(ctx context.Context, id core.ID) (core.Object, error) {
	b.pointReads++
	return b.stateMailbox.Get(ctx, id)
}

func (*placementTestMailbox) CreateMailFolder(context.Context, core.MailFolderPlacement) (core.Object, error) {
	panic("placement import must not create a second folder")
}

func (b *placementTestMailbox) UpdateMailFolder(ctx context.Context, before core.Object, placement core.MailFolderPlacement) (core.Object, error) {
	b.updates++
	for i, current := range b.page.Objects {
		if current.ID != before.ID {
			continue
		}
		if current.Revision != before.Revision || current.Parent != before.Parent ||
			current.Properties[core.DisplayName] != before.Properties[core.DisplayName] {
			return core.Object{}, core.ErrConflict
		}
		current.Properties, _ = core.CloneProperties(current.Properties)
		current.Revision++
		current.Properties[core.MetadataRevision] = core.Integer(current.Revision)
		current.Properties[core.DisplayName] = core.Text(placement.Name)
		current.Parent = placement.Parent
		b.page.Objects[i] = current
		if b.fail {
			return core.Object{}, context.DeadlineExceeded
		}
		return b.Get(ctx, before.ID)
	}
	return core.Object{}, core.ErrNotFound
}

func placementImportWire(request hierarchyImport) []byte {
	out := []byte{0x73, 4, 2, 6, 0}
	var parent []byte
	if request.parent != ([22]byte{}) {
		parent = request.parent[:]
	}
	out = appendFAIRopBinary(out, 0x65e10102, parent)
	out = appendFAIRopBinary(out, 0x65e00102, request.source[:])
	out = binary.LittleEndian.AppendUint32(out, 0x30080040)
	out = binary.LittleEndian.AppendUint64(out, fileTime(time.Time(request.modified)))
	out = appendFAIRopBinary(out, 0x65e20102, []byte(request.change))
	out = appendFAIRopBinary(out, 0x65e30102, request.predecessors)
	out = binary.LittleEndian.AppendUint32(out, 0x3001001f)
	for _, ch := range utf16.Encode([]rune(request.name)) {
		out = binary.LittleEndian.AppendUint16(out, ch)
	}
	out = binary.LittleEndian.AppendUint16(out, 0)
	out = binary.LittleEndian.AppendUint16(out, 1)
	out = binary.LittleEndian.AppendUint32(out, 0x3613001e)
	return append(out, "IPF.Note\x00"...)
}

func TestNativePlacementImportThroughExecute(t *testing.T) {
	for _, scenario := range []string{"rename and move", "server wins equal timestamp", "client wins newer timestamp", "lost readback and retry"} {
		t.Run(scenario, func(t *testing.T) {
			f := newHierarchyCollectorFixture(t)
			f.transport.EnableHierarchyImport()
			f.box.page.Objects[0].Properties = core.Properties{core.DisplayName: core.Text("Root")}
			for _, id := range []core.ID{"source", "target"} {
				f.box.page.Objects = append(f.box.page.Objects, core.Object{ID: id, Kind: core.Folder, Parent: f.box.Root(), Revision: 1, Properties: core.Properties{
					core.DisplayName: core.Text(id), core.MetadataRevision: core.Integer(1),
				}})
			}
			box := &placementTestMailbox{stateMailbox: f.box}
			f.session = &testSession{box}
			faiRunOK(t, f, folderOperation(f.fids[IPMSubtree], 4, 0, 1))
			faiRunOK(t, f, []byte{1, 4, 2, 0x7e, 4, 1, 2, 0})
			object, _ := f.state.mailbox.object(f.handles[2], 4)
			store := object.binding.store
			collector := object.download.state
			ctx := context.Background()
			first, err := store.observeHierarchy(ctx, f.session, collector.root, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			fid := store.byKey[folderKey{native: "source"}]
			row := hierarchyRow(t, first, fid)
			target := hierarchyRow(t, first, store.byKey[folderKey{native: "target"}])
			clientChange := gidChangeXID(hierarchyXID([16]byte{0xcf}, 9))
			request := hierarchyImport{
				parent: target.sourceKey, source: row.sourceKey, modified: core.Timestamp(row.modified),
				change: clientChange, predecessors: row.predecessors, name: "moved €",
			}
			beforeWrite := func(req hierarchyImport, want uint32) {
				t.Helper()
				out, counts := f.run(placementImportWire(req))
				if !bytes.Equal(out, ropError(0x73, 2, want)) || counts.errors != 1 || box.updates != 0 {
					t.Fatal("unsafe request reached the writer", counts.errors, out)
				}
			}
			outside := request
			outside.parent = store.hierarchyXID(f.fids[MailboxRoot])
			beforeWrite(outside, accessDenied)
			cycle := request
			cycle.parent = request.source
			beforeWrite(cycle, 0x80040804)
			f.box.state.Owner = false
			beforeWrite(request, accessDenied)
			f.box.state.Owner = true
			_, counts := f.execute(placementImportWire(request), 13)
			if counts.errors != 0 || box.updates != 0 {
				t.Fatal("a short response budget executed a native write")
			}

			wantedUpdates, wantedName, wantedParent := 1, request.name, core.ID("target")
			conflict := scenario != "rename and move"
			switch scenario {
			case "server wins equal timestamp":
				request.predecessors = nil
				wantedUpdates, wantedName, wantedParent = 0, row.name, box.Root()
			case "client wins newer timestamp":
				request.predecessors = nil
				request.modified = core.Timestamp(row.modified.Add(time.Second))
			case "lost readback and retry":
				box.fail = true
				out, counts := f.run(placementImportWire(request))
				if len(out) != 6 || counts.errors != 1 || box.updates != 1 ||
					len(collector.seen.replicas) != 0 || store.hierarchy[folderKey{native: "source"}].cn != row.cn {
					t.Fatal("unverified readback advanced the version or state")
				}
				box.fail = false
			}
			withCreationTime := placementImportWire(request)
			binary.LittleEndian.PutUint16(withCreationTime[len(withCreationTime)-15:], 2)
			withCreationTime = binary.LittleEndian.AppendUint64(append32(withCreationTime, 0x30070040), fileTime(hierarchyTestTime))
			out := faiRunOK(t, f, withCreationTime)
			if !bytes.Equal(out, binary.LittleEndian.AppendUint64(ropError(0x73, 2, 0), 0)) || box.updates != wantedUpdates {
				t.Fatal("unexpected write count or success response")
			}
			snapshot, err := store.observeHierarchy(ctx, f.session, collector.root, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			after := hierarchyRow(t, snapshot, row.fid)
			native, err := box.Get(ctx, "source")
			if err != nil || native.Parent != wantedParent || after.name != wantedName || native.Properties[core.DisplayName] != core.Text(wantedName) ||
				after.sourceKey != row.sourceKey || after.cn <= row.cn {
				t.Fatal("native and projected placement diverged")
			}
			pcl, err := parseFAIPCL(after.predecessors)
			if err != nil || !pcl.includesXID(clientChange) || !pcl.includesXID(row.changeKey) ||
				(conflict && after.changeKey.namespace() != store.replica) || (!conflict && after.changeKey != clientChange) {
				t.Fatal("incorrect version lineage")
			}
			faiRunOK(t, f, []byte{0x82, 4, 2, 3})
			seen := hierarchyCollectorSeen(t, drainFX(t, f.run, 3, 11))
			if seen.contains(store.replica, after.cn) == conflict {
				t.Fatal("wrong final Seen for accepted or resolved version")
			}
			replay := placementImportWire(request)
			binary.LittleEndian.PutUint16(replay[len(replay)-15:], 2)
			replay = append(append32(replay, 0x3fe4000b), 1)
			out, counts = f.run(replay)
			if !bytes.Equal(out, ropError(0x73, 2, 0x80040801)) || counts.errors != 1 || box.updates != wantedUpdates {
				t.Fatal("replay attempted another native mutation")
			}
		})
	}
}

func TestSQLiteHierarchyMoveKeepsImportedLineageAfterRestart(t *testing.T) {
	f := newHierarchyFixture(t)
	ctx := t.Context()
	box := &placementTestMailbox{stateMailbox: f.box}
	f.session = &testSession{box}
	reopen := sqliteReplicaFixture(t, f)
	reopen()
	initial := f.snapshot(t, IPMSubtree, hierarchyTestTime)
	fid := f.store.byKey[folderKey{native: "child:opaque"}]
	current := hierarchyRow(t, initial, fid)
	parent := hierarchyRow(t, initial, f.binding.fids[Inbox])
	request := hierarchyImport{
		parent: parent.sourceKey, source: current.sourceKey, modified: core.Timestamp(current.modified),
		change: gidChangeXID(hierarchyXID([16]byte{0xfd}, 3)), predecessors: current.predecessors,
		name: "Moved with durable lineage",
	}
	c := hierarchyState{store: f.store, root: initial.root}
	if err := c.importMailFolderPlacement(ctx, f.session, request); err != nil {
		t.Fatal(err)
	}
	beforeRestart := f.store.hierarchy[f.store.byFID[fid]]
	reopen()
	latest := f.snapshot(t, IPMSubtree, hierarchyTestTime.Add(time.Hour))
	moved := hierarchyRow(t, latest, fid)
	if moved.parent != parent.fid || moved.changeKey != request.change || moved.cn != beforeRestart.cn || !bytes.Equal(moved.predecessors, []byte(beforeRestart.history)) {
		t.Fatal("confirmed move lost its FID, parent or imported history")
	}
	c = hierarchyState{store: f.store, root: latest.root}
	if err := c.importMailFolderPlacement(ctx, f.session, request); err != errFAIIgnore || box.updates != 1 {
		t.Fatalf("a replay repeated the native move: writes=%d err=%v", box.updates, err)
	}
}
