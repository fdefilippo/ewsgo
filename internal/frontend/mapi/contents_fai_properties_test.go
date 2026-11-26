// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"

	"ewsgo/internal/core"
)

func TestContentsFAIFlagsAndAbsentDeliveryTimePredicates(t *testing.T) {
	f, box := messageFixture(t)
	for _, read := range []bool{false, true} {
		record := faiTestRecord(f.store, f.binding.fids[Inbox], 1)
		record.object.Properties[core.IsRead] = core.Bool(read)
		seedFAI(t, f.store, record)
		observation, err := f.store.observeFAIMessages(context.Background(), f.session, f.binding.fids[Inbox])
		if err != nil || len(observation.snapshot.messages) != 1 {
			t.Fatal("FAI observation")
		}
		change, err := f.store.projectContentsRow(observation.snapshot.messages[0])
		observation.close()
		if err != nil || change.payload.values[fixedContentsProperty(0x0e070003)] != core.Integer(0x40) {
			t.Fatal("FAI flags must identify associated object without a read flag")
		}
		if _, exists := change.payload.values[fixedContentsProperty(0x0e060040)]; exists {
			t.Fatal("FAI invented a delivery timestamp")
		}
		for _, tc := range []struct {
			wire string
			want bool
		}{
			{"08 4000060e", false}, // Known, absent delivery time.
			{"02 08 4000060e", true},
			{"04 04 4000060e 40000000 0000000000000000", false},
			{"06 01 0300070e 40000000", true}, // mfFAI present.
			{"06 00 0300070e 40000000", false},
			{"06 01 0300070e 01000000", false}, // No read state in FAI flags.
			{"01 0200 08 4000060e 06 01 0300070e 40000000", true},
			{"00 0200 08 4000060e 06 01 0300070e 40000000", false},
		} {
			node, err := parseContentsRestriction(filterWire(t, tc.wire))
			if err != nil {
				t.Fatal("predicate decode", err)
			}
			node, err = bindContentsRestriction(node, func(tag uint32) (contentsProperty, error) {
				return resolveContentsProperty(tag, &f.store.named, contentsProjectionSchema(false, true))
			})
			if err != nil {
				t.Fatal("FAI properties rejected at binding", err)
			}
			if got, err := node.matches(change.payload.values); err != nil || got != tc.want {
				t.Fatal("FAI predicate ignored or absence semantics changed")
			}
		}
	}
	if box.sourceCalls != 0 {
		t.Fatal("FAI properties invoked native mail/event reader")
	}
	// Projection support must not make these properties writable or storable.
	draft := newFAIDraftForTest(t, f, syntheticFAIImport())
	before, _ := core.CloneProperties(draft.properties)
	problems, err := draft.set(context.Background(), f.session, faiSetRequest{{0x0e070003, core.Integer(8)}})
	if err != nil || !reflect.DeepEqual(problems, []faiPropertyProblem{{0, 0x0e070003, NotSupported}}) || !reflect.DeepEqual(before, draft.properties) {
		t.Fatal("unsupported FAI transport flags became writable")
	}
	if _, err := draft.set(context.Background(), f.session, faiSetRequest{{0x0e060040, core.Timestamp(hierarchyTestTime)}}); !errors.Is(err, core.ErrUnsupported) || !reflect.DeepEqual(before, draft.properties) {
		t.Fatal("absent delivery property became writable")
	}
}

func TestContentsFAIPropertiesCalendarWireAndReconnect(t *testing.T) {
	f, _ := newCalendarFixture(t) // Deliberately no mail or event content port.
	f.transport.EnableHierarchyDownloadSpike()
	f.transport.EnableContentsDownloadSpike()
	f.handles = append(f.handles, 0xffffffff, 0xffffffff)
	entry := calendarEntry(t, f)
	resolved, _ := f.run(conversionOperation(0x44, entry[22:]))
	fid := binary.LittleEndian.Uint64(resolved[6:])
	f.run(folderOperation(fid, 4, 0, 1))
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	seedFAI(t, object.binding.store, faiTestRecord(object.binding.store, fid, 1))
	c := fullContentsConfig()
	c.flags &^= 0x20
	// Independent wire: NOT Exists(DeliveryTime) AND NEZ(MessageFlags,mfFAI).
	filter := filterWire(t, "00 0200 02 08 4000060e 06 01 0300070e 40000000")
	faiRunOK(t, f, contentsConfigureOperation(c, filter))
	raw := drainFX(t, f.run, 2, 79)
	state := contentsFourTestSets(t, raw)
	flags, messages := 0, 0
	for _, token := range decodeTestContents(t, raw) {
		switch token.tag {
		case 0x0e070003:
			if token.value != int32(0x40) {
				t.Fatal("wrong FAI flags on wire")
			}
			flags++
		case 0x001a001f:
			messages++
		case 0x0e060040:
			t.Fatal("delivery timestamp fabricated on wire")
		}
	}
	if flags != 1 || messages != 1 {
		t.Fatal("FAI filter did not export the local configuration")
	}
	faiRunOK(t, f, []byte{1, 4, 2})
	// Reconnect with new handles and the uploaded completed checkpoint.
	f.state.mailbox.close()
	f.state.mailbox = newMailboxObjects(f.transport.stores)
	f.handles = []uint32{0xffffffff, 0xffffffff, 0xffffffff, 0xffffffff}
	faiRunOK(t, f, logonOperation(f.identity.LegacyDN, 4, 0))
	faiRunOK(t, f, folderOperation(fid, 4, 0, 1))
	faiRunOK(t, f, contentsConfigureOperation(c, filter))
	uploadContentsWireTest(t, f, state)
	raw = drainFX(t, f.run, 2, 79)
	for _, token := range decodeTestContents(t, raw) {
		if token.tag == 0x001a001f {
			t.Fatal("unchanged FAI resent after reconnect")
		}
	}
	if !reflect.DeepEqual(contentsFourTestSets(t, raw), state) {
		t.Fatal("FAI checkpoint changed after reconnect")
	}
	faiRunOK(t, f, []byte{1, 4, 2})
	c.flags |= 0x20
	response, counts := f.run(contentsConfigureOperation(c, filter))
	if counts.errors != 1 || binary.LittleEndian.Uint32(response[2:]) != NotSupported {
		t.Fatal("Calendar event contents became readable")
	}
	f.state.mailbox.close()
}
