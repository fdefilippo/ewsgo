// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"regexp"
	"strings"

	"ewsgo/internal/core"
)

var sharedOwnerID = regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$`)
var _ core.SharedFolderMailbox = (*mailbox)(nil)

func (m *mailbox) ReadSharedMailFolder(ctx context.Context, owner core.Scope, folder core.ID) (core.SharedMailFolder, error) {
	id := strings.TrimPrefix(string(folder), "f:")
	if owner.Backend != m.scope.Backend || owner == m.scope || !sharedOwnerID.MatchString(owner.Account) ||
		string(folder) != "f:"+id || !nativeID.MatchString(id) {
		return core.SharedMailFolder{}, core.ErrInvalid
	}
	prefix := owner.Account + ":"
	request := attr(element(mailNS, "GetFolderRequest", "", attr(element(mailNS, "folder", ""), "l", prefix+id)), "depth", "0")
	request = attr(attr(attr(request, "tr", "0"), "visible", "0"), "needGranteeName", "0")
	out, err := m.native.request(ctx, mailNS, "GetFolder", "", request)
	if err != nil {
		return core.SharedMailFolder{}, semanticError(err)
	}
	n := out.child(mailNS, "folder")
	if n == nil {
		return core.SharedMailFolder{}, core.ErrNotFound
	}
	if len(out.Children) != 1 || n.get("id") != prefix+id || !strings.Contains(n.get("perm"), "r") {
		return core.SharedMailFolder{}, core.ErrAccessDenied
	}
	if view := n.get("view"); view != "" && view != "message" {
		return core.SharedMailFolder{}, core.ErrUnsupported
	}
	parent := n.get("l")
	if strings.HasPrefix(parent, prefix) {
		parent = strings.TrimPrefix(parent, prefix)
	}
	if !nativeID.MatchString(parent) || parent == id {
		return core.SharedMailFolder{}, ErrProtocol
	}
	return core.SharedMailFolder{ID: folder, Parent: core.ID("f:" + parent)}, nil
}
