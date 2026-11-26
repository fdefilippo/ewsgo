// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"ewsgo/internal/core"
)

func ruleNameRestriction(name string) []byte {
	raw := classEqualityRestriction(name)
	binary.LittleEndian.PutUint32(raw[2:6], 0x65ec001f)
	binary.LittleEndian.PutUint32(raw[6:10], 0x65ec001f)
	return raw
}

func TestPackedQueryRowsOptionalSingleBufferActualRowsAndCursor(t *testing.T) {
	for _, executeFlags := range []uint32{0, 3, 4, 7} {
		t.Run(fmt.Sprint(executeFlags), func(t *testing.T) {
			f, box := newContentsDownloadFixture(t)
			f.transport.EnableContentsTableSpike()
			parent := box.objects[0].Parent
			box.objects = nil
			for i := range 150 {
				object := messageTestObject(core.ID(fmt.Sprintf("packed-%03d", i)), parent)
				object.MIME = []byte("From: synthetic@example.invalid\r\nSubject: Synthetic\r\n\r\n" + strings.Repeat("X", 700))
				box.objects = append(box.objects, object)
			}
			table, store := openTableTest(t, f, 0x10)
			tags := []uint32{0x674a0014, 0x1000001f}
			f.run(tableColumnsWire(2, tags...))
			baseline := store.allocator.contents.used
			query := func(flags byte, count uint16) []byte {
				t.Helper()
				wire, _ := (ROPBuffer{Operations: tableQueryWire(2, flags, 1, count), Handles: f.handles}).Marshal()
				body, c := f.transport.executeStore(context.Background(), f.session, f.state, f.identity, Execute{Flags: executeFlags, ROP: wire, MaxOutput: 65535})
				if c.errors != 0 || c.unsupported != "" || binary.LittleEndian.Uint32(body[4:]) != 0 {
					t.Fatal("valid packing request rejected")
				}
				size := int(binary.LittleEndian.Uint32(body[12:]))
				if size > 32768 || binary.LittleEndian.Uint16(body[18:]) != 4 || len(body) != 20+size {
					t.Fatal("expected a single bounded Last buffer")
				}
				return storeResponse(t, body).Operations
			}
			first := decodeTableRows(t, query(3, 150), tags)
			again := decodeTableRows(t, query(3, 150), tags)
			if len(first) == 0 || len(first) >= 150 || !reflect.DeepEqual(first, again) || table.cursor != 0 {
				t.Fatal("NoAdvance or size bound lost")
			}
			var all [][]any
			for len(all) < 150 {
				rows := decodeTableRows(t, query(2, uint16(150-len(all))), tags)
				if len(rows) == 0 {
					t.Fatal("returned no rows before end")
				}
				all = append(all, rows...)
			}
			if len(all) != 150 || table.cursor != 150 || len(decodeTableRows(t, query(2, 1), tags)) != 0 {
				t.Fatal("packed pagination lost rows/cursor")
			}
			seen := map[uint64]bool{}
			for i, row := range all {
				id := row[0].(uint64)
				if seen[id] || id != uint64(table.rows[i][fixedContentsProperty(0x674a0014)].(core.Integer)) || len(row[1].(string)) != 254 {
					t.Fatal("wrong real row order/value")
				}
				seen[id] = true
			}
			if store.allocator.contents.used != baseline {
				t.Fatal("response quota leaked")
			}
		})
	}
}

func TestServerIDTableColumnAbsentAndStructuredValue(t *testing.T) {
	f, _, _, _ := tableFilterFixture(t, 0x10)
	tags := []uint32{0x674100fb, 0x674a0014}
	out, c := f.run(tableColumnsWire(2, tags...))
	if c.errors != 0 || !bytes.Equal(out, []byte{0x12, 2, 0, 0, 0, 0, 0}) {
		t.Fatal("ServerId column rejected entire batch")
	}
	rows := decodeTableRows(t, func() []byte { v, _ := f.run(tableQueryWire(2, 2, 1, 9)); return v }(), tags)
	if len(rows) != 3 {
		t.Fatal("absent ServerId lost real messages")
	}
	for _, row := range rows {
		if row[0] != uint32(0x8004010f) || row[1].(uint64) == 0 {
			t.Fatal("missing ServerId did not return column NotFound")
		}
	}
	// Independent fixed vector: length21, server form, FID/MID, instance0.
	value := []byte{1, 1, 0, 0, 0, 0, 0, 0, 0, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0}
	values := contentsValues{fixedContentsProperty(0x674100fb): core.Binary(value)}
	cell := tableCell(values, 0x674100fb, &namedPropertyRegistry{}, 1252)
	if cell.kind != 0xfb || cell.code != 0 || len(cell.data) != 23 || binary.LittleEndian.Uint16(cell.data) != 21 || !bytes.Equal(cell.data[2:], value) {
		t.Fatal("ServerId structure changed")
	}
	for _, bad := range [][]byte{nil, {1}, {2}, append(bytes.Clone(value), 0), append(bytes.Clone(value[:17]), 1, 0, 0, 0), append([]byte{0}, make([]byte, 510)...)} {
		values[fixedContentsProperty(0x674100fb)] = core.Binary(bad)
		if got := tableCell(values, 0x674100fb, &namedPropertyRegistry{}, 1252); got.kind != 0xa || got.code != NotSupported {
			t.Fatal("invalid ServerId accepted")
		}
	}
	values[fixedContentsProperty(0x674100fb)] = core.Binary{0, 0xab, 0xcd}
	if got := tableCell(values, 0x674100fb, &namedPropertyRegistry{}, 1252); !bytes.Equal(got.data, []byte{3, 0, 0, 0xab, 0xcd}) {
		t.Fatal("client-defined ServerId altered")
	}
}

func TestFAIRuleNameStoredFindFilterFXAndReconnect(t *testing.T) {
	f, box, store := newFAIExecuteFixture(t)
	f.transport.EnableContentsTableSpike()
	name := "Synthetic rule 😀"
	values := faiCreateProperties()
	values = append(values, faiWireProperty{0x65ec001f, core.Text(name)})
	if parsed, err := parseFAISet(&reader{data: faiSetOperation(3, values)[3:]}); err != nil || !reflect.DeepEqual(parsed, values) {
		t.Fatal("rule name Set codec")
	}
	faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
	faiRunOK(t, f, faiSetOperation(3, values))
	faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 0})
	// Close the initial download handle before reusing its output slot.
	faiRunOK(t, f, []byte{1, 4, 2})
	read := func() {
		t.Helper()
		faiRunOK(t, f, []byte{5, 4, 1, 2, 0x12})
		tags := []uint32{0x65ec001f, 0x674a0014}
		faiRunOK(t, f, tableColumnsWire(2, tags...))
		found := decodeFindRow(t, faiRunOK(t, f, tableFindWire(2, 0, 0, ruleNameRestriction(name), nil)), tags)
		if found[0] != name || found[1].(uint64) == 0 {
			t.Fatal("rule name FindRow ignored stored value")
		}
		missing, _ := f.run(tableFindWire(2, 0, 0, ruleNameRestriction("Different synthetic name"), nil))
		if !bytes.Equal(missing, ropError(0x4f, 2, 0x8004010f)) {
			t.Fatal("unknown rule falsely matched")
		}
		requireRestrictSuccess(t, f, ruleNameRestriction(name))
		rows := decodeTableRows(t, faiRunOK(t, f, tableQueryWire(2, 2, 1, 9)), tags)
		if len(rows) != 1 || !reflect.DeepEqual(rows[0], found) {
			t.Fatal("rule restriction lost real FAI")
		}
		faiRunOK(t, f, []byte{1, 4, 2})
	}
	read()
	c := fullContentsConfig()
	c.flags &^= 0x20
	faiRunOK(t, f, contentsConfigureOperation(c, ruleNameRestriction(name)))
	matched := 0
	for _, token := range decodeTestContents(t, drainFX(t, f.run, 2, 97)) {
		if token.tag == 0x65ec001f && token.value == name {
			matched++
		}
	}
	if matched != 1 {
		t.Fatal("rule name missing from real filtered FAI stream")
	}
	f.state.mailbox.close()
	f.state.mailbox = newMailboxObjects(f.transport.stores)
	f.handles = []uint32{0xffffffff, 0xffffffff, 0xffffffff, 0xffffffff, 0xffffffff}
	faiRunOK(t, f, logonOperation(f.identity.LegacyDN, 4, 0))
	f.open(int(Inbox))
	read()
	if box.sourceCalls != 0 || len(store.fai.records) != 1 {
		t.Fatal("FAI lookup accessed native contents or lost record")
	}
	f.state.mailbox.close()
}

func TestFAIRuleNameKnownAbsentDistinctFromUnknownAndAtomicFailure(t *testing.T) {
	f, _, _, _ := tableFilterFixture(t, 0x12)
	missing, c := f.run(tableFindWire(2, 0, 0, ruleNameRestriction("Synthetic"), nil))
	if c.errors != 1 || !bytes.Equal(missing, ropError(0x4f, 2, 0x8004010f)) {
		t.Fatal("known optional FAI property treated unknown")
	}
	f, _, table, _ := tableFilterFixture(t, 0x10)
	failed, _ := f.run(tableFindWire(2, 0, 0, ruleNameRestriction("Synthetic"), nil))
	if !bytes.Equal(failed, ropError(0x4f, 2, NotSupported)) || table.cursor != 0 {
		t.Fatal("native MIME claimed rule property capability")
	}
	for _, tag := range []uint32{0x65ec0003, 0x65ec0102} {
		if faiSetTag(tag) {
			t.Fatal("wrong rule name type allowed")
		}
	}
}
