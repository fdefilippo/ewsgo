// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"errors"
	"io"
	"net/http"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/storage/sqlite"
)

func TestMailboxProjectionVerifiesRemovalsAndKeepsTheCursorOnFailure(t *testing.T) {
	var mode atomic.Int32
	const (
		changedMissing = iota
		invalidRevision
		typedStillPresent
		unavailable
		success
	)
	client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
		raw, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		request := string(raw)
		switch {
		case strings.Contains(request, `token="40"`):
			envelope(w, `<SyncResponse xmlns="urn:zimbraMail" token="41"><m id="67"/>
				<deleted ids="7,8,9,23,55"><m ids="7"/><folder ids="55"/></deleted></SyncResponse>`)
		case strings.Contains(request, `token="41"`):
			envelope(w, `<SyncResponse xmlns="urn:zimbraMail" token="42"><m id="67"/></SyncResponse>`)
		case strings.Contains(request, `id="67"`):
			if !strings.Contains(request, `max="1"`) || !strings.Contains(request, `wantModSeq="1"`) || !strings.Contains(request, `read="0"`) {
				t.Error("expected a bounded metadata read with no flag changes")
			}
			switch mode.Load() {
			case changedMissing:
				fault(w, "mail.NO_SUCH_MSG")
			case invalidRevision:
				envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="67" l="2" ms="broken"/></GetMsgResponse>`)
			default:
				envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="67" l="2" ms="42"/></GetMsgResponse>`)
			}
		case strings.Contains(request, `id="7"`):
			if mode.Load() == typedStillPresent {
				envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="7" l="2" ms="20"/></GetMsgResponse>`)
			} else {
				fault(w, "mail.NO_SUCH_MSG")
			}
		case strings.Contains(request, `id="8"`):
			if mode.Load() == unavailable {
				fault(w, "service.RESOURCE_UNREACHABLE")
			} else {
				fault(w, "mail.NO_SUCH_ITEM")
			}
		case strings.Contains(request, `id="9"`):
			envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="9" l="2" ms="20"/></GetMsgResponse>`)
		case strings.Contains(request, "GetFolderRequest") && strings.Contains(request, `l="55"`) &&
			strings.Contains(request, `depth="0"`) && strings.Contains(request, `tr="0"`):
			fault(w, "mail.NO_SUCH_FOLDER")
		default:
			t.Error("unexpected SOAP request, including lookup of an untracked removal")
			w.WriteHeader(http.StatusBadRequest)
		}
	}, Config{})
	ctx := t.Context()
	store, err := sqlite.Open(ctx, filepath.Join(t.TempDir(), "fixture.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	scope := core.Scope{Backend: "zimbra", Account: "fixture"}
	session := &accountSession{
		native:   &Session{client: client, token: fixtureToken, expires: time.Now().Add(time.Minute)},
		identity: core.Identity{Scope: scope},
	}
	mb, err := store.BindMailbox(ctx, scope, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	projection, err := store.OpenNativeProjection(ctx, scope, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	baseline := []core.NativeObject{
		{ID: "m:7", Kind: core.Message, Parent: "f:2", Revision: 20},
		{ID: "m:8", Kind: core.Message, Parent: "f:2", Revision: 20},
		{ID: "m:9", Kind: core.Message, Parent: "f:2", Revision: 20},
		{ID: "f:55", Kind: core.Folder, Parent: "f:1", Revision: 18},
	}
	if err := projection.Apply(ctx, core.NativeCheckpoint{}, "40", baseline, nil); err != nil {
		t.Fatal(err)
	}
	before, err := projection.Checkpoint(ctx)
	if err != nil {
		t.Fatal(err)
	}
	head, err := store.JournalHead(ctx, mb.MailboxID)
	if err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		mode int32
		want error
	}{
		{changedMissing, core.ErrConflict},
		{invalidRevision, ErrProtocol},
		{typedStillPresent, core.ErrConflict},
		{unavailable, nil},
	} {
		mode.Store(test.mode)
		_, err := ProjectMailboxDelta(ctx, session, store, "fixture", 8)
		if err == nil || test.want != nil && !errors.Is(err, test.want) {
			t.Fatalf("invalid or interrupted metadata reads must fail the page: %v", err)
		}
		if checkpoint, err := projection.Checkpoint(ctx); err != nil || checkpoint != before {
			t.Fatal("failed observation advanced the checkpoint")
		}
		if current, err := store.JournalHead(ctx, mb.MailboxID); err != nil || current != head {
			t.Fatal("failed observation added local journal revisions")
		}
	}
	mode.Store(success)
	if more, err := ProjectMailboxDelta(ctx, session, store, "fixture", 8); more || err != nil {
		t.Fatalf("apply verified page: %v, %v", more, err)
	}
	checkpoint, err := projection.Checkpoint(ctx)
	if err != nil || checkpoint.Cursor != "41" {
		t.Fatal("did not commit the page cursor with the verified changes")
	}
	ids := []core.ID{"m:7", "m:8", "m:9", "f:55", "m:67", "m:23"}
	members, err := projection.Members(ctx, checkpoint, ids)
	if err != nil || len(members) != 2 || members["m:67"].Revision != 42 || members["m:9"] != baseline[2] {
		t.Fatal("projection did not distinguish confirmed, unknown and live removals")
	}
	page, err := store.Journal(ctx, mb.MailboxID, head, 8)
	if err != nil || len(page.Changes) != 4 || page.Changes[0].ID != "m:67" || page.Changes[0].Kind != core.Created {
		t.Fatal("journal did not record the creation and three verified removals")
	}
	for _, change := range page.Changes[1:] {
		if change.Kind != core.Deleted || change.PreviousParent == "" {
			t.Fatal("journal lost previous membership of a confirmed removal")
		}
	}
	if _, err := ProjectMailboxDelta(ctx, session, store, "fixture", 8); err != nil {
		t.Fatal(err)
	}
	if checkpoint, err := projection.Checkpoint(ctx); err != nil || checkpoint.Cursor != "42" || checkpoint.Revision != page.Next.Revision {
		t.Fatal("identical observation must advance only the native cursor")
	}
}
