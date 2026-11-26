// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"time"
)

// LogonRequest is MS-OXCROPS 2.2.3.1.1. Parsing preserves flags for later
// semantic authorization; it does not infer permissions from TAKE_OWNERSHIP.
// EssdnSize==0 is permitted by MS-OXCSTOR 2.2.1.1.1.
type LogonRequest struct {
	Flags                 byte
	OpenFlags, StoreState uint32
	ESSDN                 string
}

func parseLogonRequest(r *reader) (LogonRequest, error) {
	fixed := r.take(11)
	if r.err != nil {
		return LogonRequest{}, ErrMalformed
	}
	v := LogonRequest{Flags: fixed[0], OpenFlags: binary.LittleEndian.Uint32(fixed[1:]), StoreState: binary.LittleEndian.Uint32(fixed[5:])}
	size := int(binary.LittleEndian.Uint16(fixed[9:]))
	if size > 4096 {
		return LogonRequest{}, ErrMalformed
	}
	name := r.take(size)
	if r.err != nil {
		return LogonRequest{}, ErrMalformed
	}
	if size == 0 {
		return v, nil
	}
	if name[size-1] != 0 || bytes.IndexByte(name[:size-1], 0) >= 0 {
		return LogonRequest{}, ErrMalformed
	}
	for _, b := range name[:size-1] {
		if b < 32 || b > 127 {
			return LogonRequest{}, ErrMalformed
		}
	}
	v.ESSDN = string(name[:size-1])
	return v, nil
}

// PrivateLogonResponse is the 166-byte ROP body in MS-OXCROPS 2.2.3.1.2.
// GUID arrays are already in Microsoft wire order. All identities, rights and
// FIDs must be supplied by an authorized store; this codec invents none and
// does not create a server handle or imply an opened mailbox.
type PrivateLogonResponse struct {
	OutputHandleIndex, LogonFlags, ResponseFlags byte
	FolderIDs                                    [13]uint64
	MailboxGUID, ReplicaGUID                     [16]byte
	ReplicaID                                    uint16
	LogonTime                                    time.Time
	GwartTime                                    uint64
}

func (v PrivateLogonResponse) Marshal() ([]byte, error) {
	tm := v.LogonTime.UTC()
	if v.LogonFlags&1 == 0 || v.LogonFlags&^byte(7) != 0 || v.ResponseFlags&1 == 0 || v.ResponseFlags&^byte(0x17) != 0 ||
		v.MailboxGUID == [16]byte{} || v.ReplicaGUID == [16]byte{} || v.ReplicaID == 0 || v.LogonTime.IsZero() || tm.Year() < 1 || tm.Year() > 65535 {
		return nil, ErrMalformed
	}
	for _, id := range v.FolderIDs {
		if id == 0 {
			return nil, ErrMalformed
		}
	}
	out := append32([]byte{0xfe, v.OutputHandleIndex}, 0)
	out = append(out, v.LogonFlags)
	for _, id := range v.FolderIDs {
		out = binary.LittleEndian.AppendUint64(out, id)
	}
	out = append(out, v.ResponseFlags)
	out = append(out, v.MailboxGUID[:]...)
	out = binary.LittleEndian.AppendUint16(out, v.ReplicaID)
	out = append(out, v.ReplicaGUID[:]...)
	out = append(out, byte(tm.Second()), byte(tm.Minute()), byte(tm.Hour()), byte(tm.Weekday()), byte(tm.Day()), byte(tm.Month()))
	out = binary.LittleEndian.AppendUint16(out, uint16(tm.Year()))
	out = binary.LittleEndian.AppendUint64(out, v.GwartTime)
	return append32(out, 0), nil // StoreState is always zero.
}
