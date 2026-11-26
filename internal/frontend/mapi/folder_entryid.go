// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"ewsgo/internal/core"
)

const folderEntryIDBytes = 46

const (
	folderEntryIDTag       = 0x0fff0102
	parentFolderEntryIDTag = 0x0e090102
)

func marshalFolderEntryID(mailbox, replica [16]byte, fid uint64) []byte {
	out := make([]byte, folderEntryIDBytes)
	copy(out[4:20], mailbox[:])
	binary.LittleEndian.PutUint16(out[20:], 1)
	copy(out[22:38], replica[:])
	wire := binary.LittleEndian.AppendUint64(nil, fid)
	copy(out[38:44], wire[2:])
	return out
}

// Validate before any lookup. Only assigned Folder FIDs in this RAM epoch can
// be resolved; no mapping is created from an untrusted counter.
func parseFolderEntryID(data []byte, mailbox, replica [16]byte) (uint64, error) {
	if len(data) != folderEntryIDBytes || binary.LittleEndian.Uint32(data) != 0 || !bytes.Equal(data[4:20], mailbox[:]) || binary.LittleEndian.Uint16(data[20:]) != 1 || !bytes.Equal(data[22:38], replica[:]) || binary.LittleEndian.Uint16(data[44:]) != 0 {
		return 0, core.ErrInvalid
	}
	return fidFromGlobalCounter(data[38:44]), nil
}

func fidFromGlobalCounter(counter []byte) uint64 {
	var wire [8]byte
	wire[0] = 1
	copy(wire[2:], counter)
	return binary.LittleEndian.Uint64(wire[:])
}

func convertFolderID(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if !object.isLogon {
		return fail(NotSupported)
	}
	s := object.binding.store
	if _, err := s.folder(ctx, session, object.binding.fids[MailboxRoot]); err != nil {
		return fail(storeError(err))
	}
	if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
		return fail(storeError(err))
	}
	if op.ID == 0x44 {
		var guid [16]byte
		copy(guid[:], op.LongTermID[:16])
		if guid == ([16]byte{}) {
			return fail(0x80070057)
		}
		// MS-OXCSTOR 3.2.5.9 ignores the two padding bytes and translates
		// namespaces without requiring a corresponding existing object.
		var wire [8]byte
		copy(wire[2:], op.LongTermID[16:22])
		if wire == ([8]byte{}) {
			return binary.LittleEndian.AppendUint64(fail(0), 0)
		}
		replid, code := s.replicaID(ctx, guid)
		if code != 0 {
			return fail(code)
		}
		binary.LittleEndian.PutUint16(wire[:2], replid)
		return append(fail(0), wire[:]...)
	}
	guid, code := s.replicaGUID(ctx, uint16(op.Folder))
	if code != 0 {
		return fail(code)
	}
	wire := binary.LittleEndian.AppendUint64(nil, op.Folder)
	out := append(fail(0), guid[:]...)
	out = append(out, wire[2:]...)
	return append(out, 0, 0)
}
