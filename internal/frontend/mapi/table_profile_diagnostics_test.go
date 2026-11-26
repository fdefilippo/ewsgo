// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestTableProfileParserDiagnosticAndAtomicPrefix(t *testing.T) {
	cases := []struct {
		wire           []byte
		profiles, tags []string
	}{
		{tableQueryWire(2, 0x82, 1, 10), []string{"query_rows:flags", "query_rows:enable_packed_buffers", "query_rows:reserved_flags"}, nil},
		{tableQueryWire(2, 0x85, 0, 10), []string{"query_rows:flags", "query_rows:no_advance", "query_rows:reserved_flags"}, nil},
		{tableQueryWire(2, 0, 1, 0), []string{"query_rows:zero_count"}, nil},
		{tableColumnsWire(2), []string{"set_columns:column_count"}, nil},
		{tableColumnsWire(2, 0x30013003), []string{"set_columns:column_type"}, []string{"0x30013003"}},
		{tableColumnsWire(2, 0x80013003), []string{"set_columns:column_type"}, nil},
	}
	flags := tableColumnsWire(2, 0x3001001f)
	flags[3] = 0x81
	cases = append(cases, struct {
		wire           []byte
		profiles, tags []string
	}{flags, []string{"set_columns:flags", "set_columns:async", "set_columns:reserved_flags"}, nil})
	for _, item := range cases {
		var previous []byte
		for _, enabled := range []bool{false, true} {
			f, box := newContentsDownloadFixture(t)
			f.transport.EnableContentsTableSpike()
			f.transport.unknownPropertyTrace = enabled
			f.transport.synchronizationOptionsTrace = enabled
			wire := append([]byte{1, 4, 1}, item.wire...)
			output, c := f.execute(wire, 65535)
			if enabled && !bytes.Equal(output, previous) {
				t.Fatal("diagnostic changed response")
			}
			previous = output
			want := []string{"none"}
			if enabled {
				want = item.profiles
			}
			if !reflect.DeepEqual(c.execution.TableProfiles(), want) {
				t.Fatal("parser profile", c.execution.TableProfiles(), want)
			}
			tags := c.properties.UnknownProperties()
			if enabled && !reflect.DeepEqual(nonNilStrings(tags.Tags), nonNilStrings(item.tags)) {
				t.Fatal("parser tags", tags)
			}
			if !enabled && (len(tags.Tags) != 0 || tags.HighIDs != 0 || tags.ReservedIDs != 0) {
				t.Fatal("off captured tags")
			}
			if enabled && binary.LittleEndian.Uint32(item.wire[len(item.wire)-4:]) == 0x80013003 && tags.HighIDs != 1 {
				t.Fatal("high ID not counted")
			}
			if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 || box.sourceCalls != 0 {
				t.Fatal("failed batch executed prefix")
			}
			text := fmt.Sprintf("%v %#v %v %#v", c.execution, c.execution, c.properties, c.properties)
			if strings.Contains(text, "30013003") || strings.Contains(text, "80013003") {
				t.Fatal("default formatter leaks tag")
			}
		}
	}
}

func nonNilStrings(v []string) []string {
	if v == nil {
		return []string{}
	}
	return v
}

func TestTableProfileRequiresCompleteReachableParserRequest(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	f.transport.EnableUnknownPropertyTrace()
	f.transport.EnableSynchronizationOptionsTrace()
	request := tableColumnsWire(2, 0x12343003)
	for n := 1; n < len(request); n++ {
		_, c := f.execute(request[:n], 65535)
		if !reflect.DeepEqual(c.execution.TableProfiles(), []string{"none"}) || len(c.properties.UnknownProperties().Tags) != 0 {
			t.Fatal("truncated request captured")
		}
	}
	for _, request := range [][]byte{
		append([]byte{0xff, 4, 1}, request...),
		tableQueryWire(9, 2, 1, 10), tableQueryWire(2, 2, 2, 10),
	} {
		_, c := f.execute(request, 65535)
		if !reflect.DeepEqual(c.execution.TableProfiles(), []string{"none"}) || len(c.properties.UnknownProperties().Tags) != 0 {
			t.Fatal("unreachable/invalid request captured")
		}
	}
}

func TestTableProfileActualBindingFailureAndGates(t *testing.T) {
	for _, unknown := range []bool{false, true} {
		for _, options := range []bool{false, true} {
			f, _, table, _ := tableFilterFixture(t, 0x10)
			f.transport.unknownPropertyTrace = unknown
			f.transport.synchronizationOptionsTrace = options
			table.cursor = 2
			wire := tableFindWire(2, 0, 0, rejectedBindingFilter(t), nil)
			out, c := f.run(wire)
			if !bytes.Equal(out, ropError(0x4f, 2, NotSupported)) || table.cursor != 2 {
				t.Fatal("diagnostic changed failed search")
			}
			want := []string{"none"}
			if unknown && options {
				want = []string{"find_row:normal:binding"}
			}
			if !reflect.DeepEqual(c.execution.TableProfiles(), want) {
				t.Fatal("combined profile gate", c.execution.TableProfiles())
			}
			tags := c.properties.UnknownProperties()
			if unknown && options {
				if !reflect.DeepEqual(tags.Tags, []string{"0x0ffe0003", "0x12340003"}) || tags.HighIDs != 1 || tags.ReservedIDs != 2 || c.properties.Requests != 0 || !c.bindingProperties {
					t.Fatal("binding metadata/counters", tags)
				}
			} else if len(tags.Tags) != 0 || tags.HighIDs != 0 || tags.ReservedIDs != 0 || c.bindingProperties {
				t.Fatal("combined tag gate")
			}
			// No execution-time binding metadata from a malformed entire batch or a
			// response rejected by output preflight.
			for _, bad := range []struct {
				wire    []byte
				maximum uint32
			}{{append(bytes.Clone(wire), 0xff, 4, 0), 65535}, {wire, 1}} {
				_, counts := f.execute(bad.wire, bad.maximum)
				if counts.bindingProperties || len(counts.properties.UnknownProperties().Tags) != 0 || !reflect.DeepEqual(counts.execution.TableProfiles(), []string{"none"}) {
					t.Fatal("binding diagnostic before execution")
				}
			}
		}
	}
}

func TestTableProfileSharedBudgetAndBindingProvenance(t *testing.T) {
	f, _, _, _ := tableFilterFixture(t, 0x10)
	restriction, _ := parseContentsRestriction(rejectedBindingFilter(t))
	op := Operation{ID: 0x4f, Logon: 4, Input: 2, Table: &tableRequest{restriction: restriction}}
	c := storeResults{tableProfileEnabled: true}
	c.properties.unknown.enabled = true
	c.recordTableBinding(f.state.mailbox, op, f.handles, ropError(0x4f, 2, NotSupported))
	if len(c.properties.UnknownProperties().Tags) != 0 || !reflect.DeepEqual(c.execution.TableProfiles(), []string{"none"}) {
		t.Fatal("generic failure mislabeled as binding")
	}
	for i := range 15 {
		c.properties.request([]uint32{uint32(0x4000+i)<<16 | 3})
	}
	op.Table.bindingRejected = true
	c.recordTableBinding(f.state.mailbox, op, f.handles, ropError(0x4f, 2, NotSupported))
	tags := c.properties.UnknownProperties()
	if len(tags.Tags) != 16 || tags.Tags[15] != "0x0ffe0003" || tags.Overflow != 1 || tags.HighIDs != 1 || tags.ReservedIDs != 2 || c.properties.Requests != 15 {
		t.Fatal("shared batch budget", tags)
	}
	op.Table.restriction = nil
	op.Table.bindingRejected = false
	// Successful and NotFound operations never generate a rejection profile.
	c = storeResults{tableProfileEnabled: true}
	c.properties.unknown.enabled = true
	c.recordTableBinding(f.state.mailbox, op, f.handles, ropError(0x4f, 2, 0))
	c.recordTableBinding(f.state.mailbox, op, f.handles, ropError(0x4f, 2, 0x8004010f))
	if c.execution.tableProfiles != 0 || c.bindingProperties {
		t.Fatal("normal result labeled failed")
	}
}

func TestTableProfileAssociatedAndHierarchyClassification(t *testing.T) {
	f, _, table, _ := tableFilterFixture(t, 0x10)
	restriction, _ := parseContentsRestriction(rejectedBindingFilter(t))
	for _, associated := range []bool{false, true} {
		for _, hierarchy := range []bool{false, true} {
			table.associated, table.hierarchy = associated, hierarchy
			op := Operation{ID: 0x14, Logon: 4, Input: 2, Table: &tableRequest{restriction: restriction, bindingRejected: true}}
			c := storeResults{tableProfileEnabled: true}
			c.properties.unknown.enabled = true
			c.recordTableBinding(f.state.mailbox, op, f.handles, ropError(0x14, 2, NotSupported))
			family := "normal"
			if associated {
				family = "associated"
			}
			if hierarchy {
				family = "hierarchy"
			}
			if !reflect.DeepEqual(c.execution.TableProfiles(), []string{"restrict:" + family + ":binding"}) {
				t.Fatal("wrong table family")
			}
		}
	}
}
