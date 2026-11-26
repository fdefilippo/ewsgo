// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/xml"
	"errors"
	"math"
	"strconv"
	"strings"

	"ewsgo/internal/core"
)

func (m *mailbox) queryMessages(ctx context.Context, q core.Query) (core.Page, error) {
	query := "is:anywhere"
	if q.Parent != "" {
		folder, ok := parseDeltaObject(q.Parent)
		if !ok || folder.kind != core.Folder {
			return core.Page{}, core.ErrInvalid
		}
		tree, err := m.ownFolderTree(ctx)
		if err != nil {
			return core.Page{}, semanticError(err)
		}
		if _, err := tree.mailFolder(folder.ref); err != nil {
			return core.Page{}, err
		}
		query = "inid:" + folder.ref
	}
	limit := q.Limit
	if limit == 0 {
		limit = 100
	}
	if q.Offset > math.MaxInt32-limit {
		return core.Page{}, core.ErrInvalid
	}
	hits, more, err := m.native.searchMessagePage(ctx, query, q.Offset, 0, limit)
	if err != nil {
		return core.Page{}, err
	}
	result := core.Page{NextOffset: q.Offset + len(hits), More: more}
	for _, hit := range hits {
		parent := core.ID("f:" + hit.parent)
		if q.Parent != "" && q.Parent != parent {
			return core.Page{}, ErrProtocol
		}
		object, err := m.getMessageSummary(ctx, hit.id)
		if errors.Is(err, core.ErrNotFound) {
			return core.Page{}, core.ErrConflict
		}
		if err != nil {
			return core.Page{}, err
		}
		if object.Parent != parent {
			return core.Page{}, core.ErrConflict
		}
		if _, ok := object.Properties[core.MetadataRevision]; !ok {
			return core.Page{}, ErrProtocol
		}
		if _, ok := object.Properties[core.ContentRevision]; !ok {
			return core.Page{}, ErrProtocol
		}
		result.Objects = append(result.Objects, object)
	}
	return result, nil
}

func (m *mailbox) getMessageSummary(ctx context.Context, ref string) (core.Object, error) {
	item, err := m.native.getMessageNode(ctx, ref, "8192")
	if err != nil {
		return core.Object{}, semanticError(err)
	}
	return messageSummary(item)
}

func messageSummary(item node) (core.Object, error) {
	if !nativeID.MatchString(item.get("l")) {
		return core.Object{}, ErrProtocol
	}
	message := messageFromNode(item)
	object := messageObject(message)
	projectMeetingMessage(object.Properties, item)
	// Optional header projections must not prevent reading the original MIME
	// when Zimbra returns an unsupported address group or unusual header.
	if recipients, err := mailContentRecipients(item); err == nil {
		object.Recipients = recipients
	}
	if conversation := item.get("cid"); conversation != "" {
		// Negative conversation IDs are valid for Zimbra singleton threads.
		// The special -1 value means there is no conversation.
		value, err := strconv.ParseInt(conversation, 10, 32)
		if err == nil && value != -1 && value != 0 {
			object.Properties[core.ConversationID] = core.Text(conversation)
		}
	}
	object.Properties[core.MailCategories] = zimbraTagNames(item.get("tn"))
	projectMessageMetadata(object, ewsgoMessageMetadata(item))
	for _, field := range []struct {
		name string
		key  core.Key
	}{
		{"ms", core.MetadataRevision},
		{"rev", core.ContentRevision},
		{"d", core.ReceivedAt},
		{"s", core.MessageSize},
	} {
		value, err := optionalFolderNumber(item, field.name)
		if err != nil || field.name == "d" && value != nil && *value > 253402300799999 {
			return core.Object{}, ErrProtocol
		}
		if value == nil {
			delete(object.Properties, field.key)
		} else if field.name == "ms" {
			object.Properties[field.key] = core.Integer(*value)
		}
	}
	return object, nil
}

// zimbraTagNames reads comma-delimited SOAP tag names. Zimbra escapes a
// literal comma or backslash with a backslash.
func zimbraTagNames(encoded string) core.Strings {
	tags := make(core.Strings, 0)
	var tag strings.Builder
	escaped := false
	for _, c := range encoded {
		switch {
		case escaped:
			tag.WriteRune(c)
			escaped = false
		case c == '\\':
			escaped = true
		case c == ',':
			if name := tag.String(); name != "" {
				tags = append(tags, name)
			}
			tag.Reset()
		default:
			tag.WriteRune(c)
		}
	}
	if name := tag.String(); name != "" {
		tags = append(tags, name)
	}
	return tags
}

type messageSearchHit struct {
	id, parent string
	number     uint64
}

// searchMessagePage uses Zimbra's numeric order with either the common offset
// API or an internal keyset continuation for snapshot scans.
func (s *Session) searchMessagePage(ctx context.Context, query string, offset int, after uint64, limit int) ([]messageSearchHit, bool, error) {
	request := element(mailNS, "SearchRequest", "", element(mailNS, "query", query))
	if after != 0 {
		wire := strconv.FormatUint(after, 10)
		cursor := attr(attr(element(mailNS, "cursor", ""), "id", wire), "sortVal", wire)
		request.Children = append(request.Children, cursor)
	} else {
		request = attr(request, "offset", strconv.Itoa(offset))
	}
	request = attr(attr(request, "types", "message"), "sortBy", "idAsc")
	request = attr(attr(request, "limit", strconv.Itoa(limit)), "includeTagDeleted", "1")
	response, err := s.request(ctx, mailNS, "Search", "", request)
	if err != nil {
		return nil, false, semanticError(err)
	}
	if response.get("sortBy") != "idAsc" || after == 0 && response.get("offset") != strconv.Itoa(offset) {
		return nil, false, ErrProtocol
	}
	var more bool
	switch response.get("more") {
	case "1", "true":
		more = true
	case "0", "false":
	default:
		return nil, false, ErrProtocol
	}
	var hits []messageSearchHit
	last := after
	for _, child := range response.Children {
		id, parent := child.get("id"), child.get("l")
		native, ok := syncNumber(id)
		if child.XMLName != (xml.Name{Space: mailNS, Local: "m"}) || !ok || native <= last ||
			child.get("sf") != id || !nativeID.MatchString(parent) || len(hits) == limit {
			return nil, false, ErrProtocol
		}
		last = native
		hits = append(hits, messageSearchHit{id: id, parent: parent, number: native})
	}
	if more && len(hits) == 0 {
		return nil, false, ErrProtocol
	}
	return hits, more, nil
}
