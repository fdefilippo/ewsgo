// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"reflect"
	"testing"
)

// Independent fixture transcribed from the owner-approved public-ID table;
// deliberately not derived from the production lookup or decoder.
var publicPropertyFixture = []struct {
	id   uint16
	name string
}{
	{0x3001, "display_name"}, {0x6748, "folder_id"},
	{0x6749, "parent_folder_id"}, {0x0ffe, "object_type"},
	{0x0e09, "parent_entry_id"}, {0x0ff4, "access"},
	{0x0ff6, "instance_key"}, {0x0ff7, "access_level"},
	{0x0ff8, "mapping_signature"}, {0x0ff9, "record_key"},
	{0x0ffb, "store_entry_id"}, {0x0fff, "entry_id"},
	{0x3007, "creation_time"}, {0x3008, "last_modification_time"},
	{0x300b, "search_key"}, {0x340d, "store_support_mask"},
	{0x35df, "valid_folder_mask"}, {0x35e0, "ipm_subtree_entry_id"},
	{0x35e2, "ipm_outbox_entry_id"}, {0x35e3, "ipm_wastebasket_entry_id"},
	{0x35e4, "ipm_sent_mail_entry_id"}, {0x3601, "folder_type"},
	{0x3602, "content_count"}, {0x3603, "content_unread_count"},
	{0x360a, "subfolders"}, {0x3613, "container_class"},
	{0x36d0, "ipm_appointment_entry_id"}, {0x36d1, "ipm_contact_entry_id"},
	{0x36d2, "ipm_journal_entry_id"}, {0x36d3, "ipm_note_entry_id"},
	{0x36d4, "ipm_task_entry_id"}, {0x36d5, "reminders_online_entry_id"},
	{0x36d7, "ipm_drafts_entry_id"}, {0x36d8, "additional_ren_entry_ids"},
	{0x36d9, "additional_ren_entry_ids_ex"}, {0x36da, "extended_folder_flags"},
	{0x65e0, "source_key"}, {0x65e1, "parent_source_key"},
	{0x65e2, "change_key"}, {0x65e3, "predecessor_change_list"},
}

func TestPublicPropertyDiagnosticExactVocabulary(t *testing.T) {
	want := map[uint16]string{}
	for _, property := range publicPropertyFixture {
		if _, duplicate := want[property.id]; duplicate {
			t.Fatal("duplicate fixture ID")
		}
		want[property.id] = property.name
	}
	if len(want) != 40 {
		t.Fatal("fixture must contain exactly forty IDs")
	}
	// Exhaust the ID space: no unapproved ID can become a name. Requested
	// type bits are deliberately arbitrary; they never become a log field.
	for id := 0; id <= 0xffff; id++ {
		var diagnostic StorePropertyDiagnostic
		diagnostic.request([]uint32{uint32(id)<<16 | 0xffff})
		name, known := want[uint16(id)]
		names := diagnostic.PropertyNames()
		if diagnostic.Requests != 1 || diagnostic.Requested != 1 || diagnostic.Values != 0 || diagnostic.Errors != 0 {
			t.Fatal("request accounting")
		}
		if known {
			if !reflect.DeepEqual(names, []string{name}) || diagnostic.Other != 0 {
				t.Fatal("public ID incorrectly named", id)
			}
		} else if len(names) != 0 || diagnostic.Other != 1 {
			t.Fatal("unapproved ID named", id)
		}
	}
}

func TestPublicPropertyDiagnosticBatchDeduplicatesWithoutLosingCounts(t *testing.T) {
	var diagnostic StorePropertyDiagnostic
	var want []string
	for _, property := range publicPropertyFixture {
		want = append(want, property.name)
	}
	// Two ROP requests, with the first four IDs repeated across the batch
	// and two occurrences of an unknown ID. Name cardinality stays forty.
	var tags []uint32
	for _, property := range publicPropertyFixture {
		tags = append(tags, uint32(property.id)<<16)
	}
	diagnostic.request(tags)
	diagnostic.request([]uint32{0x3001001f, 0x67480014, 0x67490014, 0x0ffe0003, 0x7ffd0102, 0x7ffd0000})
	if !reflect.DeepEqual(diagnostic.PropertyNames(), want) || diagnostic.Requests != 2 || diagnostic.Requested != 46 || diagnostic.Other != 2 {
		t.Fatal("batch names or counts")
	}
	names := diagnostic.PropertyNames()
	names[0] = "private-value"
	if !reflect.DeepEqual(diagnostic.PropertyNames(), want) {
		t.Fatal("caller can change closed vocabulary")
	}
	var maximum StorePropertyDiagnostic
	columns := make([]uint32, 256)
	for i := range columns {
		columns[i] = 0x7ffdffff
	}
	for range 1024 {
		maximum.request(columns)
	}
	if maximum.Requests != 1024 || maximum.Requested != 262144 || maximum.Other != 262144 || len(maximum.PropertyNames()) != 0 {
		t.Fatal("maximum batch accounting")
	}
}

func TestPublicDiagnosticNamesDoNotAddFolderValues(t *testing.T) {
	for _, property := range publicPropertyFixture[3:] {
		for _, kind := range []uint32{0, 3, 0x14, 0x1e, 0x1f, 0x102, 0xffff} {
			value := projectFolderProperty(storeFolder{}, uint32(property.id)<<16|kind, true, 1252)
			if (kind == 0 || kind == 3) && property.id == 0x3601 {
				if value.code != 0 || value.kind != 3 || !reflect.DeepEqual(value.data, []byte{0, 0, 0, 0}) {
					t.Fatal("root folder type")
				}
				continue
			}
			if (kind == 0 || kind == 3) && (property.id == 0x3602 || property.id == 0x3603) {
				if value.code != PropertyNotFound || value.kind != 0x0a || len(value.data) != 0 {
					t.Fatal("absent count fabricated")
				}
				continue
			}
			if value.code != NotSupported || value.kind != 0x0a || len(value.data) != 0 {
				t.Fatal("diagnostic name fabricated a property value", property.id)
			}
		}
	}
}
