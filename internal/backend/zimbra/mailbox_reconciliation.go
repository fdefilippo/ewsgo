// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"errors"
	"slices"
	"strings"

	"ewsgo/internal/core"
)

func (s *Session) reconcileMailbox(ctx context.Context, projection core.NativeProjection, limit int) error {
	before, err := projection.Baseline(ctx)
	if err != nil {
		return err
	}
	cursor, err := s.mailboxDeltaAnchor(ctx)
	if err != nil {
		return err
	}
	objects, err := s.snapshotFolders(ctx)
	if err != nil {
		return err
	}
	if err := s.scanMessages(ctx, objects, limit); err != nil {
		return err
	}

	// An index or tree omission is insufficient evidence of deletion.
	// Resolve every recognized prior member outside the enumeration.
	var tracked []core.ID
	for _, old := range before.Objects {
		ref, recognized := parseDeltaObject(old.ID)
		if !recognized {
			if before.Legacy {
				continue
			}
			return ErrProtocol
		}
		tracked = append(tracked, old.ID)
		if _, seen := objects[old.ID]; !seen {
			object, err := s.observeDeltaObject(ctx, ref)
			if errors.Is(err, core.ErrNotFound) {
				continue
			}
			if err != nil {
				return err
			}
			objects[object.ID] = object
		}
	}

	cursor, err = s.catchUpSnapshot(ctx, cursor, objects, limit)
	if err != nil {
		return err
	}
	if err := checkSnapshotHierarchy(objects); err != nil {
		return err
	}
	observed := make([]core.NativeObject, 0, len(objects))
	for _, object := range objects {
		observed = append(observed, object)
	}
	slices.SortFunc(observed, func(a, b core.NativeObject) int {
		return strings.Compare(string(a.ID), string(b.ID))
	})
	var removed []core.ID
	for _, id := range tracked {
		if _, present := objects[id]; !present {
			removed = append(removed, id)
		}
	}
	slices.Sort(removed)
	return projection.Reconcile(ctx, before, core.Cursor(cursor), observed, removed)
}

func (s *Session) snapshotFolders(ctx context.Context) (map[core.ID]core.NativeObject, error) {
	folders, err := s.folderCatalog(ctx, "", true)
	if err != nil {
		return nil, semanticError(err)
	}
	objects := make(map[core.ID]core.NativeObject, len(folders))
	for _, folder := range folders {
		if !nativeID.MatchString(folder.ID) || folder.MetadataRevision == nil || *folder.MetadataRevision == 0 {
			return nil, ErrProtocol
		}
		id := core.ID("f:" + folder.ID)
		if _, duplicate := objects[id]; duplicate {
			return nil, ErrProtocol
		}
		var parent core.ID
		if id != "f:1" {
			if !nativeID.MatchString(folder.Parent) {
				return nil, ErrProtocol
			}
			parent = core.ID("f:" + folder.Parent)
		}
		objects[id] = core.NativeObject{ID: id, Parent: parent, Kind: core.Folder, Revision: *folder.MetadataRevision}
	}
	if err := checkSnapshotHierarchy(objects); err != nil {
		return nil, err
	}
	return objects, nil
}

func (s *Session) scanMessages(ctx context.Context, objects map[core.ID]core.NativeObject, limit int) error {
	var after uint64
	for {
		refs, last, more, err := s.searchMailboxPage(ctx, after, limit)
		if err != nil {
			return err
		}
		for _, ref := range refs {
			object, err := s.observeDeltaObject(ctx, deltaObject{core.Message, ref})
			if errors.Is(err, core.ErrNotFound) {
				// Search and the direct lookup are separate reads. A concurrent
				// removal is covered by catch-up and by prior-member verification.
				continue
			}
			if err != nil {
				return err
			}
			objects[object.ID] = object
		}
		if !more {
			return nil
		}
		after = last
	}
}

func (s *Session) searchMailboxPage(ctx context.Context, after uint64, limit int) ([]string, uint64, bool, error) {
	hits, more, err := s.searchMessagePage(ctx, "is:anywhere", 0, after, limit)
	if err != nil {
		return nil, 0, false, err
	}
	refs := make([]string, 0, len(hits))
	last := after
	for _, hit := range hits {
		refs = append(refs, hit.id)
		last = hit.number
	}
	return refs, last, more, nil
}

func (s *Session) catchUpSnapshot(ctx context.Context, cursor string, objects map[core.ID]core.NativeObject, limit int) (string, error) {
	for {
		page, err := s.readMailboxDelta(ctx, cursor, limit)
		if err != nil {
			return "", err
		}
		observed, err := s.observeMailboxChanges(ctx, page)
		if err != nil {
			return "", err
		}
		removed, err := s.verifyMailboxRemovals(ctx, objects, mailboxRemovalHints(page))
		if err != nil {
			return "", err
		}
		for _, object := range observed {
			if old, seen := objects[object.ID]; seen && (old.Revision > object.Revision || old.Revision == object.Revision && old.Parent != object.Parent) {
				return "", core.ErrConflict
			}
			objects[object.ID] = object
		}
		for _, id := range removed {
			delete(objects, id)
		}
		cursor = page.Next
		if !page.More {
			return cursor, nil
		}
	}
}

func checkSnapshotHierarchy(objects map[core.ID]core.NativeObject) error {
	if root, found := objects["f:1"]; !found || root.Parent != "" || root.Kind != core.Folder {
		return ErrProtocol
	}
	rooted := map[core.ID]bool{"f:1": true}
	var path []core.ID
	for start := range objects {
		path = path[:0]
		for id := start; !rooted[id]; {
			object, found := objects[id]
			parent, hasParent := objects[object.Parent]
			if !found || !hasParent || parent.Kind != core.Folder || len(path) >= len(objects) {
				return core.ErrConflict
			}
			path = append(path, id)
			id = object.Parent
		}
		for _, id := range path {
			rooted[id] = true
		}
	}
	return nil
}

func parseDeltaObject(id core.ID) (deltaObject, bool) {
	prefix, ref, ok := strings.Cut(string(id), ":")
	if !ok || !nativeID.MatchString(ref) {
		return deltaObject{}, false
	}
	switch prefix {
	case "f":
		return deltaObject{core.Folder, ref}, true
	case "m":
		return deltaObject{core.Message, ref}, true
	default:
		return deltaObject{}, false
	}
}
