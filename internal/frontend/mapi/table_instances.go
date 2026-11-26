// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"maps"

	"ewsgo/internal/core"
)

const maxTableInstanceRows = 2048

var keywordsName = propertyName{
	Set:  [16]byte{0x29, 0x03, 0x02, 0, 0, 0, 0, 0, 0xc0, 0, 0, 0, 0, 0, 0, 0x46},
	Kind: 1,
	Name: "Keywords",
}

var keywordsValuesProperty = contentsProperty{name: keywordsName, kind: 0x101f}
var keywordsInstanceProperty = contentsProperty{name: keywordsName, kind: 0x301f}

func tableInstanceKey(keys []boundTableSort) (contentsProperty, bool) {
	for _, key := range keys {
		if key.property.kind&0x3000 == 0x3000 {
			return key.property, true
		}
	}
	return contentsProperty{}, false
}

func compatibleInstanceColumns(columns []uint32, names *namedPropertyRegistry, property contentsProperty) bool {
	for _, tag := range columns {
		if uint16(tag)&0x3000 != 0x3000 {
			continue
		}
		column, err := resolveContentsIdentity(tag, names)
		if column.kind == 0x301e {
			column.kind = 0x301f
		}
		if err != nil || column != property {
			return false
		}
	}
	return true
}

func expandTableInstances(ctx context.Context, store *ramStore, source []contentsValues, property contentsProperty) ([]contentsValues, *contentsLease, error) {
	values := property
	values.kind &^= 0x2000
	count, used := 0, 0
	for _, row := range source {
		if err := ctx.Err(); err != nil {
			return nil, nil, err
		}
		items, _ := row[values].(core.Strings)
		count += max(1, len(items))
		if count > maxTableInstanceRows {
			return nil, nil, core.ErrLimit
		}
		used += 8
		if len(items) != 0 {
			used += len(items) * (520 + 128*len(row))
		}
	}
	lease, err := store.reserveContentsMemory(used)
	if err != nil {
		return nil, nil, err
	}
	out := make([]contentsValues, 0, count)
	for _, row := range source {
		if err := ctx.Err(); err != nil {
			lease.release()
			return nil, nil, err
		}
		items, _ := row[values].(core.Strings)
		if len(items) == 0 {
			out = append(out, row)
			continue
		}
		for i, item := range items {
			number := uint32(i + 1)
			instance := maps.Clone(row)
			instance[property] = core.Text(item)
			putContentsValue(instance, tableInstanceNumTag, core.Integer(number))
			server := fixedContentsProperty(instanceServerIDTag)
			if original, ok := row[server].(core.Binary); ok && len(original) == 21 {
				instance[server] = core.Binary(binary.LittleEndian.AppendUint32(append([]byte(nil), original[:17]...), number))
			}
			out = append(out, instance)
		}
	}
	return out, lease, nil
}

// The caller owns the detached source order and retains the expansion lease
// for as long as the expanded rows belong to the view.
func prepareTableSort(ctx context.Context, store *ramStore, rows []contentsValues, keys []boundTableSort) ([]contentsValues, *contentsLease, error) {
	var lease *contentsLease
	if property, ok := tableInstanceKey(keys); ok {
		var err error
		rows, lease, err = expandTableInstances(ctx, store, rows, property)
		if err != nil {
			return nil, nil, err
		}
	}
	if err := sortBoundTableRows(rows, keys); err != nil {
		lease.release()
		return nil, nil, err
	}
	return rows, lease, nil
}
