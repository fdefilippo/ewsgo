// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"unicode/utf16"

	"ewsgo/internal/core"
)

// The client supplies both subject components (MS-OXCMSG 2.2.1.9–10).
// Missing components initially mean empty text. There is no RE/FW guessing.
// The complete subject is their concatenation (2.2.1.46). These properties
// belong only to authorized local FAI drafts, never native mail writers.
func applyFAISubject(next core.Properties, values faiSetRequest) error {
	partsChanged, fullSupplied := false, false
	for _, v := range values {
		partsChanged = partsChanged || v.tag == 0x003d001f || v.tag == 0x0e1d001f
		fullSupplied = fullSupplied || v.tag == 0x0037001f
	}
	if partsChanged {
		prefix, normalized := core.Text(""), core.Text("")
		if v, ok := next[core.Key{ID: 0x3d}]; ok {
			var valid bool
			prefix, valid = v.(core.Text)
			if !valid {
				return core.ErrInvalid
			}
		}
		if v, ok := next[core.Key{ID: 0xe1d}]; ok {
			var valid bool
			normalized, valid = v.(core.Text)
			if !valid {
				return core.ErrInvalid
			}
		}
		if !validFAISubjectParts(prefix, normalized) {
			return core.ErrInvalid
		}
		full := prefix + normalized
		if fullSupplied && next[core.Subject] != full {
			return core.ErrInvalid
		}
		next[core.Key{ID: 0x3d}], next[core.Key{ID: 0xe1d}], next[core.Subject] = prefix, normalized, full
	}
	return validateFAISubject(next)
}

func validFAISubjectParts(prefix, normalized core.Text) bool {
	if !contentsValueType(0x1f, prefix) || !contentsValueType(0x1f, normalized) {
		return false
	}
	units := 0
	for _, value := range []core.Text{prefix, normalized} {
		for _, r := range value {
			units += utf16.RuneLen(r)
			if units >= 254 {
				return false
			}
		}
	}
	return true
}

func validateFAISubject(properties core.Properties) error {
	p, hasPrefix := properties[core.Key{ID: 0x3d}]
	n, hasNormalized := properties[core.Key{ID: 0xe1d}]
	if !hasPrefix && !hasNormalized {
		return nil // Historical FAI records need not carry subject components.
	}
	prefix, pok := p.(core.Text)
	normalized, nok := n.(core.Text)
	if !pok || !nok || !validFAISubjectParts(prefix, normalized) || properties[core.Subject] != prefix+normalized {
		return core.ErrInvalid
	}
	return nil
}
