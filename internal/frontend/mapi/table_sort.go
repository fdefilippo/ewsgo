// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"slices"
	"sort"

	"ewsgo/internal/core"
)

const maxTableSortKeys = 64

type tableSortKey struct {
	tag   uint32
	order byte
}
type tableSortRequest struct {
	flags                byte
	categories, expanded uint16
	keys                 []tableSortKey
	fingerprint          [32]byte
}
type boundTableSort struct {
	property   contentsProperty
	descending bool
	maximum    bool
}

func (*tableSortRequest) String() string   { return "tableSortRequest{private}" }
func (*tableSortRequest) GoString() string { return "tableSortRequest{private}" }

// MS-OXCROPS 2.2.5.2.1 / MS-OXCDATA 2.13.1. Decode the complete bounded
// request before any effects.
func parseTableSort(r *reader) (*tableSortRequest, error) {
	h := r.take(7)
	if r.err != nil {
		return nil, r.err
	}
	n := int(binary.LittleEndian.Uint16(h[1:]))
	categories, expanded := binary.LittleEndian.Uint16(h[3:]), binary.LittleEndian.Uint16(h[5:])
	raw := r.take(5 * n)
	if r.err != nil {
		return nil, r.err
	}
	if categories > uint16(n) || expanded > categories || h[0]&^byte(1) != 0 {
		return nil, ErrMalformed
	}
	if n > maxTableSortKeys {
		return nil, ErrUnsupported
	}
	q := &tableSortRequest{flags: h[0], categories: categories, expanded: expanded, keys: make([]tableSortKey, n)}
	// ExpandedCount only sets initial visibility. TBL_ASYNC is a request flag.
	if categories != 0 {
		digest := sha256.New()
		digest.Write(h[1:5])
		digest.Write(raw)
		copy(q.fingerprint[:], digest.Sum(nil))
	}
	for i := range q.keys {
		tag, order := binary.LittleEndian.Uint32(raw[5*i:]), raw[5*i+4]
		if order != 0 && order != 1 && (order != 4 || categories == 0 || i != int(categories)) {
			return nil, ErrMalformed
		}
		q.keys[i] = tableSortKey{tag, order}
	}
	return q, nil
}

func tableSortSchema(table *contentsTable, p contentsProperty) bool {
	if table.inboxRules {
		if p.name != (propertyName{}) {
			return false
		}
		switch uint32(p.id)<<16 | uint32(p.kind) {
		case ruleID, ruleSequence, ruleState, ruleName, ruleProvider, ruleUserFlags, ruleLevel:
			return true
		default:
			return false
		}
	}
	if p == fixedContentsProperty(0x67480014) || p == fixedContentsProperty(0x674a0014) {
		return true
	}
	if table.search != nil && isCalendarSearchProperty(p) {
		return true
	}
	if table.special == core.ContactsFolder && contactSchema(p) {
		return true
	}
	if table.flags&tableSoftDeletes != 0 {
		if p.name != (propertyName{}) {
			return false
		}
		switch uint32(p.id)<<16 | uint32(p.kind) {
		case 0x001a001f, 0x0037001f, 0x0042001f, 0x0c1a001f, 0x0c1f001f, 0x0e04001f, 0x0e060040, 0x0e080003,
			0x0e090102, 0x0ff60102, 0x0fff0102, 0x668f0040, 0x674d0014, 0x674e0003:
			return true
		}
		return false
	}
	return contentsProjectionSchema(!table.associated, table.associated)(p)
}

func bindTableSort(table *contentsTable, names *namedPropertyRegistry, q *tableSortRequest) ([]boundTableSort, uint32, error) {
	if q == nil || table.hierarchy || table.permissions != nil || q.categories != 0 && (table.attachmentMessage != nil || table.inboxRules) ||
		q.categories > maxCategoryDepth {
		return nil, 0, core.ErrUnsupported
	}
	keys := make([]boundTableSort, 0, len(q.keys))
	hasInstance := false
	for _, key := range q.keys {
		p, err := resolveContentsIdentity(key.tag, names)
		if err != nil {
			return nil, key.tag, err
		}
		// String8 and Unicode refer to the same projected text. Sort complete
		// values, never the clipped/encoded 510-byte cells sent by QueryRows.
		if p.kind == 0x1e {
			p.kind = 0x1f
		} else if p.kind == 0x301e {
			p.kind = 0x301f
		}
		switch p.kind {
		case 2, 3, 0xb, 0x14, 0x1f, 0x40, 0x102:
		case 0x301f:
			if p != keywordsInstanceProperty || hasInstance || table.associated || table.attachmentMessage != nil ||
				table.search != nil || table.flags&tableSoftDeletes != 0 {
				return nil, key.tag, core.ErrUnsupported
			}
			hasInstance = true
		default:
			return nil, key.tag, core.ErrUnsupported
		}
		// Native mail has no creation timestamp in this view. Sort the
		// missing cells as such and let subsequent keys decide their order.
		absentCreationTime := p == fixedContentsProperty(0x30070040) && !table.associated &&
			table.attachmentMessage == nil && table.flags&tableSoftDeletes == 0
		if p.kind != 0x301f && !tableSortSchema(table, p) && !absentCreationTime {
			return nil, key.tag, core.ErrUnsupported
		}
		keys = append(keys, boundTableSort{property: p, descending: key.order == 1, maximum: key.order == 4})
	}
	return keys, 0, nil
}

func compareTableValues(kind uint16, a, b core.Value) int {
	switch {
	case a == nil && b != nil:
		return -1
	case a != nil && b == nil:
		return 1
	case a == nil:
		return 0
	default:
		comparison, _ := compareContentsValues(kind&^0x3000, a, b)
		return comparison
	}
}

// An omitted known column sorts before a present value in ascending order.
// Validate types before sorting so a failed request never partially reorders.
// Equal keys preserve the previous row order. Text compares ordinally.
func sortBoundTableRows(rows []contentsValues, keys []boundTableSort) error {
	for _, row := range rows {
		for _, key := range keys {
			if value, ok := row[key.property]; ok && !contentsValueType(key.property.kind&^0x3000, value) {
				return core.ErrInvalid
			}
		}
	}
	sort.SliceStable(rows, func(i, j int) bool {
		for _, key := range keys {
			if key.maximum {
				continue
			}
			comparison := compareTableValues(key.property.kind, rows[i][key.property], rows[j][key.property])
			if key.descending {
				comparison = -comparison
			}
			if comparison != 0 {
				return comparison < 0
			}
		}
		return false
	})
	return nil
}

func (t *SessionTransport) sortContentsTable(ctx context.Context, session core.Session, object serverObject, op Operation) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	table := object.table
	keys, rejectedTag, err := bindTableSort(table, &object.binding.store.named, op.Table.sort)
	if err != nil {
		if rejectedTag != 0 {
			op.Table.bindingRejected = true
			op.Table.rejection.tag = rejectedTag
		}
		return fail(storeError(err))
	}
	if property, expanded := tableInstanceKey(keys); expanded && !compatibleInstanceColumns(table.columns, &object.binding.store.named, property) {
		return fail(NotSupported)
	}
	// Old and new sort specifications coexist until commit. Row maps/values
	// are immutable and shared; only the row-pointer order is detached.
	source := table.rows
	if table.categories != nil {
		source = table.categories.leaves
	}
	_, previousInstances := tableInstanceKey(table.sortKeys)
	if previousInstances {
		source = table.baseRows
	}
	lease, err := object.binding.store.reserveContentsMemory(1024 + 8*len(source) + 384*len(keys))
	if err != nil {
		return fail(storeError(err))
	}
	defer func() { lease.release() }()
	var rows []contentsValues
	if previousInstances {
		rows, err = restrictedTableRows(ctx, source, table.restriction)
	} else {
		rows = slices.Clone(source)
	}
	if err != nil {
		return fail(storeError(err))
	}
	rows, instances, err := prepareTableSort(ctx, object.binding.store, rows, keys)
	if err != nil {
		return fail(storeError(err))
	}
	defer func() { instances.release() }()
	var categories *categoryView
	if op.Table.sort.categories != 0 {
		scope := table.categoryScope(object.fid, op.Table.sort.fingerprint, table.restrictionID)
		categories, err = buildCategoryView(ctx, object.binding.store, scope, rows, keys,
			int(op.Table.sort.categories), int(op.Table.sort.expanded))
		if err != nil {
			return fail(storeError(err))
		}
		defer func() { categories.close() }()
	}
	if err = t.authorizeTable(ctx, session, object, op.ID); err != nil {
		return fail(object.table.authorizationError(err))
	}
	if err = ctx.Err(); err != nil {
		return fail(storeError(err))
	}
	retained := 0
	if len(keys) != 0 {
		retained = 1024 + 128*len(keys)
	}
	if err = lease.shrink(retained); err != nil {
		return fail(storeError(err))
	}
	table.rows, table.sortKeys = rows, keys
	table.categorySort, table.categoryExpanded = op.Table.sort.fingerprint, int(op.Table.sort.expanded)
	table.replaceCategories(categories)
	categories = nil
	table.sortLease.release()
	table.sortLease, lease = lease, nil
	table.instanceLease.release()
	table.instanceLease, instances = instances, nil
	table.cursor = 0
	table.clearBookmarks()
	table.subscribe()
	return append(fail(0), 0) // Synchronous TBLSTAT_COMPLETE, including TBL_ASYNC.
}
