// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

// Exact owner-approved vocabulary in docs/mapi-folder-property-diagnostics-design.md.
// This table names requested IDs only: it does not define types, applicability,
// property values or protocol capabilities. Keep the original four names first.
var publicStoreProperties = [...]struct {
	id   uint16
	name string
}{
	{0x3001, "display_name"},
	{0x6748, "folder_id"},
	{0x6749, "parent_folder_id"},
	{0x0ffe, "object_type"},
	{0x0e09, "parent_entry_id"},
	{0x0ff4, "access"},
	{0x0ff6, "instance_key"},
	{0x0ff7, "access_level"},
	{0x0ff8, "mapping_signature"},
	{0x0ff9, "record_key"},
	{0x0ffb, "store_entry_id"},
	{0x0fff, "entry_id"},
	{0x3007, "creation_time"},
	{0x3008, "last_modification_time"},
	{0x300b, "search_key"},
	{0x340d, "store_support_mask"},
	{0x35df, "valid_folder_mask"},
	{0x35e0, "ipm_subtree_entry_id"},
	{0x35e2, "ipm_outbox_entry_id"},
	{0x35e3, "ipm_wastebasket_entry_id"},
	{0x35e4, "ipm_sent_mail_entry_id"},
	{0x3601, "folder_type"},
	{0x3602, "content_count"},
	{0x3603, "content_unread_count"},
	{0x360a, "subfolders"},
	{0x3613, "container_class"},
	{0x36d0, "ipm_appointment_entry_id"},
	{0x36d1, "ipm_contact_entry_id"},
	{0x36d2, "ipm_journal_entry_id"},
	{0x36d3, "ipm_note_entry_id"},
	{0x36d4, "ipm_task_entry_id"},
	{0x36d5, "reminders_online_entry_id"},
	{0x36d7, "ipm_drafts_entry_id"},
	{0x36d8, "additional_ren_entry_ids"},
	{0x36d9, "additional_ren_entry_ids_ex"},
	{0x36da, "extended_folder_flags"},
	{0x65e0, "source_key"},
	{0x65e1, "parent_source_key"},
	{0x65e2, "change_key"},
	{0x65e3, "predecessor_change_list"},
}

func publicStorePropertyIndex(id uint16) int {
	for i, property := range publicStoreProperties {
		if property.id == id {
			return i
		}
	}
	return -1
}

// PropertyNames returns a fresh, deduplicated list from the closed vocabulary.
// This accessor never exposes tags; UnknownProperties applies separate opt-in
// filtering for the temporary technical diagnostic.
func (d StorePropertyDiagnostic) PropertyNames() []string {
	names := []string{}
	for i, present := range d.propertyNames {
		if present {
			names = append(names, publicStoreProperties[i].name)
		}
	}
	return names
}
