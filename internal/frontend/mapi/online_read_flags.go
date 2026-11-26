// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"

	"ewsgo/internal/core"
)

const (
	ropSetMessageReadFlag   = 0x11
	ropSetReadFlags         = 0x66
	readFlagsResponseSize   = 7
	readFlagSuppressReceipt = 0x01
	readFlagClear           = 0x04
	readFlagReserved        = 0x0a
)

func parseSetReadFlags(r *reader, op *Operation) error {
	header := r.take(4)
	if r.err != nil {
		return r.err
	}
	count := int(binary.LittleEndian.Uint16(header[2:]))
	wire := r.take(8 * count)
	if r.err != nil || header[0] > 1 {
		return ErrMalformed
	}
	if count > core.MaxMailContentMessages {
		return ErrUnsupported
	}
	op.Flags = header[1]
	var err error
	op.ReadMIDs, err = decodeDistinctMessageIDs(wire)
	return err
}

func setMessageReadFlag(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Output, code) }
	if _, code := objects.lookup(handles[op.Output], op.Logon); code != 0 {
		return fail(code)
	}
	message, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	if message.message == nil || message.message.draft != nil || message.message.mailDraft != nil || message.message.includeSoftDeleted {
		return fail(NotSupported)
	}
	flags := op.Flags &^ readFlagReserved
	if !validOnlineReadFlags(flags) {
		return fail(NotSupported)
	}
	store := message.binding.store
	folder, err := store.messageAnchor(ctx, session, message.fid, false)
	if err == nil {
		err = applyOnlineReadFlag(ctx, session.Mailbox(), store, folder.native, message.message.mid, flags)
	}
	if errors.Is(err, core.ErrConflict) {
		return fail(0x80040109)
	}
	if err != nil {
		return fail(storeError(err))
	}
	// Private mailboxes omit the public-folder ClientData and LogonId.
	return append(fail(0), 0)
}

func setReadFlags(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	reply := func(code uint32, partial bool) []byte {
		out := ropError(op.ID, op.Input, code)
		if partial {
			return append(out, 1)
		}
		return append(out, 0)
	}
	source, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return reply(code, false)
	}
	flags := op.Flags &^ readFlagReserved
	if source.isLogon || source.download != nil || !validOnlineReadFlags(flags) {
		return reply(NotSupported, false)
	}
	store := source.binding.store
	folder, err := store.messageAnchor(ctx, session, source.fid, false)
	if err != nil {
		return reply(storeError(err), false)
	}
	if folder.native == "" {
		return reply(NotSupported, false)
	}
	mids := op.ReadMIDs
	if len(mids) == 0 {
		view, err := store.observeMailMessages(ctx, session, source.fid)
		if err != nil {
			return reply(storeError(err), false)
		}
		mids = make([]uint64, 0, len(view.messages))
		read := flags&readFlagClear == 0
		for _, message := range view.messages {
			if message.read != read {
				mids = append(mids, message.mid)
			}
		}
	}
	box := session.Mailbox()
	code, partial := executeMessageBatch(ctx, mids, func(mid uint64) error {
		return applyOnlineReadFlag(ctx, box, store, folder.native, mid, flags)
	})
	return reply(code, partial)
}

func validOnlineReadFlags(flags byte) bool {
	return flags == 0 || flags == readFlagSuppressReceipt || flags == readFlagClear ||
		flags == readFlagClear|readFlagSuppressReceipt
}

func applyOnlineReadFlag(ctx context.Context, box core.Mailbox, store *ramStore, folder core.ID, mid uint64, flags byte) error {
	err := setOnlineReadFlag(ctx, box, store, folder, mid, flags)
	if errors.Is(err, core.ErrConflict) {
		// The request names a message in this folder, without a revision.
		// Retry a definite conflict only after rechecking that membership.
		return setOnlineReadFlag(ctx, box, store, folder, mid, flags)
	}
	return err
}

func setOnlineReadFlag(ctx context.Context, box core.Mailbox, store *ramStore, folder core.ID, mid uint64, flags byte) error {
	current, err := store.messagePlacementSource(ctx, box, folder, mid)
	if err != nil {
		return err
	}
	read := flags&readFlagClear == 0
	var updated core.Object
	if read {
		writer, ok := box.(core.MailReadWriter)
		if !ok {
			return core.ErrUnsupported
		}
		updated, err = writer.MarkMailRead(ctx, current.ID, current.Revision, flags&readFlagSuppressReceipt != 0)
	} else {
		writer, ok := box.(core.MailFlagWriter)
		if !ok {
			return core.ErrUnsupported
		}
		updated, err = writer.SetMailFlag(ctx, current.ID, current.Revision, core.MailFlagRead, false)
	}
	if err != nil {
		return err
	}
	if updated.ID != current.ID || updated.Kind != core.Message || updated.Parent != folder ||
		updated.Properties[core.IsRead] != core.Bool(read) {
		return core.ErrOutcomeUnknown
	}
	return nil
}
