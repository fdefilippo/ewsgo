// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

// Server execution metadata only. No handles, tags, masks, values, identity or
// error text is retained. Labels are closed even for an unexpected server code.
type contentsConfigureFailure struct {
	stage        contentsConfigureStage
	code         uint32
	binding      byte
	metadataOnly bool
}
type contentsConfigureStage byte

const (
	contentsInputStage contentsConfigureStage = iota + 1
	contentsObjectStage
	contentsOptionsStage
	contentsRestrictionStage
	contentsSelectionStage
	contentsStateStage
	contentsHandleStage
)

func (contentsConfigureFailure) String() string   { return "contentsConfigureFailure{redacted}" }
func (contentsConfigureFailure) GoString() string { return "contentsConfigureFailure{redacted}" }

func contentsBindingClass(tag uint32) byte {
	if tag == 0x0e070003 {
		return 1
	} // Public PidTagMessageFlags, no value.
	id := uint16(tag >> 16)
	if id > 0 && id < 0x8000 {
		return 2
	}
	if id > 0x8000 && id < 0xffff {
		return 3
	}
	return 4
}
func (d contentsConfigureFailure) labels() []string {
	if d.code == 0 {
		return nil
	}
	stage := "configure_stage_other"
	switch d.stage {
	case contentsInputStage:
		stage = "configure_input"
	case contentsObjectStage:
		stage = "configure_object"
	case contentsOptionsStage:
		stage = "configure_options"
	case contentsRestrictionStage:
		stage = "configure_restriction_binding"
	case contentsSelectionStage:
		stage = "configure_selection_binding"
	case contentsStateStage:
		stage = "configure_state"
	case contentsHandleStage:
		stage = "configure_handle"
	}
	result := "configure_other_error"
	switch d.code {
	case NotSupported:
		result = "configure_not_supported"
	case 0x8004010f:
		result = "configure_not_found"
	case nullObject:
		result = "configure_null_object"
	case invalidObject:
		result = "configure_invalid_object"
	case 0x8007000e:
		result = "configure_out_of_memory"
	case 0x80070057:
		result = "configure_invalid_parameter"
	case accessDenied:
		result = "configure_access_denied"
	case 0x80040111:
		result = "configure_authentication_error"
	case 0x80040805:
		result = "configure_resync_required"
	case 0x000004b6:
		result = "configure_malformed"
	case 0x80004005:
		result = "configure_failure"
	}
	labels := []string{stage, result}
	if d.stage == contentsStateStage && d.metadataOnly {
		labels = append(labels, "configure_normal_metadata_only")
	}
	if d.stage == contentsRestrictionStage {
		switch d.binding {
		case 1:
			labels = append(labels, "binding_message_flags")
		case 2:
			labels = append(labels, "binding_other_fixed_property")
		case 3:
			labels = append(labels, "binding_named_property")
		default:
			labels = append(labels, "binding_other_property")
		}
	}
	return labels
}
