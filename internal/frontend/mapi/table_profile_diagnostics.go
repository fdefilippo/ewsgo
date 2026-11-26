// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import "encoding/binary"

// Public protocol metadata only. A parser rejection retains at most the one
// failing column tag, never columns/criteria/values or any buffer fragment.
type tableProfileRejection struct {
	reason uint8
	flags  byte
	tag    uint32
}

func (tableProfileRejection) String() string   { return "tableProfileRejection{redacted}" }
func (tableProfileRejection) GoString() string { return "tableProfileRejection{redacted}" }

const (
	tableColumnCount uint8 = iota + 1
	tableColumnFlags
	tableColumnType
	tableQueryFlags
	tableQueryCount
)

var tableProfileLabels = [...]string{
	"set_columns:column_count", "set_columns:flags", "set_columns:column_type",
	"query_rows:flags", "query_rows:zero_count",
	"query_rows:no_advance", "query_rows:enable_packed_buffers", "query_rows:reserved_flags",
	"set_columns:async", "set_columns:reserved_flags",
	"restrict:normal:binding", "restrict:associated:binding", "restrict:hierarchy:binding", "restrict:permissions:binding",
	"find_row:normal:binding", "find_row:associated:binding", "find_row:hierarchy:binding", "find_row:permissions:binding",
	"sort_table:normal:binding", "sort_table:associated:binding",
	"get_contents_table:input_type", "get_contents_table:budget", "get_contents_table:metadata_only",
	"get_contents_table:recoverable_capture", "get_contents_table:associated_capture",
	"get_contents_table:search_capture", "get_contents_table:normal_capture",
}

const (
	contentsTableWrongType uint32 = 1 << (20 + iota)
	contentsTableBudget
	contentsTableMetadataOnly
	contentsTableRecoverable
	contentsTableAssociated
	contentsTableSearch
	contentsTableNormal
)

// TableProfiles uses a closed vocabulary; technical tags share the existing
// per-batch sixteen-tag budget with GetProperties and Contents binding.
func (d ExecutionDiagnostic) TableProfiles() []string {
	out := []string{}
	for i, label := range tableProfileLabels {
		if d.tableProfiles&(1<<i) != 0 {
			out = append(out, label)
		}
	}
	if len(out) == 0 {
		return []string{"none"}
	}
	return out
}

func (c *storeResults) recordTableRejection(d tableProfileRejection) {
	if !c.tableProfileEnabled || d.reason < tableColumnCount || d.reason > tableQueryCount {
		return
	}
	c.execution.tableProfiles |= 1 << (d.reason - 1)
	if d.reason == tableColumnType {
		c.recordTableTag(d.tag)
	}
	if d.reason == tableQueryFlags || d.reason == tableQueryCount {
		if d.flags&1 != 0 {
			c.execution.tableProfiles |= 1 << 5
		}
		if d.flags&2 != 0 {
			c.execution.tableProfiles |= 1 << 6
		}
		if d.flags&^byte(3) != 0 {
			c.execution.tableProfiles |= 1 << 7
		}
	}
	if d.reason == tableColumnFlags {
		if d.flags&1 != 0 {
			c.execution.tableProfiles |= 1 << 8
		}
		if d.flags&^byte(1) != 0 {
			c.execution.tableProfiles |= 1 << 9
		}
	}
}

func (c *storeResults) recordTableTag(tag uint32) {
	c.bindingProperties = true
	c.properties.unknown.record(tag)
	if index := publicStorePropertyIndex(uint16(tag >> 16)); index >= 0 {
		c.properties.propertyNames[index] = true
	}
}

// Inspect an executed, rejected sort or filter against its actual Table schema. This
// pure walk performs no I/O, allocations of named IDs, or binding-tree copies.
// A general NotSupported is not mislabeled as a binding failure.
func (c *storeResults) recordTableBinding(objects *mailboxObjects, op Operation, handles []uint32, response []byte) {
	if !c.tableProfileEnabled || (op.ID != 0x13 && op.ID != 0x14 && op.ID != 0x4f) || !op.Table.bindingRejected || len(response) < 6 || binary.LittleEndian.Uint32(response[2:]) != NotSupported {
		return
	}
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 || object.table == nil || object.binding.store == nil {
		return
	}
	table := object.table
	family := 0
	if table.associated {
		family = 1
	}
	if op.ID == 0x13 {
		c.execution.tableProfiles |= 1 << (18 + family)
		c.recordTableTag(op.Table.rejection.tag)
		return
	}
	if table.hierarchy {
		family = 2
	}
	if table.permissions != nil {
		family = 3
	}
	failed := table.permissions != nil // This schema has no restriction support.
	nodes := 0
	var visit func(*contentsRestriction, int)
	visit = func(n *contentsRestriction, depth int) {
		if n == nil || depth > maxContentsRestrictionDepth || nodes >= maxContentsRestrictionNodes {
			return
		}
		nodes++
		if n.op == 3 || n.op == 4 || n.op == 6 || n.op == 8 {
			_, err := resolveContentsProperty(n.tag, &object.binding.store.named, func(p contentsProperty) bool { return tableRestrictionSchema(table, p) })
			if err != nil {
				failed = true
				c.recordTableTag(n.tag)
			}
		}
		for _, child := range n.children {
			visit(child, depth+1)
		}
	}
	visit(op.Table.restriction, 1)
	if failed {
		base := 10
		if op.ID == 0x4f {
			base = 14
		}
		c.execution.tableProfiles |= 1 << (base + family)
	}
}
