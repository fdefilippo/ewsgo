// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"ewsgo/internal/core"
)

// Configure before requests, after EnableRAMStore. Native contents remain outside
// this capability; the backend Get/Query view stays mail-only.
func (t *SessionTransport) EnableCalendarMetadataSpike() {
	t.calendarMetadataSpike = true
	if t.stores != nil {
		t.stores.calendarMetadata = true
	}
}

func readSpecialFolder(ctx context.Context, box core.Mailbox, role metadataFolderRole) (core.Object, error) {
	if role == draftsReferenceRole {
		return core.Object{}, core.ErrUnsupported
	}
	reader, ok := box.(core.SpecialFolderReader)
	if !ok {
		return core.Object{}, core.ErrUnsupported
	}
	if err := hierarchyOwner(ctx, box); err != nil {
		return core.Object{}, err
	}
	object, err := reader.SpecialFolder(ctx, role.kind())
	if err != nil {
		return core.Object{}, err
	}
	if object.Kind != core.Folder || object.ID == "" || object.ID == box.Root() || object.Parent == "" || object.ID == object.Parent || object.Properties[core.WellKnown] != core.Text(role.wellKnown()) {
		return core.Object{}, core.ErrInvalid
	}
	props, err := core.CloneProperties(object.Properties)
	if err != nil {
		return core.Object{}, err
	}
	object.Properties = props
	if err := ctx.Err(); err != nil {
		return core.Object{}, err
	}
	return object, nil
}

func (s *ramStore) specialFolderObject(ctx context.Context, session core.Session, role metadataFolderRole) (core.Object, error) {
	if !s.metadataRoleEnabled(role) {
		return core.Object{}, core.ErrUnsupported
	}
	objects, err := s.metadataFolderObjects(ctx, session)
	if err != nil {
		return core.Object{}, err
	}
	box := session.Mailbox()
	var target core.Object
	for _, o := range objects {
		if o.Properties[core.WellKnown] == core.Text(role.wellKnown()) {
			if target.ID != "" {
				return core.Object{}, core.ErrConflict
			}
			target = o
		}
	}
	if target.ID == "" {
		if _, ok := box.(core.SpecialFolderReader); !ok && role != draftsReferenceRole {
			return core.Object{}, core.ErrUnsupported
		}
		return core.Object{}, core.ErrNotFound
	}
	if err := ctx.Err(); err != nil {
		return core.Object{}, err
	}
	return target, nil
}

func (s *ramStore) specialFolderFID(ctx context.Context, session core.Session, role metadataFolderRole) (uint64, error) {
	key := folderKey{role: role.serviceRole()}
	if key.role != MailboxRoot {
		if session == nil || session.Identity().Scope != s.scope {
			return 0, core.ErrAccessDenied
		}
		box := session.Mailbox()
		if box == nil || box.Root() != s.root {
			return 0, core.ErrConflict
		}
		if err := hierarchyOwner(ctx, box); err != nil {
			return 0, err
		}
	} else {
		target, err := s.specialFolderObject(ctx, session, role)
		if err != nil {
			return 0, err
		}
		key.native = target.ID
	}
	ids, err := s.assign(ctx, []folderKey{key})
	if err != nil {
		return 0, err
	}
	return ids[0], nil
}

func specialFolderReference(ctx context.Context, session core.Session, object serverObject, tag uint32, role metadataFolderRole) folderPropertyValue {
	fail := func(code uint32) folderPropertyValue { return folderPropertyValue{kind: 0xa, code: code} }
	if uint16(tag) != 0 && uint16(tag) != 0x102 {
		return fail(NotSupported)
	}
	if object.fid != object.binding.fids[Inbox] && object.fid != object.binding.fids[MailboxRoot] {
		return fail(PropertyNotFound)
	}
	fid, err := object.binding.store.specialFolderFID(ctx, session, role)
	if err != nil {
		return fail(storeError(err))
	}
	// Reauthorize the mapped target; a previous mapping is never a grant.
	if role.serviceRole() == MailboxRoot {
		if _, err := object.binding.store.folder(ctx, session, fid); err != nil {
			return fail(storeError(err))
		}
	}
	entry := marshalFolderEntryID(object.binding.mailboxGUID, object.binding.store.replica, fid)
	return folderPropertyValue{kind: 0x102, data: append(binary.LittleEndian.AppendUint16(nil, uint16(len(entry))), entry...)}
}

func (t *SessionTransport) EnableContactsMetadataSpike() {
	t.contactsMetadataSpike = true
	if t.stores != nil {
		t.stores.contactsMetadata = true
	}
}

// Fixed semantic roles keep protocol tags/classes in the frontend and native
// IDs in the backend. Their ordinal is used only in bounded snapshot arrays.
type metadataFolderRole uint8

const (
	calendarMetadataRole metadataFolderRole = iota
	contactsMetadataRole
	tasksMetadataRole
	draftsReferenceRole
	journalReferenceRole
	notesReferenceRole
	metadataFolderRoleCount
)

var metadataFolderDescriptors = [metadataFolderRoleCount]struct {
	kind             core.SpecialFolderKind
	wellKnown, class string
	tag              uint32
	service          SpecialFolderRole
}{
	{core.CalendarFolder, "calendar", "IPF.Appointment", 0x36d00102, 0},
	{core.ContactsFolder, "contacts", "IPF.Contact", 0x36d10102, 0},
	{core.TasksFolder, "tasks", "IPF.Task", 0x36d40102, 0},
	{0, "drafts", "IPF.Note", 0x36d70102, 0}, // Ordinary mail catalog; no SpecialFolderReader call.
	{tag: 0x36d20102, service: journalFolderRole},
	{tag: 0x36d30102, service: notesFolderRole},
}

func (r metadataFolderRole) kind() core.SpecialFolderKind { return metadataFolderDescriptors[r].kind }
func (r metadataFolderRole) wellKnown() string            { return metadataFolderDescriptors[r].wellKnown }
func (r metadataFolderRole) class() string                { return metadataFolderDescriptors[r].class }
func (r metadataFolderRole) tag() uint32                  { return metadataFolderDescriptors[r].tag }
func (r metadataFolderRole) serviceRole() SpecialFolderRole {
	return metadataFolderDescriptors[r].service
}
func (s *ramStore) metadataRoleEnabled(r metadataFolderRole) bool {
	return r == calendarMetadataRole && s.calendarMetadata || r == contactsMetadataRole && s.contactsMetadata || r == tasksMetadataRole && s.tasksMetadata || r == draftsReferenceRole && s.draftsReference || r.serviceRole() != MailboxRoot
}
func (t *SessionTransport) EnableTasksMetadataSpike() {
	t.tasksMetadataSpike = true
	if t.stores != nil {
		t.stores.tasksMetadata = true
	}
}
func metadataClass(properties core.Properties) string {
	if class, ok := properties[core.GroupwareFolderClass].(core.Text); ok {
		switch class {
		case "IPF.Appointment", "IPF.Contact":
			return string(class)
		}
	}
	for r := metadataFolderRole(0); r < draftsReferenceRole; r++ {
		if properties[core.WellKnown] == core.Text(r.wellKnown()) {
			return r.class()
		}
	}
	return ""
}

// Resolve a mapped native metadata folder through the same complete authorized
// tree; stale mappings and arbitrary native IDs never become an access grant.
func (s *ramStore) metadataFolderByID(ctx context.Context, session core.Session, id core.ID) (core.Object, error) {
	objects, err := s.metadataFolderObjects(ctx, session)
	if err != nil {
		return core.Object{}, err
	}
	for _, object := range objects {
		if object.ID != id {
			continue
		}
		if object.Properties[core.GroupwareFolderClass] == core.Text("IPF.Appointment") && s.calendarMetadata ||
			object.Properties[core.GroupwareFolderClass] == core.Text("IPF.Contact") && s.contactsMetadata {
			return object, nil
		}
		for role := metadataFolderRole(0); role < journalReferenceRole; role++ {
			if s.metadataRoleEnabled(role) && object.Properties[core.WellKnown] == core.Text(role.wellKnown()) {
				return object, nil
			}
		}
	}
	return core.Object{}, core.ErrNotFound
}

func (s *ramStore) metadataFolderObjects(ctx context.Context, session core.Session) ([]core.Object, error) {
	if session == nil || session.Identity().Scope != s.scope {
		return nil, core.ErrAccessDenied
	}
	box := session.Mailbox()
	if box == nil || box.Root() != s.root {
		return nil, core.ErrConflict
	}
	if err := hierarchyOwner(ctx, box); err != nil {
		return nil, err
	}
	catalog, objects, err := readPrivateFolderTreeWithSpecialFolders(ctx, session, s.calendarMetadata, s.contactsMetadata, s.tasksMetadata)
	if err != nil {
		return nil, err
	}
	if catalog.scope != s.scope || catalog.folders[IPMSubtree].NativeID != s.root {
		return nil, core.ErrConflict
	}
	var seen [metadataFolderRoleCount]bool
	for _, object := range objects {
		for role := metadataFolderRole(0); role < journalReferenceRole; role++ {
			if !s.metadataRoleEnabled(role) || object.Properties[core.WellKnown] != core.Text(role.wellKnown()) {
				continue
			}
			if object.ID == s.root || object.Parent == "" || object.Parent == object.ID {
				return nil, core.ErrInvalid
			}
			if seen[role] {
				return nil, core.ErrConflict
			}
			seen[role] = true
		}
	}
	if err := hierarchyOwner(ctx, box); err != nil {
		return nil, err
	}
	if session.Identity().Scope != s.scope || session.Mailbox() == nil || session.Mailbox().Root() != s.root {
		return nil, core.ErrAccessDenied
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	return objects, nil
}

// Drafts already belongs to the ordinary mail catalog. This option adds only
// its protocol reference/class; it never asks a backend for another capability.
func (t *SessionTransport) EnableDraftsReferenceSpike() {
	t.draftsReferenceSpike = true
	if t.stores != nil {
		t.stores.draftsReference = true
	}
}
func nativeFolderClass(properties core.Properties, drafts bool) string {
	if drafts && properties[core.WellKnown] == core.Text("drafts") {
		return "IPF.Note"
	}
	return metadataClass(properties)
}
