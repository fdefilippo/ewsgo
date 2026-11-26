// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/xml"
	"net/mail"
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
)

func (m *mailbox) ReadAvailability(ctx context.Context, addresses []string, start, end time.Time) ([][]core.AvailabilityInterval, error) {
	if len(addresses) == 0 || len(addresses) > core.MaxAvailabilityMailboxes ||
		!end.After(start) || end.Sub(start) > core.MaxAvailabilityWindow {
		return nil, core.ErrInvalid
	}
	keys := make([]string, len(addresses))
	results := make(map[string][]core.AvailabilityInterval, len(addresses))
	var recipients []string
	for i, address := range addresses {
		parsed, err := mail.ParseAddress(address)
		if err != nil || parsed.Address != address || strings.ContainsAny(address, ",\r\n") || len(address) > 320 {
			return nil, core.ErrInvalid
		}
		key := strings.ToLower(address)
		keys[i] = key
		if _, seen := results[key]; !seen {
			recipients = append(recipients, address)
			results[key] = nil
		}
	}
	lastMillis := end.UnixMilli()
	if end.Nanosecond()%int(time.Millisecond) != 0 {
		lastMillis++
	}
	request := element(mailNS, "GetFreeBusyRequest", "")
	request = attr(request, "name", strings.Join(recipients, ","))
	request = attr(request, "s", strconv.FormatInt(start.UnixMilli(), 10))
	request = attr(request, "e", strconv.FormatInt(lastMillis, 10))
	root, err := m.native.request(ctx, mailNS, "GetFreeBusy", "", request)
	if err != nil {
		return nil, semanticError(err)
	}
	if len(root.Children) != len(results) {
		return nil, ErrProtocol
	}
	for _, row := range root.Children {
		key := strings.ToLower(row.get("id"))
		prev, exists := results[key]
		if row.XMLName != (xml.Name{Space: mailNS, Local: "usr"}) || !exists || prev != nil {
			return nil, ErrProtocol
		}
		result, err := zimbraAvailabilityIntervals(row, start, end)
		if err != nil {
			return nil, err
		}
		results[key] = result
	}
	output := make([][]core.AvailabilityInterval, len(keys))
	for i, key := range keys {
		output[i] = results[key]
	}
	return output, ctx.Err()
}

func zimbraAvailabilityIntervals(row node, start, end time.Time) ([]core.AvailabilityInterval, error) {
	stateNames := map[string]core.AvailabilityState{
		"f": core.AvailabilityFree, "t": core.AvailabilityTentative, "b": core.AvailabilityBusy,
		"o": core.AvailabilityOutOfOffice, "n": core.AvailabilityNoData,
	}
	cursor := start
	var result []core.AvailabilityInterval
	for _, segment := range row.Children {
		state, known := stateNames[segment.XMLName.Local]
		fromMS, fromErr := strconv.ParseInt(segment.get("s"), 10, 64)
		toMS, toErr := strconv.ParseInt(segment.get("e"), 10, 64)
		if !known || segment.XMLName.Space != mailNS || len(segment.Children) != 0 ||
			fromErr != nil || toErr != nil || fromMS >= toMS {
			return nil, ErrProtocol
		}
		from := time.UnixMilli(fromMS).UTC()
		to := time.UnixMilli(toMS).UTC()
		if !to.After(start) || !from.Before(end) {
			continue
		}
		if from.Before(start) {
			from = start
		}
		if to.After(end) {
			to = end
		}
		if from.Before(cursor) {
			return nil, ErrProtocol
		}
		if from.After(cursor) {
			result = append(result, core.AvailabilityInterval{Start: cursor, End: from, State: core.AvailabilityNoData})
		}
		result = append(result, core.AvailabilityInterval{Start: from, End: to, State: state})
		cursor = to
	}
	if cursor.Before(end) {
		result = append(result, core.AvailabilityInterval{Start: cursor, End: end, State: core.AvailabilityNoData})
	}
	return result, nil
}
