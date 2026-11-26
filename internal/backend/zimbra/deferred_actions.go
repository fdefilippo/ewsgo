// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/base64"
	"regexp"
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
)

var _ core.DeferredActionMailbox = (*mailbox)(nil)
var deferredActionID = regexp.MustCompile(`^[1-9][0-9]{0,15}-[a-f0-9]{32}$`)

func (m *mailbox) deferredRequest(ctx context.Context, operation string, req node) (node, error) {
	out, err := m.native.request(ctx, incomingRulesNS, operation, "", req)
	if ruleMissing(err) {
		return node{}, core.ErrUnsupported
	}
	if err != nil {
		return node{}, semanticError(err)
	}
	if out.get("status") != "applied" {
		return node{}, ErrProtocol
	}
	return out, nil
}

func (m *mailbox) ReadDeferredActions(ctx context.Context) ([]core.DeferredAction, error) {
	req := attr(element(incomingRulesNS, "ReadDeferredActionsRequest", ""), "reconcile", "1")
	out, err := m.deferredRequest(ctx, "ReadDeferredActions", req)
	if err != nil {
		return nil, err
	}
	if len(out.Children) > core.MaxMailContentIndexItems {
		return nil, ErrProtocol
	}
	result := make([]core.DeferredAction, 0, len(out.Children))
	seen := make(map[string]bool, len(out.Children))
	for _, row := range out.Children {
		id, folder, message := row.get("id"), row.get("folder"), row.get("message")
		definition, err := base64.RawURLEncoding.DecodeString(row.get("definition"))
		rule, provider, _, actions, valid := parseDeferredDefinition(definition)
		client, e2 := base64.StdEncoding.DecodeString(row.get("client"))
		modified, e3 := strconv.ParseInt(row.get("modified"), 10, 64)
		created := modified // Older extension versions returned only the modification time.
		var createdErr error
		if value := row.get("created"); value != "" {
			created, createdErr = strconv.ParseInt(value, 10, 64)
		}
		if row.XMLName.Space != incomingRulesNS || row.XMLName.Local != "action" ||
			!deferredActionID.MatchString(id) || !strings.HasPrefix(id, message+"-") || seen[id] ||
			!nativeID.MatchString(folder) || !nativeID.MatchString(message) ||
			err != nil || !valid ||
			e2 != nil || len(client) > core.MaxDeferredActionBytes || e3 != nil || createdErr != nil || created <= 0 || modified < created {
			return nil, ErrProtocol
		}
		seen[id] = true
		result = append(result, core.DeferredAction{
			ID: core.ID("dam:" + id), Folder: core.ID("f:" + folder), Message: core.ID("m:" + message),
			RuleID: rule, Provider: provider, Actions: actions, ClientEntry: client,
			Created: time.UnixMilli(created).UTC(), Modified: time.UnixMilli(modified).UTC(),
		})
	}
	return result, nil
}

func (m *mailbox) DeferredActionTargets(ctx context.Context) (map[core.ID]struct{}, error) {
	req := attr(element(incomingRulesNS, "ReadDeferredActionsRequest", ""), "targets", "1")
	out, err := m.deferredRequest(ctx, "ReadDeferredActions", req)
	if err != nil {
		return nil, err
	}
	if len(out.Children) > core.MaxMailContentIndexItems {
		return nil, ErrProtocol
	}
	result := make(map[core.ID]struct{}, len(out.Children))
	for _, row := range out.Children {
		id := row.get("id")
		// Older versions of the extension return full rows for this request.
		if row.XMLName.Local == "action" && deferredActionID.MatchString(id) && strings.HasPrefix(id, row.get("message")+"-") {
			id = row.get("message")
		} else if row.XMLName.Local != "message" {
			return nil, ErrProtocol
		}
		if row.XMLName.Space != incomingRulesNS || !nativeID.MatchString(id) {
			return nil, ErrProtocol
		}
		result[core.ID("m:"+id)] = struct{}{}
	}
	return result, nil
}

func (m *mailbox) PatchDeferredActions(ctx context.Context, folder, message core.ID, server, client []byte) error {
	request := attr(attr(element(incomingRulesNS, "PatchDeferredActionsRequest", ""), "server",
		base64.StdEncoding.EncodeToString(server)), "client", base64.StdEncoding.EncodeToString(client))
	if folder != "" || message != "" {
		f, fOK := strings.CutPrefix(string(folder), "f:")
		mid, mOK := strings.CutPrefix(string(message), "m:")
		if !fOK || !mOK || !nativeID.MatchString(f) || !nativeID.MatchString(mid) {
			return core.ErrInvalid
		}
		request = attr(attr(request, "folder", f), "message", mid)
	}
	_, err := m.deferredRequest(ctx, "PatchDeferredActions", request)
	return err
}

func (m *mailbox) DeleteDeferredAction(ctx context.Context, id core.ID) error {
	value, ok := strings.CutPrefix(string(id), "dam:")
	if !ok || !deferredActionID.MatchString(value) {
		return core.ErrInvalid
	}
	_, err := m.deferredRequest(ctx, "DeleteDeferredAction", attr(element(incomingRulesNS, "DeleteDeferredActionRequest", ""), "id", value))
	return err
}
