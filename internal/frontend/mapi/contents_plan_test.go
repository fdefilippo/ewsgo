// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"reflect"
	"sync"
	"testing"

	"ewsgo/internal/core"
)

func preparedContentsForTest(t *testing.T, f *hierarchyFixture, c contentsConfigure, initial contentsSyncState) (*contentsState, *contentsPreparation) {
	t.Helper()
	s := contentsStateForTest(t, f, c)
	uploadContentsTestState(t, s, f.session, initial)
	p, err := s.prepareContents(context.Background(), f.session, c.frame)
	if err != nil {
		t.Fatal("prepare Contents", err)
	}
	t.Cleanup(p.close)
	return s, p
}
func finishPreparedForTest(t *testing.T, s *contentsState, p *contentsPreparation) contentsSyncState {
	t.Helper()
	final := cloneContentsState(p.delta.final)
	if err := p.ticket.finish(t.Context(), true); err != nil {
		t.Fatal(err)
	}
	p.close()
	s.close()
	return final
}
func TestContentsPlanningFullIdenticalContentReadAndFAI(t *testing.T) {
	f, box := messageFixture(t)
	inbox := f.binding.fids[Inbox]
	seedFAI(t, f.store, faiTestRecord(f.store, inbox, 1))
	c := testContentsStateConfig()
	s, p := preparedContentsForTest(t, f, c, contentsSyncState{})
	if len(p.delta.changes) != 2 || len(p.delta.unread) != 0 || len(p.delta.read) != 0 {
		t.Fatal("full capture/class/read state")
	}
	normal, fai := p.delta.changes[0].row, p.delta.changes[1].row
	if normal.associated || !fai.associated || !p.delta.final.seen.contains(f.store.replica, normal.cn) || !p.delta.final.seenFAI.contains(f.store.replica, fai.cn) {
		t.Fatal("class states mixed")
	}
	if !p.delta.final.read.contains(f.store.replica, normal.readCN) || p.delta.final.read.contains(f.store.replica, fai.readCN) {
		t.Fatal("read checkpoint included a FAI or omitted the full message's read state")
	}
	final := finishPreparedForTest(t, s, p)
	s, p = preparedContentsForTest(t, f, c, final)
	if len(p.delta.changes)+len(p.delta.deleted)+len(p.delta.noLonger)+len(p.delta.read)+len(p.delta.unread) != 0 || !reflect.DeepEqual(final, p.delta.final) {
		t.Fatal("identical snapshot resends/advances")
	}
	finishPreparedForTest(t, s, p)
	box.objects[0].Properties[core.IsRead] = core.Bool(true)
	box.objects[0].Revision++
	box.objects[0].Properties[core.MetadataRevision] = core.Integer(box.objects[0].Revision)
	s, p = preparedContentsForTest(t, f, c, final)
	if len(p.delta.changes) != 0 || len(p.delta.read) != 1 || p.delta.read[0] != normal.mid || !reflect.DeepEqual(final.seen, p.delta.final.seen) {
		t.Fatal("read-only transition sent content")
	}
	final = finishPreparedForTest(t, s, p)
	box.objects[0].MIME = []byte("From: synthetic@example.invalid\r\n\r\nChanged full content")
	box.objects[0].Properties[core.ContentRevision] = core.Integer(8)
	record := faiTestRecord(f.store, inbox, 1)
	record.object.Revision++
	record.object.Properties[core.Key{ID: 0x7006}] = core.Text("Changed synthetic view")
	seedFAI(t, f.store, record)
	s, p = preparedContentsForTest(t, f, c, final)
	if len(p.delta.changes) != 2 || len(p.delta.read)+len(p.delta.unread) != 0 || !reflect.DeepEqual(final.read, p.delta.final.read) {
		t.Fatal("content/FAI change not separated from read state")
	}
	finishPreparedForTest(t, s, p)
}
func TestContentsPlanningFilterAbsenceSuppressionAndReturn(t *testing.T) {
	for _, flags := range []uint16{0, 2, 4} {
		t.Run(map[uint16]string{0: "ordinary", 2: "no-deletions", 4: "ignore-no-longer"}[flags], func(t *testing.T) {
			f, box := messageFixture(t)
			box.objects[0].MIME = []byte("Subject: Synthetic\r\n\r\nBody")
			c := testContentsStateConfig()
			c.frame.flags |= flags
			c.restriction = &contentsRestriction{op: 8, tag: 0x0037001f}
			s, p := preparedContentsForTest(t, f, c, contentsSyncState{})
			if len(p.delta.changes) != 1 {
				t.Fatal("restriction omitted match")
			}
			mid := p.delta.changes[0].row.mid
			final := finishPreparedForTest(t, s, p)
			box.objects[0].MIME = []byte("From: synthetic@example.invalid\r\n\r\nBody")
			box.objects[0].Properties[core.ContentRevision] = core.Integer(9)
			s, p = preparedContentsForTest(t, f, c, final)
			suppressed := flags != 0
			if len(p.delta.changes) != 0 || (len(p.delta.noLonger) == 1) == suppressed || p.delta.final.given.contains(f.store.replica, folderGlobalCount(mid)) != suppressed {
				t.Fatal("no-longer/suppression misclassified")
			}
			final = finishPreparedForTest(t, s, p)
			box.objects = nil
			s, p = preparedContentsForTest(t, f, c, final)
			if len(p.delta.deleted) != map[uint16]int{0: 0, 2: 0, 4: 1}[flags] || p.delta.final.given.contains(f.store.replica, folderGlobalCount(mid)) != (flags == 2) {
				t.Fatal("absence/suppression state")
			}
			final = finishPreparedForTest(t, s, p)
			box.objects = []core.Object{messageTestObject("opaque-message", f.store.byFID[f.binding.fids[Inbox]].native)}
			box.objects[0].MIME = []byte("Subject: Returned\r\n\r\nBody")
			s, p = preparedContentsForTest(t, f, c, final)
			if len(p.delta.changes) != 1 || p.delta.changes[0].row.mid != mid {
				t.Fatal("return lost identity/change")
			}
			finishPreparedForTest(t, s, p)
		})
	}
}
func TestContentsPlanningMoveRemovesOnlySourceAndDoesNotAcknowledgeUnseenCN(t *testing.T) {
	f, box := messageFixture(t)
	c := testContentsStateConfig()
	s, p := preparedContentsForTest(t, f, c, contentsSyncState{})
	old := p.delta.changes[0].row
	final := finishPreparedForTest(t, s, p)
	box.objects[0].Parent = f.store.byFID[f.binding.fids[SentItems]].native
	moved := messageObserve(t, f, SentItems).messages[0]
	box.objects = nil
	s, p = preparedContentsForTest(t, f, c, final)
	if len(p.delta.deleted) != 1 || p.delta.deleted[0] != old.mid || p.delta.final.seen.contains(f.store.replica, moved.cn) || p.delta.final.read.contains(f.store.replica, moved.readCN) && moved.readCN != old.readCN {
		t.Fatal("source move leaked destination acknowledgement")
	}
	finishPreparedForTest(t, s, p)
}
func TestContentsPlanningFailureNoPublicationQuotaAndRevocation(t *testing.T) {
	for _, mode := range []string{"quota", "source", "projection", "revocation", "cancel"} {
		t.Run(mode, func(t *testing.T) {
			f, box := messageFixture(t)
			c := testContentsStateConfig()
			s := contentsStateForTest(t, f, c)
			baseline := f.stores.contents.used
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			var blocker *contentsLease
			switch mode {
			case "quota":
				blocker, _ = f.store.reserveContentsMemory(maxStoreContentsMemory - baseline - 1)
			case "source":
				box.sourceErr = core.ErrConflict
			case "projection":
				box.objects[0].Properties[core.ReceivedAt] = core.Text("Invalid backend envelope type")
			case "revocation":
				box.readHook = func() { f.box.state.Owner = false }
			case "cancel":
				box.readHook = cancel
			}
			p, err := s.prepareContents(ctx, f.session, c.frame)
			if err == nil || p != nil || len(f.store.contentsProofs) != 0 || !reflect.DeepEqual(s.value, contentsSyncState{}) {
				t.Fatal("failed plan published/mutated client state")
			}
			blocker.release()
			if f.stores.contents.used != baseline {
				t.Fatal("failed plan leaked buffers")
			}
		})
	}
}
func TestContentsPlanningUnpublishedCheckpointCannotReplayAndConfigCannotChange(t *testing.T) {
	f, _ := messageFixture(t)
	c := testContentsStateConfig()
	s, p := preparedContentsForTest(t, f, c, contentsSyncState{})
	next := contentsStateForTest(t, f, c)
	uploadContentsTestState(t, next, f.session, p.delta.final)
	if _, err := next.beginPlanning(context.Background(), f.session); !errors.Is(err, core.ErrResync) {
		t.Fatal("planned but undelivered state replayed")
	}
	p.close()
	s.close()
	other := contentsStateForTest(t, f, c)
	changed := c.frame
	changed.flags |= 2
	if _, err := other.prepareContents(context.Background(), f.session, changed); !errors.Is(err, core.ErrInvalid) || other.started {
		t.Fatal("prepare replaced negotiated config")
	}
}
func TestContentsPlanningRejectsMissingKnownAndForeignReplica(t *testing.T) {
	f, box := messageFixture(t)
	change := syntheticContentsChange(t, f, box)
	initial := syntheticMessageState(change.row, f.store.replica)
	plan := contentsFilterPlan{normal: true, associated: true, readState: true}
	if _, err := planContentsDelta(f.store.replica, plan, 0, initial, nil, nil); !errors.Is(err, core.ErrResync) {
		t.Fatal("unknown Given silently discarded")
	}
	if _, err := planContentsDelta([16]byte{99}, plan, 0, initial, nil, nil); !errors.Is(err, core.ErrResync) {
		t.Fatal("foreign ranges rebound")
	}
}
func TestContentsProjectionSchemaRejectsUnknownAndMixedNamed(t *testing.T) {
	named := contentsProperty{kind: 0x1f, name: propertyName{Kind: 1, Name: "Synthetic", Set: [16]byte{1}}}
	if contentsProjectionSchema(true, true)(named) || !contentsProjectionSchema(false, true)(named) || contentsProjectionSchema(true, true)(fixedContentsProperty(0x1234001f)) {
		t.Fatal("unsupported schema silently treated absent")
	}
	if !contentsProjectionSchema(true, false)(fixedContentsProperty(0x10130102)) || !contentsProjectionSchema(false, true)(fixedContentsProperty(0x70010102)) {
		t.Fatal("real schema property refused")
	}
}
func TestContentsMemoryShrinkDoesNotGrowOrReviveAndRacesRelease(t *testing.T) {
	f, _ := messageFixture(t)
	l, _ := f.store.reserveContentsMemory(100)
	if l.shrink(101) == nil || l.size() != 100 || l.shrink(40) != nil || f.stores.contents.used != 40 {
		t.Fatal("atomic shrink accounting")
	}
	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); _ = l.shrink(0); l.release() }()
	}
	wg.Wait()
	if f.stores.contents.used != 0 || l.shrink(1) == nil {
		t.Fatal("reservation revived/leaked")
	}
}
