// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/xml"
	"errors"
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
)

// The index retains bounded metadata and brackets pagination with the native
// folder's contents sequence and count. A failed scan never proves absence.
func (m *mailbox) ReadMailContentIndex(ctx context.Context, folder core.ID) (core.MailContents, error) {
	native, ok := strings.CutPrefix(string(folder), "f:")
	if !ok || !nativeID.MatchString(native) {
		return core.MailContents{}, core.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for range 3 {
		index, err := m.readMailContentIndex(ctx, folder, native)
		if !errors.Is(err, core.ErrConflict) {
			return index, semanticError(err)
		}
	}
	return core.MailContents{}, core.ErrConflict
}

type mailIndexEntry struct {
	id       string
	revision uint64
}

func (m *mailbox) readMailContentIndex(ctx context.Context, folder core.ID, native string) (core.MailContents, error) {
	before, err := m.folderObservationMarker(ctx, native, 0)
	if err != nil {
		return core.MailContents{}, err
	}
	if before.MessageCount > core.MaxMailContentIndexItems {
		return core.MailContents{}, ErrLimit
	}
	index, err := m.native.mailIndexEntries(ctx, native)
	if err != nil {
		return core.MailContents{}, err
	}
	if uint64(len(index)) != before.MessageCount {
		return core.MailContents{}, core.ErrConflict
	}
	result := core.MailContents{Folder: folder, Messages: make([]core.Object, 0, len(index))}
	used := 0
	for start := 0; start < len(index); start += 16 {
		batch := index[start:min(start+16, len(index))]
		items, err := m.native.mailContentMetadataBatch(ctx, batch)
		if err != nil {
			return core.MailContents{}, mailContentRace(err)
		}
		for i, item := range items {
			if item.message.Folder != native || item.message.ContentRevision != batch[i].revision {
				return core.MailContents{}, core.ErrConflict
			}
			bytes := item.bytes()
			used += bytes
			if bytes > 64<<10 || used > core.MaxMailContentViewBytes {
				return core.MailContents{}, ErrLimit
			}
			object := item.object()
			if item.modifiedSeconds == nil {
				stamp, flags, err := m.native.mailContentModified(ctx, item.message.ID)
				if err != nil {
					return core.MailContents{}, mailContentRace(err)
				}
				if flags != item.message.Flags {
					return core.MailContents{}, core.ErrConflict
				}
				object.Properties[core.LastModifiedAt] = core.Timestamp(stamp)
			}
			result.Messages = append(result.Messages, object)
		}
	}
	after, err := m.folderObservationMarker(ctx, native, 0)
	if err != nil {
		return core.MailContents{}, err
	}
	if before != after {
		return core.MailContents{}, core.ErrConflict
	}
	return result, nil
}

func (s *Session) mailIndexEntries(ctx context.Context, folder string) ([]mailIndexEntry, error) {
	const pageSize = 100
	seen := make(map[string]bool)
	var entries []mailIndexEntry
	for {
		offset := strconv.Itoa(len(entries))
		q := element(mailNS, "SearchRequest", "", element(mailNS, "query", "inid:"+folder))
		q = attr(attr(attr(attr(q, "types", "message"), "limit", strconv.Itoa(pageSize)), "offset", offset), "sortBy", "dateDesc")
		page, err := s.request(ctx, mailNS, "Search", "", q)
		if err != nil {
			return nil, err
		}
		if page.get("offset") != offset || len(page.Children) > pageSize {
			return nil, ErrProtocol
		}
		for _, row := range page.Children {
			id := row.get("id")
			revision, ok := syncNumber(row.get("rev"))
			if row.XMLName != (xml.Name{Space: mailNS, Local: "m"}) || !nativeID.MatchString(id) || !ok {
				return nil, ErrProtocol
			}
			if seen[id] || row.get("l") != folder {
				return nil, core.ErrConflict
			}
			seen[id] = true
			entries = append(entries, mailIndexEntry{id, revision})
		}
		if len(entries) > core.MaxMailContentIndexItems {
			return nil, ErrLimit
		}
		switch page.get("more") {
		case "0", "false":
			return entries, nil
		case "1", "true":
			if len(page.Children) == 0 {
				return nil, ErrProtocol
			}
		default:
			return nil, ErrProtocol
		}
	}
}

func (s *Session) mailContentMetadataBatch(ctx context.Context, entries []mailIndexEntry) ([]mailContentMetadata, error) {
	q := attr(element(contextNS, "BatchRequest", ""), "onerror", "stop")
	for i, entry := range entries {
		q.Children = append(q.Children, attr(mailContentMetadataRequest(entry.id), "requestId", strconv.Itoa(i+1)))
	}
	response, err := s.request(ctx, contextNS, "Batch", "", q)
	if err != nil {
		return nil, err
	}
	if len(response.Children) != len(entries) {
		// A concurrent removal can stop a batch before its last response.
		return nil, core.ErrConflict
	}
	items := make([]mailContentMetadata, len(entries))
	for i, child := range response.Children {
		if child.get("requestId") != strconv.Itoa(i+1) {
			return nil, ErrProtocol
		}
		if child.XMLName == (xml.Name{Space: soapNS, Local: "Fault"}) {
			return nil, core.ErrConflict
		}
		item, err := parseMailContentMetadata(child, entries[i].id)
		if err != nil {
			return nil, err
		}
		items[i] = item
	}
	return items, nil
}
