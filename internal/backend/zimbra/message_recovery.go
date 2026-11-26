// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/xml"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"

	"ewsgo/internal/core"
)

var _ core.RecoverableMailReader = (*mailbox)(nil)
var _ core.RecoverableMailWriter = (*mailbox)(nil)

func (m *mailbox) RecoverMailMessage(ctx context.Context, id, origin, destination core.ID) (writeErr error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: "RecoverMailMessage", Result: core.ActivityOutcome(writeErr), Duration: time.Since(started)})
	}()
	source, sourceOK := parseDeltaObject(id)
	parent, parentOK := parseDeltaObject(origin)
	target, targetOK := parseDeltaObject(destination)
	if !sourceOK || source.kind != core.Message || !parentOK || parent.kind != core.Folder || !targetOK || target.kind != core.Folder {
		return core.ErrInvalid
	}
	if err := m.mailContentFolder(ctx, target.ref); err != nil {
		return semanticError(err)
	}
	if err := m.native.requireVisibleDumpster(ctx); err != nil {
		return err
	}
	before, err := m.native.findDumpsterMessage(ctx, source.ref)
	if err != nil {
		return err
	}
	if before == nil || before.get("l") != parent.ref {
		return core.ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	action := attr(attr(attr(element(mailNS, "action", ""), "id", source.ref), "op", "recover"), "l", target.ref)
	response, err := m.native.request(ctx, mailNS, "ItemAction", "", element(mailNS, "ItemActionRequest", "", action))
	if err != nil {
		return messageWriteError(err)
	}
	ack := response.child(mailNS, "action")
	if len(response.Children) != 1 || ack == nil || ack.get("id") != source.ref || ack.get("op") != "recover" {
		return core.ErrOutcomeUnknown
	}
	// Zimbra recovers atomically, assigning a new active ID without returning
	// it in this acknowledgement. Normal folder sync discovers the new item.
	if after, err := m.native.findDumpsterMessage(ctx, source.ref); err != nil || after != nil {
		return core.ErrOutcomeUnknown
	}
	return nil
}

func (m *mailbox) ReadRecoverableMail(ctx context.Context, folder core.ID) (result []core.Object, readErr error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: "ReadRecoverableMail", Result: core.ActivityOutcome(readErr), Duration: time.Since(started)})
	}()
	query, parent := "is:anywhere", ""
	if folder != "" {
		key, ok := parseDeltaObject(folder)
		if !ok || key.kind != core.Folder {
			return nil, core.ErrInvalid
		}
		parent = key.ref
		if err := m.mailContentFolder(ctx, parent); err != nil {
			return nil, semanticError(err)
		}
		query = "inid:" + parent
	}
	if err := m.native.requireVisibleDumpster(ctx); err != nil {
		return nil, err
	}
	const pageSize = 128
	var after uint64
	for {
		request := element(mailNS, "SearchRequest", "", element(mailNS, "query", query))
		request = attr(attr(attr(attr(request, "inDumpster", "1"), "types", "message"), "sortBy", "idAsc"), "limit", strconv.Itoa(pageSize))
		request = attr(request, "recip", "2") // Sender and primary recipients.
		if after == 0 {
			request = attr(request, "offset", "0")
		} else {
			lastID := strconv.FormatUint(after, 10)
			request.Children = append(request.Children, attr(attr(element(mailNS, "cursor", ""), "id", lastID), "sortVal", lastID))
		}
		page, err := m.native.request(ctx, mailNS, "Search", "", request)
		if err != nil {
			return nil, semanticError(err)
		}
		if page.get("sortBy") != "idAsc" || len(page.Children) > pageSize || after == 0 && page.get("offset") != "0" {
			return nil, ErrProtocol
		}
		if len(result)+len(page.Children) > core.MaxRecoverableMailMessages {
			return nil, core.ErrUnsupported
		}
		for _, child := range page.Children {
			id, source := child.get("id"), child.get("l")
			number, ok := syncNumber(id)
			if child.XMLName != (xml.Name{Space: mailNS, Local: "m"}) || !ok || number <= after || child.get("sf") != id || !nativeID.MatchString(source) || parent != "" && source != parent {
				return nil, ErrProtocol
			}
			object, err := messageSummary(child)
			if err != nil {
				return nil, err
			}
			projectRecoverableEnvelope(child, &object)
			result = append(result, object)
			after = number
		}
		switch page.get("more") {
		case "0", "false":
			m.native.readRecoverableDeletionTimes(ctx, result)
			if err := ctx.Err(); err != nil {
				return nil, err
			}
			return result, nil
		case "1", "true":
			if len(page.Children) == 0 {
				return nil, ErrProtocol
			}
			if len(result) == core.MaxRecoverableMailMessages {
				return nil, core.ErrUnsupported
			}
		default:
			return nil, ErrProtocol
		}
	}
}

func (s *Session) readRecoverableDeletionTimes(ctx context.Context, items []core.Object) {
	if len(items) == 0 {
		return
	}
	started := time.Now()
	headCtx, cancel := context.WithTimeout(ctx, s.client.http.Timeout)
	defer cancel()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: "ReadRecoverableDeletionTimes", Result: core.ActivityOutcome(headCtx.Err()), Duration: time.Since(started)})
	}()

	// Search omits change_date. In the dumpster the native ContentServlet's
	// X-Zimbra-Modified header represents the deletion time.
	var reads sync.WaitGroup
	workers := min(4, len(items))
	for worker := range workers {
		reads.Go(func() {
			for i := worker; i < len(items) && headCtx.Err() == nil; i += workers {
				ref := strings.TrimPrefix(string(items[i].ID), "m:")
				query := url.Values{"id": {ref}, "sync": {"1"}, "dumpster": {"1"}}
				stamp, _, err := s.mailContentInfo(headCtx, http.MethodHead, query)
				if err == nil && stamp.UnixMilli() > 0 {
					items[i].Properties[core.DeletedOn] = core.Timestamp(stamp)
				}
			}
		})
	}
	reads.Wait()
}

func projectRecoverableEnvelope(item node, object *core.Object) {
	object.Properties[core.SentRepresentingName] = core.Text("")
	hasTo := false
	for _, address := range item.Children {
		if address.XMLName != (xml.Name{Space: mailNS, Local: "e"}) {
			continue
		}
		switch address.get("t") {
		case "f":
			name := address.get("p")
			if name == "" {
				name = address.get("d")
			}
			if name == "" {
				name = address.get("a")
			}
			object.Properties[core.SentRepresentingName] = core.Text(clip(name, 255))
		case "t":
			hasTo = true
		}
	}
	if hasTo && len(object.Recipients) == 0 {
		return // The shared recipient parser could not represent this header.
	}
	var names []string
	for _, recipient := range object.Recipients {
		if recipient.Kind != core.RecipientTo {
			continue
		}
		name := recipient.DisplayName
		if name == "" {
			name = recipient.Address
		}
		names = append(names, name)
	}
	object.Properties[core.DisplayTo] = core.Text(clip(strings.Join(names, "; "), 255))
}
