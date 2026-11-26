// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"crypto/sha256"
	"encoding/binary"

	"ewsgo/internal/core"
)

// MS-OXCROPS 2.2.5.3.1 and 2.2.5.13.1. Restrictions have the same bounded
// ROP encoding as Contents synchronization.
func parseTableRestriction(r *reader, id byte, q *tableRequest) error {
	h := r.take(3)
	if r.err != nil {
		return r.err
	}
	q.flags = h[0]
	raw := r.take(int(binary.LittleEndian.Uint16(h[1:])))
	if r.err != nil {
		return r.err
	}
	if id == 0x14 && len(raw) != 0 {
		q.restrictionID = sha256.Sum256(raw)
	}
	if id == 0x4f {
		h = r.take(3)
		if r.err != nil {
			return r.err
		}
		q.origin = h[0]
		n := int(binary.LittleEndian.Uint16(h[1:]))
		q.bookmark = r.take(n)
		if r.err != nil {
			return r.err
		}
		if q.origin > 3 || (q.origin != 3 && n != 0) {
			return ErrMalformed
		}
	}
	if q.flags & ^byte(1) != 0 {
		return ErrMalformed
	}
	var err error
	q.restriction, err = parseContentsRestriction(raw)
	return err
}

func tableRestrictionSchema(table *contentsTable, p contentsProperty) bool {
	if table.permissions != nil {
		return false
	}
	if table.hierarchy {
		if p.name != (propertyName{}) {
			return false
		}
		switch uint32(p.id)<<16 | uint32(p.kind) {
		case folderHiddenTag, 0x67480014, 0x67490014, 0x3001001f, 0x66380003, 0x360a000b, 0x65e00102, 0x65e20102, 0x30080040, 0x3613001f, 0x36010003, 0x36020003, 0x36030003:
			return true
		}
		return false
	}
	return tableSortSchema(table, p)
}

func bindTableRestriction(table *contentsTable, store *ramStore, n *contentsRestriction) (*contentsRestriction, error) {
	if table.permissions != nil {
		return nil, core.ErrUnsupported
	}
	return bindContentsRestriction(n, func(tag uint32) (contentsProperty, error) {
		return resolveContentsProperty(tag, &store.named, func(p contentsProperty) bool { return tableRestrictionSchema(table, p) })
	})
}

func tableRestrictionBytes(n *contentsRestriction) int {
	if n == nil {
		return 0
	}
	used := 256
	switch v := n.value.(type) {
	case core.Text:
		used += 3 * len(v)
	case core.Binary:
		used += len(v)
	}
	for _, child := range n.children {
		used += tableRestrictionBytes(child)
	}
	return used
}

// Rows are immutable maps. Keep a separate pointer list even without a
// restriction so sorting the selected view cannot reorder its complete source.
func restrictedTableRows(ctx context.Context, source []contentsValues, restriction *contentsRestriction) ([]contentsValues, error) {
	rows := make([]contentsValues, 0, len(source))
	for _, row := range source {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		match, err := restriction.matches(row)
		if err != nil {
			return nil, err
		}
		if match {
			rows = append(rows, row)
		}
	}
	return rows, nil
}

func (t *SessionTransport) restrictTable(ctx context.Context, session core.Session, object serverObject, op Operation) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	table, store := object.table, object.binding.store
	// Reserve for both decoded and bound trees before binding. The request is
	// bounded to 8 KiB/128 nodes; replacement keeps the old tree until commit.
	lease, err := store.reserveContentsMemory(4*maxContentsRestrictionBytes + 512*maxContentsRestrictionNodes + 8*len(table.baseRows))
	if err != nil {
		return fail(storeError(err))
	}
	defer func() { lease.release() }()
	bound, err := bindTableRestriction(table, store, op.Table.restriction)
	if err != nil {
		op.Table.bindingRejected = true
		return fail(storeError(err))
	}
	rows, err := restrictedTableRows(ctx, table.baseRows, bound)
	if err != nil {
		return fail(storeError(err))
	}
	rows, instances, err := prepareTableSort(ctx, store, rows, table.sortKeys)
	if err != nil {
		return fail(storeError(err))
	}
	defer func() { instances.release() }()
	var categories *categoryView
	if table.categories != nil {
		scope := table.categoryScope(object.fid, table.categorySort, op.Table.restrictionID)
		categories, err = buildCategoryView(ctx, store, scope, rows, table.sortKeys, table.categories.depth, table.categoryExpanded)
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
	if err = lease.shrink(tableRestrictionBytes(bound)); err != nil {
		return fail(storeError(err))
	}
	table.rows, table.restriction, table.cursor = rows, bound, 0
	table.restrictionID = op.Table.restrictionID
	table.replaceCategories(categories)
	categories = nil
	table.instanceLease.release()
	table.instanceLease, instances = instances, nil
	table.filterLease.release()
	table.filterLease, lease = lease, nil
	table.clearBookmarks()
	table.subscribe()
	return append(fail(0), 0) // TBLSTAT_COMPLETE, including TBL_ASYNC.
}

func (t *SessionTransport) findTableRow(ctx context.Context, session core.Session, object serverObject, handle uint32, op Operation, codePage uint32) ([]byte, *contentsLease) {
	fail := func(code uint32) ([]byte, *contentsLease) { return ropError(op.ID, op.Input, code), nil }
	table, store := object.table, object.binding.store
	position, step := table.cursor, 1
	var invisible byte
	switch op.Table.origin {
	case 0:
		position = 0
	case 2:
		position = len(table.rows)
	case 3:
		var code uint32
		position, invisible, code = table.bookmarkPosition(handle, op.Table.bookmark)
		if code != 0 {
			return fail(code)
		}
	}
	if table.columns == nil {
		return fail(nullObject)
	}
	lease, err := store.reserveContentsMemory(4*maxContentsRestrictionBytes + 512*maxContentsRestrictionNodes + 2*maxTableRowBytes + 4*op.Table.limit)
	if err != nil {
		return fail(storeError(err))
	}
	failed := func(code uint32) ([]byte, *contentsLease) { lease.release(); return fail(code) }
	bound, err := bindTableRestriction(table, store, op.Table.restriction)
	if err != nil {
		op.Table.bindingRejected = true
		return failed(storeError(err))
	}
	if op.Table.flags&1 != 0 {
		position--
		step = -1
	}
	for ; position >= 0 && position < len(table.rows); position += step {
		if err = ctx.Err(); err != nil {
			return failed(storeError(err))
		}
		match, err := bound.matches(table.rows[position])
		if err != nil {
			return failed(storeError(err))
		}
		if !match {
			continue
		}
		row := table.encodeRow(table.rows[position], &store.named, codePage)
		if len(row) > maxTableRowBytes {
			return failed(NotSupported)
		}
		response := append(ropError(op.ID, op.Input, 0), invisible, 0)
		if len(response)+len(row) <= op.Table.limit {
			response[7] = 1
			response = append(response, row...)
		}
		if err = t.authorizeTable(ctx, session, object, op.ID); err != nil {
			return failed(object.table.authorizationError(err))
		}
		if err = ctx.Err(); err != nil {
			return failed(storeError(err))
		}
		table.cursor = position // Find positions at the row, not after it.
		table.subscribe()
		return response, lease
	}
	if err = t.authorizeTable(ctx, session, object, op.ID); err != nil {
		return failed(object.table.authorizationError(err))
	}
	// The protocol leaves the cursor undefined on NotFound; retain it so a
	// failed search does not silently advance another caller's navigation.
	return failed(0x8004010f)
}
