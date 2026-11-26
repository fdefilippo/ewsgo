// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"testing"
)

// Outlook can keep completed downloads and immutable GetTransferState handles
// open while starting another folder. Completion is not a RopRelease.
func TestContentsRetainedTransfersDoNotStarveNextInbox(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	f.transport.EnableSynchronizationOptionsTrace()
	drain := func(slot byte) []byte {
		t.Helper()
		var raw []byte
		for range 10000 {
			wire, counts := f.run(hierarchyBufferOperation(4, slot, 4096))
			if counts.errors != 0 || len(wire) < 15 || binary.LittleEndian.Uint32(wire[2:]) != 0 {
				_, _, _, labels, _ := counts.execution.Fields()
				t.Fatal("GetBuffer failed with retained transfers", labels)
			}
			raw = append(raw, wire[15:]...)
			if binary.LittleEndian.Uint16(wire[6:]) == 3 {
				return raw
			}
		}
		t.Fatal("stream did not complete")
		return nil
	}
	associated := fullContentsConfig()
	associated.flags &^= 0x20
	configureContentsTest(t, f, associated)
	drain(2)
	wire, counts := f.run([]byte{0x82, 4, 2, 3})
	if counts.errors != 0 || binary.LittleEndian.Uint32(wire[2:]) != 0 {
		t.Fatal("immutable state source unavailable with retained downloads")
	}
	drain(3)
	// Keep both server handles reachable, then reuse the output slots.
	f.handles = append(f.handles, f.handles[2], f.handles[3])
	configureContentsTest(t, f, fullContentsConfig())
	raw := drain(2)
	changes := 0
	for _, token := range decodeTestContents(t, raw) {
		if token.tag == 0x65e00102 {
			changes++
		}
	}
	if changes != 1 {
		t.Fatal("next Inbox did not export the native fixture message")
	}
	object, _ := f.state.mailbox.object(f.handles[2], 4)
	store := object.binding.store
	if err := store.validateContentsCheckpoint(t.Context(), object.download.contents.state.scope, object.download.contents.final); err != nil {
		t.Fatal("completed Inbox state unavailable", err)
	}
	if store.allocator.contents.used >= 6<<20 {
		t.Fatal("small retained streams still consume maximum reservations")
	}
	// All earlier immutable sources and completed streams must remain usable.
	for slot := 4; slot < len(f.handles); slot++ {
		wire, counts := f.run(hierarchyBufferOperation(4, byte(slot), 4096))
		if counts.errors != 0 || !bytes.Equal(wire[15:], nil) || binary.LittleEndian.Uint16(wire[6:]) != 3 {
			t.Fatal("retained completed source lost its Done semantics")
		}
	}
	f.state.mailbox.close()
	if store.allocator.contents.used != len(store.contentsProofs)*contentsProofMemory || f.transport.stores.transfers.Load() != 0 {
		t.Fatal("disconnect leaked stream/state reservations")
	}
}
