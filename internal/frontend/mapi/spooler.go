// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"

	"ewsgo/internal/core"
)

type transportNewMailRequest struct {
	fid, mid uint64
	class    string
	flags    uint32
}

func (t *SessionTransport) executeSpooler(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	reply := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	handle := handles[op.Input]
	logon, code := objects.logonObject(handle, op.Logon)
	if code != 0 {
		return reply(code)
	}
	store := logon.binding.store
	if session == nil || session.Identity().Scope != store.scope || session.Mailbox() == nil || session.Mailbox().Root() != store.root {
		return reply(accessDenied)
	}
	if op.ID == ropSetSpooler {
		if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
			return reply(storeError(err))
		}
		if err := t.recoverSpoolerDispatches(ctx, session, store, 0, 0); err != nil {
			return reply(storeError(err))
		}
		logon.spooler = true
		objects.objects[handle] = logon
		return reply(0)
	}
	if !logon.spooler {
		return reply(accessDenied)
	}
	return reply(store.publishTransportNewMail(ctx, session, op.TransportNewMail))
}

func (s *ramStore) publishTransportNewMail(ctx context.Context, session core.Session, request transportNewMailRequest) uint32 {
	if request.class == "" || !validReceiveClass([]byte(request.class)) {
		return 0x80070057
	}
	key, folder, err := s.openedMessageScope(ctx, session, request.fid, openedMessage{mid: request.mid})
	if err != nil {
		return storeError(err)
	}
	if key.associated || key.special != 0 || folder.native == "" || folder.role == Outbox {
		return PropertyNotFound
	}
	reader, ok := session.Mailbox().(core.MailMessageEnvelopeReader)
	if !ok {
		return NotSupported
	}
	message, err := reader.ReadMailMessageEnvelope(ctx, folder.native, key.id, false)
	if err != nil {
		return storeError(err)
	}
	if message.Kind != core.Message || message.ID != key.id || message.Parent != folder.native {
		return PropertyNotFound
	}
	if err := ctx.Err(); err != nil {
		return storeError(err)
	}
	s.mu.Lock()
	s.localNotifications.publish(objectNotification{
		flag: notifyNewMail | notifyMessage, fid: request.fid, mid: request.mid,
		messageFlags: request.flags, messageClass: request.class,
	})
	s.mu.Unlock()
	return 0
}
