// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"

	"ewsgo/internal/core"
)

const (
	ropFastTransferSourceCopyProperties = 0x69
	ropFastTransferDestinationConfigure = 0x53
	ropFastTransferDestinationPutBuffer = 0x54
	fastTransferPutBufferResponseSize   = 15
)

type fastTransferCopyRequest struct {
	level, sendOptions byte
	flags              uint32
	excluded           map[uint16]bool
	included           map[uint32]bool
}

func parseFastTransferCopy(r *reader, op *Operation, handles int) error {
	headerSize := 7
	if op.ID == ropFastTransferSourceCopyProperties {
		headerSize = 4
	}
	header := r.take(headerSize + 2)
	if r.err != nil || int(header[0]) >= handles {
		return ErrMalformed
	}
	count := int(binary.LittleEndian.Uint16(header[headerSize:]))
	tags := r.take(4 * count)
	if r.err != nil {
		return ErrMalformed
	}
	op.Output = header[0]
	request := fastTransferCopyRequest{level: header[1], sendOptions: header[headerSize-1]}
	if op.ID == ropFastTransferSourceCopyProperties {
		request.flags = uint32(header[2])
		request.included = make(map[uint32]bool, count)
		for len(tags) != 0 {
			request.included[binary.LittleEndian.Uint32(tags)] = true
			tags = tags[4:]
		}
	} else {
		request.flags = binary.LittleEndian.Uint32(header[2:6])
		request.excluded = make(map[uint16]bool, count)
		for len(tags) != 0 {
			// CopyTo excludes an ID across representations.
			request.excluded[binary.LittleEndian.Uint16(tags[2:4])] = true
			tags = tags[4:]
		}
	}
	op.TransferCopy = request
	return nil
}

func (r fastTransferCopyRequest) includes(tag uint32) bool {
	if r.included != nil {
		return r.included[tag]
	}
	return !r.excluded[uint16(tag>>16)]
}

func openFastTransferCopy(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32, connectionCodePage uint32) []byte {
	result := func(code uint32) []byte { return ropError(op.ID, op.Output, code) }
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return result(code)
	}
	if object.download != nil || object.message == nil && object.attachment == nil {
		return result(NotSupported)
	}
	request := op.TransferCopy
	// Both requests ignore the low reserved flags. Only CopyTo defines the
	// high flags and permits a request without a tag list.
	allowedFlags := uint32(0x0e)
	if request.included == nil {
		allowedFlags |= 0x600
		if object.message != nil {
			allowedFlags |= 0x2000 // BestBody
		}
	} else if len(request.included) == 0 {
		return result(0x80070057)
	}
	// The Move flag is optional, and requires verifying rights to delete.
	// UseCpid is for server-to-server upload. PartialItem is for ICS.
	if request.flags & ^allowedFlags != 0 {
		return result(0x80070057)
	}
	string8CodePage, code := fastTransferCopyString8(request.sendOptions, connectionCodePage)
	if code != 0 {
		return result(code)
	}
	if !objects.hasDownloadCapacity() {
		return result(NotSupported)
	}

	store := object.binding.store
	lease, err := store.reserveContentsPeak(ctx, 2*maxMailProjectionBytes+maxContentsFastTransferBytes)
	if err != nil {
		return result(storeError(err))
	}
	defer func() { lease.release() }()
	stream := &fastTransfer{string8CodePage: string8CodePage, limit: maxContentsFastTransferBytes}
	var source openedMessage
	if object.message != nil {
		source = *object.message
		err = store.writeCopyMessage(ctx, session, object.fid, source, request, stream)
	} else {
		source = object.attachment.message
		var rows []contentsValues
		rows, err = store.readOpenedAttachments(ctx, session, object.binding.mailboxGUID, object.fid, source)
		if err == nil {
			number := object.attachment.number
			if number >= uint32(len(rows)) {
				err = core.ErrNotFound
			} else {
				err = store.writeFastTransferProperties(stream, rows[number], nil, request.includes)
			}
		}
	}
	if err != nil {
		return result(attachmentError(err))
	}
	if err = ctx.Err(); err != nil {
		return result(storeError(err))
	}
	if err = lease.shrink(cap(stream.data) + 16*cap(stream.atoms) + 512); err != nil {
		return result(storeError(err))
	}
	// Keep a transfer handle distinct from the source's Message/Attachment.
	handle, err := objects.addDownload(serverObject{logon: op.Logon, binding: object.binding, fid: object.fid},
		&hierarchyDownload{copySource: &source, copyLease: lease, stream: stream})
	if err != nil {
		return result(storeError(err))
	}
	lease = nil
	handles[op.Output] = handle
	return result(0)
}

func fastTransferCopyString8(options byte, connectionCodePage uint32) (uint32, uint32) {
	if options & ^byte(0x6d) != 0 {
		return 0, 0x80070057
	}
	if options&0x09 != 0 {
		return 0, 0
	}
	if connectionCodePage != 1252 && connectionCodePage != 65001 {
		return 0, NotSupported
	}
	return connectionCodePage, 0
}

func (s *ramStore) writeCopyMessage(ctx context.Context, session core.Session, fid uint64, message openedMessage, request fastTransferCopyRequest, stream *fastTransfer) error {
	payload, err := s.readCopyMessagePayload(ctx, session, fid, message)
	if err != nil {
		return err
	}
	if err = s.writeFastTransferProperties(stream, payload.values, nil, request.includes); err != nil {
		return err
	}
	if request.level != 0 {
		return nil
	}
	return s.writeFastTransferMessageChildren(stream, payload, request.includes)
}

func (s *ramStore) readCopyMessagePayload(ctx context.Context, session core.Session, fid uint64, message openedMessage) (projectedMessage, error) {
	if message.embedded != nil {
		return s.readOpenedMailProjection(ctx, session, fid, message)
	}
	key, folder, err := s.openedMessageScope(ctx, session, fid, message)
	if err != nil {
		return projectedMessage{}, err
	}
	var row messageRow
	if key.associated {
		s.contentsMu.Lock()
		row, err = s.readFAIRowLocked(ctx, fid, key.id)
		s.contentsMu.Unlock()
	} else {
		row.object, err = readOpenedMessageMIME(ctx, session, folder, key, message)
	}
	if err != nil {
		return projectedMessage{}, err
	}
	return s.projectContentsPayload(row)
}
