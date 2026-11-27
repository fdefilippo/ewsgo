// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import "encoding/binary"

const ropModifyRules = 0x41
const maxRuleNesting = 16

// ModifyRules has no payload length. Read the standard rule structure to
// locate the next ROP, without retaining rule properties or their values.
func skipModifyRules(r *reader) error {
	header := r.take(3) // ModifyRulesFlags and RulesCount.
	if header == nil {
		return r.err
	}
	rows := int(binary.LittleEndian.Uint16(header[1:]))
	if rows == 0 && header[0] != 1 {
		return ErrMalformed
	}
	for range rows {
		row := r.take(3) // RuleDataFlags and PropertyValueCount.
		if row == nil {
			return r.err
		}
		count := int(binary.LittleEndian.Uint16(row[1:]))
		if count == 0 {
			return ErrMalformed
		}
		if err := skipRuleProperties(r, count, 0); err != nil {
			return err
		}
	}
	return r.err
}

func skipRuleProperties(r *reader, count, depth int) error {
	for range count {
		tag := r.u32()
		if r.err != nil {
			return r.err
		}
		if err := skipRuleProperty(r, uint16(tag), depth); err != nil {
			return err
		}
	}
	return r.err
}

func skipRuleProperty(r *reader, kind uint16, depth int) error {
	if depth > maxRuleNesting {
		return ErrUnsupported
	}
	// A multivalue instance has the scalar representation.
	if kind&0x3000 == 0x3000 {
		kind &^= 0x3000
	}
	switch kind {
	case 0x0000: // TypedPropertyValue.
		typ := r.take(2)
		if typ == nil {
			return r.err
		}
		return skipRuleProperty(r, binary.LittleEndian.Uint16(typ), depth+1)
	case 0x000d, 0x00fb: // Object and ServerId carry a uint16 length.
		skipScalarPropertyValue(r, 0x0102)
	case 0x00fd:
		return skipRuleRestriction(r, depth+1)
	case 0x00fe:
		return skipRuleActions(r)
	case 0x1002, 0x1003, 0x1004, 0x1005, 0x1006, 0x1007, 0x1014, 0x101e, 0x101f, 0x1040, 0x1048, 0x1102:
		count := r.u32()
		for ; count > 0 && r.err == nil; count-- {
			skipScalarPropertyValue(r, kind&^0x1000)
		}
	default:
		if !skipScalarPropertyValue(r, kind) {
			return ErrUnsupported
		}
	}
	return r.err
}

func skipRuleRestriction(r *reader, depth int) error {
	if depth > maxRuleNesting {
		return ErrUnsupported
	}
	op := r.take(1)
	if op == nil {
		return r.err
	}
	switch op[0] {
	case 0, 1: // And, Or.
		count := r.take(2)
		if count == nil {
			return r.err
		}
		for range int(binary.LittleEndian.Uint16(count)) {
			if err := skipRuleRestriction(r, depth+1); err != nil {
				return err
			}
		}
	case 2: // Not.
		return skipRuleRestriction(r, depth+1)
	case 3: // Content.
		r.take(8)
		return skipRuleProperties(r, 1, depth)
	case 4: // Property.
		r.take(5)
		return skipRuleProperties(r, 1, depth)
	case 5, 6, 7: // CompareProperties, BitMask, Size.
		r.take(9)
	case 8: // Exist.
		r.take(4)
	case 9, 11: // SubRestriction, Count.
		if r.take(4) == nil {
			return r.err
		}
		return skipRuleRestriction(r, depth+1)
	case 10, 12: // Comment, Gromox Annotation.
		count := r.take(1)
		if count == nil {
			return r.err
		}
		if count[0] == 0 {
			return ErrMalformed
		}
		if err := skipRuleProperties(r, int(count[0]), depth); err != nil {
			return err
		}
		present := r.take(1)
		if present != nil && present[0] != 0 {
			return skipRuleRestriction(r, depth+1)
		}
	case 0xff: // Gromox null restriction.
	default:
		return ErrUnsupported
	}
	return r.err
}

func skipRuleActions(r *reader) error {
	wire := r.take(2)
	if wire == nil {
		return r.err
	}
	count := int(binary.LittleEndian.Uint16(wire))
	if count == 0 {
		return ErrMalformed
	}
	for range count {
		wire = r.take(2)
		if wire == nil {
			return r.err
		}
		size := int(binary.LittleEndian.Uint16(wire))
		if size < 9 { // ActionType, ActionFlavor and ActionFlags.
			return ErrMalformed
		}
		if r.take(size) == nil {
			return r.err
		}
	}
	return r.err
}
