// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"reflect"
	"slices"

	"ewsgo/internal/core"
)

const (
	tableChangedNotificationBytes          = 10
	tableRowNotificationHeaderBytes        = tableChangedNotificationBytes + 8 + 8 + 2
	tableChanged                    uint16 = 0x0001
	tableRowAdded                   uint16 = 0x0003
	tableRowModified                uint16 = 0x0005
)

func (t *contentsTable) updating() bool {
	return t != nil && t.permissions == nil && !t.inboxRules && t.attachmentMessage == nil && t.flags&(0x10|tableSoftDeletes) == 0
}

func (t *contentsTable) subscribe() {
	if t.updating() {
		t.subscribed = true
	}
}

// The context is exclusively acquired by executeStore. A refresh retains the
// old view and its lease until a complete, authorized new view is available.
// Failed reads never replace a view with an empty stand-in or consume pending
// notifications. Bookmarked rows are resolved again by identity.
func (t *SessionTransport) refreshTable(ctx context.Context, session core.Session, object serverObject) error {
	table := object.table
	if !table.updating() {
		return nil
	}
	var next *contentsTable
	var err error
	if table.hierarchy {
		next, err = captureHierarchyTable(ctx, session, object.binding, object.fid, table.flags, t.folderAggregates && slices.ContainsFunc(table.columns, isFolderAggregateTag))
	} else {
		next, err = captureContentsTable(ctx, session, object.binding, object.fid, table.flags)
	}
	if err != nil {
		return err
	}
	defer next.close()
	if next.sharedRights != table.sharedRights {
		return core.ErrAccessDenied
	}
	next.baseRows = next.rows
	if reflect.DeepEqual(table.baseRows, next.baseRows) {
		table.search = next.search
		table.hierarchyCreations = next.hierarchyCreations
		return nil
	}
	next.rows, err = restrictedTableRows(ctx, next.baseRows, table.restriction)
	if err != nil {
		return err
	}
	next.rows, next.instanceLease, err = prepareTableSort(ctx, object.binding.store, next.rows, table.sortKeys)
	if err != nil {
		return err
	}
	if table.categories != nil {
		current := table.categories
		next.categories, err = buildCategoryView(ctx, object.binding.store, current.scope, next.rows,
			table.sortKeys, current.depth, table.categoryExpanded)
		if err != nil {
			return err
		}
		for id, node := range next.categories.byID {
			if old := current.byID[id]; old != nil {
				node.setExpanded(old.expanded)
			}
		}
		next.categories.rebuild()
		next.rows = next.categories.visible
	}
	notify := table.subscribed && !table.suppressHierarchyCreations(next)
	if table.hierarchy && notify {
		if table.hierarchyChanges[0].kind == 0 {
			table.hierarchyChanges = table.hierarchyChange(table.rows, next.rows)
			if table.hierarchyChanges[0].kind == 0 {
				table.hierarchyChanges[0].kind = tableChanged
			}
		} else {
			table.invalidateHierarchyRowEvent()
		}
	} else if table.pending && table.hierarchy {
		// A silent change can still move the row of an earlier pending
		// external notification. Keep that notification with a full reload.
		table.invalidateHierarchyRowEvent()
	}
	// Swap the retained lease too: old+new peak allocation has already passed
	// the shared ledger. Deferred cleanup releases exactly the old snapshot.
	table.rows, next.rows = next.rows, table.rows
	table.baseRows, next.baseRows = next.baseRows, table.baseRows
	table.hierarchyCreations = next.hierarchyCreations
	table.lease, next.lease = next.lease, table.lease
	table.instanceLease, next.instanceLease = next.instanceLease, table.instanceLease
	table.categories, next.categories = next.categories, table.categories
	table.search = next.search
	table.cursor = 0
	table.pending = table.pending || notify
	return nil
}

type hierarchyRowChange struct {
	kind  uint16
	index int
}

// An insertion can also update the visible direct parent. A message move
// can update both folder rows without changing their positions.
func (t *contentsTable) hierarchyChange(before, after []contentsValues) (changes [2]hierarchyRowChange) {
	if len(t.columns) == 0 {
		return changes
	}
	switch len(after) - len(before) {
	case 1:
		index := 0
		for index < len(before) && t.bookmarkRow(before[index]) == t.bookmarkRow(after[index]) {
			index++
		}
		parent, _ := after[index][fixedContentsProperty(0x67490014)].(core.Integer)
		var updated hierarchyRowChange
		for i, old := range before {
			j := i
			if i >= index {
				j++
			}
			row := after[j]
			if t.sameHierarchyRow(old, row) {
				continue
			}
			if parent == 0 || t.bookmarkRow(old).id != parent ||
				t.bookmarkRow(old) != t.bookmarkRow(row) || updated.kind != 0 {
				return changes
			}
			updated = hierarchyRowChange{tableRowModified, j}
		}
		return [2]hierarchyRowChange{{tableRowAdded, index}, updated}
	case 0:
		count := 0
		for i, row := range after {
			if t.bookmarkRow(before[i]) != t.bookmarkRow(row) {
				return [2]hierarchyRowChange{}
			}
			if !t.sameHierarchyRow(before[i], row) {
				if count == len(changes) {
					return [2]hierarchyRowChange{}
				}
				changes[count] = hierarchyRowChange{tableRowModified, i}
				count++
			}
		}
	}
	return changes
}

func (t *contentsTable) sameHierarchyRow(a, b contentsValues) bool {
	if t.bookmarkRow(a) != t.bookmarkRow(b) {
		return false
	}
	for _, tag := range t.columns {
		if uint16(tag) == 0x001e {
			tag = tag&0xffff0000 | 0x001f // Hierarchy string cells are stored as Unicode.
		}
		property := fixedContentsProperty(tag)
		if !reflect.DeepEqual(a[property], b[property]) {
			return false
		}
	}
	return true
}

func (t *contentsTable) invalidateHierarchyRowEvent() {
	if t.hierarchyChanges[0].kind != 0 {
		t.hierarchyChanges = [2]hierarchyRowChange{{kind: tableChanged}}
	}
}

func (t *contentsTable) hierarchyRowPayload(store *ramStore, codePage uint32, available, index int) ([]byte, uint64, uint64) {
	if len(t.columns) == 0 || available <= tableRowNotificationHeaderBytes {
		return nil, 0, 0
	}
	fid := uint64(t.bookmarkRow(t.rows[index]).id)
	var previous uint64
	if index > 0 {
		previous = uint64(t.bookmarkRow(t.rows[index-1]).id)
		if previous == 0 {
			return nil, 0, 0
		}
	}
	if fid == 0 || fid == previous {
		return nil, 0, 0
	}
	lease, err := store.reserveContentsMemory(2 * maxTableRowBytes)
	if err != nil {
		return nil, 0, 0
	}
	defer lease.release()
	row := t.encodeRow(t.rows[index], &store.named, codePage)
	if len(row) > min(available-tableRowNotificationHeaderBytes, maxTableRowBytes) {
		return nil, 0, 0
	}
	return row, fid, previous
}

type tableNotificationKind uint8

const (
	noTableNotification tableNotificationKind = 0
)

const (
	hierarchyTableChanged tableNotificationKind = 1 << iota
	contentsTableChanged
	hierarchyTableRowAdded
	hierarchyTableRowModified
	tableNotificationPending
)

// Poll one subscribed view per successful Execute, including an empty polling
// request. Monotonic handle order gives bounded, fair work without goroutines,
// retained backend sessions, timers or an unbounded notification queue. Table
// navigation itself also refreshes its own view, so targeted reads are current.
func (t *SessionTransport) appendTableNotifications(ctx context.Context, session core.Session, objects *mailboxObjects, responses []byte, handles int, maxOutput, codePage uint32) ([]byte, tableNotificationKind) {
	available := min(65535, int(maxOutput)) - 10 - 4*handles - len(responses)
	if available < pendingNotificationBytes || ctx.Err() != nil {
		return responses, noTableNotification
	}
	var first, following, firstPending, nextPending uint32
	for handle, object := range objects.objects {
		if object.table == nil || !object.table.updating() || !object.table.subscribed {
			continue
		}
		if object.table.pending {
			if firstPending == 0 || handle < firstPending {
				firstPending = handle
			}
			if handle > objects.tablePollAfter && (nextPending == 0 || handle < nextPending) {
				nextPending = handle
			}
		}
		if first == 0 || handle < first {
			first = handle
		}
		if handle > objects.tablePollAfter && (following == 0 || handle < following) {
			following = handle
		}
	}
	handle := nextPending
	if handle == 0 {
		handle = firstPending
	}
	if handle == 0 {
		handle = following
	}
	if handle == 0 {
		handle = first
	}
	if handle == 0 {
		return responses, noTableNotification
	}
	objects.tablePollAfter = handle
	object := objects.objects[handle]
	if err := t.refreshTable(ctx, session, object); err != nil {
		// A failed priority refresh retains the pending bit, but should
		// not prevent an ordinary poll of the next subscribed view.
		next := following
		if next == 0 {
			next = first
		}
		if next == handle || ctx.Err() != nil {
			return responses, noTableNotification
		}
		handle = next
		objects.tablePollAfter = handle
		object = objects.objects[handle]
		if err := t.refreshTable(ctx, session, object); err != nil {
			return responses, noTableNotification
		}
	}
	if !object.table.pending {
		return responses, noTableNotification
	}
	if available < tableChangedNotificationBytes {
		return responses, tableNotificationPending
	}
	changes := object.table.hierarchyChanges
	object.table.hierarchyChanges = [2]hierarchyRowChange{}
	object.table.pending = false
	start := len(responses)
	var kinds tableNotificationKind
	for _, change := range changes {
		if change.kind == 0 {
			break
		}
		if change.kind == tableChanged {
			break
		}
		row, fid, previous := object.table.hierarchyRowPayload(object.binding.store, codePage, available-(len(responses)-start), change.index)
		if row == nil {
			// The exact changes describe one view. Never publish half of it.
			responses, kinds = responses[:start], 0
			break
		}
		responses = appendTableEvent(responses, handle, object.logon, change.kind)
		responses = binary.LittleEndian.AppendUint64(responses, fid)
		responses = binary.LittleEndian.AppendUint64(responses, previous)
		responses = binary.LittleEndian.AppendUint16(responses, uint16(len(row)))
		responses = append(responses, row...)
		if change.kind == tableRowModified {
			kinds |= hierarchyTableRowModified
		} else {
			kinds |= hierarchyTableRowAdded
		}
	}
	if kinds != 0 {
		return responses, kinds
	}
	responses = appendTableEvent(responses, handle, object.logon, tableChanged)
	if object.table.hierarchy {
		return responses, hierarchyTableChanged
	}
	return responses, contentsTableChanged
}

func appendTableEvent(responses []byte, handle uint32, logon byte, event uint16) []byte {
	// MS-OXCROPS 2.2.14.2: no input index or ReturnValue in RopNotify.
	responses = append32(append(responses, 0x2a), handle)
	responses = append(responses, logon)
	responses = binary.LittleEndian.AppendUint16(responses, 0x0100)
	return binary.LittleEndian.AppendUint16(responses, event)
}
