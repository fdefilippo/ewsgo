// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

// Closed technical reasons only. No opcode number, property identity, value,
// restriction bytes or client text is retained in an error or diagnostic.
type contentsRestrictionRejection byte

const (
	restrictionRejected contentsRestrictionRejection = iota + 1
	restrictionBudget
	restrictionString8
	restrictionValueType
	restrictionCompareProperties
	restrictionBitMask
	restrictionSize
	restrictionSubObject
	restrictionComment
	restrictionCount
	restrictionOperator
	restrictionContentType
	restrictionFuzzyMode
	restrictionFuzzyBits
	restrictionFuzzyNonspacing
	restrictionFuzzyLoose
	restrictionBinaryFuzzy
	restrictionComparison
)

func (contentsRestrictionRejection) Error() string  { return ErrUnsupported.Error() }
func (contentsRestrictionRejection) Unwrap() error  { return ErrUnsupported }
func (contentsRestrictionRejection) String() string { return "contentsRestrictionRejection{redacted}" }
func (contentsRestrictionRejection) GoString() string {
	return "contentsRestrictionRejection{redacted}"
}
func (r contentsRestrictionRejection) label() string {
	switch r {
	case restrictionBudget:
		return "restriction_budget"
	case restrictionString8:
		return "restriction_string8"
	case restrictionValueType:
		return "restriction_value_type"
	case restrictionCompareProperties:
		return "restriction_compare_properties"
	case restrictionBitMask:
		return "restriction_bitmask"
	case restrictionSize:
		return "restriction_size"
	case restrictionSubObject:
		return "restriction_subobject"
	case restrictionComment:
		return "restriction_comment"
	case restrictionCount:
		return "restriction_count"
	case restrictionOperator:
		return "restriction_operator"
	case restrictionContentType:
		return "restriction_content_type"
	case restrictionFuzzyMode:
		return "restriction_fuzzy_mode"
	case restrictionFuzzyBits:
		return "restriction_fuzzy_bits"
	case restrictionFuzzyNonspacing:
		return "restriction_fuzzy_nonspacing"
	case restrictionFuzzyLoose:
		return "restriction_fuzzy_loose"
	case restrictionBinaryFuzzy:
		return "restriction_binary_fuzzy"
	case restrictionComparison:
		return "restriction_comparison"
	default:
		return "restriction_profile"
	}
}

type contentsRestrictionTypeRejection struct {
	contentsRestrictionRejection
	operator, propertyType string
}

func (r contentsRestrictionTypeRejection) Unwrap() error { return r.contentsRestrictionRejection }

func rejectedRestrictionType(op byte, kind uint16) error {
	rejection := contentsRestrictionTypeRejection{contentsRestrictionRejection: restrictionValueType}
	switch op {
	case 3:
		rejection.operator = "content"
	case 4:
		rejection.operator = "property"
	case 8:
		rejection.operator = "exist"
	}
	rejection.propertyType = restrictionPropertyType(kind)
	if kind == 0x001e {
		rejection.contentsRestrictionRejection = restrictionString8
	}
	return rejection
}

func restrictionPropertyType(kind uint16) string {
	switch {
	case kind == 0:
		return "unspecified"
	case kind == 1:
		return "null"
	case kind == 2 || kind == 3 || kind == 0x14:
		return "integer"
	case kind >= 4 && kind <= 7:
		return "decimal_or_floating_point"
	case kind == 0x000a:
		return "error"
	case kind == 0x000b:
		return "boolean"
	case kind == 0x000d:
		return "object"
	case kind == 0x001e:
		return "string8"
	case kind == 0x001f:
		return "unicode"
	case kind == 0x0040:
		return "time"
	case kind == 0x0048:
		return "guid"
	case kind == 0x00fb:
		return "server_id"
	case kind == 0x0102:
		return "binary"
	case kind&0x3000 == 0x3000:
		return "multi_value_instance"
	case kind&0x1000 != 0:
		return "multi_value"
	default:
		return "other"
	}
}
func rejectedRestrictionOperator(op byte) error {
	switch op {
	case 5:
		return restrictionCompareProperties
	case 6:
		return restrictionBitMask
	case 7:
		return restrictionSize
	case 9:
		return restrictionSubObject
	case 10:
		return restrictionComment
	case 11:
		return restrictionCount
	default:
		return restrictionOperator
	}
}
