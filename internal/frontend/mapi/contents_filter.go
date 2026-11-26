// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"cmp"
	"encoding/binary"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"ewsgo/internal/core"
)

const (
	maxContentsRestrictionBytes = 8 << 10
	maxContentsRestrictionNodes = 128
	maxContentsRestrictionDepth = 16
	parentServerIDTag           = 0x0e0900fb // PR_PARENT_SVREID
	instanceServerIDTag         = 0x0ff600fb // PR_INSTANCE_SVREID
)

var parentServerIDProperty = contentsProperty{id: parentServerIDTag >> 16, kind: parentServerIDTag & 0xffff}

// Canonical named identity is independent of the per-store numeric wire ID.
// Fixed properties have id set; named properties have name set instead.
type contentsProperty struct {
	id, kind uint16
	name     propertyName
}
type contentsValues map[contentsProperty]core.Value
type contentsRestriction struct {
	bound    bool
	op, rel  byte
	fuzzy    uint32
	mask     uint32
	tag      uint32
	property contentsProperty
	value    core.Value
	children []*contentsRestriction
}
type contentsResolver func(uint32) (contentsProperty, error)

func (contentsProperty) String() string       { return "contentsProperty{private}" }
func (contentsProperty) GoString() string     { return "contentsProperty{private}" }
func (contentsValues) String() string         { return "contentsValues{private}" }
func (contentsValues) GoString() string       { return "contentsValues{private}" }
func (*contentsRestriction) String() string   { return "contentsRestriction{private}" }
func (*contentsRestriction) GoString() string { return "contentsRestriction{private}" }

// Pure bounded ROP codec, MS-OXCDATA 2.12. No backend access or registry writes.
func parseContentsRestriction(raw []byte) (*contentsRestriction, error) {
	return parseContentsRestrictionText(raw, nil)
}

func parseContentsRestrictionText(raw []byte, text *searchRestrictionText) (*contentsRestriction, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	if len(raw) > maxContentsRestrictionBytes {
		return nil, restrictionBudget
	}
	r := reader{data: raw}
	nodes := 0
	root, err := readContentsRestriction(&r, 1, &nodes, text)
	if err != nil {
		return nil, err
	}
	if r.done() != nil {
		return nil, ErrMalformed
	}
	return root, nil
}

func readContentsRestriction(r *reader, depth int, nodes *int, text *searchRestrictionText) (*contentsRestriction, error) {
	if depth > maxContentsRestrictionDepth || *nodes >= maxContentsRestrictionNodes {
		return nil, restrictionBudget
	}
	(*nodes)++
	h := r.take(1)
	if r.err != nil {
		return nil, ErrMalformed
	}
	n := &contentsRestriction{op: h[0]}
	switch n.op {
	case 0, 1: // AND/OR, ROP COUNT is uint16, not extended-rules uint32.
		wire := r.take(2)
		if r.err != nil {
			return nil, ErrMalformed
		}
		count := int(binary.LittleEndian.Uint16(wire))
		if count > maxContentsRestrictionNodes-*nodes {
			return nil, restrictionBudget
		}
		for i := 0; i < count; i++ {
			child, err := readContentsRestriction(r, depth+1, nodes, text)
			if err != nil {
				return nil, err
			}
			n.children = append(n.children, child)
		}
	case 2:
		child, err := readContentsRestriction(r, depth+1, nodes, text)
		if err != nil {
			return nil, err
		}
		n.children = []*contentsRestriction{child}
	case 3, 4:
		if n.op == 3 {
			n.fuzzy = r.u32()
		} else {
			v := r.take(1)
			if r.err != nil {
				return nil, ErrMalformed
			}
			n.rel = v[0]
		}
		tagStart := r.pos
		n.tag = r.u32()
		valueTag := r.u32()
		if r.err != nil {
			return nil, ErrMalformed
		}
		if uint16(n.tag) != uint16(valueTag) {
			return nil, ErrMalformed
		}
		n.tag = text.normalizeTag(n.tag)
		kind := uint16(n.tag)
		if !contentsScalarType(kind) && kind != 0xfb {
			return nil, rejectedRestrictionType(n.op, kind)
		}
		if n.op == 3 {
			// FULLSTRING/SUBSTRING/PREFIX, exact or Unicode simple case fold.
			// Nonspacing/loose and multivalue require a separate complete slice.
			switch {
			case kind != 0x1f && kind != 0x102:
				return nil, restrictionContentType
			case n.fuzzy&0xffff > 2:
				return nil, restrictionFuzzyMode
			case n.fuzzy&0x20000 != 0:
				return nil, restrictionFuzzyNonspacing
			case n.fuzzy&0x40000 != 0:
				return nil, restrictionFuzzyLoose
			case n.fuzzy & ^uint32(0x00010003) != 0:
				return nil, restrictionFuzzyBits
			case kind == 0x102 && n.fuzzy>>16 != 0:
				return nil, restrictionBinaryFuzzy
			}
		} else if n.rel > 5 || ((kind == 0xb || kind == 0xfb) && n.rel != 4 && n.rel != 5) {
			return nil, restrictionComparison
		}
		var err error
		if kind == 0xfb {
			n.value, err = readContentsValue(r, 0x102)
			if err == nil && !validScalarServerID(n.value.(core.Binary)) {
				return nil, ErrMalformed
			}
		} else if uint16(valueTag) == 0x1e {
			n.value, err = text.readString8(r)
		} else {
			n.value, err = readContentsValue(r, kind)
		}
		if err != nil {
			return nil, err
		}
		if kind == 0x1f {
			text.remember(tagStart, r.pos, n.value.(core.Text), true)
		}
	case 6: // MS-OXCDATA 2.12.7.1: single-valued Int32, EQZ or NEZ.
		wire := r.take(9)
		if r.err != nil {
			return nil, ErrMalformed
		}
		n.rel = wire[0]
		n.tag = binary.LittleEndian.Uint32(wire[1:])
		n.mask = binary.LittleEndian.Uint32(wire[5:])
		if n.rel > 1 || uint16(n.tag) != 3 {
			return nil, restrictionBitMask
		}
	case 8:
		tagStart := r.pos
		n.tag = text.normalizeTag(r.u32())
		if r.err != nil {
			return nil, ErrMalformed
		}
		if kind := uint16(n.tag); !contentsScalarType(kind) && kind != 0xfb {
			return nil, rejectedRestrictionType(n.op, kind)
		}
		if uint16(n.tag) == 0x1f {
			text.remember(tagStart, r.pos, "", false)
		}
	default:
		return nil, rejectedRestrictionOperator(n.op)
	}
	return n, nil
}

func contentsScalarType(kind uint16) bool {
	switch kind {
	case 2, 3, 0x14, 0xb, 0x1f, 0x40, 0x102:
		return true
	}
	return false
}

func readContentsValue(r *reader, kind uint16) (core.Value, error) {
	var size int
	switch kind {
	case 2:
		size = 2
	case 3:
		size = 4
	case 0x14, 0x40:
		size = 8
	case 0xb:
		size = 1
	case 0x1f:
		text, err := createFolderString(r, maxContentsRestrictionBytes/2)
		return core.Text(text), err
	case 0x102:
		wire := r.take(2)
		if r.err != nil {
			return nil, ErrMalformed
		}
		data := r.take(int(binary.LittleEndian.Uint16(wire)))
		if r.err != nil {
			return nil, ErrMalformed
		}
		return core.Binary(bytes.Clone(data)), nil
	default:
		return nil, ErrUnsupported
	}
	data := r.take(size)
	if r.err != nil {
		return nil, ErrMalformed
	}
	switch kind {
	case 2:
		return core.Integer(int16(binary.LittleEndian.Uint16(data))), nil
	case 3:
		return core.Integer(int32(binary.LittleEndian.Uint32(data))), nil
	case 0x14:
		return core.Integer(int64(binary.LittleEndian.Uint64(data))), nil
	case 0xb:
		if data[0] > 1 {
			return nil, ErrMalformed
		}
		return core.Bool(data[0] != 0), nil
	case 0x40:
		v := binary.LittleEndian.Uint64(data)
		// FILETIME range 1601 through 9999, rejecting overflow before int64.
		if v > 2650467743999999999 {
			return nil, ErrMalformed
		}
		return core.Timestamp(time.Unix(int64(v/10000000)-11644473600, int64(v%10000000)*100).UTC()), nil
	}
	return nil, ErrUnsupported
}

func validScalarServerID(data []byte) bool {
	// MS-OXCDATA 2.11.1.4. The scalar form has a zero instance number.
	return len(data) > 0 && (data[0] == 0 ||
		data[0] == 1 && len(data) == 21 && binary.LittleEndian.Uint32(data[17:]) == 0)
}

// Binding is pure and returns a separate tree. A schema resolver must reject
// unknown/unprojectable properties even if they occur in a short-circuited arm.
func bindContentsRestriction(n *contentsRestriction, resolve contentsResolver) (*contentsRestriction, error) {
	if n == nil {
		return nil, nil
	}
	if resolve == nil {
		return nil, core.ErrInvalid
	}
	out := *n
	out.bound = true
	out.children = nil
	if n.op == 3 || n.op == 4 || n.op == 6 || n.op == 8 {
		p, err := resolve(n.tag)
		if err != nil {
			return nil, err
		}
		if p.kind != uint16(n.tag) || (p.id == 0 && p.name == (propertyName{})) {
			return nil, core.ErrInvalid
		}
		out.property = p
		out.tag = 0
	}
	for _, child := range n.children {
		bound, err := bindContentsRestriction(child, resolve)
		if err != nil {
			return nil, err
		}
		out.children = append(out.children, bound)
	}
	return &out, nil
}

// resolveContentsProperty never creates a name. The caller's closed schema
// distinguishes a known-but-absent property from an unsupported property.
func resolveContentsProperty(tag uint32, registry *namedPropertyRegistry, supports func(contentsProperty) bool) (contentsProperty, error) {
	if !contentsScalarType(uint16(tag)) {
		return contentsProperty{}, core.ErrUnsupported
	}
	p, err := resolveContentsIdentity(tag, registry)
	if err != nil {
		return contentsProperty{}, err
	}
	if supports == nil || !supports(p) {
		return contentsProperty{}, core.ErrUnsupported
	}
	return p, nil
}

func resolveContentsIdentity(tag uint32, registry *namedPropertyRegistry) (contentsProperty, error) {
	p := contentsProperty{id: uint16(tag >> 16), kind: uint16(tag)}
	if p.id == 0 || p.id == 0x8000 || p.id == 0xffff {
		return contentsProperty{}, core.ErrUnsupported
	}
	if p.id >= 0x8001 {
		if registry == nil {
			return contentsProperty{}, core.ErrUnsupported
		}
		registry.mu.Lock()
		name, ok := registry.registeredName(p.id)
		registry.mu.Unlock()
		if !ok {
			return contentsProperty{}, core.ErrUnsupported
		}
		p.name = name
		p.id = 0
	}
	return p, nil
}

func (n *contentsRestriction) matches(values contentsValues) (bool, error) {
	return n.matchesInFolder(values, 0)
}

func (n *contentsRestriction) matchesInFolder(values contentsValues, parent uint64) (bool, error) {
	if n == nil {
		return true, nil
	}
	if !n.bound {
		return false, core.ErrInvalid
	}
	switch n.op {
	case 0, 1:
		result := n.op == 0
		for _, child := range n.children {
			v, err := child.matchesInFolder(values, parent)
			if err != nil {
				return false, err
			}
			if n.op == 0 {
				result = result && v
			} else {
				result = result || v
			}
		}
		return result, nil
	case 2:
		v, err := n.children[0].matchesInFolder(values, parent)
		if err != nil {
			return false, err
		}
		return !v, nil
	}
	if n.property == parentServerIDProperty {
		return n.matchesParentServerID(parent), nil
	}
	v, exists := values[n.property]
	if !exists {
		return false, nil
	}
	if !contentsValueType(n.property.kind, v) {
		return false, core.ErrInvalid
	}
	if n.op == 8 {
		return true, nil
	}
	if n.op == 6 {
		value, ok := v.(core.Integer)
		if !ok || n.property.kind != 3 || n.rel > 1 {
			return false, core.ErrInvalid
		}
		// Preserve all 32 bits, including the sign bit of PtypInteger32.
		zero := uint32(value)&n.mask == 0
		return zero == (n.rel == 0), nil
	}
	if n.op == 3 {
		var haystack, needle []byte
		switch value := v.(type) {
		case core.Text:
			constant, ok := n.value.(core.Text)
			if !ok {
				return false, core.ErrInvalid
			}
			a, b := string(value), string(constant)
			if n.fuzzy&0x10000 != 0 || n.property.id == 0x1a {
				a, b = foldContentsText(a), foldContentsText(b)
			}
			haystack, needle = []byte(a), []byte(b)
		case core.Binary:
			constant, ok := n.value.(core.Binary)
			if !ok {
				return false, core.ErrInvalid
			}
			haystack, needle = value, constant
		default:
			return false, core.ErrInvalid
		}
		switch n.fuzzy & 0xffff {
		case 0:
			return bytes.Equal(haystack, needle), nil
		case 1:
			return bytes.Contains(haystack, needle), nil
		case 2:
			return bytes.HasPrefix(haystack, needle), nil
		}
	}
	if n.op != 4 {
		return false, core.ErrInvalid
	}
	constant := n.value
	// MessageClass comparisons are case-insensitive even without a fuzzy
	// flag (MS-OXCMSG 2.2.1.3). Keep the original stored value unchanged.
	if n.property.id == 0x1a && n.property.kind == 0x1f {
		text, ok := constant.(core.Text)
		if !ok {
			return false, core.ErrInvalid
		}
		v = core.Text(foldContentsText(string(v.(core.Text))))
		constant = core.Text(foldContentsText(string(text)))
	}
	c, err := compareContentsValues(n.property.kind, v, constant)
	if err != nil {
		return false, err
	}
	switch n.rel {
	case 0:
		return c < 0, nil
	case 1:
		return c <= 0, nil
	case 2:
		return c > 0, nil
	case 3:
		return c >= 0, nil
	case 4:
		return c == 0, nil
	case 5:
		return c != 0, nil
	}
	return false, core.ErrInvalid
}

func (n *contentsRestriction) matchesParentServerID(parent uint64) bool {
	if parent == 0 {
		return false
	}
	if n.op == 8 {
		return true
	}
	// A folder reference has a zero MID and a zero instance number.
	data := [21]byte{1}
	binary.LittleEndian.PutUint64(data[1:], parent)
	equal := bytes.Equal(data[:], n.value.(core.Binary))
	return equal == (n.rel == 4)
}

// Use a canonical Unicode simple-fold representative rather than ToLower:
// Greek final sigma and Kelvin sign must match their equivalent case forms.
func foldContentsText(v string) string {
	return strings.Map(func(r rune) rune {
		lowest := r
		for next := unicode.SimpleFold(r); next != r; next = unicode.SimpleFold(next) {
			if next < lowest {
				lowest = next
			}
		}
		return lowest
	}, v)
}

func contentsValueType(kind uint16, v core.Value) bool {
	switch value := v.(type) {
	case core.Integer:
		return kind == 0x14 || (kind == 2 && value >= -32768 && value <= 32767) || (kind == 3 && value >= -2147483648 && value <= 2147483647)
	case core.Bool:
		return kind == 0xb
	case core.Text:
		return kind == 0x1f && utf8.ValidString(string(value)) && !strings.ContainsRune(string(value), 0)
	case core.Binary:
		return kind == 0x102
	case core.Int32s:
		return kind == 0x1003
	case core.Timestamp:
		return kind == 0x40 && time.Time(value).Year() >= 1601 && time.Time(value).Year() <= 9999
	}
	return false
}

func compareContentsValues(kind uint16, a, b core.Value) (int, error) {
	if !contentsValueType(kind, a) || !contentsValueType(kind, b) {
		return 0, core.ErrInvalid
	}
	switch v := a.(type) {
	case core.Integer:
		return cmp.Compare(v, b.(core.Integer)), nil
	case core.Bool:
		if v == b.(core.Bool) {
			return 0, nil
		}
		if v {
			return 1, nil
		}
		return -1, nil
	case core.Text:
		return strings.Compare(string(v), string(b.(core.Text))), nil
	case core.Binary:
		return bytes.Compare(v, b.(core.Binary)), nil
	case core.Timestamp:
		return time.Time(v).Compare(time.Time(b.(core.Timestamp))), nil
	}
	return 0, core.ErrInvalid
}
