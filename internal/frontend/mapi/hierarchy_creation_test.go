// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"ewsgo/internal/core"
)

type creationTestMailbox struct {
	placementTestMailbox
	creates int
	failure string
	reject  error
	before  func()
}

func (b *creationTestMailbox) CreateMailFolder(_ context.Context, placement core.MailFolderPlacement) (core.Object, error) {
	b.creates++
	b.before()
	if b.reject != nil {
		return core.Object{}, b.reject
	}
	if b.failure == "rejected" {
		return core.Object{}, core.ErrConflict
	}
	folder := core.Object{
		ID: "new-mail-folder", Kind: core.Folder, Parent: placement.Parent, Revision: 1,
		Properties: core.Properties{core.DisplayName: core.Text(placement.Name), core.MetadataRevision: core.Integer(1)},
	}
	b.page.Objects = append(b.page.Objects, folder)
	switch b.failure {
	case "known":
		return core.Object{ID: folder.ID, Kind: core.Folder}, context.DeadlineExceeded
	case "unknown":
		return core.Object{}, context.DeadlineExceeded
	}
	return folder, nil
}

func TestSQLiteHierarchyCreationReleasesRejectedReservations(t *testing.T) {
	ctx := t.Context()
	f := newHierarchyFixture(t)
	box := &creationTestMailbox{placementTestMailbox: placementTestMailbox{stateMailbox: f.box}, before: func() {}}
	f.session = &testSession{box}
	reopen := sqliteReplicaFixture(t, f)
	db := reopen()
	const attempts = maxPendingHierarchyCreations + 2
	start, code := f.store.reserveLocalIDs(ctx, attempts)
	if code != 0 {
		t.Fatalf("reserve FIDs: %#x", code)
	}
	first := folderGlobalCount(fidFromGlobalCounter(start[:]))
	collector := hierarchyState{store: f.store, root: f.binding.fids[IPMSubtree]}
	rejections := []error{core.ErrInvalid, core.ErrConflict, core.ErrAccessDenied, core.ErrNotFound,
		core.ErrAuthentication, core.ErrUnsupported, core.ErrLimit, errors.Join(core.ErrWriteNotDispatched, context.DeadlineExceeded)}
	for i := range attempts {
		request := hierarchyImport{source: hierarchyXID(f.store.replica, first+uint64(i)), name: "Rejected creation",
			modified: core.Timestamp(hierarchyTestTime), change: gidChangeXID(hierarchyXID([16]byte{0xfc}, 1))}
		box.reject = rejections[i%len(rejections)]
		if err := collector.importMailFolderPlacement(ctx, f.session, request); !errors.Is(err, box.reject) {
			t.Fatalf("rejection %d: %v", i, err)
		}
	}
	if f.store.unboundCreations != 0 || box.creates != attempts {
		t.Fatalf("definitive rejections consumed quota: pending %d, attempts %d", f.store.unboundCreations, box.creates)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	reopen()
	collector = hierarchyState{store: f.store, root: f.binding.fids[IPMSubtree]}
	request := hierarchyImport{source: hierarchyXID(f.store.replica, first), name: "Retry after restart",
		modified: core.Timestamp(hierarchyTestTime), change: gidChangeXID(hierarchyXID([16]byte{0xfc}, 1))}
	box.reject = nil
	if err := collector.importMailFolderPlacement(ctx, f.session, request); err != nil || box.creates != attempts+1 {
		t.Fatalf("definitive rejection could not be retried: %v", err)
	}
}

func TestHierarchyFolderCreationThroughExecute(t *testing.T) {
	for _, failure := range []string{"", "known", "unknown", "rejected"} {
		t.Run(failure, func(t *testing.T) {
			f := newHierarchyCollectorFixture(t)
			f.transport.EnableHierarchyImport()
			f.box.page.Objects[0].Properties = core.Properties{core.DisplayName: core.Text("Root")}
			box := &creationTestMailbox{placementTestMailbox: placementTestMailbox{stateMailbox: f.box}, failure: failure}
			f.session = &testSession{box}
			faiRunOK(t, f, folderOperation(f.fids[IPMSubtree], 4, 0, 1))
			faiRunOK(t, f, []byte{1, 4, 2, 0x7e, 4, 1, 2, 0})
			object, _ := f.state.mailbox.object(f.handles[2], 4)
			collector := object.download.state
			store := object.binding.store
			ctx := context.Background()
			grant := faiRunOK(t, f, []byte{0x7f, 4, 0, 2, 0, 0, 0})
			var gid [22]byte
			copy(gid[:], grant[6:])
			fid := binary.LittleEndian.Uint64(append([]byte{1, 0}, gid[16:]...))
			box.before = func() {
				t.Helper()
				store.mu.Lock()
				defer store.mu.Unlock()
				if _, err := store.importedMIDLocked(messageNode{
					key:  messageKey{id: "same-fid-as-mid", associated: true},
					sync: &faiSyncVersion{source: gid},
				}); err != core.ErrConflict {
					t.Fatal("FAI claimed the in-flight folder identity")
				}
			}
			request := hierarchyImport{
				source: gid, name: "New by ICS", modified: core.Timestamp(time.Now()),
				change: gidChangeXID(hierarchyXID([16]byte{0xf1}, 7)),
			}
			unreserved := request
			unreserved.source[21] += 2
			out, counts := f.run(placementImportWire(unreserved))
			if !bytes.Equal(out, ropError(0x73, 2, NotSupported)) || counts.errors != 1 || box.creates != 0 {
				t.Fatal("creation accepted without reservation")
			}
			missingParent := request
			missingParent.parent = gid
			missingParent.parent[21]++
			out, counts = f.run(placementImportWire(missingParent))
			if !bytes.Equal(out, ropError(0x73, 2, 0x80040803)) || counts.errors != 1 || box.creates != 0 {
				t.Fatal("creation accepted before the parent existed")
			}
			wire := placementImportWire(request)
			if failure != "" {
				out, counts = f.run(wire)
				if len(out) != 6 || counts.errors != 1 || box.creates != 1 || len(collector.seen.replicas) != 0 {
					t.Fatal("failed native creation reported success or Seen")
				}
				// Simulate a downloader reading the native tree before the retry.
				if _, err := store.observeHierarchy(ctx, f.session, collector.root, time.Now()); err != nil {
					t.Fatal(err)
				}
				box.failure = ""
			}
			if failure == "unknown" {
				out, counts = f.run(wire)
				if !bytes.Equal(out, ropError(0x73, 2, 0x80040802)) || counts.errors != 1 || box.creates != 1 || store.byFID[fid].native != "" {
					t.Fatal("an uncertain write was replayed or adopted by name")
				}
				return
			}
			wantWrites := 1
			if failure == "rejected" {
				wantWrites = 2
			}
			out = faiRunOK(t, f, wire)
			if len(out) != 14 || box.creates != wantWrites {
				t.Fatal("wrong creation response or native write count")
			}
			faiRunOK(t, f, folderOperation(fid, 4, 0, 1))
			snapshot, err := store.observeHierarchy(ctx, f.session, collector.root, time.Now())
			if err != nil {
				t.Fatal(err)
			}
			row := hierarchyRow(t, snapshot, fid)
			if row.sourceKey != gid || row.changeKey != request.change || row.name != request.name ||
				!collector.seen.contains(store.replica, row.cn) || store.unboundCreations != 0 || len(store.pendingCreations) != 0 {
				t.Fatal("reservation or synchronization state did not survive native readback")
			}
			out, counts = f.run(wire)
			if !bytes.Equal(out, ropError(0x73, 2, 0x80040801)) || counts.errors != 1 || box.creates != wantWrites {
				t.Fatal("completed creation replayed a native mutation")
			}
		})
	}
}

func TestSQLiteHierarchyCreationResumesOnlyAcknowledgedIdentity(t *testing.T) {
	for _, failure := range []string{"known", "unknown"} {
		t.Run(failure, func(t *testing.T) {
			f := newHierarchyFixture(t)
			ctx := t.Context()
			box := &creationTestMailbox{
				placementTestMailbox: placementTestMailbox{stateMailbox: f.box},
				failure:              failure, before: func() {},
			}
			f.session = &testSession{box}
			reopen := sqliteReplicaFixture(t, f)
			db := reopen()
			start, code := f.store.reserveLocalIDs(ctx, 1)
			if code != 0 {
				t.Fatalf("reserve FID: %#x", code)
			}
			request := hierarchyImport{
				name: "Created before response loss", modified: core.Timestamp(hierarchyTestTime),
				change: gidChangeXID(hierarchyXID([16]byte{0xfc}, 1)),
			}
			copy(request.source[:], f.store.replica[:])
			copy(request.source[16:], start[:])
			fid := fidFromGlobalCounter(start[:])
			collector := hierarchyState{store: f.store, root: f.binding.fids[IPMSubtree]}
			if err := collector.importMailFolderPlacement(ctx, f.session, request); err != context.DeadlineExceeded {
				t.Fatalf("lost response: %v", err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			reopen()
			collector = hierarchyState{store: f.store, root: f.binding.fids[IPMSubtree]}
			box.failure = ""
			err := collector.importMailFolderPlacement(ctx, f.session, request)
			if failure == "unknown" {
				if err != core.ErrConflict || box.creates != 1 || f.store.byFID[fid].native != "" {
					t.Fatalf("uncertain creation was retried: writes=%d err=%v", box.creates, err)
				}
				return
			}
			if err != nil || box.creates != 1 {
				t.Fatalf("known creation did not resume: writes=%d err=%v", box.creates, err)
			}
			version := f.store.hierarchy[f.store.byFID[fid]]
			if version.change != request.change || !collector.seen.contains(f.store.replica, version.cn) {
				t.Fatal("the verified folder or client lineage was lost")
			}
			reopen()
			collector = hierarchyState{store: f.store, root: f.binding.fids[IPMSubtree]}
			if got := f.store.hierarchy[f.store.byFID[fid]]; got != version {
				t.Fatal("finalized creation version changed after restart")
			}
			if err := collector.importMailFolderPlacement(ctx, f.session, request); err != errFAIIgnore || box.creates != 1 {
				t.Fatalf("finalized request repeated: writes=%d err=%v", box.creates, err)
			}
		})
	}
}
