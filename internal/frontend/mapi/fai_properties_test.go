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
	"testing"

	"ewsgo/internal/core"
)

// An independent wire expectation, including PT_UNSPECIFIED's resolved type.
func faiChangeKeyResponse(input byte, key changeXID, unspecified bool) []byte {
	out := ropError(7, input, 0)
	if unspecified {
		out = append(out, 1, 2, 1, 0)
	} else {
		out = append(out, 0)
	}
	return append(binary.LittleEndian.AppendUint16(out, uint16(len(key))), []byte(key)...)
}

func TestFAIChangeKeyAfterSaveEveryModeAndXIDWidth(t *testing.T) {
	for width := 1; width <= 8; width++ {
		for _, flags := range []byte{0, 1, 2, 4, 8, 9, 10, 12} {
			t.Run(fmt.Sprintf("%d/%02x", width, flags), func(t *testing.T) {
				f, box, store := newFAIExecuteFixture(t)
				t.Cleanup(f.state.mailbox.close)
				request := variableFAIImport(width)
				faiRunOK(t, f, faiImportOperation(request))
				faiRunOK(t, f, faiSetOperation(3, faiCreateProperties()))
				ops := append([]byte{0x0c, 4, 1, 3, flags}, folderPropertiesOperation(4, 3, 1, 0x65e20102)...)
				out := faiRunOK(t, f, ops)
				if len(out) != 15+9+16+width || !bytes.Equal(out[15:], faiChangeKeyResponse(3, request.change, false)) {
					t.Fatal("Save/read Message ChangeKey wire mismatch")
				}
				before, memory := store.next, f.transport.stores.contents.used
				for _, tag := range []uint32{0x65e20102, 0x65e20000} {
					got := faiRunOK(t, f, folderPropertiesOperation(4, 3, 1, tag))
					if !bytes.Equal(got, faiChangeKeyResponse(3, request.change, uint16(tag) == 0)) || store.next != before || f.transport.stores.contents.used != memory || box.sourceCalls != 0 {
						t.Fatal("property read changed version, memory or native contents")
					}
				}
				object, _ := f.state.mailbox.object(f.handles[3], 4)
				draft := object.download.draft
				faiRunOK(t, f, []byte{1, 4, 3})
				if !draft.closed || draft.lease != nil || draft.properties != nil {
					t.Fatal("Release retained draft memory")
				}
				got, _ := f.run(folderPropertiesOperation(4, 3, 1, 0x65e20102))
				if !bytes.Equal(got, ropError(7, 3, invalidObject)) || len(store.fai.records) != 1 {
					t.Fatal("released handle read succeeded or removed committed record")
				}
			})
		}
	}
}

func TestFAIChangeKeyPendingErrorsAndLocalVersion(t *testing.T) {
	f, box, store := newFAIExecuteFixture(t)
	t.Cleanup(f.state.mailbox.close)
	r := variableFAIImport(8)
	faiRunOK(t, f, faiImportOperation(r))
	want := append(ropError(7, 3, 0), append32([]byte{1, 0x0a}, PropertyNotFound)...)
	if got := faiRunOK(t, f, folderPropertiesOperation(4, 3, 1, 0x65e20102)); !bytes.Equal(got, want) || len(store.fai.records) != 0 {
		t.Fatal("unsaved import exposed a committed key")
	}
	faiRunOK(t, f, faiSetOperation(3, faiCreateProperties()))
	faiRunOK(t, f, []byte{0x0c, 4, 1, 3, 0})
	// Wrong types and other Message properties are explicit property errors.
	want = append(append(ropError(7, 3, 0), append32([]byte{1, 0x0a}, NotSupported)...), append32([]byte{0x0a}, NotSupported)...)
	if got := faiRunOK(t, f, folderPropertiesOperation(4, 3, 1, 0x65e20003, 0x67480014)); !bytes.Equal(got, want) {
		t.Fatal("unsupported Message properties acquired values")
	}
	faiRunOK(t, f, faiSetOperation(3, faiSetRequest{{0x7006001f, core.Text("Local change")}}))
	if got := faiRunOK(t, f, folderPropertiesOperation(4, 3, 1, 0x65e20102)); !bytes.Equal(got, faiChangeKeyResponse(3, r.change, false)) {
		t.Fatal("pending property edit changed committed key")
	}
	ops := append([]byte{0x0c, 4, 1, 3, 0}, folderPropertiesOperation(4, 3, 1, 0x65e20102)...)
	got := faiRunOK(t, f, ops)
	obj, _ := f.state.mailbox.object(f.handles[3], 4)
	d := obj.download.draft
	committed := store.fai.records[d.id].sync.change
	if committed == r.change || len(committed) != 22 || !bytes.Equal(got[15:], faiChangeKeyResponse(3, committed, false)) || box.sourceCalls != 0 {
		t.Fatal("local Save key disagrees with committed version")
	}
}

func TestFAIChangeKeyPermissionCancellationAndStaleDraft(t *testing.T) {
	for _, mode := range []string{"revoked", "scope", "cancel", "replacement", "deleted", "closed"} {
		t.Run(mode, func(t *testing.T) {
			f, _ := messageFixture(t)
			d := newFAIDraftForTest(t, f, variableFAIImport(8))
			saveFAIDraftForTest(t, f, d, 0)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			session, want := core.Session(f.session), core.ErrAccessDenied
			switch mode {
			case "revoked":
				f.box.state.Owner = false
			case "scope":
				identity := f.session.Identity()
				identity.Scope.Account = "other"
				session = scopedSession{Session: f.session, identity: identity}
			case "cancel":
				cancel()
				want = context.Canceled
			case "replacement", "deleted":
				record := f.store.fai.records[d.id]
				record.object.Revision++
				record.deleted = mode == "deleted"
				f.store.fai.records[d.id] = record
				want = core.ErrConflict
			case "closed":
				d.close()
				want = core.ErrClosed
			}
			before, size := f.store.next, f.store.fai.bytes
			if key, err := d.committedChangeKey(ctx, session); key != nil || !errors.Is(err, want) || f.store.next != before || f.store.fai.bytes != size {
				t.Fatal("invalid read exposed key or changed state")
			}
		})
	}
}

func TestFAIChangeKeyBatchReservesWorstCaseBeforeSave(t *testing.T) {
	for _, tag := range []uint32{0x65e20102, 0x65e20000} {
		f, _, store := newFAIExecuteFixture(t)
		t.Cleanup(f.state.mailbox.close)
		faiRunOK(t, f, faiImportOperation(variableFAIImport(8)))
		object, _ := f.state.mailbox.object(f.handles[3], 4)
		props, _ := core.CloneProperties(object.download.draft.properties)
		handles := append([]uint32(nil), f.handles...)
		next, memory := store.next, f.transport.stores.contents.used
		ops := append(faiSetOperation(3, faiCreateProperties()), 0x0c, 4, 1, 3, 0)
		ops = append(ops, folderPropertiesOperation(4, 3, 1, tag)...)
		// Enough for the previous four-byte error bound, not a 24-byte XID.
		oldReadBound := 12
		if uint16(tag) == 0 {
			oldReadBound += 2
		}
		maximum := uint32(10 + 4*len(f.handles) + 8 + 10*len(faiCreateProperties()) + 15 + oldReadBound)
		f.execute(ops, maximum)
		if len(store.fai.records) != 0 || object.download.draft.saved || store.next != next || f.transport.stores.contents.used != memory || !reflect.DeepEqual(handles, f.handles) || !reflect.DeepEqual(props, object.download.draft.properties) {
			t.Fatal("undersized batch executed Set or Save")
		}
		faiRunOK(t, f, ops)
	}
}
