// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/xml"
	"errors"
	"slices"
	"strconv"
	"strings"

	"ewsgo/internal/core"
)

type groupwareTarget struct {
	nativeFolder string
	searchFolder string
	itemPrefix   string
	viewPrivate  bool
}

var _ core.SharedGroupwareFolders = (*mailbox)(nil)

func (m *mailbox) groupwareFolder(ctx context.Context, folder core.ID, role core.SpecialFolderKind) (groupwareTarget, error) {
	spec := specialFolderSpec(role)
	if folder == core.ID("f:"+spec.id) && spec.id != "" {
		return groupwareTarget{nativeFolder: spec.id, searchFolder: spec.id}, nil
	}
	link, err := m.readGroupwareLink(ctx, folder)
	if err != nil {
		return groupwareTarget{}, err
	}
	if link.get("view") != spec.view {
		return groupwareTarget{}, core.ErrNotFound
	}
	prefix := link.get("zid") + ":"
	return groupwareTarget{nativeFolder: prefix + link.get("rid"), searchFolder: link.get("id"),
		itemPrefix: prefix, viewPrivate: strings.ContainsRune(link.get("perm"), 'p')}, nil
}

func (m *mailbox) readGroupwareLink(ctx context.Context, folder core.ID) (node, error) {
	localID, ok := strings.CutPrefix(string(folder), core.SharedGroupwareFolderIDPrefix)
	if !ok || !nativeID.MatchString(localID) {
		return node{}, core.ErrNotFound
	}
	out, err := m.getLocalGroupwareFolders(ctx, localID, "0")
	if err != nil {
		return node{}, err
	}
	return m.groupwareLinkFromResponse(out, localID)
}

func (m *mailbox) groupwareLinkFromResponse(out node, localID string) (node, error) {
	if out.XMLName != (xml.Name{Space: mailNS, Local: "GetFolderResponse"}) {
		return node{}, ErrProtocol
	}
	if len(out.Children) != 1 || out.Children[0].get("id") != localID {
		return node{}, core.ErrNotFound
	}
	link := out.Children[0]
	if err := m.validateGroupwareLink(link); err != nil {
		return node{}, err
	}
	return link, nil
}

func (m *mailbox) getLocalGroupwareFolders(ctx context.Context, id, depth string) (node, error) {
	out, err := m.native.request(ctx, mailNS, "GetFolder", "", groupwareFoldersRequest(id, depth))
	return out, semanticError(err)
}

func groupwareFoldersRequest(id, depth string) node {
	selector := attr(element(mailNS, "folder", ""), "l", id)
	return attr(attr(attr(element(mailNS, "GetFolderRequest", "", selector), "depth", depth), "tr", "0"), "visible", "0")
}

func (m *mailbox) groupwareLinkIdentity(link node) error {
	if link.XMLName != (xml.Name{Space: mailNS, Local: "link"}) || groupwareClass(link.get("view")) == "" {
		return core.ErrNotFound
	}
	if !nativeID.MatchString(link.get("id")) || !nativeID.MatchString(link.get("l")) ||
		!nativeID.MatchString(link.get("rid")) || !sharedOwnerID.MatchString(link.get("zid")) ||
		link.get("zid") == m.scope.Account || link.get("id") == link.get("l") {
		return ErrProtocol
	}
	return nil
}

func (m *mailbox) validateGroupwareLink(link node) error {
	if err := m.groupwareLinkIdentity(link); err != nil {
		return err
	}
	if !strings.ContainsRune(link.get("perm"), 'r') {
		return core.ErrAccessDenied
	}
	return nil
}

func groupwareClass(view string) string {
	switch view {
	case "appointment":
		return "IPF.Appointment"
	case "contact":
		return "IPF.Contact"
	}
	return ""
}

func sharedGroupwareObject(link node) (core.Object, error) {
	folder, err := folderFromNode(link)
	if err != nil {
		return core.Object{}, err
	}
	object := folderObject(folder)
	rights, err := aclRights(link.get("perm"))
	if err != nil {
		return core.Object{}, err
	}
	object.ID = core.ID(core.SharedGroupwareFolderIDPrefix + folder.ID)
	object.Properties[core.GroupwareFolderClass] = core.Text(groupwareClass(folder.View))
	object.Properties[core.SharedFolderRights] = core.Integer(rights)
	delete(object.Properties, core.WellKnown)
	// Link counts are metadata in the delegate's mailbox and can lag the
	// remote view. Report counts only through the live item collection.
	delete(object.Properties, core.FolderItemCount)
	delete(object.Properties, core.FolderUnreadCount)
	return object, nil
}

func (m *mailbox) ReadSharedGroupwareFolder(ctx context.Context, folder core.ID, role core.SpecialFolderKind) (core.Object, error) {
	link, err := m.readGroupwareLink(ctx, folder)
	if err != nil {
		return core.Object{}, err
	}
	if role != 0 && link.get("view") != specialFolderSpec(role).view {
		return core.Object{}, core.ErrNotFound
	}
	return sharedGroupwareObject(link)
}

func (m *mailbox) observeSharedFolderContents(ctx context.Context, folder core.ID) (core.MailFolderObservation, error) {
	link, err := m.readGroupwareLink(ctx, folder)
	if err != nil {
		return core.MailFolderObservation{}, err
	}
	role := core.ContactsFolder
	if link.get("view") == "appointment" {
		role = core.CalendarFolder
	}
	remote := link.get("zid") + ":" + link.get("rid")
	before, err := m.sharedContentsMarker(ctx, remote, link.get("view"))
	if err != nil {
		return core.MailFolderObservation{}, err
	}
	ids, err := m.folderContentsIDs(ctx, folder, role)
	if err != nil {
		return core.MailFolderObservation{}, err
	}
	after, err := m.sharedContentsMarker(ctx, remote, link.get("view"))
	if err != nil {
		return core.MailFolderObservation{}, err
	}
	current, err := m.readGroupwareLink(ctx, folder)
	if err != nil {
		return core.MailFolderObservation{}, err
	}
	if before != after || current.get("zid") != link.get("zid") || current.get("rid") != link.get("rid") ||
		current.get("view") != link.get("view") || current.get("perm") != link.get("perm") {
		return core.MailFolderObservation{}, core.ErrConflict
	}
	// The remote folder's total can include private appointments. Count the
	// items returned through the delegate's current access instead.
	return core.MailFolderObservation{Sequence: after.Sequence, MessageCount: uint64(len(ids)), MessageIDs: ids}, nil
}

func (m *mailbox) sharedContentsMarker(ctx context.Context, remote, view string) (core.MailFolderMarker, error) {
	out, err := m.getLocalGroupwareFolders(ctx, remote, "0")
	if err != nil {
		return core.MailFolderMarker{}, err
	}
	if out.XMLName != (xml.Name{Space: mailNS, Local: "GetFolderResponse"}) || len(out.Children) != 1 {
		return core.MailFolderMarker{}, ErrProtocol
	}
	item := out.Children[0]
	if item.XMLName != (xml.Name{Space: mailNS, Local: "folder"}) || item.get("id") != remote || item.get("view") != view {
		return core.MailFolderMarker{}, ErrProtocol
	}
	if !strings.ContainsRune(item.get("perm"), 'r') {
		return core.MailFolderMarker{}, core.ErrAccessDenied
	}
	sequence, err := optionalFolderNumber(item, "i4ms")
	if err != nil {
		return core.MailFolderMarker{}, err
	}
	if sequence == nil {
		return core.MailFolderMarker{}, core.ErrUnsupported
	}
	marker := core.MailFolderMarker{Sequence: *sequence}
	if view == "appointment" {
		count, err := optionalFolderNumber(item, "n")
		if err != nil {
			return core.MailFolderMarker{}, err
		}
		if count == nil {
			return core.MailFolderMarker{}, core.ErrUnsupported
		}
		marker.MessageCount = *count
	}
	return marker, ctx.Err()
}

func (m *mailbox) GroupwareFolderTree(ctx context.Context, roles []core.SpecialFolderKind) ([]core.Object, error) {
	if !slices.Contains(roles, core.CalendarFolder) && !slices.Contains(roles, core.ContactsFolder) {
		return m.OwnFolderTree(ctx, roles)
	}
	var links []node
	tree, err := m.ownFolderTreeWithLinks(ctx, &links)
	if err != nil {
		return nil, folderTreeError(err)
	}
	if len(tree)+len(links) > 1000 {
		return nil, core.ErrLimit
	}
	items, err := ownFolderObjects(ctx, tree, roles)
	if err != nil {
		return nil, err
	}
	parents := make(map[string]bool, len(items))
	for _, item := range items {
		if id, ok := strings.CutPrefix(string(item.ID), "f:"); ok {
			parents[id] = true
		}
	}
	typeID := make(map[string]core.SpecialFolderKind, len(roles))
	for _, role := range roles {
		typeID[specialFolderSpec(role).view] = role
	}
	selected := make([]node, 0, len(links))
	seen := make(map[string]bool)
	for _, link := range links {
		if err := m.groupwareLinkIdentity(link); err != nil {
			if errors.Is(err, core.ErrNotFound) {
				continue
			}
			return nil, err
		}
		if typeID[link.get("view")] == 0 || !parents[link.get("l")] {
			continue
		}
		id := link.get("id")
		if seen[id] || tree[id].ID != "" {
			return nil, ErrProtocol
		}
		seen[id] = true
		selected = append(selected, link)
	}
	for start := 0; start < len(selected); start += 16 {
		batch, err := m.readGroupwareLinks(ctx, selected[start:min(start+16, len(selected))])
		if err != nil {
			return nil, err
		}
		items = append(items, batch...)
	}
	return items, ctx.Err()
}

// A tree response omits effective permissions on links. Batch specific
// GetFolder calls so dead and revoked mounts are omitted from the catalog.
func (m *mailbox) readGroupwareLinks(ctx context.Context, links []node) ([]core.Object, error) {
	request := attr(element(contextNS, "BatchRequest", ""), "onerror", "continue")
	for i, link := range links {
		request.Children = append(request.Children, attr(groupwareFoldersRequest(link.get("id"), "0"), "requestId", strconv.Itoa(i+1)))
	}
	out, err := m.native.request(ctx, contextNS, "Batch", "", request)
	if err != nil {
		return nil, semanticError(err)
	}
	if len(out.Children) != len(links) {
		return nil, ErrProtocol
	}
	var items []core.Object
	for i, answer := range out.Children {
		if answer.get("requestId") != strconv.Itoa(i+1) {
			return nil, ErrProtocol
		}
		if answer.XMLName == (xml.Name{Space: soapNS, Local: "Fault"}) {
			if code := answer.find(contextNS, "Code"); code != nil {
				switch code.Text {
				case "service.PERM_DENIED", "mail.NO_SUCH_FOLDER", "mail.NO_SUCH_ITEM":
					continue
				}
			}
			return nil, ErrProtocol
		}
		original := links[i]
		link, err := m.groupwareLinkFromResponse(answer, original.get("id"))
		if errors.Is(err, core.ErrAccessDenied) || errors.Is(err, core.ErrNotFound) {
			continue
		}
		if err != nil {
			return nil, err
		}
		if link.get("l") != original.get("l") || link.get("view") != original.get("view") ||
			link.get("zid") != original.get("zid") || link.get("rid") != original.get("rid") {
			return nil, core.ErrConflict
		}
		item, err := sharedGroupwareObject(link)
		if err != nil {
			return nil, err
		}
		items = append(items, item)
	}
	return items, nil
}

func (target groupwareTarget) validItem(id string) bool {
	local, ok := strings.CutPrefix(id, target.itemPrefix)
	return ok && nativeID.MatchString(local)
}

func (target groupwareTarget) canReadAppointment(class string) bool {
	return target.itemPrefix == "" || target.viewPrivate || class == "PUB"
}
