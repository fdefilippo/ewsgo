// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import "encoding/binary"

// Structural codec dispatched only by the hierarchy download laboratory opt-in.
// MS-OXCROPS 2.2.13.1.1; common RopId/LogonId/InputIndex precede these fields.
// Restrictions are skipped, not interpreted or retained. Contents are not
// supported by the proposed hierarchy profile.
type synchronizationConfigure struct {
	output, kind, sendOptions byte
	flags                     uint16
	restrictionBytes          uint16
	extraFlags                uint32
	tags                      []uint32
}

// Prevent incidental formatting from exposing filters, tags or request data.
func (synchronizationConfigure) String() string   { return "MAPI synchronization request [private]" }
func (synchronizationConfigure) GoString() string { return "MAPI synchronization request [private]" }

func parseSynchronizationConfigure(r *reader, handles int) (synchronizationConfigure, error) {
	return readSynchronizationConfigure(r, handles, nil)
}

// The hierarchy codec skips restrictions. The Contents prerequisite instead
// validates and retains a private AST through this callback, before any effects.
func readSynchronizationConfigure(r *reader, handles int, restriction func([]byte) error) (synchronizationConfigure, error) {
	var zero synchronizationConfigure
	header := r.take(7)
	if r.err != nil || handles < 1 || handles > MaxHandles || int(header[0]) >= handles {
		return zero, ErrMalformed
	}
	request := synchronizationConfigure{
		output: header[0], kind: header[1], sendOptions: header[2],
		flags: binary.LittleEndian.Uint16(header[3:]), restrictionBytes: binary.LittleEndian.Uint16(header[5:]),
	}
	raw := r.take(int(request.restrictionBytes))
	if r.err != nil {
		return zero, ErrMalformed
	}
	tail := r.take(6)
	if r.err != nil {
		return zero, ErrMalformed
	}
	request.extraFlags = binary.LittleEndian.Uint32(tail)
	count := int(binary.LittleEndian.Uint16(tail[4:]))
	// Check framing before the local profile quota, without allocating from
	// an untrusted count or retaining any restriction bytes.
	if count > (len(r.data)-r.pos)/4 {
		return zero, ErrMalformed
	}
	if count > maxFolderPropertyTags {
		return zero, ErrUnsupported
	}
	if count > 0 {
		request.tags = make([]uint32, count)
	}
	for i := range request.tags {
		request.tags[i] = r.u32()
	}
	if restriction != nil {
		if err := restriction(raw); err != nil {
			return zero, err
		}
	}
	return request, nil
}
