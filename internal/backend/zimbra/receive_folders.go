// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
)

var _ core.ReceiveFolderMailbox = (*mailbox)(nil)

func (m *mailbox) ReadReceiveFolders(ctx context.Context) ([]core.ReceiveFolderAssociation, error) {
	out, err := m.native.request(ctx, incomingRulesNS, "ReadReceiveFolders", "", element(incomingRulesNS, "ReadReceiveFoldersRequest", ""))
	if ruleMissing(err) {
		return nil, core.ErrUnsupported
	}
	if err != nil {
		return nil, semanticError(err)
	}
	if len(out.Children) > 64 {
		return nil, ErrProtocol
	}
	routes := make([]core.ReceiveFolderAssociation, 0, len(out.Children))
	classes := make(map[string]struct{}, len(out.Children))
	for _, row := range out.Children {
		if row.XMLName.Space != incomingRulesNS || row.XMLName.Local != "route" || len(row.Children) != 0 {
			return nil, ErrProtocol
		}
		id, class := row.get("folder"), row.get("class")
		modified, err := strconv.ParseInt(row.get("modified"), 10, 64)
		key := strings.ToUpper(class)
		if _, duplicate := classes[key]; duplicate || !nativeID.MatchString(id) || err != nil || modified <= 0 {
			return nil, ErrProtocol
		}
		classes[key] = struct{}{}
		routes = append(routes, core.ReceiveFolderAssociation{Class: class, Folder: core.ID("f:" + id), Modified: time.UnixMilli(modified).UTC()})
	}
	return routes, nil
}

func (m *mailbox) SetReceiveFolder(ctx context.Context, class string, folder core.ID) error {
	id := "0"
	if folder != "" {
		var ok bool
		id, ok = strings.CutPrefix(string(folder), "f:")
		if !ok || !nativeID.MatchString(id) {
			return core.ErrInvalid
		}
	}
	request := attr(attr(element(incomingRulesNS, "SetReceiveFolderRequest", ""), "class", class), "folder", id)
	out, err := m.native.request(ctx, incomingRulesNS, "SetReceiveFolder", "", request)
	if ruleMissing(err) {
		return core.ErrUnsupported
	}
	if err != nil {
		return semanticError(err)
	}
	if out.get("status") != "applied" || len(out.Children) != 0 {
		return ErrProtocol
	}
	return nil
}
