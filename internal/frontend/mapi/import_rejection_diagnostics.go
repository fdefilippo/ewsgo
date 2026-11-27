// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import "ewsgo/internal/core"

// importRejection identifies only the branch that refused an import.
// It retains no client flag, count, property, identifier, value or error text.
type importRejection uint8

const (
	importUnknownFlags importRejection = iota + 1
	importNormalMessage
	importPropertyCount
	importXIDProfile
	importPCLProfile
	importHierarchyUnsupported
	importDeleteHierarchyUnsupported
	importDeletePropertyProfile
	importDeleteCountProfile
	importDeleteGIDProfile
	importFAICollector
	importFAIDraftProfile
	importFAIHandleCapacity
	importNormalFolderProfile
	importNormalBackendProfile
	importNormalSourceReplica
	importNormalSourceReservation
	importNormalExistingMessage
	importAppointmentSaveMode
	importAppointmentSaveEmpty
	importAppointmentSaveDecode
	importAppointmentSaveProfile
	importAppointmentSaveStartWhole
	importAppointmentSaveEndWhole
	importAppointmentSaveRange
	importAppointmentSaveFixedDates
	importAppointmentSaveCommonDates
	importAppointmentSaveDuration
	importAppointmentSaveText
	importAppointmentSaveCollector
	importAppointmentSaveNative
	importAppointmentSaveZone
	importAppointmentRecurrenceDecode
	importAppointmentRecurrenceRule
	importAppointmentRecurrenceDates
	importAppointmentRecurrenceException
	importAppointmentRecurrenceAttachment
	importAppointmentSaveMessageFlags
	importAppointmentSaveMeetingState
	importAppointmentSaveReminder
	importAppointmentSaveNativeProfile
	importAppointmentExceptionWallTime
	importAppointmentExceptionBounds
	importAppointmentExceptionEmbeddedAnchor
)

type normalImportRejection struct{ reason importRejection }

func (e normalImportRejection) Error() string { return e.reason.Error() }
func (normalImportRejection) Unwrap() error   { return core.ErrUnsupported }

type appointmentImportRejection struct {
	reason importRejection
	cause  error
}

func (e appointmentImportRejection) Error() string { return e.reason.Error() }
func (e appointmentImportRejection) Unwrap() error { return e.cause }

func (importRejection) Error() string    { return "importRejection{redacted}" }
func (importRejection) String() string   { return "importRejection{redacted}" }
func (importRejection) GoString() string { return "importRejection{redacted}" }
func (importRejection) Unwrap() error    { return ErrUnsupported }

func (r importRejection) label() string {
	switch r {
	case importUnknownFlags:
		return "import_unknown_flags"
	case importNormalMessage:
		return "import_normal_not_supported"
	case importPropertyCount:
		return "import_property_count_profile"
	case importXIDProfile:
		return "import_xid_profile"
	case importPCLProfile:
		return "import_pcl_profile"
	case importHierarchyUnsupported:
		return "import_hierarchy_not_supported"
	case importDeleteHierarchyUnsupported:
		return "import_delete_hierarchy_not_supported"
	case importDeletePropertyProfile:
		return "import_delete_property_profile"
	case importDeleteCountProfile:
		return "import_delete_count_profile"
	case importDeleteGIDProfile:
		return "import_delete_gid_profile"
	case importFAICollector:
		return "import_fai_collector"
	case importFAIDraftProfile:
		return "import_fai_draft_profile"
	case importFAIHandleCapacity:
		return "import_fai_handle_capacity"
	case importNormalFolderProfile:
		return "import_normal_folder_profile"
	case importNormalBackendProfile:
		return "import_normal_backend_profile"
	case importNormalSourceReplica:
		return "import_normal_source_replica"
	case importNormalSourceReservation:
		return "import_normal_source_reservation"
	case importNormalExistingMessage:
		return "import_normal_existing_message"
	case importAppointmentSaveMode:
		return "import_appointment_save_mode"
	case importAppointmentSaveEmpty:
		return "import_appointment_save_empty"
	case importAppointmentSaveDecode:
		return "import_appointment_save_decode"
	case importAppointmentSaveProfile:
		return "import_appointment_save_profile"
	case importAppointmentSaveMessageFlags:
		return "import_appointment_save_message_flags"
	case importAppointmentSaveMeetingState:
		return "import_appointment_save_meeting_state"
	case importAppointmentSaveReminder:
		return "import_appointment_save_reminder"
	case importAppointmentSaveNativeProfile:
		return "import_appointment_save_native_profile"
	case importAppointmentSaveStartWhole:
		return "import_appointment_save_start_whole"
	case importAppointmentSaveEndWhole:
		return "import_appointment_save_end_whole"
	case importAppointmentSaveRange:
		return "import_appointment_save_range"
	case importAppointmentSaveFixedDates:
		return "import_appointment_save_fixed_dates"
	case importAppointmentSaveCommonDates:
		return "import_appointment_save_common_dates"
	case importAppointmentSaveDuration:
		return "import_appointment_save_duration"
	case importAppointmentSaveText:
		return "import_appointment_save_text"
	case importAppointmentSaveCollector:
		return "import_appointment_save_collector"
	case importAppointmentSaveNative:
		return "import_appointment_save_native"
	case importAppointmentSaveZone:
		return "import_appointment_save_zone"
	case importAppointmentRecurrenceDecode:
		return "import_appointment_recurrence_decode"
	case importAppointmentRecurrenceRule:
		return "import_appointment_recurrence_rule"
	case importAppointmentRecurrenceDates:
		return "import_appointment_recurrence_dates"
	case importAppointmentRecurrenceException:
		return "import_appointment_recurrence_exception"
	case importAppointmentRecurrenceAttachment:
		return "import_appointment_recurrence_attachment"
	case importAppointmentExceptionWallTime:
		return "import_appointment_exception_wall_time"
	case importAppointmentExceptionBounds:
		return "import_appointment_exception_bounds"
	case importAppointmentExceptionEmbeddedAnchor:
		return "import_appointment_exception_embedded_anchor"
	default:
		return "import_other_profile"
	}
}
