// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

func TestSynchronizationTypeDiagnosticAtDecodedBoundary(t *testing.T) {
	for _, prefix := range [][]byte{nil, folderOperation(123, 4, 0, 1), []byte{1, 4, 1}, receiveFolderOperation(4, 0, "IPM.Private.SENTINEL")} {
		for _, kind := range []byte{0, 1, 2, 255} {
			request := []byte{0x70, 4, 1, 2, kind, 0, 0, 0, 0, 0}
			// The body is deliberately not parsed, retained or considered valid.
			request = append(request, []byte("PRIVATE-restriction-tags-values")...)
			ops, err := (ROPBuffer{Operations: append(bytes.Clone(prefix), request...), Handles: []uint32{1, 2, 3}}).DecodeOperations()
			var unsupported unsupportedOperation
			if ops != nil || !errors.As(err, &unsupported) || unsupported.label != "RopSynchronizationConfigure" {
				t.Fatal("unsupported full batch")
			}
			state, got := unsupported.synchronization.Fields()
			want := "other"
			if kind == 1 {
				want = "contents"
			}
			if kind == 2 {
				want = "hierarchy"
			}
			if state != "header_only" || got != want {
				t.Fatal("closed type")
			}
			if bytes.Contains([]byte(err.Error()), []byte("PRIVATE")) {
				t.Fatal("payload leaked")
			}
		}
	}
	for n := 3; n < 10; n++ {
		wire := []byte{0x70, 4, 0, 1, 2, 0, 0, 0, 0, 0}[:n]
		_, err := (ROPBuffer{Operations: wire, Handles: []uint32{1, 2}}).DecodeOperations()
		var unsupported unsupportedOperation
		if !errors.As(err, &unsupported) {
			t.Fatal("unsupported short header")
		}
		state, kind := unsupported.synchronization.Fields()
		if state != "malformed" || kind != "none" {
			t.Fatal("short header diagnosed as complete")
		}
	}
	state, kind := inspectSynchronizationHeader([]byte{2, 1, 0, 0, 0, 0, 0}, 2).Fields()
	if state != "malformed" || kind != "none" {
		t.Fatal("invalid output slot")
	}
}

func TestSynchronizationDiagnosticLeavesBatchUnsupportedWithoutEffects(t *testing.T) {
	f := newOSTFixture(t)
	f.transport.EnableSynchronizationTrace()
	before := f.box.calls
	body, c := f.execute(append([]byte{1, 4, 1}, 0x70, 4, 0, 1, 2, 0, 0, 0, 0, 0), 4096)
	state, kind := c.synchronization.Fields()
	if binary.LittleEndian.Uint32(body[4:]) != NotSupported || state != "header_only" || kind != "hierarchy" || f.box.calls != before || c.folders != 0 || c.errors != 0 {
		t.Fatal("diagnostic changed execution")
	}
	if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
		t.Fatal("unsupported prefix released object")
	}
}
