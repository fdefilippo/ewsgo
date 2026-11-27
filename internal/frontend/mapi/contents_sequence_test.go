// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"ewsgo/internal/core"
)

type indexedContentsMailbox struct {
	*messageFixtureMailbox
	mimeReads []core.ID
}

type deferredIndexedMailbox struct {
	*indexedContentsMailbox
	core.DeferredActionMailbox
	targets map[core.ID]struct{}
}

func (b *deferredIndexedMailbox) DeferredActionTargets(context.Context) (map[core.ID]struct{}, error) {
	return b.targets, nil
}

func (b *indexedContentsMailbox) ReadMailContentIndex(_ context.Context, folder core.ID) (core.MailContents, error) {
	result := core.MailContents{Folder: folder}
	for _, source := range b.objects {
		if source.Parent == folder {
			source.MIME = nil
			result.Messages = append(result.Messages, source)
		}
	}
	return result, nil
}

func (b *indexedContentsMailbox) ReadMailMessageMIME(_ context.Context, folder, id core.ID, _ bool) (core.Object, error) {
	b.mimeReads = append(b.mimeReads, id)
	for _, source := range b.objects {
		if source.Parent == folder && source.ID == id {
			return source, nil
		}
	}
	return core.Object{}, core.ErrNotFound
}

func indexedContentsFixture(t *testing.T, count, bodySize int) (*ostFixture, *indexedContentsMailbox) {
	t.Helper()
	f, old := newContentsDownloadFixture(t)
	parent := old.objects[0].Parent
	b := &indexedContentsMailbox{messageFixtureMailbox: old}
	b.objects = nil
	for i := range count {
		id := core.ID(fmt.Sprintf("indexed-%02d", i))
		object := messageTestObject(id, parent)
		object.MIME = append([]byte("Subject: Synthetic large message\r\n\r\n"), bytes.Repeat([]byte("a"), bodySize)...)
		object.Properties[core.VersionedMIME] = core.Bool(true)
		object.Properties[core.MessageSize] = core.Integer(len(object.MIME))
		b.objects = append(b.objects, object)
	}
	f.session = &testSession{b}
	return f, b
}

func TestContentsSequenceLargeFolderAndMetadataOnlyDelta(t *testing.T) {
	f, box := indexedContentsFixture(t, 9, 520<<10)
	c := fullContentsConfig()
	configureContentsTest(t, f, c)
	start, status := getFXBuffer(t, f.run, 2, 8192)
	if status != 1 || len(box.mimeReads) != 1 {
		t.Fatalf("first buffer read %d MIME objects, status %d", len(box.mimeReads), status)
	}
	raw := append(start, drainFX(t, f.run, 2, 8192)...)
	state := contentsFourTestSets(t, raw)
	if len(raw) <= 4<<20 || len(box.mimeReads) != 9 || box.sourceCalls != 0 {
		t.Fatalf("progressive download bytes=%d MIME=%d eager=%d", len(raw), len(box.mimeReads), box.sourceCalls)
	}
	f.run([]byte{1, 4, 2})

	next := func(state contentsSyncState) []byte {
		configureContentsTest(t, f, c)
		uploadContentsWireTest(t, f, state)
		raw := drainFX(t, f.run, 2, 8192)
		f.run([]byte{1, 4, 2})
		return raw
	}
	if again := contentsFourTestSets(t, next(state)); !reflect.DeepEqual(again, state) || len(box.mimeReads) != 9 {
		t.Fatal("unchanged delta read MIME or altered the checkpoint")
	}
	box.objects[0].Revision++
	box.objects[0].Properties[core.MetadataRevision] = core.Integer(box.objects[0].Revision)
	box.objects[0].Properties[core.IsRead] = core.Bool(true)
	raw = next(state)
	updated := contentsFourTestSets(t, raw)
	if !reflect.DeepEqual(state.seen, updated.seen) || reflect.DeepEqual(state.read, updated.read) || len(box.mimeReads) != 9 {
		t.Fatal("read flag downloaded content or did not advance read state")
	}
	for _, token := range decodeTestContents(t, raw) {
		if token.tag == 0x402d0102 {
			return
		}
	}
	t.Fatal("missing read-state delta")
}

func TestContentsSequenceTracksDeferredActionsWithoutMIMERevisionChange(t *testing.T) {
	f, indexed := indexedContentsFixture(t, 1, 256)
	box := &deferredIndexedMailbox{indexedContentsMailbox: indexed, targets: map[core.ID]struct{}{indexed.objects[0].ID: {}}}
	f.session = &testSession{box}
	download := func(initial contentsSyncState, want *bool) contentsSyncState {
		t.Helper()
		configureContentsTest(t, f, fullContentsConfig())
		uploadContentsWireTest(t, f, initial)
		raw := drainFX(t, f.run, 2, 8192)
		f.run([]byte{1, 4, 2})
		var values []bool
		for _, token := range decodeTestContents(t, raw) {
			if token.tag == hasDAMs {
				values = append(values, token.value.(bool))
			}
		}
		if want == nil && len(values) != 0 || want != nil && (len(values) != 1 || values[0] != *want) {
			t.Fatalf("unexpected HasDAMs values: %v", values)
		}
		return contentsFourTestSets(t, raw)
	}
	state := download(contentsSyncState{}, new(true))
	clear(box.targets)
	after := download(state, new(false))
	if reflect.DeepEqual(state.seen, after.seen) || !reflect.DeepEqual(state.given, after.given) {
		t.Fatal("the flag change did not advance the existing message")
	}
	if quiet := download(after, nil); !reflect.DeepEqual(after, quiet) || len(box.mimeReads) != 2 {
		t.Fatal("unchanged DAM flag retransmitted the message")
	}
}

func TestContentsSequenceConflictAfterPartialKeepsCheckpoint(t *testing.T) {
	f, box := indexedContentsFixture(t, 2, 200)
	c := fullContentsConfig()
	configureContentsTest(t, f, c)
	_, status := getFXBuffer(t, f.run, 2, 128)
	object, _ := f.state.mailbox.object(f.handles[2], 4)
	d := object.download.contents
	final, scope := d.final, d.state.scope
	if status != 1 || len(box.mimeReads) != 1 {
		t.Fatal("did not begin first indexed message")
	}
	// The next MIME revision changes after the index has been accepted.
	box.objects[1].Revision++
	box.objects[1].Properties[core.MetadataRevision] = core.Integer(box.objects[1].Revision)
	box.objects[1].Properties[core.ContentRevision] = core.Integer(4)
	response, counts := f.run(hierarchyBufferOperation(4, 2, 8192))
	if len(response) < 6 || counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) == 0 || d.completed || d.transfer.sequence != nil {
		t.Fatal("stale message did not abort the stream and release its data")
	}
	if object.binding.store.validateContentsCheckpoint(t.Context(), scope, final) == nil {
		t.Fatal("partial stream published the planned final checkpoint")
	}
	f.run([]byte{1, 4, 2})
	configureContentsTest(t, f, c)
	state := contentsFourTestSets(t, drainFX(t, f.run, 2, 8192))
	if !reflect.DeepEqual(state.given, final.given) || reflect.DeepEqual(state.seen, final.seen) || len(box.mimeReads) != 4 {
		t.Fatal("retry changed message identities or reused the stale versions")
	}
}

func TestContentsSequenceRestrictsBeforeReadingMIME(t *testing.T) {
	f, box := indexedContentsFixture(t, 9, 520<<10)
	c := fullContentsConfig()
	cutoff := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	for i := range box.objects {
		box.objects[i].Properties[core.ReceivedAt] = core.Timestamp(cutoff.Add(time.Duration(i-1) * time.Hour))
	}
	// RES_PROPERTY, RELOP_GT, PidTagMessageDeliveryTime.
	restriction := binary.LittleEndian.AppendUint32([]byte{4, 2}, 0x0e060040)
	restriction = binary.LittleEndian.AppendUint32(restriction, 0x0e060040)
	restriction = binary.LittleEndian.AppendUint64(restriction, fileTime(cutoff))
	response, counts := f.run(contentsConfigureOperation(c, restriction))
	if !bytes.Equal(response, []byte{0x70, 2, 0, 0, 0, 0}) || counts.errors != 0 {
		t.Fatal("Configure filtered Contents failed")
	}
	raw := drainFX(t, f.run, 2, 8192)
	want := make([]core.ID, 0, 7)
	for _, object := range slices.Backward(box.objects[2:]) {
		want = append(want, object.ID)
	}
	if !reflect.DeepEqual(box.mimeReads, want) || box.sourceCalls != 0 {
		t.Fatal("filter read MIME outside the selected set")
	}
	state := contentsFourTestSets(t, raw)
	var total uint64
	for _, r := range contentsRanges(state.given) {
		total += r.high - r.low + 1
	}
	if total != uint64(len(want)) {
		t.Fatal("filtered checkpoint includes the wrong set")
	}
}

func TestIndexedContentsMoveWithSeparateFolderCheckpoints(t *testing.T) {
	for _, destinationFirst := range []bool{false, true} {
		t.Run(fmt.Sprintf("destination-first=%t", destinationFirst), func(t *testing.T) {
			f, box := indexedContentsFixture(t, 1, 256)
			store := f.state.mailbox.objects[f.handles[0]].binding.store
			box.objects[0].Parent = store.byFID[f.fids[DeletedItems]].native
			download := func(role SpecialFolderRole, initial contentsSyncState) ([]byte, contentsSyncState) {
				t.Helper()
				f.run([]byte{1, 4, 1})
				f.open(int(role))
				configureContentsTest(t, f, fullContentsConfig())
				uploadContentsWireTest(t, f, initial)
				raw := drainFX(t, f.run, 2, 8192)
				state := contentsFourTestSets(t, raw)
				f.run([]byte{1, 4, 2})
				return raw, state
			}
			_, source := download(DeletedItems, contentsSyncState{})
			_, destination := download(SentItems, contentsSyncState{})
			if len(contentsRanges(source.given)) != 1 || len(contentsRanges(destination.given)) != 0 || len(box.mimeReads) != 1 {
				t.Fatal("unexpected initial checkpoint")
			}
			sent, _ := f.state.mailbox.object(f.handles[1], 4)
			box.objects[0].Parent = sent.binding.store.byFID[f.fids[SentItems]].native

			var afterSource, afterDestination contentsSyncState
			var sourceWire []byte
			if destinationFirst {
				_, afterDestination = download(SentItems, destination)
				sourceWire, afterSource = download(DeletedItems, source)
			} else {
				sourceWire, afterSource = download(DeletedItems, source)
				_, afterDestination = download(SentItems, destination)
			}
			if len(contentsRanges(afterSource.given)) != 0 || !reflect.DeepEqual(source.seen, afterSource.seen) ||
				!reflect.DeepEqual(source.given, afterDestination.given) || reflect.DeepEqual(source.seen, afterDestination.seen) ||
				len(box.mimeReads) != 2 || box.sourceCalls != 0 {
				t.Fatal("move omitted deletion, changed the identity, or downloaded the wrong revision")
			}
			var deleted []idRange
			for _, token := range decodeTestContents(t, sourceWire) {
				if token.tag == 0x67e50102 {
					payload := token.value.([]byte)
					if len(payload) < 2 || binary.LittleEndian.Uint16(payload) != 1 || deleted != nil {
						t.Fatal("invalid deletion replica")
					}
					wire := reader{data: payload[2:]}
					ranges, err := parseGlobset(&wire, new(int))
					if err != nil || wire.pos != len(wire.data) {
						t.Fatal("invalid deletion set")
					}
					deleted = ranges
				}
			}
			if !reflect.DeepEqual(deleted, contentsRanges(source.given)) {
				t.Fatal("source transfer did not explicitly delete the original identity")
			}
			if _, check := download(SentItems, afterDestination); !reflect.DeepEqual(check, afterDestination) || len(box.mimeReads) != 2 {
				t.Fatal("unchanged destination retransmitted the moved message")
			}
		})
	}
}

func TestUnsupportedMIMEKeepsTableAndContentsUsable(t *testing.T) {
	f, box := indexedContentsFixture(t, 2, 0)
	f.transport.EnableContentsTableSpike()
	box.objects[0].MIME = []byte("From: synthetic@example.invalid\r\nSubject: Ordinary mail\r\n\r\nOrdinary body")
	bad := &box.objects[1]
	original := []byte("Subject: Signed mail\r\nContent-Type: multipart/signed; boundary=test\r\n\r\n--test\r\n\r\nSigned content\r\n--test--\r\n")
	bad.MIME = bytes.Clone(original)
	bad.Properties[core.Subject] = core.Text("Signed mail")
	for i := range box.objects {
		box.objects[i].Properties[core.MessageSize] = core.Integer(len(box.objects[i].MIME))
	}
	if _, err := projectMailContents(*bad); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("invalid fixture, strict projection returned %v", err)
	}
	table, _ := openTableTest(t, f, 0x50)
	if len(table.rows) != 2 || table.rows[0][fixedContentsProperty(0x0037001f)] != core.Text("Ordinary mail") ||
		table.rows[1][fixedContentsProperty(0x0037001f)] != core.Text("Signed mail") {
		t.Fatal("a MIME-format error removed a valid contents table row")
	}
	f.run([]byte{1, 4, 2})
	download := func(initial contentsSyncState, want ...string) contentsSyncState {
		t.Helper()
		configureContentsTest(t, f, fullContentsConfig())
		uploadContentsWireTest(t, f, initial)
		raw := drainFX(t, f.run, 2, 4096)
		f.run([]byte{1, 4, 2})
		var bodies []string
		for _, token := range decodeTestContents(t, raw) {
			if token.tag == 0x1000001f {
				bodies = append(bodies, token.value.(string))
			}
		}
		if !slices.Equal(bodies, want) {
			t.Fatalf("unexpected body projections: %q", bodies)
		}
		return contentsFourTestSets(t, raw)
	}
	initial := download(contentsSyncState{}, "Ordinary body", unprojectableMailBody)
	if !bytes.Equal(bad.MIME, original) {
		t.Fatal("MAPI changed the original MIME")
	}

	bad.MIME = []byte("Subject: Updated mail\r\n\r\nReprocessed body")
	bad.Revision++
	bad.Properties[core.MetadataRevision] = core.Integer(bad.Revision)
	bad.Properties[core.ContentRevision] = core.Integer(4)
	bad.Properties[core.MessageSize] = core.Integer(len(bad.MIME))
	updated := download(initial, "Reprocessed body")
	if !reflect.DeepEqual(initial.given, updated.given) || reflect.DeepEqual(initial.seen, updated.seen) {
		t.Fatal("the reprocessed message should retain its identity and send an updated version")
	}
}

func TestContentsTableUsesIndexWhenFullMIMECaptureExceedsLimit(t *testing.T) {
	f, box := indexedContentsFixture(t, 340, 0)
	f.transport.EnableContentsTableSpike()
	box.sourceErr = core.ErrLimit
	table, _ := openTableTest(t, f, 0x50)
	if len(table.rows) != 340 || len(box.mimeReads) != 0 {
		t.Fatalf("large contents table returned %d rows and read %d MIME objects", len(table.rows), len(box.mimeReads))
	}
}
