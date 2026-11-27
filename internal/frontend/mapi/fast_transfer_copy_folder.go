// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"

	"ewsgo/internal/core"
)

const ropFastTransferSourceCopyFolder byte = 0x4c

func openFastTransferCopyFolder(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32, codePage uint32) []byte {
	reply := func(code uint32) []byte { return ropError(op.ID, op.Output, code) }
	source, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return reply(code)
	}
	if source.isLogon || source.download != nil {
		return reply(NotSupported)
	}
	if op.Flags & ^byte(0x11) != 0 {
		return reply(0x80070057)
	}
	if op.Flags != 0 || !objects.hasDownloadCapacity() {
		return reply(NotSupported)
	}
	string8, code := fastTransferCopyString8(op.TransferCopy.sendOptions, codePage)
	if code != 0 {
		return reply(code)
	}
	store := source.binding.store
	anchor, err := store.messageAnchor(ctx, session, source.fid, false)
	if err != nil {
		return reply(storeError(err))
	}
	if anchor.native == "" {
		return reply(NotSupported)
	}
	lease, err := store.reserveContentsPeak(ctx, 2*core.MaxMailContentViewBytes+maxContentsFastTransferBytes)
	if err != nil {
		return reply(storeError(err))
	}
	defer func() { lease.release() }()
	folder, err := store.folder(ctx, session, source.fid)
	if err != nil {
		return reply(storeError(err))
	}
	if folder.native != anchor.native {
		return reply(PropertyNotFound)
	}
	stream := &fastTransfer{string8CodePage: string8, limit: maxContentsFastTransferBytes}
	if err := store.writeCopiedFolder(ctx, session, source, folder, stream); err != nil {
		return reply(storeError(err))
	}
	if err := ctx.Err(); err != nil {
		return reply(storeError(err))
	}
	if err := lease.shrink(cap(stream.data) + 16*cap(stream.atoms) + 512); err != nil {
		return reply(storeError(err))
	}
	handle, err := objects.addDownload(source, &hierarchyDownload{copyFolder: anchor.native, copyLease: lease, stream: stream})
	if err != nil {
		return reply(storeError(err))
	}
	lease = nil
	handles[op.Output] = handle
	return reply(0)
}

func (s *ramStore) writeCopiedFolder(ctx context.Context, session core.Session, source serverObject, folder storeFolder, stream *fastTransfer) error {
	name, ok := folder.properties[core.DisplayName].(core.Text)
	if !ok || name == "" {
		return core.ErrInvalid
	}
	values := contentsValues{}
	putContentsValue(values, 0x3001001f, name) // DisplayName.
	s.mu.Lock()
	metadata := s.metadata[source.fid]
	class := core.Text(metadata[0x3613])
	hidden := metadata[folderHiddenTag>>16]
	s.mu.Unlock()
	if class == "" {
		class = "IPF.Note"
	}
	putContentsValue(values, 0x3613001f, class) // ContainerClass.
	if len(hidden) == 1 {
		putContentsValue(values, folderHiddenTag, core.Bool(hidden[0] != 0))
	}
	stream.word(0x40090003) // StartTopFld.
	if err := s.writeFastTransferProperties(stream, values, nil, nil); err != nil {
		return err
	}

	associated, err := s.observeFAIMessages(ctx, session, source.fid)
	if err != nil {
		return err
	}
	err = s.writeCopiedFolderMessages(ctx, session.Identity(), source, stream, associated.snapshot.messages, folder.native)
	associated.close()
	if err != nil {
		return err
	}
	normal, err := s.observeMailMessages(ctx, session, source.fid)
	if err != nil {
		return err
	}
	if err := s.writeCopiedFolderMessages(ctx, session.Identity(), source, stream, normal.messages, folder.native); err != nil {
		return err
	}
	stream.word(0x400b0003) // EndFolder.
	return stream.err
}

func (s *ramStore) writeCopiedFolderMessages(ctx context.Context, identity core.Identity, source serverObject, stream *fastTransfer, rows []messageRow, anchor core.ID) error {
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return err
		}
		if row.object.Parent != anchor {
			return core.ErrConflict
		}
		if err := s.writeCopiedMessage(stream, source, row, identity, false); err != nil {
			return err
		}
	}
	return nil
}
