// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"errors"
	"math"
	"path/filepath"
	"reflect"
	"slices"
	"sync"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestObjectPropertiesSurviveReopenAndShareJournal(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "properties.sqlite")
	store, mailbox := openJournalFixture(t, path)
	const message core.ID = "one-message"
	created := core.Change{Kind: core.Created, ID: message, Parent: "inbox"}
	checkpoint, err := store.ApplyDelta(ctx, mailbox, DeltaCheckpoint{}, "page-one", []DeltaEvent{{Change: created}})
	if err != nil {
		t.Fatal(err)
	}

	zero := core.Key{Set: testPropertySet}
	empty := core.Key{Set: testPropertySet, EmptyName: true}
	registry, err := store.NamedPropertyIDs(ctx, mailbox, []core.Key{zero, empty}, true)
	if err != nil {
		t.Fatal(err)
	}
	header := core.Key{Set: internetHeadersSet, Name: "X-Test"}
	canonicalHeader := core.Key{Set: internetHeadersSet, Name: "x-test"}
	stamp := time.Date(2026, 11, 18, 21, 31, 0, 123456789, time.FixedZone("fixture", 3600))
	values := core.Properties{
		zero:    core.Bool(false),
		empty:   core.Text(""),
		{ID: 1}: core.Integer(math.MinInt64),
		{ID: 2}: core.Binary{0x00, 0xff, 0x01},
		{ID: 3}: core.Timestamp(stamp),
		header:  core.Strings{"", "è"},
	}
	local, err := store.PatchObjectProperties(ctx, mailbox, message, checkpoint.Revision, core.Patch{Set: values})
	if err != nil || local != 2 {
		t.Fatalf("first local revision: %d, %v", local, err)
	}
	if native, err := store.DeltaCheckpoint(ctx, mailbox); err != nil || native != checkpoint {
		t.Fatalf("local patch moved native checkpoint: %+v, %v", native, err)
	}

	moved := core.Change{Kind: core.Moved, ID: message, Parent: "archive", PreviousParent: "inbox"}
	native, err := store.ApplyDelta(ctx, mailbox, checkpoint, "page-two", []DeltaEvent{{Change: moved}})
	if err != nil || native.Revision != 3 {
		t.Fatalf("in-flight native page after local edit: %+v, %v", native, err)
	}
	values[core.Key{ID: 3}] = core.Timestamp(stamp.UTC())
	values[canonicalHeader] = values[header]
	delete(values, header)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, _ = openJournalFixture(t, path)
	actual, rev, err := store.ObjectProperties(ctx, mailbox, message)
	if err != nil || rev != 3 || !reflect.DeepEqual(actual, values) {
		t.Fatalf("persisted typed values at revision %d: %+v, %v", rev, actual, err)
	}
	if _, err := store.PatchObjectProperties(ctx, mailbox, message, 2, core.Patch{Remove: []core.Key{zero}}); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("stale object revision: %v", err)
	}
	if _, err := store.PatchObjectProperties(ctx, mailbox, message, 3, core.Patch{Set: core.Properties{zero: core.Text("wrong type")}}); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("replacement changes value type: %v", err)
	}
	if _, err := store.PatchObjectProperties(ctx, mailbox, message, 3, core.Patch{Set: core.Properties{header: core.Strings{}}, Remove: []core.Key{canonicalHeader}}); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("contradictory patch after key normalization: %v", err)
	}
	local, err = store.PatchObjectProperties(ctx, mailbox, message, 3, core.Patch{Set: values, Remove: []core.Key{{ID: 10}}})
	if err != nil || local != 3 {
		t.Fatalf("no-op: %d, %v", local, err)
	}
	local, err = store.PatchObjectProperties(ctx, mailbox, message, 3, core.Patch{Remove: []core.Key{zero}})
	if err != nil || local != 4 {
		t.Fatalf("explicit removal: %d, %v", local, err)
	}
	delete(values, zero)
	actual, rev, err = store.ObjectProperties(ctx, mailbox, message)
	if err != nil || rev != local || !reflect.DeepEqual(actual, values) {
		t.Fatalf("values after removal at revision %d: %+v, %v", rev, actual, err)
	}
	other, err := store.BindMailbox(ctx, core.Scope{Backend: "fixture", Account: "journal-test"}, "another-instance")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.ApplyDelta(ctx, other.MailboxID, DeltaCheckpoint{}, "other-page", []DeltaEvent{{Change: created}}); err != nil {
		t.Fatal(err)
	}
	if got, rev, err := store.ObjectProperties(ctx, other.MailboxID, message); err != nil || rev != 1 || len(got) != 0 {
		t.Fatalf("properties escaped scope: %+v at %d, %v", got, rev, err)
	}

	deleted := core.Change{Kind: core.Deleted, ID: message, PreviousParent: "archive"}
	afterDelete, err := store.ApplyDelta(ctx, mailbox, native, "delete-page", []DeltaEvent{{Change: deleted}})
	if err != nil || afterDelete.Revision != 5 {
		t.Fatalf("native deletion: %+v, %v", afterDelete, err)
	}
	if _, _, err := store.ObjectProperties(ctx, mailbox, message); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("deleted object still visible: %v", err)
	}
	var remaining int
	if err := store.db.QueryRowContext(ctx, "SELECT count(*) FROM object_properties WHERE mailbox_id = ?", mailbox).Scan(&remaining); err != nil || remaining != 0 {
		t.Fatalf("deleted object's values left on disk: %d, %v", remaining, err)
	}
	assigned, err := store.NamedPropertyIDs(ctx, mailbox, []core.Key{zero, empty}, false)
	if err != nil || !slices.Equal(assigned, registry) {
		t.Fatalf("deletion removed named registry: %x, %v", assigned, err)
	}
	created.Revision, moved.Revision, deleted.Revision = 1, 3, 5
	want := []core.Change{
		created, {Kind: core.Updated, ID: message, Parent: "inbox", Revision: 2},
		moved, {Kind: core.Updated, ID: message, Parent: "archive", Revision: 4}, deleted,
	}
	got, err := store.Journal(ctx, mailbox, JournalCursor{}, 10)
	if err != nil || !slices.Equal(got.Changes, want) {
		t.Fatalf("shared journal: %+v, %v", got, err)
	}
}

func TestConcurrentPropertyPatchesAndJournalFailure(t *testing.T) {
	ctx := t.Context()
	store, mailbox := openJournalFixture(t, filepath.Join(t.TempDir(), "patch.sqlite"))
	object := core.Change{Kind: core.Created, ID: "object"}
	page, err := store.ApplyDelta(ctx, mailbox, DeltaCheckpoint{}, "first", []DeltaEvent{{Change: object}})
	if err != nil {
		t.Fatal(err)
	}
	key := core.Key{ID: 5}
	var wg sync.WaitGroup
	var outcomes [2]error
	for i := range outcomes {
		wg.Go(func() {
			patch := core.Patch{Set: core.Properties{key: core.Integer(i)}}
			_, outcomes[i] = store.PatchObjectProperties(ctx, mailbox, object.ID, page.Revision, patch)
		})
	}
	wg.Wait()
	switch {
	case outcomes[0] == nil && errors.Is(outcomes[1], core.ErrConflict):
	case outcomes[1] == nil && errors.Is(outcomes[0], core.ErrConflict):
	default:
		t.Fatalf("expected one success and one conflict: %v", outcomes)
	}
	before, revision, err := store.ObjectProperties(ctx, mailbox, object.ID)
	if err != nil || revision != 2 || len(before) != 1 {
		t.Fatalf("committed property: %+v at %d, %v", before, revision, err)
	}
	if _, err := store.db.ExecContext(ctx, `
		CREATE TRIGGER fail_journal BEFORE INSERT ON journal
		BEGIN SELECT RAISE(ABORT, 'fixture journal failure'); END;
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := store.PatchObjectProperties(ctx, mailbox, object.ID, 2, core.Patch{Remove: []core.Key{key}}); err == nil {
		t.Fatal("expected journal append to fail")
	}
	after, revision, err := store.ObjectProperties(ctx, mailbox, object.ID)
	if err != nil || revision != 2 || !reflect.DeepEqual(before, after) {
		t.Fatalf("failed append leaked property mutation: %+v at %d, %v", after, revision, err)
	}
	if _, err := store.db.ExecContext(ctx, "DROP TRIGGER fail_journal"); err != nil {
		t.Fatal(err)
	}
	if rev, err := store.PatchObjectProperties(ctx, mailbox, object.ID, 2, core.Patch{Remove: []core.Key{key}}); err != nil || rev != 3 {
		t.Fatalf("failed append consumed revision: %d, %v", rev, err)
	}
}

func TestPropertyRevisionsContinueAfterVersionTwoMigration(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "version-two.sqlite")
	store, mailbox := openJournalFixture(t, path)
	object := core.Change{Kind: core.Created, ID: "before-migration"}
	page, err := store.ApplyDelta(ctx, mailbox, DeltaCheckpoint{}, "nonempty", []DeltaEvent{{Change: object}})
	if err != nil {
		t.Fatal(err)
	}
	empty, err := store.ApplyDelta(ctx, mailbox, page, "empty-advance", nil)
	if err != nil || empty.Revision != 2 {
		t.Fatalf("empty advancing page: %+v, %v", empty, err)
	}
	if _, err := store.db.ExecContext(ctx, `
		DROP TABLE mapi_message_moves;
		DROP TABLE mapi_message_imports;
		DROP TABLE mapi_upload_proofs;
		DROP TABLE mapi_foreign_replicas;
		DROP TABLE mapi_folder_creations;
		DROP TABLE mapi_folder_values;
		DROP TABLE mapi_draft_placements;
		DROP TABLE mapi_staged_ids;
		DROP TABLE mapi_fai_tombstones;
		DROP TABLE mapi_fai_objects;
		DROP TABLE mapi_projection_versions;
		DROP TABLE mapi_contents_proofs;
		DROP TABLE mapi_local_id_ranges;
		DROP TABLE mapi_object_ids;
		DROP TABLE mapi_replicas;
		DROP TABLE mail_draft_creations;
		DROP TABLE draft_string_properties;
		DROP TABLE store_keys;
		DROP INDEX journal_kind_revision;
		ALTER TABLE journal DROP COLUMN object_kind;
		ALTER TABLE journal DROP COLUMN observed_at;
		DROP TABLE send_intents;
		DROP TABLE native_projection;
		ALTER TABLE delta_checkpoints DROP COLUMN projected;
		DROP TABLE named_property_ids;
		DROP TABLE object_properties;
		DROP INDEX journal_object_revision;
		ALTER TABLE mailboxes DROP COLUMN journal_floor;
		ALTER TABLE mailboxes DROP COLUMN journal_generation;
		ALTER TABLE mailboxes DROP COLUMN journal_revision;
		DROP TABLE mapi_spooler;
		DROP TABLE mapi_public_read_state;
		DROP TABLE mapi_permission_members;
		PRAGMA user_version = 2;
	`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, _ = openJournalFixture(t, path)
	if checkpoint, err := store.DeltaCheckpoint(ctx, mailbox); err != nil || checkpoint != empty {
		t.Fatalf("checkpoint changed during migration: %+v, %v", checkpoint, err)
	}
	patch := core.Patch{Set: core.Properties{{ID: 1}: core.Bool(true)}}
	if rev, err := store.PatchObjectProperties(ctx, mailbox, object.ID, page.Revision, patch); err != nil || rev != 3 {
		t.Fatalf("migration reused an already allocated revision: %d, %v", rev, err)
	}
	if next, err := store.ApplyDelta(ctx, mailbox, empty, "next", nil); err != nil || next.Revision != 4 {
		t.Fatalf("native feed did not resume after migration: %+v, %v", next, err)
	}
}
