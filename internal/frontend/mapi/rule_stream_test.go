// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"testing"

	"ewsgo/internal/core"
)

func TestRuleOrganizerStreamSaveAndReopen(t *testing.T) {
	f, _, store := newFAIExecuteFixture(t)
	object, _ := f.state.mailbox.object(f.handles[1], 4)
	create := binary.LittleEndian.AppendUint64([]byte{0x06, 4, 1, 3, 0xe4, 4}, object.fid)
	faiRunOK(t, f, append(create, 1))
	faiRunOK(t, f, faiSetOperation(3, faiSetRequest{{0x001a001f, core.Text("IPM.RuleOrganizer")}, {0x0037001f, core.Text("Outlook Rules Organizer")}}))
	raw, _ := f.run(streamOpenWire(3, 4, 0, rwRulesStreamTag))
	if !bytes.Equal(raw, ropError(0x2b, 4, PropertyNotFound)) {
		t.Fatalf("absent stream: %x", raw)
	}
	faiRunOK(t, f, streamOpenWire(3, 4, 2, rwRulesStreamTag))
	payload := []byte{0x00, 0x80, 0xff, 0x00, 0x11}
	write := binary.LittleEndian.AppendUint16([]byte{ropWriteStream, 4, 4}, uint16(len(payload)))
	faiRunOK(t, f, append(write, payload...))
	faiRunOK(t, f, []byte{ropCommitStream, 4, 4})
	faiRunOK(t, f, []byte{1, 4, 4})
	faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 0})
	var mid uint64
	for key, id := range store.byMessage {
		if key.associated {
			mid = id
		}
	}
	if mid == 0 {
		t.Fatal("saved associated message not indexed")
	}
	request := binary.LittleEndian.AppendUint64([]byte{0x03, 4, 0, 3, 0xe4, 4}, object.fid)
	request = binary.LittleEndian.AppendUint64(append(request, 0), mid)
	faiRunOK(t, f, request)
	faiRunOK(t, f, streamOpenWire(3, 4, 0, rwRulesStreamTag))
	got := faiRunOK(t, f, streamReadWire(4, 64))
	if !bytes.Equal(got[8:], payload) {
		t.Fatalf("reopened payload: %x", got)
	}
	raw, _ = f.run(append(write, payload...))
	if !bytes.Equal(raw, ropError(ropWriteStream, 4, 0x80030005)) {
		t.Fatalf("read-only stream write: %x", raw)
	}
}
