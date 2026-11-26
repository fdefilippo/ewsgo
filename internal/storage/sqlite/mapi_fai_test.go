// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"bytes"
	"errors"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestMAPIFAIPersistsTypedValuesAndTombstonesAtomically(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "fai.sqlite")
	store, mailbox := openJournalFixture(t, path)
	snapshot, err := store.LoadMAPIReplica(ctx, mailbox)
	if err != nil {
		t.Fatal(err)
	}
	head := snapshot.Version
	now := time.Date(2026, 11, 24, 11, 0, 0, 123456700, time.UTC)
	properties := core.Properties{
		core.Subject:                          core.Text("Vista con accento è"),
		core.IsRead:                           core.Bool(false),
		core.LastModifiedAt:                   core.Timestamp(now),
		{ID: 0x1000}:                          core.Binary{0, 255, 0, 1},
		{Set: testPropertySet, ID: 19}:        core.Integer(-129),
		{Set: testPropertySet, Name: "Views"}: core.Strings{"", "α", "β"},
	}
	var source [22]byte
	copy(source[:], head.Epoch[:])
	source[21] = 2
	object := MAPIFAIObject{Counter: 2, Source: source, Properties: properties, CacheBytes: 2000}
	mappings := []MAPIObjectID{
		{Namespace: MAPIFolder, Key: []byte("inbox"), Counter: 1},
		{Namespace: MAPIAssociatedMessage, Key: []byte("view"), Counter: 2},
	}
	version := MAPIProjectionVersion{
		Counter: 2, Parent: 1, ChangeNumber: 3, ReadNumber: 4, NativeRevision: 1, Modified: now, Present: true,
		ChangeKey: append(bytes.Clone(source[:16]), 0, 0, 0, 0, 0, 3),
	}
	version.Predecessors = append([]byte{22}, version.ChangeKey...)
	if err := store.CommitMAPIFAI(ctx, mailbox, head, 4, mappings, []MAPIProjectionVersion{version}, MAPIFAIChanges{Objects: []MAPIFAIObject{object}}); err != nil {
		t.Fatal(err)
	}
	head.HighWater = 4

	stale := object
	stale.Properties = core.Properties{core.Subject: core.Text("non pubblicata")}
	next := version
	next.ChangeNumber, next.NativeRevision = 5, 2
	if err := store.CommitMAPIFAI(ctx, mailbox, head, 5, nil, []MAPIProjectionVersion{next}, MAPIFAIChanges{
		Objects: []MAPIFAIObject{stale}, Tombstones: []MAPIFAITombstone{{Source: source, Folder: 1}},
	}); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("duplicate source committed: %v", err)
	}
	tooMany := make([]MAPIFAITombstone, maxMAPIFAIObjects)
	for i := range tooMany {
		tooMany[i] = MAPIFAITombstone{Source: [22]byte{0x56, byte(i + 1)}, Folder: 1}
	}
	if err := store.CommitMAPIFAI(ctx, mailbox, head, 5, nil, nil, MAPIFAIChanges{Tombstones: tooMany}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("quota accepted: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _ := openJournalFixture(t, path)
	snapshot, err = reopened.LoadMAPIReplica(ctx, mailbox)
	if err != nil || snapshot.Version != head || len(snapshot.Projection) != 1 || len(snapshot.FAI.Objects) != 1 || len(snapshot.FAI.Tombstones) != 0 {
		t.Fatalf("atomic namespace after reopening: %v", err)
	}
	got := snapshot.FAI.Objects[0]
	if got.Counter != object.Counter || got.Source != source || !reflect.DeepEqual(got.Properties, properties) ||
		snapshot.Projection[0].NativeRevision != 1 || !bytes.Equal(snapshot.Projection[0].Predecessors, version.Predecessors) {
		t.Fatal("properties or version changed across reopening")
	}
	deleted := version
	deleted.ChangeNumber, deleted.NativeRevision, deleted.Present = 5, 2, false
	empty := object
	empty.Properties = nil
	absent := MAPIFAITombstone{Source: [22]byte{0x4f, 2}, Folder: 1}
	if err := reopened.CommitMAPIFAI(ctx, mailbox, head, 5, nil, []MAPIProjectionVersion{deleted}, MAPIFAIChanges{
		Objects: []MAPIFAIObject{empty}, Tombstones: []MAPIFAITombstone{absent},
	}); err != nil {
		t.Fatal(err)
	}
	check, _ := openJournalFixture(t, path)
	snapshot, err = check.LoadMAPIReplica(ctx, mailbox)
	if err != nil || snapshot.Version.HighWater != 5 || snapshot.Projection[0].Present ||
		len(snapshot.FAI.Objects) != 1 || len(snapshot.FAI.Objects[0].Properties) != 0 ||
		len(snapshot.FAI.Tombstones) != 1 || snapshot.FAI.Tombstones[0] != absent {
		t.Fatalf("deletions after reopening: %v", err)
	}
	other, err := check.BindMailbox(ctx, core.Scope{Backend: "fixture", Account: "independent"}, "one")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = check.LoadMAPIReplica(ctx, other.MailboxID)
	if err != nil || len(snapshot.FAI.Objects) != 0 || len(snapshot.FAI.Tombstones) != 0 {
		t.Fatalf("private data leaked across mailboxes: %v", err)
	}
}

func TestMAPIFAIBackupKeepsPrivateConfigurationInNewWireEpoch(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	live, archive, restored := filepath.Join(dir, "live.sqlite"), filepath.Join(dir, "backup.sqlite"), filepath.Join(dir, "restored.sqlite")
	store, mailbox := openJournalFixture(t, live)
	snapshot, err := store.LoadMAPIReplica(ctx, mailbox)
	if err != nil {
		t.Fatal(err)
	}
	old := snapshot.Version.Epoch
	source := [22]byte{16: 0x80, 21: 2}
	copy(source[:16], old[:])
	missing := source
	missing[21] = 7
	local := append(bytes.Clone(old[:]), 0, 0, 0, 0, 0, 3)
	foreign := append(bytes.Repeat([]byte{0xff}, 16), 20)
	pcl := append(append([]byte{22}, local...), append([]byte{17}, foreign...)...)
	values := core.Properties{{ID: 0x1000}: core.Binary{0, 255, 0, 1}}
	err = store.CommitMAPIFAI(ctx, mailbox, snapshot.Version, 3,
		[]MAPIObjectID{{Namespace: MAPIFolder, Key: []byte("freebusy"), Counter: 1}, {Namespace: MAPIMessage, Key: []byte("config"), Counter: 2}},
		[]MAPIProjectionVersion{{Counter: 2, Parent: 1, ChangeNumber: 3, Present: true, Modified: time.Now(), ChangeKey: local, Predecessors: pcl}},
		MAPIFAIChanges{
			Objects:    []MAPIFAIObject{{Counter: 2, Source: source, PrivateConfig: true, CacheBytes: 512, Properties: values}},
			Tombstones: []MAPIFAITombstone{{Source: missing, Folder: 1}},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := Backup(ctx, live, archive); err != nil {
		t.Fatal(err)
	}
	if err := Restore(ctx, archive, restored); err != nil {
		t.Fatal(err)
	}
	recovered, _ := openJournalFixture(t, restored)
	got, err := recovered.LoadMAPIReplica(ctx, mailbox)
	if err != nil || got.Version.Epoch == old || got.Version.HighWater != 3 ||
		len(got.FAI.Objects) != 1 || len(got.FAI.Tombstones) != 1 || len(got.Projection) != 1 {
		t.Fatalf("load restored configuration: %v", err)
	}
	current := got.Version.Epoch
	copy(source[:16], current[:])
	copy(missing[:16], current[:])
	copy(local[:16], current[:])
	copy(pcl[1:17], current[:])
	config := got.FAI.Objects[0]
	if !config.PrivateConfig || config.Source != source || !reflect.DeepEqual(config.Properties, values) ||
		got.FAI.Tombstones[0] != (MAPIFAITombstone{Source: missing, Folder: 1}) ||
		!bytes.Equal(got.Projection[0].ChangeKey, local) || !bytes.Equal(got.Projection[0].Predecessors, pcl) {
		t.Fatal("restore changed private values or did not rebase only local replica keys")
	}
}

func TestMAPIFAIImportClaimsReservedIDWithItsVersion(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "import.sqlite")
	store, mailbox := openJournalFixture(t, path)
	snapshot, err := store.LoadMAPIReplica(ctx, mailbox)
	if err != nil {
		t.Fatal(err)
	}
	head := snapshot.Version
	root := MAPIObjectID{Namespace: MAPIFolder, Key: []byte("root"), Counter: 1}
	if err := store.AdvanceMAPIReplica(ctx, mailbox, head, 2, []MAPIObjectID{root}); err != nil {
		t.Fatal(err)
	}
	head.HighWater = 2 // Counter 2 is a server allocation gap.
	issued, err := store.ReserveMAPIReplicaIDs(ctx, mailbox, head, 1)
	if err != nil {
		t.Fatal(err)
	}
	head.HighWater = issued.Last
	source := [22]byte{21: byte(issued.First)}
	copy(source[:16], head.Epoch[:])
	now := time.Now()
	mutation := MAPIReplicaMutation{
		HighWater:        head.HighWater + 1,
		ReservedBindings: []MAPIObjectID{{Namespace: MAPIAssociatedMessage, Key: []byte("associated"), Counter: 2}},
		Projection:       []MAPIProjectionVersion{{Counter: issued.First, Parent: 1, ChangeNumber: head.HighWater + 1, Modified: now, Present: true}},
		FAI: &MAPIFAIChanges{
			Objects:    []MAPIFAIObject{{Counter: issued.First, Source: source, CacheBytes: 512}},
			Tombstones: []MAPIFAITombstone{{Source: source, Folder: 1}},
		},
	}
	if err := store.CommitMAPIReplica(ctx, mailbox, head, mutation); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("consumed unreserved gap: %v", err)
	}
	mutation.ReservedBindings[0].Counter = issued.First
	if err := store.CommitMAPIReplica(ctx, mailbox, head, mutation); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("conflicting FAI write accepted: %v", err)
	}
	reopened, _ := openJournalFixture(t, path)
	got, err := reopened.LoadMAPIReplica(ctx, mailbox)
	if err != nil || got.Version != head || len(got.Bindings) != 1 || len(got.Projection) != 0 || len(got.FAI.Objects) != 0 {
		t.Fatalf("failed write consumed a reservation or published partial state: %v", err)
	}
	mutation.FAI.Tombstones = nil
	if err := reopened.CommitMAPIReplica(ctx, mailbox, head, mutation); err != nil {
		t.Fatal(err)
	}
	got, err = store.LoadMAPIReplica(ctx, mailbox)
	if err != nil || got.Version.HighWater != mutation.HighWater || len(got.Bindings) != 2 ||
		len(got.FAI.Objects) != 1 || got.FAI.Objects[0].Source != source || len(got.Projection) != 1 {
		t.Fatalf("reserved FAI publication missing: %v", err)
	}
}
