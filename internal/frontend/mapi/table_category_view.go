// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/binary"
	"hash"
	"slices"
	"time"

	"ewsgo/internal/core"
)

const (
	maxCategoryDepth = 8
	maxCategoryNodes = 2048

	tableMIDTag         = 0x674a0014
	tableInstIDTag      = 0x674d0014
	tableInstanceNumTag = 0x674e0003
	tableRowTypeTag     = 0x0ff50003
	tableDepthTag       = 0x30050003
	tableContentCount   = 0x36020003
	tableUnreadCount    = 0x36030003
)

// The sorted leaves share immutable property maps with the table snapshot.
// Headers belong to this view. An interval indexes the underlying leaves of
// each group, even while one or more parents keep those leaves hidden.
type categoryView struct {
	scope        [32]byte
	depth        int
	defaultDepth int
	leaves       []contentsValues
	visible      []contentsValues
	roots        []*tableCategory
	byID         map[uint64]*tableCategory
	lease        *contentsLease
}

type tableCategory struct {
	id         uint64
	depth      int
	start, end int
	expanded   bool
	children   []*tableCategory
	header     contentsValues
}

func (t *contentsTable) categoryScope(fid uint64, sortID, restrictionID [32]byte) [32]byte {
	kind := t.flags & tableSoftDeletes
	if t.associated {
		kind |= 1
	}
	in := binary.LittleEndian.AppendUint64([]byte{kind}, fid)
	in = append(in, sortID[:]...)
	return sha256.Sum256(append(in, restrictionID[:]...))
}

func buildCategoryView(ctx context.Context, store *ramStore, scope [32]byte, leaves []contentsValues, keys []boundTableSort, depth, expanded int) (*categoryView, error) {
	nodes := min(maxCategoryNodes, len(leaves)*depth)
	nodeBytes := 1056 + 64*depth
	var maximum *contentsProperty
	if len(keys) > depth && keys[depth].maximum {
		maximum = &keys[depth].property
		nodeBytes += 64
	}
	lease, err := store.reserveContentsMemory(1024 + nodeBytes*nodes + 16*len(leaves))
	if err != nil {
		return nil, err
	}
	view := &categoryView{
		scope: scope, leaves: leaves, depth: depth, defaultDepth: expanded,
		byID: make(map[uint64]*tableCategory), lease: lease,
	}
	view.roots, err = view.group(ctx, store, keys[:depth], maximum, 0, 0, 0, len(leaves))
	if err != nil {
		view.close()
		return nil, err
	}
	view.visible = make([]contentsValues, 0, len(leaves)+len(view.byID))
	view.rebuild()
	if err := lease.shrink(1024 + nodeBytes*len(view.byID) + 16*len(leaves)); err != nil {
		view.close()
		return nil, err
	}
	return view, nil
}

func (v *categoryView) group(ctx context.Context, store *ramStore, keys []boundTableSort, maximum *contentsProperty, depth int, parent uint64, begin, end int) ([]*tableCategory, error) {
	var groups []*tableCategory
	aggregate := maximum != nil && depth+1 == v.depth
	property := keys[depth].property
	for first := begin; first < end; {
		if err := ctx.Err(); err != nil {
			return nil, err
		}
		if len(v.byID) >= maxCategoryNodes {
			return nil, core.ErrUnsupported
		}
		value := v.leaves[first][property]
		last := first + 1
		for last < end && categoryValueEqual(property.kind, value, v.leaves[last][property]) {
			last++
		}
		id := categoryInstanceID(store.categoryStateKey, v.scope, parent, value)
		if v.byID[id] != nil {
			return nil, core.ErrConflict
		}
		header := make(contentsValues, depth+9)
		for _, key := range keys[:depth+1] {
			if value := v.leaves[first][key.property]; value != nil {
				header[key.property] = value
			}
		}
		node := &tableCategory{id: id, depth: depth, start: first, end: last, header: header}
		v.byID[id] = node
		node.setExpanded(depth < v.defaultDepth)
		putContentsValue(header, tableInstIDTag, core.Integer(id))
		putContentsValue(header, tableInstanceNumTag, core.Integer(0))
		putContentsValue(header, tableDepthTag, core.Integer(depth))
		putContentsValue(header, tableContentCount, core.Integer(last-first))
		header[fixedContentsProperty(0x67480014)] = v.leaves[first][fixedContentsProperty(0x67480014)]
		unread, known := 0, true
		var maxValue core.Value
		for _, row := range v.leaves[first:last] {
			if aggregate {
				if value := row[*maximum]; compareTableValues(maximum.kind, value, maxValue) > 0 {
					maxValue = value
				}
			}
			read, ok := row[fixedContentsProperty(0x0e69000b)].(core.Bool)
			known = known && ok
			if ok && !bool(read) {
				unread++
			}
		}
		if known {
			putContentsValue(header, tableUnreadCount, core.Integer(unread))
		}
		if aggregate && maxValue != nil {
			header[*maximum] = maxValue
		}
		if depth+1 < v.depth {
			children, err := v.group(ctx, store, keys, maximum, depth+1, id, first, last)
			if err != nil {
				return nil, err
			}
			node.children = children
		}
		groups = append(groups, node)
		first = last
	}
	if aggregate {
		// Keep the original category order for ties. The aggregate sorts
		// sibling headers only, without entering the leaf sort.
		slices.SortStableFunc(groups, func(a, b *tableCategory) int {
			comparison := compareTableValues(maximum.kind, a.header[*maximum], b.header[*maximum])
			if keys[depth].descending {
				return -comparison
			}
			return comparison
		})
	}
	return groups, nil
}

func categoryValueEqual(kind uint16, a, b core.Value) bool {
	if a == nil || b == nil {
		return a == nil && b == nil
	}
	comparison, err := compareContentsValues(kind&^0x3000, a, b)
	return err == nil && comparison == 0
}

func categoryInstanceID(key, scope [32]byte, parent uint64, value core.Value) uint64 {
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte{0})
	mac.Write(scope[:])
	mac.Write(binary.LittleEndian.AppendUint64(nil, parent))
	writeCategoryValue(mac, value)
	// MIDs are in replica 1. Category instance IDs use a separate wire prefix.
	return binary.LittleEndian.Uint64(mac.Sum(nil))&^0xffff | 2
}

func writeCategoryValue(digest hash.Hash, value core.Value) {
	switch v := value.(type) {
	case core.Integer:
		digest.Write(binary.LittleEndian.AppendUint64([]byte{1}, uint64(v)))
	case core.Bool:
		if v {
			digest.Write([]byte{2, 1})
		} else {
			digest.Write([]byte{2, 0})
		}
	case core.Text:
		digest.Write([]byte{3})
		digest.Write([]byte(v))
	case core.Timestamp:
		stamp := time.Time(v)
		wire := binary.LittleEndian.AppendUint64([]byte{4}, uint64(stamp.Unix()))
		digest.Write(append32(wire, uint32(stamp.Nanosecond())))
	case core.Binary:
		digest.Write([]byte{5})
		digest.Write(v)
	default:
		digest.Write([]byte{0}) // Missing is a distinct group.
	}
}

func (n *tableCategory) setExpanded(expanded bool) {
	n.expanded = expanded
	kind := core.Integer(4)
	if expanded {
		kind = 3
	}
	putContentsValue(n.header, tableRowTypeTag, kind)
}

func (v *categoryView) appendVisible(rows []contentsValues, roots []*tableCategory) []contentsValues {
	for _, node := range roots {
		rows = append(rows, node.header)
		if node.expanded {
			rows = v.appendChildren(rows, node)
		}
	}
	return rows
}

func (v *categoryView) appendChildren(rows []contentsValues, node *tableCategory) []contentsValues {
	if len(node.children) == 0 {
		return append(rows, v.leaves[node.start:node.end]...)
	}
	return v.appendVisible(rows, node.children)
}

func (v *categoryView) rebuild() {
	clear(v.visible)
	v.visible = v.appendVisible(v.visible[:0], v.roots)
}

func (v *categoryView) close() {
	if v != nil {
		v.lease.release()
	}
}

func (t *contentsTable) replaceCategories(view *categoryView) {
	t.categories.close()
	t.categories = view
	if view != nil {
		t.rows = view.visible
	}
}

func putTableRowIdentity(row contentsValues, mid uint64) {
	putContentsValue(row, tableInstIDTag, core.Integer(mid))
	putContentsValue(row, tableInstanceNumTag, core.Integer(0))
	putContentsValue(row, tableRowTypeTag, core.Integer(1))
	putContentsValue(row, tableDepthTag, core.Integer(0))
}

func (t *contentsTable) encodeRow(row contentsValues, names *namedPropertyRegistry, codePage uint32) []byte {
	return folderPropertyRow(specificProperties{tags: t.columns}, t.rowCells(row, names, codePage))
}

func (t *contentsTable) rowCells(row contentsValues, names *namedPropertyRegistry, codePage uint32) []folderPropertyValue {
	values := make([]folderPropertyValue, len(t.columns))
	for i, tag := range t.columns {
		// Category headers carry their own depth. Leaves share immutable maps
		// with the flat snapshot, so substitute their depth only on output.
		if tag == tableDepthTag && t.categories != nil && row[fixedContentsProperty(tableRowTypeTag)] == core.Integer(1) {
			values[i] = folderPropertyValue{kind: 3, data: append32(nil, uint32(t.categories.depth))}
		} else {
			values[i] = tableCell(row, tag, names, codePage)
		}
	}
	return values
}
