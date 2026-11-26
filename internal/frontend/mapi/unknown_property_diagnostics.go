// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import "fmt"

const maxUnknownPropertyTags = 16

// EnableUnknownPropertyTrace enables the owner-approved temporary diagnostic.
// Configure before serving. The service requires Outlook laboratory tracing,
// the RAM store spike and TLS; this does not enable any property values.
func (t *SessionTransport) EnableUnknownPropertyTrace() { t.unknownPropertyTrace = true }

type unknownPropertyTrace struct {
	enabled                        bool
	tags                           [maxUnknownPropertyTags]uint32
	count                          int
	overflow, highIDs, reservedIDs int
}

// GetProperties callers select IDs outside the forty-name vocabulary. Table
// profile diagnostics also use this shared budget for rejected public tags.
func (d *unknownPropertyTrace) record(tag uint32) {
	if !d.enabled {
		return
	}
	id := uint16(tag >> 16)
	switch {
	case id == 0 || id == 0xffff:
		d.reservedIDs++
	case id >= 0x8000:
		d.highIDs++
	default:
		for _, previous := range d.tags[:d.count] {
			if previous == tag {
				return
			}
		}
		if d.count == len(d.tags) {
			d.overflow++ // Occurrences, not distinct omitted tags.
			return
		}
		d.tags[d.count] = tag
		d.count++
	}
}

// UnknownPropertyDiagnostic is a detached snapshot of bounded technical tags.
// High/reserved IDs never reach Tags. Values and identities cannot enter it.
type UnknownPropertyDiagnostic struct {
	Tags                           []string
	Overflow, HighIDs, ReservedIDs int
}

// UnknownProperties exposes only server-formatted hexadecimal ID/type pairs.
// The zero/default diagnostic contains no request tags, including internally.
func (d StorePropertyDiagnostic) UnknownProperties() UnknownPropertyDiagnostic {
	return d.unknown.fields()
}

func (d unknownPropertyTrace) fields() UnknownPropertyDiagnostic {
	out := UnknownPropertyDiagnostic{Tags: []string{}, Overflow: d.overflow, HighIDs: d.highIDs, ReservedIDs: d.reservedIDs}
	for _, tag := range d.tags[:d.count] {
		out.Tags = append(out.Tags, fmt.Sprintf("0x%08x", tag))
	}
	return out
}
