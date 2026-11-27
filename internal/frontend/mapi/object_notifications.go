// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"sort"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const (
	notifyNewMail         = 0x0002
	notifyCreated         = 0x0004
	notifyDeleted         = 0x0008
	notifyModified        = 0x0010
	notifyMoved           = 0x0020
	notifySearchCompleted = 0x0080
	notifyExtended        = 0x0400
	notifyHasTotal        = 0x1000
	notifyHasUnread       = 0x2000
	notifyMessage         = 0x8000
	nativeEventMask       = notifyCreated | notifyDeleted | notifyModified | notifyMoved
	objectEventMask       = nativeEventMask | notifySearchCompleted | notifyNewMail
	knownEventMask        = 0x05fe

	notificationPageSize        = 32
	notificationRefreshInterval = 5 * time.Second
)

// NotificationSource exposes the committed changes of the authenticated
// mailbox. Refresh reconciles native changes. Page only reads committed data.
// The opaque mailbox and generation identify the retained journal window.
type NotificationSource interface {
	Head(context.Context, core.Session) (NotificationPosition, error)
	Refresh(context.Context, core.Session) error
	Page(context.Context, core.Session, NotificationPosition, int) ([]NotificationEvent, NotificationPosition, error)
}

type NotificationPosition struct {
	Mailbox, Generation string
	Scope               core.Scope
	Revision            core.Revision
}

type NotificationEvent struct {
	core.Change
	ObjectKind core.Kind
}

// EnableObjectNotifications supplements implicit table subscriptions with the
// authenticated mailbox's change journal. Configure before serving requests.
func (t *SessionTransport) EnableObjectNotifications(source NotificationSource) {
	t.notificationSource = source
}

type notificationRequest struct {
	mask     uint16
	whole    bool
	fid, mid uint64
}

func parseNotificationRegistration(r *reader, op *Operation, handles int) error {
	header := r.take(3)
	if r.err != nil || int(header[0]) >= handles {
		return ErrMalformed
	}
	request := notificationRequest{mask: binary.LittleEndian.Uint16(header[1:])}
	if request.mask&notifyExtended != 0 {
		reserved := r.take(1)
		if r.err != nil || reserved[0] != 0 {
			return ErrMalformed
		}
	}
	whole := r.take(1)
	if r.err != nil {
		return r.err
	}
	request.whole = whole[0] != 0
	if !request.whole {
		ids := r.take(16)
		if r.err != nil {
			return r.err
		}
		request.fid = binary.LittleEndian.Uint64(ids)
		request.mid = binary.LittleEndian.Uint64(ids[8:])
	}
	op.Output, op.Notification = header[0], &request
	return nil
}

type notificationSubscription struct {
	position        NotificationPosition
	folder, message core.ID
	mask            uint16
	fid, mid        uint64
	localAfter      uint64
	deferred        uint64
	native          bool
}

type objectNotificationTarget struct {
	handle uint32
	logon  byte
	store  *ramStore
	sub    notificationSubscription
}

func (s *mailboxObjects) notificationTargets() []objectNotificationTarget {
	var targets []objectNotificationTarget
	for handle, object := range s.objects {
		if object.notification != nil {
			targets = append(targets, objectNotificationTarget{handle, object.logon, object.binding.store, *object.notification})
		}
	}
	sort.Slice(targets, func(i, j int) bool { return targets[i].handle < targets[j].handle })
	return targets
}

func (t *SessionTransport) registerObjectNotification(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	reply := func(code uint32) []byte { return append32([]byte{op.ID, op.Output}, code) }
	if op.Notification.mask&objectEventMask == 0 {
		return reply(NotSupported)
	}
	if op.Notification.mask & ^uint16(knownEventMask) != 0 {
		return reply(0x80070057)
	}
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return reply(code)
	}
	if object.table != nil || object.download != nil || object.notification != nil || object.stream != nil {
		return reply(NotSupported)
	}
	if len(objects.objects) >= maxLiveStoreHandles || objects.allocated >= maxContextHandleAllocations {
		return reply(0x8007000e)
	}
	sub, err := object.binding.store.notificationSelector(ctx, session, *op.Notification)
	if err != nil {
		return reply(storeError(err))
	}
	if sub.native {
		if t.notificationSource == nil {
			return reply(NotSupported)
		}
		sub.position, err = t.notificationSource.Head(ctx, session)
		if err != nil {
			return reply(storeError(err))
		}
		objects.notificationRefresh = time.Now()
	}
	handle, err := objects.allocator.handle()
	if err != nil {
		return reply(0x8007000e)
	}
	objects.objects[handle] = serverObject{logon: op.Logon, binding: object.binding, notification: &sub}
	objects.allocated++
	handles[op.Output] = handle
	return reply(0)
}

func (s *ramStore) notificationSelector(ctx context.Context, session core.Session, request notificationRequest) (notificationSubscription, error) {
	sub := notificationSubscription{mask: request.mask & objectEventMask, fid: request.fid, mid: request.mid}
	s.mu.Lock()
	sub.localAfter = s.localNotifications.head
	s.mu.Unlock()
	if session == nil || session.Mailbox() == nil || session.Identity().Scope != s.scope || session.Mailbox().Root() != s.root {
		return sub, core.ErrAccessDenied
	}
	if request.whole {
		sub.native = sub.mask&nativeEventMask != 0
		return sub, hierarchyOwner(ctx, session.Mailbox())
	}
	folder, err := s.folder(ctx, session, request.fid)
	if err != nil {
		return sub, err
	}
	sub.folder = folder.native
	sub.native = sub.folder != "" && sub.mask&nativeEventMask != 0
	if !sub.native {
		if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
			return sub, err
		}
	}
	if s.isDeferredActionFolder(request.fid) && sub.mask&nativeEventMask != 0 {
		if _, supported := session.Mailbox().(core.DeferredActionMailbox); supported {
			if _, err := s.observeDeferredActions(ctx, session, request.fid); err != nil {
				return sub, err
			}
			sub.deferred = request.fid
			s.mu.Lock()
			sub.localAfter = s.localNotifications.head
			s.mu.Unlock()
		}
	}
	if request.mid != 0 {
		s.mu.Lock()
		key, known := s.byMID[request.mid]
		version := s.messages[key]
		s.mu.Unlock()
		if !known || !version.present || version.parent != request.fid {
			return sub, core.ErrNotFound
		}
		sub.message = key.id
		sub.native = sub.native && !key.associated
	}
	return sub, nil
}

func (s notificationSubscription) matches(event NotificationEvent) bool {
	if s.mask&notificationFlag(event.Kind) == 0 {
		return false
	}
	if s.message != "" {
		return event.ObjectKind == core.Message && event.ID == s.message
	}
	return s.folder == "" || event.Parent == s.folder || event.PreviousParent == s.folder ||
		(event.ObjectKind == core.Folder && event.ID == s.folder)
}

// A message change affects its owning folder, and a new folder affects its
// parent. Deliver those changes to direct and whole-mailbox subscribers.
func (s notificationSubscription) affectedFolders(event NotificationEvent) [2]core.ID {
	var folders [2]core.ID
	if s.message != "" || s.mask&notifyModified == 0 {
		return folders
	}
	switch event.ObjectKind {
	case core.Message:
		switch event.Kind {
		case core.Created, core.Updated:
			folders[0] = event.Parent
		case core.Deleted:
			folders[0] = event.PreviousParent
		case core.Moved:
			folders[0] = event.Parent
			if event.PreviousParent != event.Parent {
				folders[1] = event.PreviousParent
			}
		}
	case core.Folder:
		if event.Kind == core.Created {
			folders[0] = event.Parent
		}
	}
	for i, id := range folders {
		if s.folder != "" && id != s.folder {
			folders[i] = ""
		}
	}
	return folders
}

func notificationFlag(kind core.ChangeKind) uint16 {
	switch kind {
	case core.Created:
		return notifyCreated
	case core.Deleted:
		return notifyDeleted
	case core.Updated:
		return notifyModified
	case core.Moved:
		return notifyMoved
	default:
		return 0
	}
}

type objectNotification struct {
	flag              uint16
	fid, mid, parent  uint64
	oldFID, oldParent uint64
	oldMID            uint64
	counts            folderCounters
	messageFlags      uint32
	messageClass      string
}

func (n objectNotification) appendTo(out []byte, target objectNotificationTarget) []byte {
	out = append32(append(out, 0x2a), target.handle)
	out = append(out, target.logon)
	flags := n.flag
	if n.flag == notifyModified {
		for i, bit := range [2]uint16{notifyHasTotal, notifyHasUnread} {
			if n.counts.present[i] {
				flags |= bit
			}
		}
	}
	out = binary.LittleEndian.AppendUint16(out, flags)
	out = binary.LittleEndian.AppendUint64(out, n.fid)
	if n.mid != 0 {
		out = binary.LittleEndian.AppendUint64(out, n.mid)
	} else if n.flag == notifyCreated || n.flag == notifyDeleted || n.flag == notifyMoved {
		out = binary.LittleEndian.AppendUint64(out, n.parent)
	}
	switch n.flag &^ notifyMessage {
	case notifyNewMail:
		out = append32(out, n.messageFlags)
		out = append(out, 0) // The transport supplies an ASCII message class.
		out = append(out, n.messageClass...)
		out = append(out, 0)
	case notifyMoved:
		out = binary.LittleEndian.AppendUint64(out, n.oldFID)
		if n.mid != 0 {
			oldMID := n.oldMID
			if oldMID == 0 {
				oldMID = n.mid
			}
			out = binary.LittleEndian.AppendUint64(out, oldMID)
		} else {
			out = binary.LittleEndian.AppendUint64(out, n.oldParent)
		}
	case notifyCreated, notifyModified:
		out = binary.LittleEndian.AppendUint16(out, 0)
	}
	for i, bit := range [2]uint16{notifyHasTotal, notifyHasUnread} {
		if flags&bit != 0 {
			out = append32(out, n.counts.values[i])
		}
	}
	return out
}

// Resolve journal objects through the existing MAPI view. A removed object
// may be addressed by an existing mapping, never by exposing an unseen
// special folder. A new MID reserves identity only. The contents snapshot
// still decides when the message and its revisions become visible to ICS.
func (s *ramStore) projectObjectNotification(ctx context.Context, event NotificationEvent, visible map[core.ID]hierarchyNode, allocate bool) (objectNotification, error) {
	n := objectNotification{flag: notificationFlag(event.Kind)}
	parent := event.Parent
	if event.Kind == core.Deleted {
		parent = event.PreviousParent
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	var allocationErr error
	folder := func(id core.ID, historical bool) (uint64, bool) {
		if id == "" {
			return 0, false
		}
		if _, ok := visible[id]; !ok && !historical {
			return 0, false
		}
		fid := s.byKey[folderKey{native: id}]
		if fid == 0 {
			if _, ok := visible[id]; !ok {
				return 0, false
			}
			if !allocate {
				return 1, true
			}
			if s.mappingCountLocked() >= s.mappingLimit() || s.next >= maxGlobalCount {
				allocationErr = core.ErrLimit
				return 0, false
			}
			next := s.next + 1
			fid = folderID(next)
			key := folderKey{native: id}
			if s.durable.store != nil {
				binding, err := persistedFolderBinding(key, fid)
				if err == nil {
					err = s.commitReplica(ctx, mapireplica.Mutation{HighWater: next, Bindings: []mapireplica.Binding{binding}})
				}
				if err != nil {
					allocationErr = err
					return 0, false
				}
			}
			s.next = next
			s.byKey[key], s.byFID[fid] = fid, key
		}
		return fid, true
	}
	var ok bool
	switch event.ObjectKind {
	case core.Folder:
		n.fid, ok = folder(event.ID, event.Kind == core.Deleted)
		if !ok {
			break
		}
		if event.Kind == core.Updated {
			n.counts = visible[event.ID].counts
		} else {
			n.parent, ok = folder(parent, event.Kind == core.Deleted)
		}
		if ok && event.Kind == core.Moved {
			n.oldFID = n.fid
			n.oldParent, ok = folder(event.PreviousParent, true)
		}
	case core.Message:
		if placement := s.outboxDrafts[event.ID]; placement.folder != "" && placement.folder == parent &&
			(event.Kind == core.Created || event.Kind == core.Updated) {
			// These saves are visible under the Outbox FID via the local journal.
			break
		}
		node, shown := visible[parent]
		if shown && node.containerClass != "" && node.containerClass != "IPF.Note" {
			break
		}
		n.fid, ok = folder(parent, event.Kind == core.Deleted)
		if !ok {
			break
		}
		if event.Kind == core.Moved {
			n.oldFID, ok = folder(event.PreviousParent, true)
		}
		if ok {
			if event.Kind == core.Moved {
				for _, move := range s.messageMoves {
					if move.NativeID != event.ID || folderID(move.From) != n.oldFID || folderID(move.To) != n.fid {
						continue
					}
					if move.Result == 0 {
						return objectNotification{}, core.ErrOutcomeUnknown
					}
					if n.oldMID != 0 {
						// The native journal has no MAPI MID or replica change number.
						// An ambiguous historical move can only invalidate the table.
						return objectNotification{}, core.ErrNotFound
					}
					n.mid, n.oldMID = folderID(move.Destination), folderID(move.Source)
				}
			}
			if s.byMessage[messageKey{id: event.ID}] == 0 {
				for _, p := range s.pendingMessageImports {
					if (p.NativeID == "" || p.NativeID == event.ID) && folderID(p.Parent) == n.fid {
						return objectNotification{}, core.ErrOutcomeUnknown
					}
				}
				for _, draft := range s.pendingMailDrafts {
					if draft.dispatched && (draft.fid == n.fid || draft.fid == n.oldFID ||
						draft.folder != "" && (draft.folder == parent || draft.folder == event.PreviousParent)) {
						// The native row may have committed without a reply.
						// Keep the journal position until the MID is resolved.
						return objectNotification{}, core.ErrOutcomeUnknown
					}
				}
			}
			if n.mid == 0 {
				n.mid, allocationErr = s.nativeMessageMIDLocked(ctx, event.ID, allocate)
			}
		}
		n.flag |= notifyMessage
	}
	if allocationErr != nil {
		return objectNotification{}, allocationErr
	}
	if !ok {
		return objectNotification{}, core.ErrNotFound
	}
	return n, nil
}

func (s *ramStore) nativeMessageMIDLocked(ctx context.Context, id core.ID, allocate bool) (uint64, error) {
	if len(id) == 0 || len(id) > maxHierarchyText {
		return 0, core.ErrNotFound
	}
	key := messageKey{id: id}
	if mid := s.byMessage[key]; mid != 0 {
		return mid, nil
	}
	if !allocate {
		return 1, nil
	}
	budget := 192 + len(id) + s.messageRegistryBytesLocked()
	if s.mappingCountLocked() >= s.mappingLimit() || s.next >= maxGlobalCount || budget > s.messageRegistryLimit() {
		return 0, core.ErrLimit
	}
	next := s.next + 1
	mid := folderID(next)
	if s.durable.store != nil {
		binding, err := persistedMessageBinding(key, mid)
		if err != nil {
			return 0, err
		}
		if err := s.commitReplica(ctx, mapireplica.Mutation{HighWater: next, Bindings: []mapireplica.Binding{binding}}); err != nil {
			return 0, err
		}
	}
	if s.byMessage == nil {
		s.byMessage, s.byMID = make(map[messageKey]uint64), make(map[uint64]messageKey)
	}
	s.next = next
	s.byMessage[key], s.byMID[mid] = mid, key
	return mid, nil
}

type notificationScan struct {
	output     []byte
	next       NotificationPosition
	localAfter uint64
	emitted    uint16
	pending    bool
	more       bool
}

func (s *notificationScan) emit(n objectNotification, target objectNotificationTarget) {
	s.output = n.appendTo(s.output, target)
	flag := n.flag &^ notifyMessage
	if n.flag&notifyMessage != 0 {
		flag <<= 8
	}
	s.emitted |= flag
}

func (s *notificationSubscription) advance(result notificationScan) {
	s.position, s.localAfter = result.next, result.localAfter
}

// Scan bounded pages. The cursor stops before any matched event that cannot
// fit the response. A zero output budget is a readiness probe and never
// allocates MAPI IDs or consumes a deliverable event.
func (t *SessionTransport) scanObjectNotifications(ctx context.Context, session core.Session, target objectNotificationTarget, available int, native bool) (notificationScan, error) {
	result := notificationScan{next: target.sub.position, localAfter: target.sub.localAfter}
	if err := target.store.scanLocalNotifications(ctx, session, target, available, &result); err != nil || result.pending || !native || !target.sub.native {
		return result, err
	}
	events, next, err := t.notificationSource.Page(ctx, session, target.sub.position, notificationPageSize)
	if err != nil {
		return result, err
	}
	var visible map[core.ID]hierarchyNode
	for _, event := range events {
		matches := target.sub.matches(event)
		folders := target.sub.affectedFolders(event)
		if matches || folders[0] != "" || folders[1] != "" {
			if visible == nil {
				nodes, err := target.store.currentProjection(ctx, session)
				if err != nil {
					return result, err
				}
				visible = make(map[core.ID]hierarchyNode, len(nodes))
				for _, node := range nodes {
					if node.key.native != "" {
						visible[node.key.native] = node
					}
				}
			}
			var candidates [3]NotificationEvent
			selected := candidates[:0]
			if matches {
				selected = append(selected, event)
			}
			for _, id := range folders {
				node, shown := visible[id]
				if id == "" || !shown {
					continue
				}
				if event.ObjectKind == core.Message && node.containerClass != "" && node.containerClass != "IPF.Note" {
					continue
				}
				if event.ObjectKind == core.Folder && visible[event.ID].parent != node.key {
					continue
				}
				selected = append(selected, NotificationEvent{
					Change: core.Change{Kind: core.Updated, ID: id}, ObjectKind: core.Folder,
				})
			}
			pending, err := t.appendJournalEvent(ctx, target, &result, selected, visible, available)
			if err != nil {
				return result, err
			}
			if pending {
				result.pending = true
				return result, nil
			}
		}
		result.next.Revision = event.Revision
	}
	result.next = next
	result.more = result.more || len(events) == notificationPageSize
	return result, nil
}

// Keep one journal revision pending until its complete notification batch fits.
func (t *SessionTransport) appendJournalEvent(ctx context.Context, target objectNotificationTarget, result *notificationScan,
	events []NotificationEvent, visible map[core.ID]hierarchyNode, available int,
) (bool, error) {
	var valid [3]NotificationEvent
	deliver := valid[:0]
	size := len(result.output)
	for _, event := range events {
		n, err := target.store.projectObjectNotification(ctx, event, visible, false)
		switch {
		case errors.Is(err, core.ErrNotFound):
			continue
		case errors.Is(err, core.ErrOutcomeUnknown):
			return true, nil
		case err != nil:
			return false, err
		}
		size += len(n.appendTo(nil, target))
		deliver = append(deliver, event)
	}
	if len(deliver) != 0 && size > available {
		return true, nil
	}
	var projected [3]objectNotification
	for i, event := range deliver {
		n, err := target.store.projectObjectNotification(ctx, event, visible, true)
		if errors.Is(err, core.ErrOutcomeUnknown) {
			return true, nil
		}
		if err != nil {
			return false, err
		}
		projected[i] = n
	}
	for _, n := range projected[:len(deliver)] {
		result.emit(n, target)
	}
	return false, nil
}

const pendingNotificationBytes = 3

func (t *SessionTransport) appendObjectNotifications(ctx context.Context, session core.Session, objects *mailboxObjects, out []byte, handles int, maximum uint32) ([]byte, uint16, bool) {
	targets := objects.notificationTargets()
	available := int(maximum) - 10 - 4*handles
	if len(targets) == 0 || available-len(out) < pendingNotificationBytes || ctx.Err() != nil {
		return out, 0, false
	}
	scan, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	native := true
	hasNative := hasNativeNotificationTarget(targets)
	deferred := deferredActionNotificationTarget(targets)
	if (hasNative || deferred != nil) && time.Since(objects.notificationRefresh) >= notificationRefreshInterval {
		objects.notificationRefresh = time.Now()
		if hasNative {
			native = t.notificationSource.Refresh(scan, session) == nil
		}
		if deferred != nil {
			_, _ = deferred.store.observeDeferredActions(scan, session, deferred.sub.deferred)
		}
	}
	pending := false
	var emitted uint16
	for _, target := range targets {
		result, err := t.scanObjectNotifications(scan, session, target, max(0, available-len(out)), native)
		out = append(out, result.output...)
		emitted |= result.emitted
		objects.objects[target.handle].notification.advance(result)
		if err != nil {
			continue
		}
		pending = pending || result.pending
	}
	return out, emitted, pending
}

func hasNativeNotificationTarget(targets []objectNotificationTarget) bool {
	for _, target := range targets {
		if target.sub.native {
			return true
		}
	}
	return false
}
