// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestFolderCountersPropertiesIndependentWire(t *testing.T) {
	f := newOSTFixture(t)
	for i := range f.box.page.Objects {
		if f.box.page.Objects[i].Properties[core.WellKnown] == core.Text("inbox") {
			f.box.page.Objects[i].Properties[core.FolderItemCount] = core.Integer(3)
			f.box.page.Objects[i].Properties[core.FolderUnreadCount] = core.Integer(1)
		}
	}
	out, c := f.run(folderPropertiesOperation(4, 1, 1, 0x36010003, 0x36020003, 0x36030003))
	want := []byte{7, 1, 0, 0, 0, 0, 0, 1, 0, 0, 0, 3, 0, 0, 0, 1, 0, 0, 0}
	if !bytes.Equal(out, want) || c.properties.Errors != 0 || c.properties.Values != 3 {
		t.Fatal("native type/count/unread wire fixture")
	}
	// ROP PT_UNSPECIFIED includes a resolved type per property.
	out, c = f.run(folderPropertiesOperation(4, 1, 1, 0x36020000))
	if !bytes.Equal(out, []byte{7, 1, 0, 0, 0, 0, 1, 3, 0, 0, 3, 0, 0, 0}) || c.properties.Errors != 0 {
		t.Fatal("unspecified counter type")
	}
	out, c = f.run(folderPropertiesOperation(4, 1, 1, 0x36020102))
	if c.properties.Errors != 1 || binary.LittleEndian.Uint32(out[len(out)-4:]) != NotSupported {
		t.Fatal("wrong property type accepted")
	}
	f.open(int(MailboxRoot))
	out, c = f.run(folderPropertiesOperation(4, 1, 1, 0x36010003, 0x36020003, 0x36030003))
	if c.properties.Errors != 0 || !bytes.Equal(out[7:], make([]byte, 12)) {
		t.Fatal("root FolderType or real empty service namespace")
	}
	f.open(int(SentItems))
	out, c = f.run(folderPropertiesOperation(4, 1, 1, 0x36020003, 0x36030003))
	if c.properties.Errors != 2 || binary.LittleEndian.Uint32(out[len(out)-4:]) != PropertyNotFound {
		t.Fatal("missing native counts fabricated as zero")
	}
}

func TestFolderCountersValidationAndHierarchyMetadata(t *testing.T) {
	for _, p := range []core.Properties{
		{core.FolderItemCount: core.Text("private")},
		{core.FolderUnreadCount: core.Integer(-1)},
		{core.FolderItemCount: core.Integer(1 << 31)},
		{core.FolderItemCount: core.Integer(1), core.FolderUnreadCount: core.Integer(2)},
	} {
		if got, err := readFolderCounters(p); !errors.Is(err, core.ErrInvalid) || got != (folderCounters{}) {
			t.Fatal("invalid counters escaped as partial metadata")
		}
	}
	f := newHierarchyFixture(t)
	props := f.box.page.Objects[1].Properties
	props[core.FolderItemCount], props[core.FolderUnreadCount] = core.Integer(3), core.Integer(1)
	first := f.snapshot(t, MailboxRoot, hierarchyTestTime)
	row := hierarchyRow(t, first, f.binding.fids[Inbox])
	if row.counts != (folderCounters{values: [2]uint32{3, 1}, present: [2]bool{true, true}}) {
		t.Fatal("native counters lost in snapshot")
	}
	props[core.FolderItemCount], props[core.FolderUnreadCount] = core.Integer(4), core.Integer(2)
	next := f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(time.Second))
	changed := hierarchyRow(t, next, f.binding.fids[Inbox])
	if changed.cn != row.cn || changed.modified != row.modified || changed.counts.values != [2]uint32{4, 2} {
		t.Fatal("message-only counters synthesized a Folder metadata change")
	}
	props[core.FolderUnreadCount] = core.Integer(5)
	before, mappings := f.store.next, len(f.store.byFID)
	if _, err := f.store.observeHierarchy(context.Background(), f.session, first.root, hierarchyTestTime); !errors.Is(err, core.ErrInvalid) || f.store.next != before || len(f.store.byFID) != mappings {
		t.Fatal("invalid source counters published partial hierarchy")
	}
}

func TestHierarchyCountersTableAndFilteredFastTransfer(t *testing.T) {
	f, b := hierarchyTableFixture(t)
	for i := range b.page.Objects {
		if b.page.Objects[i].Properties[core.WellKnown] == core.Text("inbox") {
			b.page.Objects[i].Properties[core.FolderItemCount] = core.Integer(3)
			b.page.Objects[i].Properties[core.FolderUnreadCount] = core.Integer(1)
		}
	}
	hierarchyTableOpen(t, f, 0x50)
	tags := []uint32{0x67480014, 0x36010003, 0x36020003, 0x36030003}
	f.run(tableColumnsWire(2, tags...))
	out, _ := f.run(tableQueryWire(2, 0, 1, 256))
	matched := false
	for _, row := range decodeTableRows(t, out, tags) {
		if row[0] == f.fids[Inbox] {
			matched = reflect.DeepEqual(row[1:], []any{uint32(1), uint32(3), uint32(1)})
		}
	}
	if !matched || b.sourceCalls != 0 {
		t.Fatal("hierarchy counters missing or read message contents")
	}
	f.run([]byte{1, 4, 2})
	s := replicaStore(t, f)
	view, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], hierarchyTestTime)
	if err != nil {
		t.Fatal("snapshot fixture")
	}
	folder := hierarchyRow(t, view, f.fids[Inbox])
	binding, code := f.state.mailbox.object(f.handles[0], 4)
	if code != 0 {
		t.Fatal("missing logon object")
	}
	for _, excluded := range []bool{false, true} {
		cfg := synchronizationConfigure{kind: 2, flags: 1, sendOptions: 1}
		if excluded {
			// Filtering uses property ID, even with an alternate client type.
			cfg.tags = []uint32{0x36010000, 0x36020014, 0x36030102}
		}
		stream, err := hierarchyFastTransfer(view.replica, binding.binding.mailboxGUID, cfg, hierarchyDelta{changes: []hierarchyFolder{folder}})
		if err != nil {
			t.Fatal("count stream fixture")
		}
		found := map[uint32]uint32{}
		for _, token := range readTestFX(t, stream.data) {
			if token.tag == 0x36010003 || token.tag == 0x36020003 || token.tag == 0x36030003 {
				found[token.tag] = binary.LittleEndian.Uint32(token.value)
			}
		}
		if excluded && len(found) != 0 || !excluded && !reflect.DeepEqual(found, map[uint32]uint32{0x36010003: 1, 0x36020003: 3, 0x36030003: 1}) {
			t.Fatal("filtered hierarchy counts fabricated or missing")
		}
	}
}
