// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"time"

	"ewsgo/internal/core"
)

const maxLiveStoreHandles = 256
const maxContextHandleAllocations = 4096

const invalidObject uint32 = 0x80040108
const nullObject uint32 = 0x000004b9
const accessDenied uint32 = 0x80070005

type serverObject struct {
	logon        byte
	isLogon      bool
	spooler      bool
	binding      storeBinding
	public       *publicStore
	fid          uint64
	folderCopy   *folderCopyJob
	download     *hierarchyDownload
	table        *contentsTable
	message      *openedMessage
	attachment   *openedAttachment
	stream       *propertyStream
	notification *notificationSubscription
}

// Only an acquired, exclusively busy EMSMDB context may mutate its handles.
// Rebind/disconnect/expiry drops the context; no handle retains a native session.
type mailboxObjects struct {
	allocator           *ramStores
	objects             map[uint32]serverObject
	released            map[uint32]bool
	logons              map[byte]uint32
	origin              uint32 // First allocator handle; stable across this context's logons.
	allocated           int
	tablePollAfter      uint32
	notificationRefresh time.Time
	streamLockSession   streamLockSession
	perUser             perUserTransfers
}

func newMailboxObjects(allocator *ramStores) *mailboxObjects {
	objects := &mailboxObjects{allocator: allocator, objects: map[uint32]serverObject{}, released: map[uint32]bool{}, logons: map[byte]uint32{}}
	objects.streamLockSession.touch()
	return objects
}

func (s *mailboxObjects) addLogon(id byte, binding storeBinding) (uint32, error) {
	if binding.store == nil {
		return 0, core.ErrInvalid
	}
	return s.addStoreLogon(serverObject{logon: id, isLogon: true, binding: binding})
}

func (s *mailboxObjects) addStoreLogon(logon serverObject) (uint32, error) {
	id := logon.logon
	freed := 0
	for _, object := range s.objects {
		if object.logon == id {
			freed++
		}
	}
	if len(s.objects)-freed >= maxLiveStoreHandles || s.allocated >= maxContextHandleAllocations {
		return 0, core.ErrUnsupported
	}
	handle, err := s.allocator.handle()
	if err != nil {
		return 0, err
	}
	if previous := s.logons[id]; previous != 0 {
		s.release(previous)
	}
	if s.origin == 0 {
		s.origin = handle
	}
	s.objects[handle] = logon
	s.logons[id] = handle
	s.allocated++
	return handle, nil
}

func (s *mailboxObjects) object(handle uint32, logon byte) (serverObject, uint32) {
	object, code := s.lookup(handle, logon)
	if code == 0 && (object.table != nil || object.message != nil || object.attachment != nil || object.stream != nil || object.notification != nil) {
		return serverObject{}, NotSupported
	}
	return object, code
}

func (s *mailboxObjects) logonObject(handle uint32, logon byte) (serverObject, uint32) {
	object, code := s.lookup(handle, logon)
	if code == 0 && !object.isLogon {
		return serverObject{}, NotSupported
	}
	return object, code
}

// Typed consumers and Release use lookup. Folder/Logon consumers use object.
func (s *mailboxObjects) lookup(handle uint32, logon byte) (serverObject, uint32) {
	object, code := s.rawLookup(handle, logon)
	if code == 0 && object.public != nil {
		return serverObject{}, NotSupported
	}
	return object, code
}

func (s *mailboxObjects) rawLookup(handle uint32, logon byte) (serverObject, uint32) {
	object, exists := s.objects[handle]
	if !exists {
		if s.released[handle] {
			return serverObject{}, invalidObject
		}
		return serverObject{}, nullObject
	}
	if object.logon != logon {
		return serverObject{}, accessDenied
	}
	return object, 0
}

func (s *mailboxObjects) openFolder(ctx context.Context, session core.Session, logon byte, input uint32, fid uint64) (uint32, storeFolder, uint32) {
	object, code := s.object(input, logon)
	if code != 0 {
		return 0, storeFolder{}, code
	}
	if object.download != nil {
		return 0, storeFolder{}, NotSupported
	}
	folder, err := object.binding.store.folder(ctx, session, fid)
	if err != nil {
		return 0, storeFolder{}, storeError(err)
	}
	if len(s.objects) >= maxLiveStoreHandles || s.allocated >= maxContextHandleAllocations {
		return 0, storeFolder{}, 0x8007000e
	}
	handle, err := s.allocator.handle()
	if err != nil {
		return 0, storeFolder{}, 0x8007000e
	}
	s.objects[handle] = serverObject{logon: logon, binding: object.binding, fid: fid}
	s.allocated++
	return handle, folder, 0
}

func (s *mailboxObjects) release(handle uint32) {
	object, exists := s.objects[handle]
	if !exists {
		return
	}
	if object.isLogon {
		for id, child := range s.objects {
			if id != handle && child.logon == object.logon {
				s.discard(id, child)
			}
		}
		s.discard(handle, object)
		delete(s.logons, object.logon)
	} else {
		s.discard(handle, object)
	}
}

func (s *mailboxObjects) discard(handle uint32, object serverObject) {
	if object.isLogon {
		defer object.binding.releasePin()
		s.perUser.releaseLogon(object.logon)
		if object.spooler {
			object.binding.store.unlockSpoolerOwner(handle)
		}
	}
	if object.folderCopy != nil {
		object.folderCopy.lease.release()
	}
	object.table.close()
	object.stream.close()
	if object.attachment != nil {
		object.attachment.draft.close()
	}
	if object.message != nil {
		object.message.draft.close()
		object.message.mailDraft.close()
		object.message.contactDraft.close()
		object.message.appointmentDraft.close()
	}
	if object.download != nil {
		object.download.copyLease.release()
		object.download.collector.close()
		object.download.draft.close()
		object.download.faiUpload.releaseHandle()
		object.download.mailImport.close()
		if object.download.mailUpload != nil {
			object.download.mailUpload.released = true
		}
		if object.download.contents != nil {
			object.download.contents.close()
		}
		if object.download.consumesTransfer() {
			s.allocator.transfers.Add(-1)
		}
	}
	delete(s.objects, handle)
	s.released[handle] = true
}

// Called only while a context is exclusively owned or idle and being dropped.
func (s *mailboxObjects) close() {
	for _, handle := range s.logons {
		s.release(handle)
	}
	for handle, object := range s.objects {
		s.discard(handle, object)
	}
	clear(s.logons)
}

func storeError(err error) uint32 {
	switch {
	case errors.Is(err, core.ErrAccessDenied):
		return accessDenied
	case errors.Is(err, core.ErrNotFound):
		return 0x8004010f
	case errors.Is(err, core.ErrUnsupported):
		return NotSupported
	case errors.Is(err, core.ErrInvalid):
		return 0x80070057
	case errors.Is(err, core.ErrAuthentication):
		return 0x80040111
	default:
		return 0x80004005
	}
}
