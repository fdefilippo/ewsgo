// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"sort"
	"time"
	"unicode/utf8"

	"ewsgo/internal/core"
)

const maxTableRowBytes = 1 + maxTableColumns*515
const tableBufferTooSmall uint32 = 0x47d

type contentsTable struct {
	attachmentMessage  *openedMessage
	search             *searchTableScope
	hierarchyChanges   [2]hierarchyRowChange
	hierarchyCreations map[uint64]hierarchyCreationOrigin
	notificationOrigin uint32

	associated       bool
	special          core.SpecialFolderKind
	sharedRights     core.AccessRights
	inboxRules       bool
	permissions      *permissionsSnapshot
	flags            byte
	subscribed       bool
	pending          bool
	hierarchy        bool
	rows             []contentsValues
	baseRows         []contentsValues // Complete capture; filtering never destroys the source view.
	columns          []uint32
	cursor           int
	lease            *contentsLease
	sortKeys         []boundTableSort
	sortLease        *contentsLease
	instanceLease    *contentsLease
	categories       *categoryView
	categorySort     [32]byte
	categoryExpanded int
	restriction      *contentsRestriction
	restrictionID    [32]byte
	filterLease      *contentsLease
	bookmarks        map[uint32]tableBookmark
	bookmarkSeq      uint32
	bookmarkMem      *contentsLease
}

func (*contentsTable) String() string   { return "contentsTable{private}" }
func (*contentsTable) GoString() string { return "contentsTable{private}" }
func (t *contentsTable) close() {
	if t == nil {
		return
	}
	t.clearBookmarks()
	t.replaceCategories(nil)
	t.rows, t.columns, t.permissions = nil, nil, nil
	t.search = nil
	t.baseRows, t.restriction, t.attachmentMessage = nil, nil, nil
	t.hierarchyCreations = nil
	t.filterLease.release()
	t.filterLease = nil
	t.sortKeys = nil
	t.sortLease.release()
	t.sortLease = nil
	t.instanceLease.release()
	t.instanceLease = nil
	t.lease.release()
	t.lease = nil
}
func (t *SessionTransport) EnableContentsTableSpike() { t.contentsTableSpike = true }

// Capture a detached, bounded view. NoNotifications keeps it fixed until
// reopen; updating Tables replace it with fresh captures before navigation.
// Each use revalidates current ownership and folder existence.
// It neither publishes an ICS checkpoint nor creates an empty stand-in source.
func captureContentsTable(ctx context.Context, session core.Session, binding storeBinding, fid uint64, flags byte) (*contentsTable, error) {
	store := binding.store
	rights, err := store.sharedContentRights(ctx, session, fid)
	if err != nil {
		return nil, err
	}
	if flags&2 == 0 && store.isSearchFolder(fid) {
		return captureSearchTable(ctx, session, binding, fid, flags)
	}
	table := &contentsTable{associated: flags&2 != 0, flags: flags, sharedRights: rights}
	fail := func(err error) (*contentsTable, error) { table.close(); return nil, err }
	var snapshot messageSnapshot
	if table.associated {
		observation, err := store.observeFAIMessages(ctx, session, fid)
		if err != nil {
			return fail(err)
		}
		defer observation.close()
		snapshot = observation.snapshot
	} else {
		lease, err := store.reserveContentsPeak(ctx, 2*core.MaxMailContentViewBytes)
		if err != nil {
			return fail(err)
		}
		defer lease.release()
		switch {
		case store.isSpoolerQueue(fid):
			snapshot, err = store.observeSpoolerQueue(ctx, session, fid)
		case store.isDeferredActionFolder(fid):
			snapshot, err = store.observeDeferredActions(ctx, session, fid)
		default:
			snapshot, err = store.observeMailMessages(ctx, session, fid)
			if _, hasReader := session.Mailbox().(core.ContactReader); hasReader && errors.Is(err, normalMetadataOnlyError{}) {
				contacts, contactErr := store.observeContacts(ctx, session, fid)
				if !errors.Is(contactErr, core.ErrNotFound) {
					table.special, snapshot, err = core.ContactsFolder, contacts, contactErr
				}
			}
			if _, hasReader := session.Mailbox().(core.AppointmentContentsReader); hasReader && errors.Is(err, normalMetadataOnlyError{}) {
				calendar, calendarErr := store.observeCalendarSearchItems(ctx, session, fid, core.CalendarFolder)
				if !errors.Is(calendarErr, core.ErrNotFound) {
					table.special, snapshot, err = core.CalendarFolder, calendar, calendarErr
				}
			}
		}
		if err != nil {
			return fail(err)
		}
		if err = lease.shrink(snapshot.retainedBytes); err != nil {
			return fail(err)
		}
	}
	if snapshot.replica != store.replica || snapshot.root != fid {
		return fail(core.ErrResync)
	}
	table.lease, err = store.reserveContentsPeak(ctx, 2*maxMailProjectionBytes)
	if err != nil {
		return fail(err)
	}
	// Account map/row/slice overhead and the maximum retained column list.
	used := 1024 + 4*maxTableColumns
	if err = table.appendMessageRows(ctx, binding, snapshot, nil, &used); err != nil {
		return fail(err)
	}
	if _, err = store.scopedMessageAnchor(ctx, session, fid, table.associated, table.special); err != nil {
		return fail(err)
	}
	if err = store.checkSharedContentRights(ctx, session, fid, rights); err != nil {
		return fail(err)
	}
	if err = table.lease.shrink(used); err != nil {
		return fail(err)
	}
	return table, nil
}

func (table *contentsTable) appendMessageRows(ctx context.Context, binding storeBinding, snapshot messageSnapshot, restriction *contentsRestriction, used *int) error {
	store := binding.store
	sort.Slice(snapshot.messages, func(i, j int) bool { return snapshot.messages[i].mid < snapshot.messages[j].mid })
	for _, row := range snapshot.messages {
		if err := ctx.Err(); err != nil {
			return err
		}
		var projected projectedMessage
		if isDeferredAction(row) {
			projected.values = make(contentsValues, len(damTags)+10)
			for _, tag := range damTags {
				value := deferredActionProperty(row, binding, tag)
				putContentsValue(projected.values, tag, value)
				if data, ok := value.(core.Binary); ok {
					projected.bytes += len(data)
				}
			}
			putContentsValue(projected.values, 0x001a001f, core.Text(damClass))
			putContentsValue(projected.values, 0x0e070003, core.Integer(0))
			projected.bytes += 512
			addContentsRowMetadata(&projected, row)
		} else {
			change, err := store.projectContentsRow(row)
			if err != nil {
				return err
			}
			projected = change.payload
		}
		values := projected.values
		if categories, ok := row.object.Properties[core.MailCategories].(core.Strings); ok && len(categories) != 0 && row.special == 0 && !row.associated {
			values[keywordsValuesProperty] = categories
			*used += 64
			for _, category := range categories {
				*used += 16 + len(category)
			}
		}
		match, err := restriction.matchesInFolder(values, row.parent)
		if err != nil {
			return err
		}
		if !match {
			continue
		}
		*used += projected.bytes + 1032 // Includes a second bounded row-pointer view.
		if table.search != nil || !table.associated {
			*used += 384 // Two EntryIDs and a row ServerId with their property values.
			putContentsValue(values, 0x0fff0102, core.Binary(marshalMessageEntryID(binding.mailboxGUID, store.replica, row.parent, row.mid)))
			putContentsValue(values, 0x0e090102, core.Binary(marshalFolderEntryID(binding.mailboxGUID, store.replica, row.parent)))
			// MS-OXCDATA 2.11.1.4: a contents row identifies its real parent
			// and message. The base row has instance zero.
			serverID := binary.LittleEndian.AppendUint64([]byte{1}, row.parent)
			serverID = binary.LittleEndian.AppendUint64(serverID, row.mid)
			putContentsValue(values, instanceServerIDTag, core.Binary(append32(serverID, 0)))
		}
		if *used > maxMailProjectionBytes {
			return core.ErrUnsupported
		}
		putContentsValue(values, 0x67480014, core.Integer(row.parent))
		putContentsValue(values, 0x674a0014, core.Integer(row.mid))
		putTableRowIdentity(values, row.mid)
		table.rows = append(table.rows, values)
	}
	return nil
}

func (t *SessionTransport) openContentsTable(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32, diagnostic *ExecutionDiagnostic) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Output, code) }
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		if diagnostic != nil && code == NotSupported {
			diagnostic.tableProfiles |= contentsTableWrongType
		}
		return fail(code)
	}
	if object.isLogon || object.download != nil {
		if diagnostic != nil {
			diagnostic.tableProfiles |= contentsTableWrongType
		}
		return fail(NotSupported)
	}
	if len(objects.objects) >= maxLiveStoreHandles || objects.allocated >= maxContextHandleAllocations {
		return fail(0x8007000e)
	}
	var table *contentsTable
	var err error
	if op.ID == ropGetRulesTable {
		if op.Table.flags & ^byte(0x40) != 0 {
			return fail(0x80070057)
		}
		table, err = captureRulesTable(ctx, session, object.binding, object.fid, op.Table.flags)
		if errors.Is(err, core.ErrUnsupported) {
			return fail(ropNotImplemented)
		}
	} else if op.ID == 0x3e {
		table, err = t.capturePermissionsTable(ctx, session, object.binding.store, object.fid, op.Table.flags)
	} else if op.ID == 0x04 {
		table, err = captureHierarchyTable(ctx, session, object.binding, object.fid, op.Table.flags, false)
	} else if op.Table.flags&tableSoftDeletes != 0 {
		table, err = captureRecoverableTable(ctx, session, object.binding, object.fid, op.Table.flags)
	} else {
		table, err = captureContentsTable(ctx, session, object.binding, object.fid, op.Table.flags)
	}
	if err != nil {
		if diagnostic != nil {
			switch {
			case errors.Is(err, contentsBudgetError{}):
				diagnostic.tableProfiles |= contentsTableBudget
			case errors.Is(err, normalMetadataOnlyError{}):
				diagnostic.tableProfiles |= contentsTableMetadataOnly
			case op.Table.flags&tableSoftDeletes != 0:
				diagnostic.tableProfiles |= contentsTableRecoverable
			case op.Table.flags&2 != 0:
				diagnostic.tableProfiles |= contentsTableAssociated
			case object.binding.store.isSearchFolder(object.fid):
				diagnostic.tableProfiles |= contentsTableSearch
			default:
				diagnostic.tableProfiles |= contentsTableNormal
			}
		}
		return fail(storeError(err))
	}
	table.baseRows = table.rows
	table.notificationOrigin = objects.origin
	handle, err := objects.allocator.handle()
	if err != nil {
		table.close()
		return fail(0x8007000e)
	}
	objects.objects[handle] = serverObject{logon: op.Logon, binding: object.binding, fid: object.fid, table: table}
	objects.allocated++
	handles[op.Output] = handle
	if op.ID == 0x3e || op.ID == ropGetRulesTable {
		return ropError(op.ID, op.Output, 0)
	}
	return append32(ropError(op.ID, op.Output, 0), uint32(len(table.rows)))
}

// The caller owns a live Table, and keeps the returned response lease through
// final ROP framing. Cursor/columns change only after reauthorization succeeds.
func (t *SessionTransport) executeTable(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32, codePage uint32, diagnostic *ExecutionDiagnostic) ([]byte, *contentsLease) {
	fail := func(code uint32) ([]byte, *contentsLease) { return ropError(op.ID, op.Input, code), nil }
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code)
	}
	table := object.table
	if table == nil {
		return fail(NotSupported)
	}
	if err := t.authorizeTable(ctx, session, object, op.ID); err != nil {
		return fail(table.authorizationError(err))
	}
	if table.attachmentMessage != nil {
		codePage = table.attachmentMessage.codePage
	}
	switch op.ID {
	case 0x13, 0x14, 0x15, 0x17, 0x18, 0x19, 0x1a, 0x1b, 0x4f:
		if table.updating() {
			if err := t.refreshTable(ctx, session, object); err != nil {
				return fail(storeError(err))
			}
		}
	case 0x59, 0x5a, 0x6b, 0x6c:
		if table.categories != nil && table.columns != nil && table.updating() {
			if err := t.refreshTable(ctx, session, object); err != nil {
				return fail(storeError(err))
			}
		}
	}
	switch op.ID {
	case 0x37:
		return table.queryColumnsAll(ctx, &object.binding.store.named, op), nil
	case 0x13:
		table.invalidateHierarchyRowEvent()
		return t.sortContentsTable(ctx, session, object, op), nil
	case 0x14:
		table.invalidateHierarchyRowEvent()
		return t.restrictTable(ctx, session, object, op), nil
	case 0x4f:
		return t.findTableRow(ctx, session, object, handles[op.Input], op, codePage)
	case 0x12:
		if property, expanded := tableInstanceKey(table.sortKeys); expanded &&
			!compatibleInstanceColumns(op.Table.columns, &object.binding.store.named, property) {
			return ropError(op.ID, op.Input, NotSupported), nil
		}
		table.invalidateHierarchyRowEvent()
		table.columns = append([]uint32(nil), op.Table.columns...)
		return append(ropError(op.ID, op.Input, 0), 0), nil // TBLSTAT_COMPLETE
	case 0x16:
		return append(ropError(op.ID, op.Input, 0), 0), nil
	case 0x17:
		table.subscribe()
		return append32(append32(ropError(op.ID, op.Input, 0), uint32(table.cursor)), uint32(len(table.rows))), nil
	case 0x18:
		start := table.cursor
		if op.Table.origin == 0 {
			start = 0
		} else if op.Table.origin == 2 {
			start = len(table.rows)
		}
		less, moved := table.seekFrom(start, op.Table.seek)
		return append32(append(ropError(op.ID, op.Input, 0), less), uint32(moved)), nil
	case 0x19, 0x1b, 0x89:
		return table.executeBookmark(object.binding.store, handles[op.Input], op), nil
	case 0x1a:
		if op.Table.denominator == 0 {
			return fail(invalidTableBookmark)
		}
		numerator := min(op.Table.numerator, op.Table.denominator)
		table.cursor = int(uint64(numerator) * uint64(len(table.rows)) / uint64(op.Table.denominator))
		table.subscribe()
		return ropError(op.ID, op.Input, 0), nil
	case 0x38:
		return fail(0x80040114) // ecUnableToAbort: table operations are synchronous.
	case 0x59, 0x5a, 0x6b, 0x6c:
		if table.hierarchy || table.permissions != nil || table.attachmentMessage != nil {
			return fail(NotSupported)
		}
		if table.columns == nil {
			return fail(nullObject)
		}
		if table.categories != nil {
			return table.executeCategory(ctx, object.binding.store, handles[op.Input], op, codePage)
		}
		if op.ID == 0x59 || op.ID == 0x5a {
			return fail(0x8004010f) // ecNotFound: a flat view has no category rows.
		}
		// A collapse state requires a view that can represent and restore it.
		return fail(NotSupported)
	case 0x81:
		table.invalidateHierarchyRowEvent()
		table.reset()
		return ropError(op.ID, op.Input, 0), nil
	case 0x15:
		if table.columns == nil {
			return fail(nullObject)
		}
		// Account response copies and the next row encoder's bounded temporaries.
		lease, err := object.binding.store.reserveContentsMemory(4*op.Table.limit + 2*maxTableRowBytes)
		if err != nil {
			return fail(storeError(err))
		}
		var missing uint64
		var inspectMissing *uint64
		var queryDiagnostic *tableQueryDiagnostic
		if diagnostic != nil {
			switch {
			case table.attachmentMessage != nil:
				queryDiagnostic = &diagnostic.attachmentTable
			case !table.hierarchy && table.flags&tableSoftDeletes != 0:
				queryDiagnostic = &diagnostic.recoverableTable.tableQueryDiagnostic
			}
		}
		if queryDiagnostic != nil {
			inspectMissing = &missing
		}
		response, next, code := table.query(object.binding.store, op, codePage, op.Table.limit, inspectMissing)
		if code != 0 {
			lease.release()
			return fail(code)
		}
		if err = t.authorizeTable(ctx, session, object, op.ID); err != nil {
			lease.release()
			return fail(table.authorizationError(err))
		}
		if op.Table.flags&1 == 0 {
			table.cursor = next
		}
		if inspectMissing != nil {
			queryDiagnostic.recordQuery(binary.LittleEndian.Uint16(response[7:]), table.columns, missing)
		}
		table.subscribe()
		return response, lease
	}
	return fail(NotSupported)
}

func (t *contentsTable) query(store *ramStore, op Operation, codePage uint32, limit int, missingColumns *uint64) ([]byte, int, uint32) {
	rows := make([][]byte, 0, min(int(op.Table.count), len(t.rows)))
	used, next := 9, t.cursor
	forward := op.Table.direction == 1
	for len(rows) < int(op.Table.count) {
		index := next
		if !forward {
			index--
		}
		if index < 0 || index >= len(t.rows) {
			break
		}
		values := t.rowCells(t.rows[index], &store.named, codePage)
		row := folderPropertyRow(specificProperties{tags: t.columns}, values)
		if len(row) > maxTableRowBytes {
			return nil, t.cursor, NotSupported
		}
		if used+len(row) > limit {
			if len(rows) == 0 {
				return nil, t.cursor, tableBufferTooSmall
			}
			break
		}
		used += len(row)
		rows = append(rows, row)
		if missingColumns != nil {
			for i, value := range values {
				if value.kind == 0xa {
					*missingColumns |= uint64(1) << i
				}
			}
		}
		if forward {
			next++
		} else {
			next--
		}
	}
	origin := byte(1)
	if forward && next == len(t.rows) {
		origin = 2
	} else if !forward && next == 0 {
		origin = 0
	}
	out := binary.LittleEndian.AppendUint16(append(ropError(op.ID, op.Input, 0), origin), uint16(len(rows)))
	if forward {
		for _, row := range rows {
			out = append(out, row...)
		}
	} else {
		for i := len(rows) - 1; i >= 0; i-- {
			out = append(out, rows[i]...)
		}
	}
	return out, next, 0
}

// MS-OXCTABL limits each variable property to 510 bytes. Binary lengths are
// uint16 here, unlike FastTransfer; Boolean is one byte. Unknown/missing cells
// are errors, never values inferred from another property or an invented ID.
func tableCell(values contentsValues, tag uint32, names *namedPropertyRegistry, codePage uint32) folderPropertyValue {
	fail := func(code uint32) folderPropertyValue { return folderPropertyValue{kind: 0xa, code: code} }
	property, err := resolveContentsIdentity(tag, names)
	if err != nil {
		return fail(0x8004010f)
	}
	kind := property.kind
	lookup := property
	if kind == 0x1e {
		lookup.kind = 0x1f
	} else if kind == 0x301e {
		lookup.kind = 0x301f
	}
	if kind&0x3000 == 0x3000 {
		kind &^= 0x3000
	}
	value, ok := values[lookup]
	if !ok {
		return fail(0x8004010f)
	}
	out := folderPropertyValue{kind: kind}
	switch v := value.(type) {
	case core.Integer:
		switch kind {
		case 2:
			out.data = binary.LittleEndian.AppendUint16(nil, uint16(v))
		case 3:
			out.data = append32(nil, uint32(v))
		case 0x14:
			out.data = binary.LittleEndian.AppendUint64(nil, uint64(v))
		default:
			return fail(NotSupported)
		}
	case core.Bool:
		if kind != 0xb {
			return fail(NotSupported)
		}
		out.data = []byte{0}
		if v {
			out.data[0] = 1
		}
	case core.Timestamp:
		if kind != 0x40 {
			return fail(NotSupported)
		}
		out.data = binary.LittleEndian.AppendUint64(nil, fileTime(time.Time(v)))
	case core.Int32s:
		if kind != 0x1003 || len(v) > 126 {
			return fail(NotSupported)
		}
		out.data = encodeInt32s(v)
	case core.Text:
		if kind != 0x1e && kind != 0x1f {
			return fail(NotSupported)
		}
		// Avoid allocating an entire large body before truncating its table view.
		text := string(v)
		if len(text) > 2048 {
			text = text[:2048]
			for !utf8.ValidString(text) {
				text = text[:len(text)-1]
			}
		}
		data, ok := propertyString(text, kind, codePage)
		if !ok {
			return fail(NotSupported)
		}
		if len(data) > 510 {
			if kind == 0x1f {
				end := 508
				unit := binary.LittleEndian.Uint16(data[end-2:])
				if unit >= 0xd800 && unit <= 0xdbff {
					end -= 2
				}
				data = append(data[:end], 0, 0)
			} else {
				end := 509
				if codePage == 65001 {
					for !utf8.Valid(data[:end]) {
						end--
					}
				}
				data = append(data[:end], 0)
			}
		}
		out.data = data
	case core.Binary:
		if (tag == ruleCondition || tag == ruleActions) && len(v) <= 510 {
			out.data = v
			break
		}
		if kind == 0xfb {
			// PtypServerId has its own structure, never an EntryID inferred
			// from the current row. A nonzero instance must match the view.
			instance, _ := values[fixedContentsProperty(tableInstanceNumTag)].(core.Integer)
			valid := instance == 0 && validScalarServerID(v) || instance > 0 && len(v) == 21 &&
				v[0] == 1 && binary.LittleEndian.Uint32(v[17:]) == uint32(instance)
			if len(v) > 510 || !valid {
				return fail(NotSupported)
			}
			out.data = append(binary.LittleEndian.AppendUint16(nil, uint16(len(v))), v...)
			break
		}
		if kind != 0x102 {
			return fail(NotSupported)
		}
		n := min(len(v), 510)
		out.data = append(binary.LittleEndian.AppendUint16(nil, uint16(n)), v[:n]...)
	default:
		return fail(NotSupported)
	}
	return out
}
