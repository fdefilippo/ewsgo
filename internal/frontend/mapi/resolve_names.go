// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"strings"

	"ewsgo/internal/core"
)

const (
	directoryNameUnresolved = 0
	directoryNameAmbiguous  = 1
	directoryNameResolved   = 2
)

type resolveNamesRequest struct {
	state *STAT
	tags  []uint32
	names []string
}

// MS-OXCMAPIHTTP 2.2.5.14 encodes the input names as null-terminated Unicode
// strings regardless of the code page used for String8 properties.
func parseResolveNames(data []byte) (resolveNamesRequest, error) {
	r := reader{data: data}
	var request resolveNamesRequest
	r.u32() // Reserved.
	if present := r.take(1); len(present) != 0 && present[0] != 0 {
		request.state = readSTAT(&r)
	}
	if present := r.take(1); len(present) != 0 && present[0] != 0 {
		count := r.u32()
		if count > MaxPropertyTags {
			return resolveNamesRequest{}, ErrMalformed
		}
		request.tags = make([]uint32, count)
		for i := range request.tags {
			request.tags[i] = r.u32()
		}
	}
	if present := r.take(1); len(present) != 0 && present[0] != 0 {
		count := r.u32()
		if count > MaxDirectoryNames {
			return resolveNamesRequest{}, ErrMalformed
		}
		request.names = make([]string, count)
		for i := range request.names {
			name, err := createFolderString(&r, 4096)
			if err != nil {
				return resolveNamesRequest{}, err
			}
			request.names[i] = name
		}
	}
	r.blob(MaxAuxiliary)
	return request, r.done()
}

type directoryName struct {
	entry          core.DirectoryEntry
	smtp, dn, name string
}

func matchDirectoryName(query string, entries []directoryName) (core.DirectoryEntry, uint32) {
	query = strings.ToLower(strings.TrimSpace(query))
	selector := ""
	typed := strings.TrimPrefix(query, "=")
	if prefix, value, ok := strings.Cut(typed, ":"); ok && (prefix == "smtp" || prefix == "ex") {
		selector, query = prefix, strings.TrimSpace(value)
	} else if strings.HasPrefix(query, "/") {
		selector = "ex"
	} else if strings.Contains(query, "@") {
		selector = "smtp"
	}

	var match core.DirectoryEntry
	result := uint32(directoryNameUnresolved)
	if query == "" {
		return match, result
	}
	for _, row := range entries {
		var matches bool
		switch selector {
		case "ex":
			matches = row.dn == query
		case "smtp":
			matches = row.smtp == query
		default:
			matches = strings.HasPrefix(row.name, query) || strings.HasPrefix(row.smtp, query)
		}
		if matches {
			if result == directoryNameResolved {
				return core.DirectoryEntry{}, directoryNameAmbiguous
			}
			match, result = row.entry, directoryNameResolved
		}
	}
	return match, result
}

func (t *SessionTransport) resolveDirectoryNames(ctx context.Context, session core.Session, state *contextState, request resolveNamesRequest) ([]byte, int) {
	cp := state.codePage
	fail := func(code uint32) ([]byte, int) {
		out := append32(append32(append32(nil, 0), code), cp)
		return append32(append(out, 0, 0), 0), 0
	}
	if request.state == nil || request.names == nil {
		return fail(0x80070057)
	}
	if request.state.ContainerID != 0 {
		return fail(0x80040405) // InvalidBookmark.
	}
	if state.directory == nil {
		return fail(NotSupported)
	}
	cp = request.state.CodePage
	if request.tags == nil {
		request.tags = []uint32{0x0fff0102, 0x0ffe0003, 0x39000003, 0x3001001f, 0x3002001f, 0x3003001f, 0x39fe001f}
	}
	if unsupportedString8Codepage(cp, request.tags) {
		return fail(0x8004011e)
	}
	visible, code := t.visibleDirectory(ctx, session)
	if code != 0 {
		return fail(code)
	}
	entries := make([]directoryName, 0, len(visible))
	for _, entry := range visible {
		entry.MailUser = entry.MailUser || entry.Identity.Scope == session.Identity().Scope
		mapped, _ := t.namespace.Mailbox(entry.Identity) // Validated by visibleDirectory.
		entries = append(entries, directoryName{entry, strings.ToLower(entry.Identity.Name), strings.ToLower(mapped.LegacyDN), strings.ToLower(entry.DisplayName)})
	}
	out := append32(append32(append32(nil, 0), 0), cp)
	out = append32(append(out, 0xff), uint32(len(request.names)))
	matches := make([]core.DirectoryEntry, 0, len(request.names))
	for _, query := range request.names {
		entry, status := matchDirectoryName(query, entries)
		out = append32(out, status)
		if status == directoryNameResolved {
			matches = append(matches, entry)
		}
	}
	out = append32(append(out, 0xff), uint32(len(request.tags)))
	for _, tag := range request.tags {
		out = append32(out, tag)
	}
	out = append32(out, uint32(len(matches)))
	for _, entry := range matches {
		values := make([]addressBookValue, len(request.tags))
		flagged := false
		for i, tag := range request.tags {
			values[i] = t.directoryProperty(entry, tag, cp)
			flagged = flagged || uint16(values[i].tag) == 0x000a
		}
		if flagged {
			out = append(out, 1)
		} else {
			out = append(out, 0)
		}
		for i, value := range values {
			if uint16(request.tags[i]) == 0 {
				out = append(out, byte(value.tag), byte(value.tag>>8))
			}
			if flagged {
				if uint16(value.tag) == 0x000a {
					out = append(out, 0x0a)
				} else {
					out = append(out, 0)
				}
			}
			out = append(out, value.data...)
		}
		if len(out) > MaxBody-4 {
			return fail(TableTooBig)
		}
	}
	return append32(out, 0), len(matches)
}
