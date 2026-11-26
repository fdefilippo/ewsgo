// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"net/mail"
	"regexp"
	"strconv"
	"strings"

	"ewsgo/internal/core"
)

var galUUID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)

type GALEntry struct {
	AccountID, Address, DisplayName string
	entryType                       string
}

func (GALEntry) String() string { return "Zimbra GAL entry [private fields redacted]" }

// GAL uses only the user's account SOAP token. No admin endpoint, delegated
// mailbox selector, cached contacts or guessed self entry is involved. This
// bounded spike deliberately refuses partial/external catalogs it cannot map.
func (s *Session) GAL(ctx context.Context) ([]GALEntry, error) {
	request := element(accountNS, "SearchGalRequest", "")
	for _, a := range [][2]string{{"name", ""}, {"type", "all"}, {"limit", strconv.Itoa(core.MaxDirectoryEntries)}, {"offset", "0"}, {"sortBy", "nameAsc"}} {
		request = attr(request, a[0], a[1])
	}
	result, err := s.request(ctx, accountNS, "SearchGal", "", request)
	if err != nil {
		return nil, err
	}
	// Missing metadata is not evidence that the result is complete. Paging is
	// not implemented here; even one truncated page fails as a whole.
	if result.get("more") == "1" || result.get("more") == "true" {
		return nil, ErrLimit
	}
	if (result.get("more") != "0" && result.get("more") != "false") || (result.get("offset") != "" && result.get("offset") != "0") {
		return nil, ErrProtocol
	}
	seen := make(map[string]bool)
	var out []GALEntry
	for _, cn := range result.Children {
		if cn.XMLName.Space != accountNS || cn.XMLName.Local != "cn" {
			return nil, ErrProtocol
		}
		fields := make(map[string]string)
		uniqueType := true
		for _, a := range cn.Children {
			if a.XMLName.Space != accountNS || a.XMLName.Local != "a" {
				continue // Unused native group membership metadata.
			}
			key := a.get("n")
			if key != "zimbraId" && key != "email" && key != "fullName" && key != "type" {
				continue
			}
			if _, duplicate := fields[key]; duplicate {
				if key != "type" {
					return nil, ErrProtocol
				}
				uniqueType = false
			}
			fields[key] = a.Text
		}
		id, address := strings.ToLower(fields["zimbraId"]), fields["email"]
		parsed, err := mail.ParseAddress(address)
		if !galUUID.MatchString(id) || id == "00000000-0000-0000-0000-000000000000" || seen[id] || err != nil || parsed.Address != address || len(address) > 320 || len(fields["fullName"]) > 1024 {
			return nil, ErrProtocol
		}
		seen[id] = true
		entryType := fields["type"]
		if !uniqueType {
			entryType = "ambiguous"
		}
		out = append(out, GALEntry{id, address, fields["fullName"], entryType})
		if len(out) > core.MaxDirectoryEntries {
			return nil, ErrLimit
		}
	}
	return out, nil
}

type nativeDirectory struct{ session *Session }

func (s *accountSession) Directory() core.DirectoryReader { return nativeDirectory{s.native} }

func (d nativeDirectory) Entries(ctx context.Context) ([]core.DirectoryEntry, error) {
	entries, err := d.session.GAL(ctx)
	if err != nil {
		return nil, semanticError(err)
	}
	out := make([]core.DirectoryEntry, 0, len(entries))
	for _, e := range entries {
		out = append(out, core.DirectoryEntry{Identity: core.Identity{Scope: core.Scope{Backend: "zimbra", Account: e.AccountID}, Name: e.Address}, DisplayName: e.DisplayName})
	}
	return out, nil
}
