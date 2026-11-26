// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"crypto/rand"
	"encoding/binary"
	"net/http"
	"strings"

	"ewsgo/internal/core"
)

const maxDirectoryMIDs = 1024

// directoryState lives exactly as long as its NSPI context. A new GUID on every
// Bind prevents clients from treating process-local MIDs as persistent IDs.
// MS-OXNSPI 3.1.3 permits this lifetime; durable core storage is not selected.
type directoryState struct {
	guid [16]byte
	ids  map[core.Scope]uint32
}

func newDirectoryState() *directoryState {
	d := &directoryState{ids: make(map[core.Scope]uint32)}
	_, _ = rand.Read(d.guid[:])
	d.guid[7] = d.guid[7]&0x0f | 0x40 // GUID wire byte order.
	d.guid[8] = d.guid[8]&0x3f | 0x80
	return d
}

// EnableNativeDirectory must be called before serving requests. It expands the
// separately enabled Bind experiment; GetProps also revalidates the native catalog on every request.
func (t *SessionTransport) EnableNativeDirectory() { t.nativeDirectory = true }

// Record only bounded counts from our own successful response, never the DNs
// or IDs. This distinguishes successful lookup processing from all-unknown DNs
// during a real Outlook test without adding body capture to the HTTP logger.
func recordDirectoryResult(w http.ResponseWriter, body []byte) {
	recorder, ok := w.(interface{ RecordDirectoryResult(int, int) })
	if !ok || len(body) < 17 || binary.LittleEndian.Uint32(body) != 0 || binary.LittleEndian.Uint32(body[4:]) != 0 || body[8] == 0 {
		return
	}
	count := binary.LittleEndian.Uint32(body[9:])
	if count > MaxDirectoryNames || len(body) != 17+int(count)*4 {
		return
	}
	resolved := 0
	for i := 0; i < int(count); i++ {
		if binary.LittleEndian.Uint32(body[13+i*4:]) != 0 {
			resolved++
		}
	}
	recorder.RecordDirectoryResult(int(count), resolved)
}

func (t *SessionTransport) directoryIDs(ctx context.Context, session core.Session, state *contextState, request DNToMId) []byte {
	if request.Names == nil {
		return DNToMIdResponse(0x80070057, nil)
	}
	if state.directory == nil {
		return DNToMIdResponse(NotSupported, nil)
	}
	entries, code := t.visibleDirectory(ctx, session)
	if code != 0 {
		return DNToMIdResponse(code, nil)
	}
	visible := make(map[string]core.Scope, len(entries))
	for _, entry := range entries {
		mapped, _ := t.namespace.Mailbox(entry.Identity) // Already validated as a complete catalog.
		visible[strings.ToLower(mapped.LegacyDN)] = entry.Identity.Scope
	}
	needed := make(map[core.Scope]bool)
	for _, dn := range request.Names {
		if key, exists := visible[strings.ToLower(dn)]; exists {
			if _, allocated := state.directory.ids[key]; !allocated {
				needed[key] = true
			}
		}
	}
	if len(needed)+len(state.directory.ids) > maxDirectoryMIDs {
		return DNToMIdResponse(0x8007000e, nil) // NotEnoughMemory; atomic, no partial allocation.
	}
	ids := make([]uint32, len(request.Names))
	for i, dn := range request.Names {
		if key, exists := visible[strings.ToLower(dn)]; exists {
			id := state.directory.ids[key]
			if id == 0 {
				id = uint32(len(state.directory.ids)) + 0x10 // 0..15 reserved by NSPI.
				state.directory.ids[key] = id
			}
			ids[i] = id
		}
	}
	return DNToMIdResponse(0, ids)
}
