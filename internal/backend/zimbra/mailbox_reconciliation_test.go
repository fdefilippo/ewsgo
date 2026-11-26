// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"errors"
	"fmt"
	"io"
	"net/http"
	"path/filepath"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/storage/sqlite"
)

func TestMailboxReconciliationLeavesLegacyStateUntilScanAndCatchupSucceed(t *testing.T) {
	var mode atomic.Int32
	var scanned atomic.Bool
	const (
		searchInterrupted = iota
		repeatedHit
		deltaInterrupted
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
		case strings.Contains(request, "NoOpRequest"):
			scanned.Store(false)
			fmt.Fprintf(w, `<s:Envelope xmlns:s="%s"><s:Header><context xmlns="urn:zimbra">
				<change token="20"/></context></s:Header><s:Body><NoOpResponse xmlns="urn:zimbraMail"/></s:Body></s:Envelope>`, soapNS)
		case strings.Contains(request, "GetFolderRequest"):
			envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1" ms="1">
				<folder id="2" ms="2" l="1"/><folder id="3" ms="3" l="1"/>
				<link id="60"><folder id="7" ms="4" l="60"/></link></folder></GetFolderResponse>`)
		case strings.Contains(request, "SearchRequest"):
			if !strings.Contains(request, "is:anywhere") || !strings.Contains(request, `types="message"`) ||
				!strings.Contains(request, `limit="1"`) || !strings.Contains(request, `sortBy="idAsc"`) {
				t.Error("expected own mailbox search, limited and sorted by native ID")
			}
			if !strings.Contains(request, "<cursor") {
				envelope(w, `<SearchResponse xmlns="urn:zimbraMail" sortBy="idAsc" offset="0" more="1">
					<m id="12" sf="12" l="2"/></SearchResponse>`)
				return
			}
			if !strings.Contains(request, `id="12" sortVal="12"`) {
				t.Error("continuation did not use the last native ID")
			}
			switch mode.Load() {
			case searchInterrupted:
				fault(w, "service.RESOURCE_UNREACHABLE")
			case repeatedHit:
				envelope(w, `<SearchResponse xmlns="urn:zimbraMail" sortBy="idAsc" more="1">
					<m id="12" sf="12" l="2"/></SearchResponse>`)
			default:
				envelope(w, `<SearchResponse xmlns="urn:zimbraMail" sortBy="idAsc" more="0">
					<m id="13" sf="13" l="2"/></SearchResponse>`)
			}
		case strings.Contains(request, "SyncRequest") && strings.Contains(request, `token="20"`):
			scanned.Store(true)
			envelope(w, `<SyncResponse xmlns="urn:zimbraMail" token="20-15:d22" more="1"><m id="12"/></SyncResponse>`)
		case strings.Contains(request, "SyncRequest") && strings.Contains(request, `token="20-15:d22"`):
			if mode.Load() == deltaInterrupted {
				fault(w, "service.RESOURCE_UNREACHABLE")
			} else {
				envelope(w, `<SyncResponse xmlns="urn:zimbraMail" token="22"><m id="15"/></SyncResponse>`)
			}
		case strings.Contains(request, `id="12"`):
			if scanned.Load() {
				envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="12" ms="21" l="3"/></GetMsgResponse>`)
			} else {
				envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="12" ms="8" l="2"/></GetMsgResponse>`)
			}
		case strings.Contains(request, `id="13"`):
			envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="13" ms="9" l="2"/></GetMsgResponse>`)
		case strings.Contains(request, `id="14"`):
			fault(w, "mail.NO_SUCH_MSG")
		case strings.Contains(request, `id="15"`):
			envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="15" ms="22" l="2"/></GetMsgResponse>`)
		case strings.Contains(request, `id="16"`):
			// This prior member is missing from Search but is still live.
			envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="16" ms="12" l="2"/></GetMsgResponse>`)
		default:
			t.Error("unexpected SOAP request")
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
	var legacy []sqlite.DeltaEvent
	for _, id := range []core.ID{"m:12", "m:14", "m:16", "unmapped"} {
		legacy = append(legacy, sqlite.DeltaEvent{Change: core.Change{Kind: core.Created, ID: id, Parent: "f:2"}})
	}
	before, err := store.ApplyDelta(ctx, mb.MailboxID, sqlite.DeltaCheckpoint{}, "5", legacy)
	if err != nil {
		t.Fatal(err)
	}
	key := core.Key{ID: 1}
	if _, err := store.PatchObjectProperties(ctx, mb.MailboxID, "m:12", 1, core.Patch{Set: core.Properties{key: core.Bool(true)}}); err != nil {
		t.Fatal(err)
	}
	head, err := store.JournalHead(ctx, mb.MailboxID)
	if err != nil {
		t.Fatal(err)
	}

	for _, phase := range []int32{searchInterrupted, repeatedHit, deltaInterrupted} {
		mode.Store(phase)
		if _, err := ProjectMailboxDelta(ctx, session, store, "fixture", 1); err == nil {
			t.Fatal("interrupted or malformed attempt returned success")
		}
		if checkpoint, err := store.DeltaCheckpoint(ctx, mb.MailboxID); err != nil || checkpoint != before {
			t.Fatal("incomplete scan replaced the checkpoint")
		}
		if current, err := store.JournalHead(ctx, mb.MailboxID); err != nil || current != head {
			t.Fatal("incomplete scan published journal revisions")
		}
	}

	mode.Store(success)
	if more, err := ProjectMailboxDelta(ctx, session, store, "fixture", 1); more || err != nil {
		t.Fatalf("legacy reconciliation: more=%v error=%v", more, err)
	}
	projection, err := store.OpenNativeProjection(ctx, scope, "fixture")
	if err != nil {
		t.Fatal(err)
	}
	checkpoint, err := projection.Checkpoint(ctx)
	if err != nil || checkpoint.Cursor != "22" {
		t.Fatal("completed reconciliation did not publish final native token")
	}
	ids := []core.ID{"m:12", "m:13", "m:14", "m:15", "m:16", "f:7"}
	members, err := projection.Members(ctx, checkpoint, ids)
	if err != nil || len(members) != 4 || members["m:12"].Parent != "f:3" ||
		members["m:13"].Revision != 9 || members["m:15"].Revision != 22 || members["m:16"].Revision != 12 {
		t.Fatal("snapshot lost a pre-existing, concurrent or directly confirmed member")
	}
	if props, _, err := store.ObjectProperties(ctx, mb.MailboxID, "m:12"); err != nil || props[key] != core.Bool(true) {
		t.Fatal("move lost gateway properties")
	}
	if _, _, err := store.ObjectProperties(ctx, mb.MailboxID, "m:14"); !errors.Is(err, core.ErrNotFound) {
		t.Fatal("confirmed missing prior object remained visible")
	}
	if _, _, err := store.ObjectProperties(ctx, mb.MailboxID, "unmapped"); err != nil {
		t.Fatal("legacy object with non-derivable identity was affected")
	}
	page, err := store.Journal(ctx, mb.MailboxID, head, 32)
	if err != nil || !slices.ContainsFunc(page.Changes, func(change core.Change) bool {
		return change.ID == "m:12" && change.Kind == core.Moved && change.PreviousParent == "f:2" && change.Parent == "f:3"
	}) {
		t.Fatal("reconciliation did not journal the move")
	}
}
