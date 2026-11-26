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
	if target.itemPrefix == "" {
		tree, err := m.mailFolderTree(ctx)
		if err != nil {
			return nil, folderTreeError(err)
		}
		if _, err := tree.specialFolder(role); err != nil {
			return nil, folderTreeError(err)
		}
	}
	query := "inid:" + target.searchFolder
	if nativeIDFilter != "" && target.itemPrefix == "" {
		query += " item:" + nativeIDFilter
	}
	request := element(mailNS, "SearchRequest", "", element(mailNS, "query", query))
	request = attr(attr(attr(attr(request, "types", spec.view), "limit", strconv.Itoa(core.MaxMailContentMessages+1)),
		"offset", "0"), "sortBy", "dateDesc")
	response, err := m.native.request(ctx, mailNS, "Search", "", request)
	if err != nil {
		return nil, semanticError(err)
	}
	switch response.get("more") {
	case "0", "false":
	case "1", "true":
		return nil, core.ErrLimit
	default:
		return nil, ErrProtocol
	}
	if response.get("offset") != "0" {
		return nil, ErrProtocol
	}
	if len(response.Children) > core.MaxMailContentMessages {
		return nil, core.ErrLimit
	}
	seen := make(map[string]bool, len(response.Children))
	items := make([]core.Object, 0, len(response.Children))
	for _, child := range response.Children {
		id := child.get("id")
		if child.XMLName != (xml.Name{Space: mailNS, Local: hit}) || !target.validItem(id) || seen[id] {
			return nil, ErrProtocol
		}
		if child.get("l") != target.nativeFolder {
			return nil, core.ErrConflict
		}
		seen[id] = true
		if nativeIDFilter != "" && id != nativeIDFilter {
			if target.itemPrefix == "" {
				return nil, ErrProtocol
			}
			continue
		}
		if !target.canReadAppointment(child.get("class")) {
			continue
		}
		item, err := calendarSearchItem(child, folder, core.ID(prefix+id), role)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return items, nil
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
