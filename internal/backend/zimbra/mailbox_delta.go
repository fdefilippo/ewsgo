// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"strconv"
	"strings"

	"ewsgo/internal/core"
)

// mailboxDelta contains native change hints for the authenticated mailbox.
// A hint does not establish whether an object is new to the projection.
type mailboxDelta struct {
	Next                            string
	More                            bool
	ChangedMessages, ChangedFolders []string
	DeletedMessages, DeletedFolders []string
	OtherRemovals                   []string
}

func (mailboxDelta) String() string { return "Zimbra mailbox delta [private cursor and IDs redacted]" }

// mailboxDeltaAnchor captures a high-water mark before a separately paginated
// initial enumeration. A nonblocking NoOp supplies it without listing items.
func (s *Session) mailboxDeltaAnchor(ctx context.Context) (string, error) {
	auth, err := s.credential()
	if err != nil {
		return "", semanticError(err)
	}
	request := attr(element(mailNS, "NoOpRequest", ""), "wait", "0")
	_, response, err := s.client.callWithResponseContext(ctx, mailNS, "NoOp", auth, "", request)
	if err != nil {
		return "", semanticError(err)
	}
	if response == nil {
		return "", ErrProtocol
	}
	var anchor string
	for _, child := range response.Children {
		if child.XMLName.Space != contextNS || child.XMLName.Local != "change" || child.get("acct") != "" {
			continue
		}
		token := child.get("token")
		if _, ok := syncNumber(token); anchor != "" || !ok {
			return "", ErrProtocol
		}
		anchor = token
	}
	if anchor == "" {
		return "", ErrProtocol
	}
	return anchor, nil
}

// readMailboxDelta uses the root scope so a move between native folders remains
// a single message change. The complete token travels unchanged between pages.
func (s *Session) readMailboxDelta(ctx context.Context, cursor string, limit int) (mailboxDelta, error) {
	previous, ok := syncPositions(cursor)
	if !ok {
		return mailboxDelta{}, core.ErrResync
	}
	response, err := s.requestSync(ctx, cursor, "1", limit)
	if err != nil {
		return mailboxDelta{}, semanticError(err)
	}
	if response.get("s") != "" {
		// An initial (unpaged) snapshot carries the mailbox size. Its nested
		// item listings cannot be interpreted as an incremental page.
		return mailboxDelta{}, ErrProtocol
	}
	next := response.get("token")
	current, ok := syncPositions(next)
	if !ok {
		return mailboxDelta{}, ErrProtocol
	}
	if current.changes.before(previous.changes) || current.deletions.before(previous.deletions) {
		return mailboxDelta{}, core.ErrResync
	}
	page := mailboxDelta{Next: next}
	switch response.get("more") {
	case "1", "true":
		page.More = true
	case "", "0", "false":
	default:
		return mailboxDelta{}, ErrProtocol
	}

	messages, folders := map[string]bool{}, map[string]bool{}
	seenDeletes := false
	for _, entry := range response.Children {
		if entry.XMLName.Space != mailNS {
			continue
		}
		switch entry.XMLName.Local {
		case "m", "folder":
			id := entry.get("id")
			if !nativeID.MatchString(id) {
				return mailboxDelta{}, ErrProtocol
			}
			if entry.XMLName.Local == "m" {
				page.ChangedMessages = appendSyncID(page.ChangedMessages, messages, id)
			} else {
				page.ChangedFolders = appendSyncID(page.ChangedFolders, folders, id)
			}
		case "deleted":
			if seenDeletes {
				return mailboxDelta{}, ErrProtocol
			}
			seenDeletes = true
			if err := page.readDeletions(entry, limit); err != nil {
				return mailboxDelta{}, err
			}
		}
	}
	for _, id := range page.DeletedMessages {
		if messages[id] {
			return mailboxDelta{}, core.ErrResync
		}
	}
	for _, id := range page.DeletedFolders {
		if folders[id] {
			return mailboxDelta{}, core.ErrResync
		}
	}
	hasChanges := len(page.ChangedMessages)+len(page.ChangedFolders)+len(page.DeletedMessages)+len(page.DeletedFolders)+len(page.OtherRemovals) > 0
	if current == previous && (page.More || hasChanges) {
		return mailboxDelta{}, ErrProtocol
	}
	return page, nil
}

func (page *mailboxDelta) readDeletions(entry node, limit int) error {
	// The aggregate includes other item types as well. Only typed evidence
	// identifies a message or folder deletion. Keep unclassified positive
	// references for membership checks by the projection.
	all := map[string]bool{}
	var order []string
	if value := entry.get("ids"); value != "" {
		for id := range strings.SplitSeq(value, ",") {
			if !nativeID.MatchString(strings.TrimPrefix(id, "-")) {
				return ErrProtocol
			}
			order = appendSyncID(order, all, id)
			if len(order) > limit {
				return ErrProtocol
			}
		}
	}
	typed := map[string]string{}
	for _, child := range entry.Children {
		if child.XMLName.Space != mailNS || child.XMLName.Local != "m" && child.XMLName.Local != "folder" {
			continue
		}
		for id := range strings.SplitSeq(child.get("ids"), ",") {
			kind := child.XMLName.Local
			if !nativeID.MatchString(id) || len(all) > 0 && !all[id] {
				return ErrProtocol
			}
			if before, exists := typed[id]; exists {
				if before != kind {
					return ErrProtocol
				}
				continue
			}
			typed[id] = kind
			if len(typed) > limit {
				return ErrProtocol
			}
			if kind == "m" {
				page.DeletedMessages = append(page.DeletedMessages, id)
			} else {
				page.DeletedFolders = append(page.DeletedFolders, id)
			}
		}
	}
	for _, id := range order {
		if typed[id] == "" && nativeID.MatchString(id) {
			page.OtherRemovals = append(page.OtherRemovals, id)
		}
	}
	return nil
}

func appendSyncID(ids []string, seen map[string]bool, id string) []string {
	if !seen[id] {
		ids = append(ids, id)
		seen[id] = true
	}
	return ids
}

func syncNumber(value string) (uint64, bool) {
	if !nativeID.MatchString(value) {
		return 0, false
	}
	parsed, err := strconv.ParseUint(value, 10, 31)
	return parsed, err == nil
}

type syncPosition struct {
	base, offset uint64
}

func (p syncPosition) before(previous syncPosition) bool {
	return p.base < previous.base || p.base == previous.base && p.offset < previous.offset
}

func parseSyncPosition(wire string) (syncPosition, bool) {
	base, offset, hasOffset := strings.Cut(wire, "-")
	parsed, ok := syncNumber(base)
	if !ok {
		return syncPosition{}, false
	}
	if !hasOffset {
		return syncPosition{base: parsed}, true
	}
	item, ok := syncNumber(offset)
	return syncPosition{base: parsed, offset: item}, ok
}

type syncTokenPositions struct {
	changes, deletions syncPosition
}

func syncPositions(token string) (syncTokenPositions, bool) {
	changes, deletes, hasDeletes := strings.Cut(token, ":d")
	modified, ok := parseSyncPosition(changes)
	if !ok {
		return syncTokenPositions{}, false
	}
	if !hasDeletes {
		// Without an explicit delete cursor, Zimbra uses the base change
		// sequence for deletions, without the offset inside a change.
		return syncTokenPositions{modified, syncPosition{base: modified.base}}, true
	}
	deleted, ok := parseSyncPosition(deletes)
	return syncTokenPositions{modified, deleted}, ok
}
