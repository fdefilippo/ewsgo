// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"bytes"
	"strings"
)

// StripDraftReferenceHeader removes the private header and its continuation
// lines from an already parsed MIME message, preserving all other bytes.
func StripDraftReferenceHeader(raw []byte) []byte {
	var filtered []byte
	removed, skipping := false, false
	for offset := 0; offset < len(raw); {
		line, _, _ := bytes.Cut(raw[offset:], []byte{'\n'})
		next := min(offset+len(line)+1, len(raw))
		if len(line) == 0 || len(line) == 1 && line[0] == '\r' {
			if !removed {
				return raw
			}
			return append(filtered, raw[offset:]...)
		}
		if line[0] != ' ' && line[0] != '\t' {
			name, _, _ := bytes.Cut(line, []byte{':'})
			skipping = strings.EqualFold(strings.TrimSpace(string(name)), DraftReferenceHeader)
		}
		if skipping && !removed {
			filtered = append(filtered, raw[:offset]...)
			removed = true
		}
		if removed && !skipping {
			filtered = append(filtered, raw[offset:next]...)
		}
		offset = next
	}
	if removed {
		return filtered
	}
	return raw
}
