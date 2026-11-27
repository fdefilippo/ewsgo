// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/xml"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"ewsgo/internal/core"
)

func (m *mailbox) ReadCalendarSearchItems(ctx context.Context, folder core.ID, role core.SpecialFolderKind) ([]core.Object, error) {
	return m.readCalendarSearchItems(ctx, folder, role, "")
}

func (m *mailbox) readCalendarSearchItems(ctx context.Context, folder core.ID, role core.SpecialFolderKind, nativeIDFilter string) ([]core.Object, error) {
	spec := specialFolderSpec(role)
	var hit, prefix string
	switch role {
	case core.CalendarFolder:
		hit, prefix = "appt", "a:"
	case core.TasksFolder:
		hit, prefix = "task", "t:"
	default:
		return nil, core.ErrUnsupported
	}
	target, err := m.groupwareFolder(ctx, folder, role)
	if err != nil {
		return nil, err
	}
	if nativeIDFilter != "" && !target.validItem(nativeIDFilter) {
		return nil, core.ErrNotFound
	}
	personal := target.itemPrefix == ""
	pointQuery := nativeIDFilter != "" && personal
	var before core.MailFolderMarker
	if personal {
		before, err = m.folderObservationMarker(ctx, target.nativeFolder, role)
	} else {
		before, err = m.sharedContentsMarker(ctx, target.nativeFolder, spec.view)
	}
	if err != nil {
		return nil, semanticError(err)
	}
	if !pointQuery && before.MessageCount > core.MaxMailContentIndexItems {
		return nil, core.ErrLimit
	}
	query := "inid:" + target.searchFolder
	if pointQuery {
		query += " item:" + nativeIDFilter
	}
	items, count, err := m.scanCalendarSearch(ctx, target, folder, role, spec.view, hit, prefix, query, nativeIDFilter, pointQuery)
	if err != nil {
		return nil, err
	}
	var after core.MailFolderMarker
	if personal {
		after, err = m.folderObservationMarker(ctx, target.nativeFolder, role)
	} else {
		after, err = m.sharedContentsMarker(ctx, target.nativeFolder, spec.view)
	}
	if err != nil {
		return nil, semanticError(err)
	}
	// A shared Search on Zimbra returns private rows as well. Compare the
	// raw count with GetFolder before exposing only the authorized entries.
	if before != after || !pointQuery && count != before.MessageCount {
		return nil, core.ErrConflict
	}
	if !personal {
		current, err := m.groupwareFolder(ctx, folder, role)
		if err != nil {
			return nil, err
		}
		if target != current {
			return nil, core.ErrConflict
		}
	}
	return items, ctx.Err()
}

func (m *mailbox) scanCalendarSearch(ctx context.Context, target groupwareTarget, folder core.ID, role core.SpecialFolderKind, view, hit, prefix, query, filter string, point bool) ([]core.Object, uint64, error) {
	const pageSize = 100
	count := 0
	var last uint64
	var items []core.Object
	for {
		offset := strconv.Itoa(count)
		request := element(mailNS, "SearchRequest", "", element(mailNS, "query", query))
		request = attr(attr(attr(attr(request, "types", view), "limit", strconv.Itoa(pageSize)), "offset", offset), "sortBy", "idAsc")
		response, err := m.native.request(ctx, mailNS, "Search", "", request)
		if err != nil {
			return nil, 0, semanticError(err)
		}
		if response.get("offset") != offset || response.get("sortBy") != "idAsc" || len(response.Children) > pageSize {
			return nil, 0, ErrProtocol
		}
		count += len(response.Children)
		if count > core.MaxMailContentIndexItems {
			return nil, 0, core.ErrLimit
		}
		for _, child := range response.Children {
			id := child.get("id")
			if child.XMLName != (xml.Name{Space: mailNS, Local: hit}) || !target.validItem(id) {
				return nil, 0, ErrProtocol
			}
			number, ok := syncNumber(strings.TrimPrefix(id, target.itemPrefix))
			if !ok || number <= last || child.get("l") != target.nativeFolder {
				return nil, 0, core.ErrConflict
			}
			last = number
			if filter != "" && id != filter {
				if point {
					return nil, 0, ErrProtocol
				}
				continue
			}
			if !target.canReadAppointment(child.get("class")) {
				continue
			}
			item, err := calendarSearchItem(child, folder, core.ID(prefix+id), role)
			if err != nil {
				return nil, 0, err
			}
			items = append(items, item)
		}
		switch response.get("more") {
		case "0", "false":
			return items, uint64(count), ctx.Err()
		case "1", "true":
			if len(response.Children) == 0 || point {
				return nil, 0, ErrProtocol
			}
		default:
			return nil, 0, ErrProtocol
		}
	}
}

func calendarSearchItem(n node, folder, id core.ID, role core.SpecialFolderKind) (core.Object, error) {
	properties := core.Properties{}
	title := n.get("name")
	if len(title) > 4096 || !utf8.ValidString(title) {
		return core.Object{}, core.ErrLimit
	}
	properties[core.Subject] = core.Text(title)
	properties[core.IsRead] = core.Bool(!strings.Contains(n.get("f"), "u"))
	modified, err := optionalFolderNumber(n, "md")
	if err != nil || modified == nil || *modified > 253402300799 {
		return core.Object{}, ErrProtocol
	}
	properties[core.LastModifiedAt] = core.Timestamp(time.Unix(int64(*modified), 0).UTC())
	revision, err := optionalFolderNumber(n, "ms")
	if err != nil || revision == nil {
		return core.Object{}, ErrProtocol
	}
	properties[core.MetadataRevision] = core.Integer(*revision)
	content, err := optionalFolderNumber(n, "rev")
	if err != nil || content == nil {
		return core.Object{}, ErrProtocol
	}
	properties[core.ContentRevision] = core.Integer(*content)
	// Zimbra keeps the alarm definition and its alarm flag after dismissal.
	// Search's alarmData describes a reminder that still needs to signal.
	alarm := n.child(mailNS, "alarmData")
	properties[core.ReminderSet] = core.Bool(alarm != nil)
	if alarm != nil {
		next, err := optionalFolderNumber(*alarm, "nextAlarm")
		if err != nil || next == nil || *next == 0 || *next > 253402300799999 {
			return core.Object{}, ErrProtocol
		}
		properties[core.ReminderSignalTime] = core.Timestamp(time.UnixMilli(int64(*next)).UTC())
	}
	if role == core.CalendarFolder {
		switch n.get("recur") {
		case "", "0", "false":
			properties[core.AppointmentRecurring] = core.Bool(false)
		case "1", "true":
			properties[core.AppointmentRecurring] = core.Bool(true)
		default:
			return core.Object{}, ErrProtocol
		}
	}
	return core.Object{ID: id, Parent: folder, Kind: core.Message, Revision: core.Revision(*revision), Properties: properties}, nil
}
