// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"unicode"

	"ewsgo/internal/core"
)

const (
	ropMoveFolder = 0x35
	ropCopyFolder = 0x36
)
const folderCycle uint32 = 0x8004060b

type folderPlacementRequest struct {
	fid         uint64
	destination byte
	async       bool
	recursive   bool
	name        string
	string8     bool
}

func (folderPlacementRequest) String() string   { return "folderPlacementRequest{redacted}" }
func (folderPlacementRequest) GoString() string { return "folderPlacementRequest{redacted}" }

func parseFolderPlacement(r *reader, handles int, rop byte) (folderPlacementRequest, error) {
	prefixSize := 2 // Destination handle and WantAsynchronous.
	if rop == ropCopyFolder {
		prefixSize++ // WantRecursive precedes UseUnicode.
	}
	header := r.take(prefixSize + 9)
	if r.err != nil || int(header[0]) >= handles {
		return folderPlacementRequest{}, ErrMalformed
	}
	request := folderPlacementRequest{
		fid: binary.LittleEndian.Uint64(header[prefixSize+1:]), destination: header[0],
		async: header[1] != 0, string8: header[prefixSize] == 0,
	}
	if rop == ropCopyFolder {
		request.recursive = header[2] != 0
	}
	if request.string8 {
		end := bytes.IndexByte(r.data[r.pos:], 0)
		if end < 0 || end > 1024 {
			return folderPlacementRequest{}, ErrMalformed
		}
		request.name = string(r.take(end + 1)[:end])
		return request, nil
	}
	var err error
	request.name, err = createFolderString(r, 255)
	return request, err
}

func folderPlacementResponse(op Operation, code uint32, partial bool) []byte {
	out := ropError(op.ID, op.Input, code)
	if code == moveCopyNullDestination {
		out = append32(out, uint32(op.FolderPlacement.destination))
	}
	if partial {
		return append(out, 1)
	}
	return append(out, 0)
}

func moveStoreFolder(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32, codePage uint32) []byte {
	request := op.FolderPlacement
	reply := func(code uint32) []byte {
		// A native folder move has one target and is applied atomically.
		return folderPlacementResponse(op, code, false)
	}
	source, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return reply(code)
	}
	destination, code := objects.object(handles[request.destination], op.Logon)
	if code == nullObject {
		return reply(moveCopyNullDestination)
	}
	if code != 0 {
		return reply(code)
	}
	if source.isLogon || destination.isLogon || source.download != nil || destination.download != nil ||
		request.async || destination.binding.store != source.binding.store {
		return reply(NotSupported)
	}
	name, code := folderPlacementName(request, codePage)
	if code != 0 {
		return reply(code)
	}
	err := source.binding.store.moveNativeFolder(ctx, session, source.fid, destination.fid, request.fid, name)
	return reply(folderPlacementCode(err))
}

func folderPlacementName(request folderPlacementRequest, codePage uint32) (string, uint32) {
	name := request.name
	if request.string8 {
		charset := restrictionCharset(codePage)
		if charset == "" {
			return "", NotSupported
		}
		var err error
		name, err = decodeMailText(charset, []byte(name))
		if err != nil {
			return "", storeError(core.ErrInvalid)
		}
	}
	if name == "" || strings.ContainsRune(name, '/') || strings.ContainsFunc(name, unicode.IsControl) {
		return "", storeError(core.ErrInvalid)
	}
	return name, 0
}

func folderPlacementCode(err error) uint32 {
	switch {
	case errors.Is(err, core.ErrFolderNameConflict):
		return duplicateFolderName
	case errors.Is(err, core.ErrFolderCycle):
		return folderCycle
	case err != nil:
		return storeError(err)
	default:
		return 0
	}
}

func (s *ramStore) moveNativeFolder(ctx context.Context, session core.Session, source, destination, fid uint64, name string) error {
	s.hierarchyMu.Lock()
	defer s.hierarchyMu.Unlock()

	from, err := s.folder(ctx, session, source)
	if err != nil {
		return err
	}
	to, err := s.folder(ctx, session, destination)
	if err != nil {
		return err
	}
	if from.native == "" || to.native == "" || to.search {
		return core.ErrUnsupported
	}
	s.mu.Lock()
	key, known := s.byFID[fid]
	s.mu.Unlock()
	if !known {
		return core.ErrNotFound
	}
	if key.native == "" || key.native == s.root {
		return core.ErrAccessDenied
	}
	box := session.Mailbox()
	writer, ok := box.(core.MailFolderHierarchyWriter)
	if !ok {
		return core.ErrUnsupported
	}
	if err := hierarchyOwner(ctx, box); err != nil {
		return err
	}
	before, err := box.Get(ctx, key.native)
	if err != nil {
		return err
	}
	if before.ID != key.native || before.Kind != core.Folder || before.Parent != from.native {
		return core.ErrNotFound
	}
	if before.Properties[core.WellKnown] != nil {
		return core.ErrAccessDenied
	}
	if before.Parent == to.native && before.Properties[core.DisplayName] == core.Text(name) {
		return nil
	}
	placed, err := writer.UpdateMailFolder(ctx, before, core.MailFolderPlacement{Parent: to.native, Name: name})
	if err != nil {
		return err
	}
	if placed.ID != key.native || placed.Kind != core.Folder || placed.Parent != to.native ||
		placed.Properties[core.DisplayName] != core.Text(name) {
		return core.ErrOutcomeUnknown
	}
	return nil
}
