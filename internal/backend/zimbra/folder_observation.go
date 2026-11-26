// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"crypto/sha256"
	"io"
	"slices"
	"strconv"
	"strings"

	"ewsgo/internal/core"
)

func (m *mailbox) ObserveMailFolder(ctx context.Context, folder core.ID, priorSequence *uint64) (core.MailFolderObservation, error) {
	return m.observeFolderContents(ctx, folder, priorSequence, false)
}

func (m *mailbox) ObserveFolderContents(ctx context.Context, folder core.ID, priorSequence *uint64) (core.MailFolderObservation, error) {
	if strings.HasPrefix(string(folder), core.SharedGroupwareFolderIDPrefix) {
		return m.observeSharedFolderContents(ctx, folder)
	}
	return m.observeFolderContents(ctx, folder, priorSequence, true)
}

func (m *mailbox) observeFolderContents(ctx context.Context, folder core.ID, priorSequence *uint64, groupware bool) (core.MailFolderObservation, error) {
	native, ok := strings.CutPrefix(string(folder), "f:")
	if !ok || !nativeID.MatchString(native) {
		return core.MailFolderObservation{}, core.ErrInvalid
	}
	role := contentsSpecialRole(native, groupware)
	before, err := m.folderObservationMarker(ctx, native, role)
	if err != nil {
		return core.MailFolderObservation{}, semanticError(err)
	}
	result := core.MailFolderObservation{Sequence: before.Sequence, Children: before.Children, MessageCount: before.MessageCount}
	if priorSequence != nil && *priorSequence == before.Sequence || before.MessageCount == 0 && priorSequence == nil {
		return result, nil
	}
	if before.MessageCount > core.MaxMailContentIndexItems {
		return core.MailFolderObservation{}, ErrLimit
	}
	ids, err := m.folderContentsIDs(ctx, folder, native, role)
	if err != nil {
		return core.MailFolderObservation{}, semanticError(err)
	}
	after, err := m.folderObservationMarker(ctx, native, role)
	if err != nil {
		return core.MailFolderObservation{}, semanticError(err)
	}
	// Verify that Search and GetFolder agree before inferring any removal.
	// Incomplete Search results or concurrent changes leave the old RAM
	// observation untouched.
	if before != after {
		return core.MailFolderObservation{}, core.ErrConflict
	}
	if uint64(len(ids)) != after.MessageCount {
		if role != 0 {
			// Native folder counts may include objects outside the supported
			// projection. A mismatch must not be used to infer removals.
			return core.MailFolderObservation{}, core.ErrUnsupported
		}
		return core.MailFolderObservation{}, core.ErrConflict
	}
	slices.Sort(ids)
	result.MessageIDs = ids
	return result, nil
}

func contentsSpecialRole(native string, groupware bool) core.SpecialFolderKind {
	if groupware {
		for _, role := range [...]core.SpecialFolderKind{core.ContactsFolder, core.CalendarFolder} {
			if native == specialFolderSpec(role).id {
				return role
			}
		}
	}
	return 0
}

func (m *mailbox) folderContentsIDs(ctx context.Context, folder core.ID, native string, role core.SpecialFolderKind) ([]core.ID, error) {
	switch role {
	case core.ContactsFolder:
		cards, err := m.ListContacts(ctx, folder)
		if err != nil {
			return nil, err
		}
		ids := make([]core.ID, len(cards))
		for i, card := range cards {
			ids[i] = card.Identity.ID
		}
		return ids, nil
	case core.CalendarFolder:
		appointments, err := m.ReadCalendarSearchItems(ctx, folder, role)
		if err != nil {
			return nil, err
		}
		ids := make([]core.ID, len(appointments))
		for i, appointment := range appointments {
			ids[i] = appointment.ID
		}
		return ids, nil
	}
	entries, err := m.native.mailIndexEntries(ctx, native)
	if err != nil {
		return nil, err
	}
	ids := make([]core.ID, len(entries))
	for i, entry := range entries {
		ids[i] = core.ID("m:" + entry.id)
	}
	return ids, nil
}

func (m *mailbox) ReadMailFolderMarkers(ctx context.Context, folders []core.ID) (map[core.ID]core.MailFolderMarker, error) {
	return m.readFolderMarkers(ctx, folders, false)
}

func (m *mailbox) ReadFolderContentsMarkers(ctx context.Context, folders []core.ID) (map[core.ID]core.MailFolderMarker, error) {
	return m.readFolderMarkers(ctx, folders, true)
}

func (m *mailbox) readFolderMarkers(ctx context.Context, folders []core.ID, groupware bool) (map[core.ID]core.MailFolderMarker, error) {
	tree, err := m.mailFolderTree(ctx)
	if err != nil {
		return nil, semanticError(err)
	}
	markers := make(map[core.ID]core.MailFolderMarker, len(folders))
	children := make(map[string][]string)
	for id, folder := range tree {
		children[folder.Parent] = append(children[folder.Parent], id)
	}
	for _, folder := range folders {
		if groupware && strings.HasPrefix(string(folder), core.SharedGroupwareFolderIDPrefix) {
			// The local tree has neither the remote contents revision nor the
			// number of items visible to this delegate.
			continue
		}
		native, ok := strings.CutPrefix(string(folder), "f:")
		if !ok || !nativeID.MatchString(native) {
			return nil, core.ErrInvalid
		}
		marker, err := tree.folderContentsMarker(native, children[native], contentsSpecialRole(native, groupware))
		if err == nil {
			markers[folder] = marker
		}
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return markers, nil
}

func (m *mailbox) folderObservationMarker(ctx context.Context, native string, role core.SpecialFolderKind) (core.MailFolderMarker, error) {
	tree, err := m.mailFolderTree(ctx)
	if err != nil {
		return core.MailFolderMarker{}, err
	}
	var children []string
	for id, folder := range tree {
		if folder.Parent == native {
			children = append(children, id)
		}
	}
	return tree.folderContentsMarker(native, children, role)
}

func (tree nativeMailFolderTree) folderContentsMarker(native string, children []string, role core.SpecialFolderKind) (core.MailFolderMarker, error) {
	var target Folder
	var err error
	if role != 0 {
		if _, err = tree.specialFolder(role); err == nil {
			target = tree[native]
		}
	} else {
		target, err = tree.mailFolder(native)
	}
	if err != nil {
		return core.MailFolderMarker{}, err
	}
	if target.ContentsSequence == nil || target.ItemCount == nil {
		return core.MailFolderMarker{}, core.ErrUnsupported
	}
	slices.Sort(children)
	hash := sha256.New()
	for _, id := range children {
		child := tree[id]
		// XML scalars cannot contain NUL. Counts and i4ms belong to
		// the child's own contents and are deliberately excluded.
		revision := ""
		if child.MetadataRevision != nil {
			revision = strconv.FormatUint(*child.MetadataRevision, 10)
		}
		_, _ = io.WriteString(hash, id+"\x00"+child.Name+"\x00"+child.View+"\x00"+revision+"\x00")
	}
	return core.MailFolderMarker{
		Sequence: *target.ContentsSequence, MessageCount: *target.ItemCount,
		Children: [32]byte(hash.Sum(nil)),
	}, nil
}
