// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"unicode/utf16"

	"ewsgo/internal/core"
)

const maxDynamicFolders = 32
const maxDynamicDepth = 8
const maxDynamicBytes = 128 << 10
const duplicateFolderName uint32 = 0x80040604

const (
	genericFolderType = 1
	searchFolderType  = 2
)

func childFolderType(search bool) uint32 {
	if search {
		return searchFolderType
	}
	return genericFolderType
}

type createFolderRequest struct {
	output, kind, reserved byte
	existing               bool
	name, comment          string
	origin                 uint32 // Set by Execute, never accepted from the wire.
}

func (createFolderRequest) String() string   { return "createFolderRequest{redacted}" }
func (createFolderRequest) GoString() string { return "createFolderRequest{redacted}" }

// The Unicode terminator is required for both strings, even an empty comment.
// Surrogate validation prevents replacement characters from changing identity.
func createFolderString(r *reader, maximum int) (string, error) {
	units := make([]uint16, 0, min(maximum, 32))
	for len(units) <= maximum {
		pair := r.take(2)
		if r.err != nil {
			return "", ErrMalformed
		}
		unit := binary.LittleEndian.Uint16(pair)
		if unit == 0 {
			for i := 0; i < len(units); i++ {
				if units[i] >= 0xd800 && units[i] <= 0xdbff {
					if i+1 >= len(units) || units[i+1] < 0xdc00 || units[i+1] > 0xdfff {
						return "", ErrMalformed
					}
					i++
				} else if units[i] >= 0xdc00 && units[i] <= 0xdfff {
					return "", ErrMalformed
				}
			}
			return string(utf16.Decode(units)), nil
		}
		units = append(units, unit)
	}
	return "", ErrMalformed
}

func parseCreateFolder(r *reader, handles int) (createFolderRequest, error) {
	h := r.take(5)
	if r.err != nil || int(h[0]) >= handles {
		return createFolderRequest{}, ErrMalformed
	}
	if h[2] == 0 {
		return createFolderRequest{}, unsupportedOperation{label: "RopCreateFolder", createFolder: inspectCreateFolderHeader(h, handles)}
	}
	out := createFolderRequest{output: h[0], kind: h[1], existing: h[3] != 0, reserved: h[4]}
	var err error
	out.name, err = createFolderString(r, 255)
	if err != nil {
		return createFolderRequest{}, err
	}
	out.comment, err = createFolderString(r, 1024)
	if err != nil {
		return createFolderRequest{}, err
	}
	return out, nil
}

func (t *SessionTransport) EnableCreateFolderSpike() { t.createFolderSpike = true }

func createStoreFolder(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(0x1c, op.Output, code) }
	request := *op.CreateFolder
	request.origin = objects.origin
	if request.kind != genericFolderType && request.kind != searchFolderType || request.reserved != 0 || request.name == "" {
		return fail(0x80070057)
	}
	parent, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if parent.isLogon || parent.download != nil {
		return fail(NotSupported)
	}
	// Reserve every fallible handle resource before changing the store. A failed
	// creation may consume an opaque allocator number, never a live handle.
	if len(objects.objects) >= maxLiveStoreHandles || objects.allocated >= maxContextHandleAllocations {
		return fail(0x8007000e)
	}
	handle, err := objects.allocator.handle()
	if err != nil {
		return fail(0x8007000e)
	}
	fid, code := parent.binding.store.createFolder(ctx, session, parent.fid, request)
	if code != 0 {
		return fail(code)
	}
	objects.objects[handle] = serverObject{logon: op.Logon, binding: parent.binding, fid: fid}
	objects.allocated++
	handles[op.Output] = handle
	// MS-OXCFOLD 2.2.1.2.2: private mailboxes set IsExistingFolder to zero,
	// including a reopen. Public-store rules/ghosting fields are absent.
	return append(binary.LittleEndian.AppendUint64(fail(0), fid), 0)
}
