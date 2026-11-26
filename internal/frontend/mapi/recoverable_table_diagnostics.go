// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"slices"
	"strconv"
)

// Record protocol shape only, using the existing laboratory table-trace opt-in.
// This holds the last successful open and QueryRows of a recoverable contents
// table in a batch. It retains neither identifiers nor row data.
type recoverableTableDiagnostic struct {
	opened    bool
	openRows  uint16
	placement byte
	tableQueryDiagnostic
}

type tableQueryDiagnostic struct {
	queried            bool
	queryRows          uint16
	requested, missing unknownPropertyTrace
}

func (d *ExecutionDiagnostic) recordRecoverableMoveCopy(copyMessage bool) {
	if !d.enabled {
		return
	}
	if copyMessage {
		d.recoverableTable.placement |= 2
	} else {
		d.recoverableTable.placement |= 1
	}
}

func (d ExecutionDiagnostic) RecoverablePlacement() []string {
	var modes []string
	if d.recoverableTable.placement&1 != 0 {
		modes = append(modes, "move")
	}
	if d.recoverableTable.placement&2 != 0 {
		modes = append(modes, "copy")
	}
	return modes
}

func (d *tableQueryDiagnostic) recordQuery(rows uint16, columns []uint32, missing uint64) {
	d.queried, d.queryRows = true, rows
	d.requested = unknownPropertyTrace{enabled: true}
	d.missing = unknownPropertyTrace{enabled: true}
	for i, tag := range columns {
		d.requested.record(tag)
		if missing&(uint64(1)<<i) != 0 && slices.Contains(d.requested.tags[:d.requested.count], tag) {
			d.missing.record(tag)
		}
	}
}

// RecoverableTable reports counts and public ID/type pairs. A missing tag
// denotes at least one returned cell that could not supply the value.
func (d ExecutionDiagnostic) RecoverableTable() (openRows, queryRows string, requested, missing UnknownPropertyDiagnostic) {
	diagnostic := d.recoverableTable
	if diagnostic.opened {
		openRows = strconv.Itoa(int(diagnostic.openRows))
	}
	queryRows, requested, missing = diagnostic.tableQueryDiagnostic.fields()
	return
}

// AttachmentTable reports the row count and public column tags of QueryRows.
func (d ExecutionDiagnostic) AttachmentTable() (string, UnknownPropertyDiagnostic, UnknownPropertyDiagnostic) {
	return d.attachmentTable.fields()
}

func (d tableQueryDiagnostic) fields() (rows string, requested, missing UnknownPropertyDiagnostic) {
	if d.queried {
		rows = strconv.Itoa(int(d.queryRows))
		requested = (StorePropertyDiagnostic{unknown: d.requested}).UnknownProperties()
		missing = (StorePropertyDiagnostic{unknown: d.missing}).UnknownProperties()
	}
	return
}
