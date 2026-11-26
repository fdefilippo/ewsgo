// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"

	"ewsgo/internal/core"
)

type hierarchyAggregateFixture struct {
	*messageFixtureMailbox
	sent               core.ID
	observation        core.MailFolderObservation
	markerReads        int
	membershipReads    int
	advanceAfterMarker bool
}

func (f *hierarchyAggregateFixture) ReadMailFolderMarkers(_ context.Context, folders []core.ID) (map[core.ID]core.MailFolderMarker, error) {
	f.markerReads++
	if !slices.Contains(folders, f.sent) {
		return nil, core.ErrNotFound
	}
	next := f.observation
	return map[core.ID]core.MailFolderMarker{f.sent: {Sequence: next.Sequence, Children: next.Children, MessageCount: next.MessageCount}}, nil
}

func (f *hierarchyAggregateFixture) ObserveMailFolder(_ context.Context, folder core.ID, prior *uint64) (core.MailFolderObservation, error) {
	if folder != f.sent {
		return core.MailFolderObservation{}, core.ErrNotFound
	}
	f.membershipReads++
	next := f.observation
	next.MessageIDs = slices.Clone(next.MessageIDs)
	if prior != nil && *prior == next.Sequence {
		next.MessageIDs = nil
	}
	if f.advanceAfterMarker {
		next.Sequence++
	}
	return next, nil
}

func TestHierarchyFolderAggregatesNotifySameCountReplacement(t *testing.T) {
	f, mailbox := hierarchyTableFixture(t)
	box := &hierarchyAggregateFixture{messageFixtureMailbox: mailbox,
		observation: core.MailFolderObservation{Sequence: 5, MessageCount: 1, MessageIDs: []core.ID{"mail-a"}}}
	for _, object := range mailbox.page.Objects {
		if object.Properties[core.WellKnown] == core.Text("sentitems") {
			box.sent = object.ID
		}
	}
	if box.sent == "" {
		t.Fatal("missing Sent fixture")
	}
	f.session = &testSession{box}
	f.transport.EnableFolderAggregates()
	table, _ := hierarchyTableOpen(t, f, 4)
	tags := []uint32{0x67480014, 0x670a0040, 0x670b0003}
	f.run(tableColumnsWire(2, tags...))
	wire, _ := f.run(tableQueryWire(2, 0, 1, 65535))
	rows := decodeTableRows(t, wire, tags)
	index := slices.IndexFunc(rows, func(row []any) bool { return row[0] == f.fids[SentItems] })
	if index < 0 || box.membershipReads != 1 || box.markerReads == 0 || rows[index][2] != uint32(0) {
		t.Fatal("initial hierarchy aggregate missing")
	}
	first, ok := rows[index][1].(uint64)
	if !ok || first == 0 {
		t.Fatal("initial commit time missing")
	}
	if wire, _ := f.run(nil); len(wire) != 0 || box.membershipReads != 1 {
		t.Fatal("unchanged hierarchy rescanned membership")
	}
	box.observation.Sequence++
	box.observation.MessageIDs = []core.ID{"mail-b"}
	var after uint32
	if changed, retry := f.transport.checkNotificationTables(context.Background(), f.session, f.state, &after); !changed || retry {
		t.Fatal("same-count replacement did not wake NotificationWait")
	}
	wire, _ = f.run(nil)
	if len(wire) <= tableRowNotificationHeaderBytes {
		t.Fatal("missing precise aggregate notification")
	}
	next := decodeTableRows(t, append([]byte{0x15, 2, 0, 0, 0, 0, 1, 1, 0}, wire[28:]...), tags)[0]
	second, ok := next[1].(uint64)
	if !ok || second <= first || next[2] != uint32(1) || box.membershipReads != 2 ||
		!reflect.DeepEqual(next, []any{f.fids[SentItems], second, uint32(1)}) {
		t.Fatal("replacement did not advance aggregate once", next)
	}
	var previous uint64
	if index > 0 {
		previous = uint64(table.bookmarkRow(table.rows[index-1]).id)
	}
	requireHierarchyRowEvent(t, wire, tableRowModified, f.handles[2], f.fids[SentItems], previous, tags, next)

	box.observation.Sequence++
	box.advanceAfterMarker = true
	if wire, _ := f.run(nil); len(wire) != 0 {
		t.Fatal("inconsistent observation emitted a notification")
	}
	box.advanceAfterMarker = false
	box.observation.Sequence--
	if wire, _ := f.run(nil); len(wire) != 0 {
		t.Fatal("inconsistent observation replaced the retained table")
	}
}

type folderObserverFixture struct {
	*stateMailbox
	next  core.MailFolderObservation
	err   error
	reads int
}

func (f *folderObserverFixture) ObserveMailFolder(_ context.Context, _ core.ID, prior *uint64) (core.MailFolderObservation, error) {
	f.reads++
	next := f.next
	next.MessageIDs = slices.Clone(next.MessageIDs)
	if prior != nil && *prior == next.Sequence {
		next.MessageIDs = nil
	}
	return next, f.err
}

func readFolderAggregatesWire(t *testing.T, f *ostFixture) folderAggregateValues {
	t.Helper()
	out, counts := f.run(folderPropertiesOperation(4, 1, 0, 0x670a0040, 0x670b0003))
	if len(out) != 19 || binary.LittleEndian.Uint32(out[2:]) != 0 || out[6] != 0 ||
		counts.properties.Values != 2 || counts.properties.Errors != 0 {
		t.Fatalf("aggregate property response unavailable: %x", out)
	}
	return folderAggregateValues{committed: binary.LittleEndian.Uint64(out[7:]), removed: binary.LittleEndian.Uint32(out[15:])}
}

func TestFolderAggregatesOnExecute(t *testing.T) {
	f := newOSTFixture(t)
	box := &folderObserverFixture{stateMailbox: f.box}
	f.session = &testSession{box}
	box.next = core.MailFolderObservation{Sequence: 7, MessageCount: 1, MessageIDs: []core.ID{"mail-a"}}
	_, counts := f.run(folderPropertiesOperation(4, 1, 0, 0x670a0040, 0x670b0003))
	if box.reads != 0 || counts.properties.Errors != 2 {
		t.Fatal("feature changed the disabled path")
	}
	f.transport.EnableFolderAggregates()
	first := readFolderAggregatesWire(t, f)
	if first.committed == 0 || first.removed != 0 || readFolderAggregatesWire(t, f) != first {
		t.Fatal("unexpected baseline or repeated observation changed the result")
	}
	box.next.Sequence++
	box.next.MessageIDs = []core.ID{"mail-b"}
	second := readFolderAggregatesWire(t, f)
	if second.committed <= first.committed || second.removed != 1 {
		t.Fatal("replacing a message preserved count but should record one removal")
	}
	// A reconnect to the same store keeps the verified observation.
	f.session = &testSession{box}
	box.next.Children[0] = 1
	third := readFolderAggregatesWire(t, f)
	if third.committed <= second.committed || third.removed != second.removed {
		t.Fatal("subfolder changes should advance only the time")
	}
	box.next.Sequence++
	box.err = core.ErrConflict
	out, counts := f.run(folderPropertiesOperation(4, 1, 0, 0x670a0040, 0x670b0003, 0x67480014))
	if out[6] != 1 || counts.properties.Errors != 2 || counts.properties.Values != 1 {
		t.Fatal("an incomplete observation should fail only its own properties")
	}
	box.err = nil
	box.next.Sequence--
	if readFolderAggregatesWire(t, f) != third {
		t.Fatal("an incomplete observation modified the replica")
	}
	store := replicaStore(t, f)
	if _, err := store.observeFolderAggregates(context.Background(), nil, store.root); !errors.Is(err, core.ErrAccessDenied) {
		t.Fatal("unauthorized aggregate access")
	}
}

func TestLargeTrashFolderAggregatesTrackRemovalThroughExecute(t *testing.T) {
	f := newOSTFixture(t)
	f.open(int(DeletedItems))
	f.transport.EnableFolderAggregates()
	box := &folderObserverFixture{stateMailbox: f.box}
	f.session = &testSession{box}
	for i := range core.MaxMailContentMessages + 2 {
		box.next.MessageIDs = append(box.next.MessageIDs, core.ID(fmt.Sprintf("mail-%03d", i)))
	}
	box.next.Sequence = 9
	box.next.MessageCount = uint64(len(box.next.MessageIDs))
	first := readFolderAggregatesWire(t, f)
	box.next.Sequence++
	box.next.MessageCount--
	box.next.MessageIDs = box.next.MessageIDs[1:]
	next := readFolderAggregatesWire(t, f)
	if first.removed != 0 || next.removed != 1 || next.committed <= first.committed || readFolderAggregatesWire(t, f) != next {
		t.Fatal("large Trash folder lost its committed removal")
	}
}
