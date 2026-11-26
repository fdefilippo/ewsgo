// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestHierarchyUploadCodecIndependentFixtures(t *testing.T) {
	for _, tc := range []struct {
		kind byte
		wire []byte
		want hierarchyUpload
	}{
		{0x75, []byte{3, 0, 0x17, 0x40, 0x20, 0, 0, 0}, hierarchyUpload{kind: 0x75, property: 0x40170003, size: 32}},
		{0x76, []byte{3, 0, 0, 0, 0xaa, 0xbb, 0xcc}, hierarchyUpload{kind: 0x76, size: 3, data: []byte{0xaa, 0xbb, 0xcc}}},
		{0x77, nil, hierarchyUpload{kind: 0x77}},
	} {
		r := reader{data: append(append([]byte(nil), tc.wire...), 0xfe)}
		op, err := parseHierarchyUpload(&r, tc.kind)
		if err != nil || !reflect.DeepEqual(op, tc.want) || r.pos != len(tc.wire) {
			t.Fatal("independent framing fixture")
		}
		for i := range r.data {
			r.data[i] = 0
		}
		if !reflect.DeepEqual(op, tc.want) {
			t.Fatal("upload codec aliases caller")
		}
		for n := 0; n < len(tc.wire); n++ {
			r := reader{data: tc.wire[:n]}
			op, err := parseHierarchyUpload(&r, tc.kind)
			if err == nil || !reflect.DeepEqual(op, hierarchyUpload{}) {
				t.Fatal("partial request accepted")
			}
		}
	}
	for _, size := range []uint32{0, 0xffffffff} {
		r := reader{data: binary.LittleEndian.AppendUint32(nil, size)}
		if _, err := parseHierarchyUpload(&r, 0x76); !errors.Is(err, ErrMalformed) {
			t.Fatal("empty or overflowing continue")
		}
	}
	r := reader{data: binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32(nil, idsetGivenTag), maxIDSetBytes+1)}
	if _, err := parseHierarchyUpload(&r, 0x75); !errors.Is(err, ErrUnsupported) {
		t.Fatal("begin allocation quota")
	}
	// No operational decoder success or prefix effects have been introduced.
	for _, kind := range []byte{0x75, 0x76, 0x77} {
		if _, err := (ROPBuffer{Operations: []byte{1, 4, 0, kind, 4, 0}, Handles: []uint32{1}}).DecodeOperations(); !errors.Is(err, ErrUnsupported) {
			t.Fatal("upload enabled before full ICS download")
		}
	}
}

func newTestHierarchyState(t *testing.T, f *hierarchyFixture, role SpecialFolderRole) *hierarchyState {
	t.Helper()
	state, err := newHierarchyState(context.Background(), f.session, f.store, f.binding.fids[role])
	if err != nil {
		t.Fatal("test hierarchy state", err)
	}
	return state
}

func uploadHierarchySet(t *testing.T, s *hierarchyState, session core.Session, property uint32, set idSet) {
	t.Helper()
	raw, err := set.marshal()
	if err != nil {
		t.Fatal("test IDSET serialization")
	}
	if err := s.upload(context.Background(), session, hierarchyUpload{kind: 0x75, property: property, size: uint32(len(raw))}); err != nil {
		t.Fatal("upload Begin", err)
	}
	for len(raw) > 0 {
		n := min(11, len(raw))
		if err := s.upload(context.Background(), session, hierarchyUpload{kind: 0x76, size: uint32(n), data: raw[:n]}); err != nil {
			t.Fatal("upload Continue", err)
		}
		raw = raw[n:]
	}
	if err := s.upload(context.Background(), session, hierarchyUpload{kind: 0x77}); err != nil {
		t.Fatal("upload End", err)
	}
}

func TestHierarchyStateFullDeltaNoChangeRenameMoveRemovalAndReplay(t *testing.T) {
	f := newHierarchyFixture(t)
	snapshot := f.snapshot(t, MailboxRoot, hierarchyTestTime)
	initial := newTestHierarchyState(t, f, MailboxRoot)
	// An explicitly uploaded empty property is a valid initial set.
	uploadHierarchySet(t, initial, f.session, idsetGivenTag, idSet{})
	uploadHierarchySet(t, initial, f.session, cnsetSeenTag, idSet{})
	full, err := initial.planHierarchy(context.Background(), f.session, snapshot)
	if err != nil || len(full.changes) != 16 || len(full.removed) != 0 {
		t.Fatal("full hierarchy", err)
	}
	unchanged := newTestHierarchyState(t, f, MailboxRoot)
	uploadHierarchySet(t, unchanged, f.session, idsetGivenTag, full.finalGiven)
	uploadHierarchySet(t, unchanged, f.session, cnsetSeenTag, full.finalSeen)
	noChanges, err := unchanged.planHierarchy(context.Background(), f.session, f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(time.Hour)))
	if err != nil || len(noChanges.changes) != 0 || len(noChanges.removed) != 0 {
		t.Fatal("identical replay created changes")
	}
	parentID := f.store.byKey[folderKey{native: "parent/opaque"}]
	childID := f.store.byKey[folderKey{native: "child:opaque"}]
	f.box.page.Objects = append(f.box.page.Objects[:4], f.box.page.Objects[5:]...)
	f.box.page.Objects[4].Parent = f.box.Root()
	f.box.page.Objects[4].Properties[core.DisplayName] = core.Text("renamed and moved")
	f.box.page.Objects = append(f.box.page.Objects, core.Object{ID: "new-native", Parent: f.box.Root(), Kind: core.Folder, Properties: core.Properties{core.DisplayName: core.Text("new")}})
	changedSnapshot := f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(2*time.Hour))
	state := newTestHierarchyState(t, f, MailboxRoot)
	uploadHierarchySet(t, state, f.session, idsetGivenTag, full.finalGiven)
	uploadHierarchySet(t, state, f.session, cnsetSeenTag, full.finalSeen)
	delta, err := state.planHierarchy(context.Background(), f.session, changedSnapshot)
	if err != nil || len(delta.changes) != 2 || !reflect.DeepEqual(delta.removed, []uint64{parentID}) {
		t.Fatal("rename/move/new/removal delta", err)
	}
	child := hierarchyRow(t, hierarchySnapshot{folders: delta.changes}, childID)
	if child.sourceKey != hierarchyRow(t, snapshot, childID).sourceKey || child.parent != snapshot.root && child.parent != f.binding.fids[IPMSubtree] {
		t.Fatal("moved identity/parent")
	}
	if delta.finalGiven.contains(snapshot.replica, folderGlobalCount(parentID)) {
		t.Fatal("deleted projection remains in final Given")
	}
	replay := newTestHierarchyState(t, f, MailboxRoot)
	uploadHierarchySet(t, replay, f.session, idsetGivenTag, delta.finalGiven)
	uploadHierarchySet(t, replay, f.session, cnsetSeenTag, delta.finalSeen)
	again, err := replay.planHierarchy(context.Background(), f.session, changedSnapshot)
	if err != nil || len(again.changes) != 0 || len(again.removed) != 0 {
		t.Fatal("completed state replay is not stable")
	}
	if err := state.upload(context.Background(), f.session, hierarchyUpload{kind: 0x75, property: idsetGivenTag}); !errors.Is(err, core.ErrConflict) {
		t.Fatal("state upload after planning")
	}
}

func TestHierarchyStateUploadSequenceAndAtomicity(t *testing.T) {
	f := newHierarchyFixture(t)
	snapshot := f.snapshot(t, MailboxRoot, hierarchyTestTime)
	s := newTestHierarchyState(t, f, MailboxRoot)
	valid := singleReplicaSet(snapshot.replica, []idRange{{1, 2}})
	uploadHierarchySet(t, s, f.session, cnsetSeenTag, valid)
	previous, _ := s.seen.marshal()
	for _, op := range []hierarchyUpload{
		{kind: 0x76, size: 1, data: []byte{0}}, {kind: 0x77},
		{kind: 0x75, property: 0x679a0102}, {kind: 0x75, property: cnsetSeenTag, size: maxIDSetBytes + 1},
	} {
		if err := s.upload(context.Background(), f.session, op); err == nil || s.pending != nil {
			t.Fatal("invalid sequence/property/size changed state")
		}
	}
	if err := s.upload(context.Background(), f.session, hierarchyUpload{kind: 0x75, property: cnsetSeenTag, size: 1}); err != nil {
		t.Fatal("Begin")
	}
	for _, op := range []hierarchyUpload{{kind: 0x75, property: cnsetSeenTag}, {kind: 0x77}, {kind: 0x76, size: 0}, {kind: 0x76, size: 2, data: []byte{1, 2}}} {
		if err := s.upload(context.Background(), f.session, op); err == nil || len(s.pending.data) != 0 {
			t.Fatal("invalid pending request advanced input")
		}
	}
	raw := []byte{0xff}
	if err := s.upload(context.Background(), f.session, hierarchyUpload{kind: 0x76, size: 1, data: raw}); err != nil {
		t.Fatal("Continue")
	}
	raw[0] = 0
	if s.pending.data[0] != 0xff {
		t.Fatal("Continue aliases caller")
	}
	if err := s.upload(context.Background(), f.session, hierarchyUpload{kind: 0x77}); !errors.Is(err, ErrMalformed) {
		t.Fatal("invalid IDSET accepted")
	}
	after, _ := s.seen.marshal()
	if !bytes.Equal(previous, after) || s.pending == nil || s.pending.data[0] != 0xff {
		t.Fatal("failed End modified previous set or pending data")
	}
	// Two contexts never share pending streams or committed slices.
	other := newTestHierarchyState(t, f, MailboxRoot)
	if other.pending != nil || len(other.seen.replicas) != 0 {
		t.Fatal("state leaks between contexts")
	}
}

func TestHierarchyStateForeignEpochUnknownIDsAndHugeRanges(t *testing.T) {
	for _, mode := range []string{"foreign replica", "old RAM epoch", "future CN", "reserved nonfolder ID", "huge Given range"} {
		t.Run(mode, func(t *testing.T) {
			f := newHierarchyFixture(t)
			snapshot := f.snapshot(t, MailboxRoot, hierarchyTestTime)
			s := newTestHierarchyState(t, f, MailboxRoot)
			replica, property := snapshot.replica, cnsetSeenTag
			ranges := []idRange{{1, 1}}
			switch mode {
			case "foreign replica":
				replica[0] ^= 1
			case "old RAM epoch":
				other := newHierarchyFixture(t)
				replica = other.store.replica
			case "future CN":
				ranges = []idRange{{snapshot.highWater + 1, snapshot.highWater + 1}}
			case "reserved nonfolder ID":
				first, code := f.store.reserveLocalIDs(context.Background(), 1)
				if code != 0 {
					t.Fatal("test reservation")
				}
				var n uint64
				for _, b := range first {
					n = n<<8 | uint64(b)
				}
				ranges = []idRange{{n, n}}
				property = idsetGivenTag
			case "huge Given range":
				f.store.next = maxGlobalCount
				ranges = []idRange{{1, maxGlobalCount}}
				property = idsetGivenTag
			}
			raw, err := singleReplicaSet(replica, ranges).marshal()
			if err != nil {
				t.Fatal("test bytes")
			}
			if s.upload(context.Background(), f.session, hierarchyUpload{kind: 0x75, property: property, size: uint32(len(raw))}) != nil || s.upload(context.Background(), f.session, hierarchyUpload{kind: 0x76, size: uint32(len(raw)), data: raw}) != nil {
				t.Fatal("test stream upload")
			}
			if err := s.upload(context.Background(), f.session, hierarchyUpload{kind: 0x77}); !errors.Is(err, core.ErrResync) || len(s.given.replicas) != 0 || len(s.seen.replicas) != 0 {
				t.Fatal("foreign/stale/impossible state silently reset or accepted", err)
			}
		})
	}
}

func TestHierarchyStateRevocationCancellationAndRootBinding(t *testing.T) {
	for _, mode := range []string{"owner", "read", "scope", "cancel", "wrong root", "wrong epoch"} {
		t.Run(mode, func(t *testing.T) {
			f := newHierarchyFixture(t)
			snapshot := f.snapshot(t, MailboxRoot, hierarchyTestTime)
			s := newTestHierarchyState(t, f, MailboxRoot)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			switch mode {
			case "owner":
				f.box.state.Owner = false
			case "read":
				f.box.getErr = core.ErrAccessDenied
			case "scope":
				id := f.session.Identity()
				id.Scope.Account = "other"
				f.session = scopedSession{Session: f.session, identity: id}
			case "cancel":
				cancel()
			case "wrong root":
				snapshot.root = f.binding.fids[Inbox]
			case "wrong epoch":
				snapshot.replica[0] ^= 1
			}
			if mode != "wrong root" && mode != "wrong epoch" {
				if err := s.upload(ctx, f.session, hierarchyUpload{kind: 0x75, property: idsetGivenTag}); err == nil || s.pending != nil {
					t.Fatal("revocation/cancellation permits upload")
				}
			}
			if partial, err := s.planHierarchy(ctx, f.session, snapshot); err == nil || s.started || !reflect.DeepEqual(partial, hierarchyDelta{}) {
				t.Fatal("invalid context starts download")
			}
		})
	}
}

func TestHierarchyStatePendingUploadRevokedAndSnapshotTooOld(t *testing.T) {
	for _, phase := range []byte{0x76, 0x77} {
		f := newHierarchyFixture(t)
		snapshot := f.snapshot(t, MailboxRoot, hierarchyTestTime)
		s := newTestHierarchyState(t, f, MailboxRoot)
		raw, _ := singleReplicaSet(snapshot.replica, []idRange{{1, 1}}).marshal()
		if err := s.upload(context.Background(), f.session, hierarchyUpload{kind: 0x75, property: cnsetSeenTag, size: uint32(len(raw))}); err != nil {
			t.Fatal("test Begin")
		}
		if phase == 0x77 {
			if err := s.upload(context.Background(), f.session, hierarchyUpload{kind: 0x76, size: uint32(len(raw)), data: raw}); err != nil {
				t.Fatal("test Continue")
			}
		}
		before := append([]byte(nil), s.pending.data...)
		f.box.state.Owner = false
		op := hierarchyUpload{kind: phase, size: uint32(len(raw)), data: raw}
		if err := s.upload(context.Background(), f.session, op); !errors.Is(err, core.ErrAccessDenied) || !bytes.Equal(before, s.pending.data) || len(s.seen.replicas) != 0 {
			t.Fatal("revocation during upload changed state")
		}
	}
	f := newHierarchyFixture(t)
	old := f.snapshot(t, MailboxRoot, hierarchyTestTime)
	s := newTestHierarchyState(t, f, MailboxRoot)
	_, code := f.store.reserveLocalIDs(context.Background(), 1)
	if code != 0 {
		t.Fatal("test counter")
	}
	uploadHierarchySet(t, s, f.session, cnsetSeenTag, singleReplicaSet(old.replica, []idRange{{f.store.next, f.store.next}}))
	if _, err := s.planHierarchy(context.Background(), f.session, old); !errors.Is(err, core.ErrResync) || s.started {
		t.Fatal("snapshot predating uploaded state accepted")
	}
}

func FuzzHierarchyUpload(f *testing.F) {
	f.Add(byte(0x75), []byte{3, 0, 0x17, 0x40, 0, 0, 0, 0})
	f.Add(byte(0x76), []byte{1, 0, 0, 0, 0})
	f.Fuzz(func(t *testing.T, kind byte, raw []byte) {
		r := reader{data: raw}
		op, err := parseHierarchyUpload(&r, kind)
		if err != nil {
			if !reflect.DeepEqual(op, hierarchyUpload{}) {
				t.Fatal("partial upload result")
			}
			return
		}
		if len(op.data) > maxIDSetBytes || op.size > maxIDSetBytes || r.pos > len(raw) {
			t.Fatal("unbounded parsed upload")
		}
	})
}
