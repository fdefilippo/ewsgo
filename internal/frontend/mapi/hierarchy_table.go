// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"strings"
	"time"

	"ewsgo/internal/core"
)

const tableSuppressNotifications byte = 0x80

// Hierarchy tables use the same bounded Table object, ledger and row codec as
// Contents tables. Reading the projection does not publish an ICS checkpoint.
func captureHierarchyTable(ctx context.Context, session core.Session, binding storeBinding, fid uint64, flags byte, aggregates bool) (*contentsTable, error) {
	store := binding.store
	table := &contentsTable{hierarchy: true, flags: flags}
	fail := func(err error) (*contentsTable, error) { table.close(); return nil, err }
	used := 1024 + 4*maxTableColumns
	if flags&tableSuppressNotifications != 0 {
		used += 64 * maxDynamicFolders // Receipt map can change without a visible row change.
	}
	if flags&tableSoftDeletes != 0 {
		// This store exposes active folders. A folder moved to Trash is still
		// active, and message recovery creates no soft-deleted folders.
		if err := authorizeHierarchyTable(ctx, session, store, fid); err != nil {
			return fail(err)
		}
		var err error
		table.lease, err = store.reserveContentsMemory(used)
		if err != nil {
			return fail(err)
		}
		return table, nil
	}
	var err error
	table.lease, err = store.reserveContentsPeak(ctx, 4*maxHierarchyBytes)
	if err != nil {
		return nil, err
	}
	view, err := store.observeHierarchy(ctx, session, fid, time.Now())
	if err != nil {
		return fail(err)
	}
	children := map[uint64]int{}
	for _, f := range view.folders {
		children[f.parent]++
	}
	var markers map[core.ID]core.MailFolderMarker
	var readMarkers func(context.Context, []core.ID) (map[core.ID]core.MailFolderMarker, error)
	if observer, ok := session.Mailbox().(core.FolderContentsObserver); (store.contactsMetadata || store.calendarMetadata) && ok {
		readMarkers = observer.ReadFolderContentsMarkers
	} else if reader, ok := session.Mailbox().(core.MailFolderMarkerReader); ok {
		readMarkers = reader.ReadMailFolderMarkers
	}
	if aggregates && readMarkers != nil {
		folders := make([]core.ID, 0, len(view.folders))
		for _, f := range view.folders {
			if f.native != "" && (flags&4 != 0 || f.parent == fid) {
				folders = append(folders, f.native)
			}
		}
		if len(folders) != 0 {
			markers, err = readMarkers(ctx, folders)
			if err != nil {
				return fail(err)
			}
		}
	}
	for _, f := range view.folders {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if flags&tableSuppressNotifications != 0 && f.creator != 0 {
			// A child outside a shallow view can still change its visible
			// parent's child count. Keep the origin of such children too.
			if table.hierarchyCreations == nil {
				table.hierarchyCreations = make(map[uint64]hierarchyCreationOrigin)
			}
			table.hierarchyCreations[f.fid] = hierarchyCreationOrigin{parent: f.parent, context: f.creator}
		}
		if flags&4 == 0 && f.parent != fid {
			continue
		}
		values := contentsValues{}
		putContentsValue(values, 0x67480014, core.Integer(f.fid))
		putContentsValue(values, 0x67490014, core.Integer(f.parent))
		putContentsValue(values, folderEntryIDTag, core.Binary(marshalFolderEntryID(binding.mailboxGUID, store.replica, f.fid)))
		putContentsValue(values, parentFolderEntryIDTag, core.Binary(marshalFolderEntryID(binding.mailboxGUID, store.replica, f.parent)))
		putContentsValue(values, 0x3001001f, core.Text(f.name))
		putContentsValue(values, 0x66380003, core.Integer(children[f.fid]))
		putContentsValue(values, 0x360a000b, core.Bool(children[f.fid] > 0))
		putContentsValue(values, 0x65e00102, core.Binary(append([]byte(nil), f.sourceKey[:]...)))
		putContentsValue(values, 0x65e20102, core.Binary([]byte(f.changeKey)))
		putContentsValue(values, 0x30080040, core.Timestamp(f.modified))
		putContentsValue(values, 0x36010003, core.Integer(childFolderType(f.search)))
		// Native descendants of the IPM subtree are ordinary folders.
		if f.native != "" && f.native != store.root && !f.search {
			putContentsValue(values, folderFlagsTag, core.Integer(ipmNormalFolderFlags))
		}
		if f.hidden != nil {
			putContentsValue(values, folderHiddenTag, core.Bool(*f.hidden))
		}
		for i, tag := range []uint32{0x36020003, 0x36030003} {
			if f.counts.present[i] {
				putContentsValue(values, tag, core.Integer(f.counts.values[i]))
			}
		}
		shared := strings.HasPrefix(string(f.native), core.SharedGroupwareFolderIDPrefix)
		marker, marked := markers[f.native]
		if marked || aggregates && shared {
			var observed *core.MailFolderMarker
			if marked {
				observed = &marker
			}
			aggregate, err := store.observeFolderAggregatesAt(ctx, session, f.native, observed)
			// A folder can have no verified contents aggregate yet. In
			// that case only the two optional cells are unavailable.
			switch {
			case err == nil:
				seconds := int64(aggregate.committed/10000000) - 11644473600
				nanos := int64(aggregate.committed%10000000) * 100
				putContentsValue(values, 0x670a0040, core.Timestamp(time.Unix(seconds, nanos).UTC()))
				putContentsValue(values, 0x670b0003, core.Integer(aggregate.removed))
				if shared || marker.UntypedCount {
					putContentsValue(values, 0x36020003, core.Integer(aggregate.count))
				}
			case ctx.Err() != nil:
				return fail(ctx.Err())
			case errors.Is(err, core.ErrAccessDenied):
				return fail(err)
			}
		}
		if f.containerClass != "" {
			putContentsValue(values, 0x3613001f, core.Text(f.containerClass))
		}
		used += 1224 + 2*(64+folderEntryIDBytes) + 3*(len(f.name)+len(f.containerClass))
		if used > 2*maxHierarchyBytes {
			return fail(core.ErrUnsupported)
		}
		table.rows = append(table.rows, values)
	}
	if err = authorizeHierarchyTable(ctx, session, store, fid); err != nil {
		return fail(err)
	}
	if err = ctx.Err(); err != nil {
		return fail(err)
	}
	if err = table.lease.shrink(used); err != nil {
		return fail(err)
	}
	return table, nil
}

func (t *SessionTransport) authorizeTable(ctx context.Context, session core.Session, object serverObject, op byte) error {
	if message := object.table.attachmentMessage; message != nil {
		return object.binding.store.authorizeAttachmentContent(ctx, session, object.fid, *message)
	}
	if object.table.inboxRules {
		_, err := inboxRulesMailbox(ctx, session, object.binding, object.fid)
		return err
	}
	if object.table.permissions != nil {
		return t.authorizePermissionsTable(ctx, session, object, op)
	}
	if object.table.hierarchy {
		return authorizeHierarchyTable(ctx, session, object.binding.store, object.fid)
	}
	if object.table.flags&tableSoftDeletes != 0 {
		_, err := recoverableMailFolder(ctx, session, object.binding, object.fid)
		return err
	}
	if object.table.search != nil && !object.table.updating() {
		return object.table.search.authorize(ctx, session, object.binding.store, object.fid)
	}
	store := object.binding.store
	if _, err := store.scopedMessageAnchor(ctx, session, object.fid, object.table.associated, object.table.special); err != nil {
		return err
	}
	return store.checkSharedContentRights(ctx, session, object.fid, object.table.sharedRights)
}

func authorizeHierarchyTable(ctx context.Context, session core.Session, store *ramStore, fid uint64) error {
	if session == nil || session.Mailbox() == nil {
		return core.ErrAccessDenied
	}
	if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
		return err
	}
	if _, err := store.folder(ctx, session, fid); err != nil {
		return err
	}
	if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
		return err
	}
	return ctx.Err()
}
