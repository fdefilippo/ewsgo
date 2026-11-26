// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"slices"

	"ewsgo/internal/core"
)

const (
	invalidCollapseState  uint32 = 0x0000045e
	tableCategoryNotFound uint32 = 0x8004010f
	collapseStateHeader          = 1 + 4 + 8 + 4 + sha256.Size
	collapseStateMin             = collapseStateHeader + sha256.Size
)

func (t *contentsTable) executeCategory(ctx context.Context, store *ramStore, handle uint32, op Operation, codePage uint32) ([]byte, *contentsLease) {
	switch op.ID {
	case 0x6b:
		return t.getCollapseState(store, handle, op)
	case 0x6c:
		return t.setCollapseState(ctx, store, handle, op), nil
	default:
		return t.changeCategory(ctx, store, op, codePage)
	}
}

func (t *contentsTable) changeCategory(ctx context.Context, store *ramStore, op Operation, codePage uint32) ([]byte, *contentsLease) {
	fail := func(code uint32) ([]byte, *contentsLease) { return ropError(op.ID, op.Input, code), nil }
	view := t.categories
	node := view.byID[op.Table.rowID]
	position, visible := t.rowPosition(tableRowRef{id: core.Integer(op.Table.rowID)})
	if node == nil || !visible {
		return fail(tableCategoryNotFound)
	}
	expand := op.ID == 0x59
	if node.expanded == expand {
		if expand {
			return fail(0x000004f8) // ecNotCollapsed
		}
		return fail(0x000004f7) // ecNotExpanded
	}
	// Walk the visible descendants according to their own expansion state.
	// Count them even when the client asks for zero rows in the response.
	temp := 8 * (len(view.leaves) + len(view.byID))
	if expand && op.Table.count != 0 {
		temp += 3*op.Table.limit + 2*maxTableRowBytes
	}
	lease, err := store.reserveContentsMemory(temp)
	if err != nil {
		return fail(storeError(err))
	}
	children := view.appendChildren(make([]contentsValues, 0, len(view.leaves)+len(view.byID)), node)
	response := append32(ropError(op.ID, op.Input, 0), uint32(len(children)))
	if expand {
		response = append(response, 0, 0)
		var count uint16
		for _, row := range children[:min(int(op.Table.count), len(children))] {
			wire := t.encodeRow(row, &store.named, codePage)
			if len(wire) > op.Table.limit-len(response) {
				break // Per MS-OXCTABL, ExpandedRowCount is independent of RowCount.
			}
			response = append(response, wire...)
			count++
		}
		binary.LittleEndian.PutUint16(response[10:12], count)
	}
	if err := ctx.Err(); err != nil {
		lease.release()
		return fail(storeError(err))
	}
	if t.cursor > position {
		if expand {
			t.cursor += len(children)
		} else {
			t.cursor = max(position, t.cursor-len(children))
		}
	}
	node.setExpanded(expand)
	view.rebuild()
	t.rows = view.visible
	t.subscribe()
	return response, lease
}

func (t *contentsTable) getCollapseState(store *ramStore, handle uint32, op Operation) ([]byte, *contentsLease) {
	fail := func(code uint32) ([]byte, *contentsLease) { return ropError(op.ID, op.Input, code), nil }
	view := t.categories
	position, found := t.rowPosition(tableRowRef{id: core.Integer(op.Table.rowID), instance: op.Table.instance})
	if !found {
		return fail(tableCategoryNotFound)
	}
	size := collapseStateMin + 8*len(view.byID)
	version := byte(1)
	if op.Table.instance != 0 {
		version = 2
		size += 4
	}
	if size+8 > op.Table.limit {
		return fail(tableBufferTooSmall)
	}
	lease, err := store.reserveContentsMemory(4*size + 64)
	if err != nil {
		return fail(storeError(err))
	}
	// The token stores only opaque identifiers and state, never group labels.
	out := binary.LittleEndian.AppendUint16(ropError(op.ID, op.Input, 0), uint16(size))
	out = append(out, version)
	out = append32(out, handle)
	out = binary.LittleEndian.AppendUint64(out, op.Table.rowID)
	out = append32(out, uint32(position))
	out = append(out, view.scope[:]...)
	if version == 2 {
		out = append32(out, op.Table.instance)
	}
	ids := make([]uint64, 0, len(view.byID))
	for id, node := range view.byID {
		if node.expanded {
			id |= 1
		}
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		out = binary.LittleEndian.AppendUint64(out, id)
	}
	out = append(out, signCollapseState(store.categoryStateKey, out[8:])...)
	return out, lease
}

func (t *contentsTable) setCollapseState(ctx context.Context, store *ramStore, handle uint32, op Operation) []byte {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	state, view := op.Table.bookmark, t.categories
	if len(state) < collapseStateMin {
		return fail(invalidCollapseState)
	}
	header := collapseStateHeader
	switch state[0] {
	case 1:
	case 2:
		header += 4
	default:
		return fail(invalidCollapseState)
	}
	size := header + sha256.Size
	if len(state) < size || len(state) > size+8*maxCategoryNodes || (len(state)-size)%8 != 0 {
		return fail(invalidCollapseState)
	}
	payload := state[:len(state)-sha256.Size]
	if !hmac.Equal(state[len(payload):], signCollapseState(store.categoryStateKey, payload)) ||
		!hmac.Equal(payload[17:collapseStateHeader], view.scope[:]) {
		return fail(invalidCollapseState)
	}
	entries := payload[header:]
	lease, err := store.reserveContentsMemory(48 * (len(entries) / 8))
	if err != nil {
		return fail(storeError(err))
	}
	defer lease.release()
	expanded := make(map[uint64]bool, len(entries)/8)
	for i := 0; i < len(entries); i += 8 {
		value := binary.LittleEndian.Uint64(entries[i:])
		expanded[value&^1] = value&1 == 1
	}
	if err := ctx.Err(); err != nil {
		return fail(storeError(err))
	}
	b := tableBookmark{
		row:      tableRowRef{id: core.Integer(binary.LittleEndian.Uint64(payload[5:13]))},
		position: int(binary.LittleEndian.Uint32(payload[13:17])),
		invalid:  binary.LittleEndian.Uint32(payload[1:5]) != handle,
	}
	if state[0] == 2 {
		b.row.instance = binary.LittleEndian.Uint32(payload[collapseStateHeader:header])
	}
	response := t.putBookmark(store, handle, op, b)
	if binary.LittleEndian.Uint32(response[2:6]) != 0 {
		return response
	}
	for id, node := range view.byID {
		open, known := expanded[id]
		node.setExpanded(open || !known && node.depth < view.defaultDepth)
	}
	view.rebuild()
	t.rows = view.visible
	if pos, found := t.rowPosition(b.row); found {
		t.cursor = pos
	} else {
		t.cursor = min(b.position, len(t.rows))
	}
	t.subscribe()
	return response
}

func signCollapseState(key [32]byte, payload []byte) []byte {
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte{1}) // Separate this domain from category instance IDs.
	mac.Write(payload)
	return mac.Sum(nil)
}
