// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import "context"

// Enumerate the available row schema independently of SetColumns and cursor.
// Include columns present in every nonempty view of this type even when the
// snapshot is empty. Named columns require an existing wire ID.
func (t *contentsTable) queryColumnsAll(ctx context.Context, names *namedPropertyRegistry, op Operation) []byte {
	var fixed []uint32
	switch {
	case t.hierarchy:
		fixed = []uint32{0x67480014, 0x67490014, folderEntryIDTag, parentFolderEntryIDTag,
			0x3001001f, 0x66380003, 0x360a000b, 0x65e00102, 0x65e20102, 0x30080040, 0x36010003}
	case t.inboxRules:
		fixed = []uint32{ruleID, ruleSequence, ruleState, ruleName, ruleProvider, ruleCondition, ruleActions, ruleUserFlags, ruleLevel, ruleProviderData}
	case t.permissions != nil:
		fixed = []uint32{0x66710014, 0x6672001f, 0x66730003, 0x0fff0102}
	case t.attachmentMessage != nil:
		fixed = []uint32{0x0e210003, 0x0ff60102, 0x0ff90102, 0x370b0003}
	default:
		fixed = []uint32{0x67480014, tableMIDTag, tableInstIDTag, tableInstanceNumTag, tableRowTypeTag, tableDepthTag}
		if t.categories != nil {
			fixed = append(fixed, tableContentCount, tableUnreadCount)
		}
	}
	seen := make(map[uint32]struct{}, len(fixed))
	for _, tag := range fixed {
		seen[tag] = struct{}{}
	}
	limit := (op.PropertyListLimit - 8) / 4
	rows := t.rows
	if t.categories != nil {
		rows = t.categories.leaves // A collapsed category does not hide its leaf schema.
	}
	for _, row := range rows {
		if err := ctx.Err(); err != nil {
			return ropError(op.ID, op.Input, storeError(err))
		}
		for property := range row {
			switch property.kind {
			case 2, 3, 0x0b, 0x14, 0x1e, 0x1f, 0x40, 0xfb, 0x102, 0x1003, 0x301f:
			default:
				continue // Types outside the table cell projection.
			}
			id := property.id
			if property.name != (propertyName{}) {
				names.mu.Lock()
				id = names.byKey[property.name]
				names.mu.Unlock()
			}
			if id == 0 {
				continue
			}
			seen[uint32(id)<<16|uint32(property.kind)] = struct{}{}
			if len(seen) > limit {
				return ropError(op.ID, op.Input, tableBufferTooSmall)
			}
		}
	}
	tags := make([]uint32, 0, len(seen))
	for tag := range seen {
		tags = append(tags, tag)
	}
	return propertyTagsResponse(op, tags)
}
