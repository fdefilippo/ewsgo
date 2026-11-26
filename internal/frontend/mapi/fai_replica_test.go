// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"errors"
	"reflect"
	"testing"

	"ewsgo/internal/core"
)

func TestSQLiteFAIRestoreValuesAndTombstones(t *testing.T) {
	for _, name := range []string{"remote-source", "reserved-source"} {
		t.Run(name, func(t *testing.T) {
			f, _ := messageFixture(t)
			reopen := sqliteReplicaFixture(t, f)
			reopen()
			request := syntheticFAIImport()
			if name == "reserved-source" {
				first, code := f.store.reserveLocalIDs(t.Context(), 2)
				if code != 0 {
					t.Fatalf("reserve: %08x", code)
				}
				copy(request.source[:16], f.store.replica[:])
				copy(request.source[16:], first[:])
			}
			draft := newFAIDraftForTest(t, f, request)
			saved := saveFAIDraftForTest(t, f, draft, 2)
			draft.close()
			reopen()
			inbox := f.binding.fids[Inbox]
			id := faiIDForSource(request.source)
			got, err := f.store.readFAIRowLocked(t.Context(), inbox, id)
			if err != nil || !reflect.DeepEqual(saved, got) {
				t.Fatalf("reloaded FAI changed identity, lineage or values: %v", err)
			}
			absent := faiTestXID(56, 10)
			if err := f.store.deleteFAI(t.Context(), f.session, inbox, faiDeleteRequest{sources: [][22]byte{absent}}); err != nil {
				t.Fatal("delete unknown source:", err)
			}
			reopen()
			if f.store.fai.absent[absent] != inbox {
				t.Fatal("tombstone for unknown source was lost")
			}
			if err := f.store.deleteFAI(t.Context(), f.session, inbox, faiDeleteRequest{sources: [][22]byte{request.source}}); err != nil {
				t.Fatal("delete saved source:", err)
			}
			reopen()
			if record := f.store.fai.records[id]; !record.deleted || len(record.object.Properties) != 0 {
				t.Fatal("saved source was not retained as a tombstone")
			}
			if _, err := f.store.newFAIDraft(t.Context(), f.session, inbox, request); !errors.Is(err, core.ErrConflict) {
				t.Fatal("deleted identity could be reused:", err)
			}
		})
	}
}

func TestSQLiteServerCreatedAssociatedMessageAndPrivateConfiguration(t *testing.T) {
	f, _ := messageFixture(t)
	f.stores.privateConfig = true
	reopen := sqliteReplicaFixture(t, f)
	reopen()
	ctx := t.Context()
	inbox := f.binding.fids[Inbox]
	unsaved, err := f.store.stageLocalMessage(ctx, f.session, inbox, false)
	if err != nil {
		t.Fatal("stage:", err)
	}
	source, discardedMID := unsaved.request.source, unsaved.newMID
	unsaved.close()
	reopen()
	request := syntheticFAIImport()
	request.source = source
	if _, err := f.store.newFAIDraft(ctx, f.session, inbox, request); !errors.Is(err, core.ErrAccessDenied) {
		t.Fatal("server-issued ID accepted as an offline reservation:", err)
	}
	draft, err := f.store.stageLocalMessage(ctx, f.session, inbox, false)
	if err != nil {
		t.Fatal("second stage:", err)
	}
	t.Cleanup(draft.close)
	if draft.newMID == discardedMID || f.store.byMID[draft.newMID] != (messageKey{}) {
		t.Fatal("staging reused or published a MID")
	}
	saved := saveFAIDraftForTest(t, f, draft, 2)
	if saved.mid != draft.newMID {
		t.Fatal("saving reassigned the server-issued MID")
	}

	fids, err := f.store.assign(ctx, []folderKey{{role: freeBusyFolderRole}})
	if err != nil {
		t.Fatal("assign private configuration folder:", err)
	}
	config, err := f.store.stageLocalMessage(ctx, f.session, fids[0], true)
	if err != nil {
		t.Fatal("stage private configuration:", err)
	}
	t.Cleanup(config.close)
	if problems, err := config.set(ctx, f.session, faiSetRequest{
		{0x001a001f, localFreeBusyClass}, {0x0037001f, localFreeBusySubject}, {delegatorWantsCopyTag, core.Bool(true)},
	}); err != nil || len(problems) != 0 {
		t.Fatalf("set private configuration: %v %v", problems, err)
	}
	private := saveFAIDraftForTest(t, f, config, 2)
	reopen()
	got, err := f.store.readFAIRowLocked(ctx, inbox, saved.object.ID)
	if err != nil || !reflect.DeepEqual(saved, got) {
		t.Fatal("reopen changed server-created FAI:", err)
	}
	if restored := f.store.freeBusyMessage(); !reflect.DeepEqual(restored, private) {
		t.Fatal("reopen changed private configuration")
	}
	edit, err := f.store.editFreeBusyMessage(private)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(edit.close)
	if problems, err := edit.set(ctx, f.session, faiSetRequest{{delegatorWantsCopyTag, core.Bool(false)}}); err != nil || len(problems) != 0 {
		t.Fatalf("edit private configuration: %v %v", problems, err)
	}
	updated := saveFAIDraftForTest(t, f, edit, 2)
	reopen()
	if updated.mid != private.mid || updated.cn <= private.cn || !reflect.DeepEqual(f.store.freeBusyMessage(), updated) {
		t.Fatal("private configuration edit was not retained with its identity")
	}
}
