// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"strings"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

type ramFolder struct {
	id            uint64
	parent        folderKey
	name, comment string
	depth         int
	creator       uint32 // Process-local MAPI context that created this folder.
	search        bool
	softDeleted   bool
	criteria      *folderSearchCriteria
}

func (ramFolder) String() string   { return "ramFolder{redacted}" }
func (ramFolder) GoString() string { return "ramFolder{redacted}" }

// Callers serialize mutations/projection observations with hierarchyMu; mu
// protects the maps and is never held over a backend operation.
func (s *ramStore) projectedNodes(objects []core.Object) ([]hierarchyNode, error) {
	s.mu.Lock()
	dynamic := make([]ramFolder, 0, len(s.dynamic))
	for _, folder := range s.dynamic {
		if !folder.softDeleted {
			dynamic = append(dynamic, folder)
		}
	}
	metadata := make(map[folderKey]folderMetadata, len(s.metadata))
	for fid, data := range s.metadata {
		metadata[s.byFID[fid]] = cloneMetadata(data)
	}
	hasLocalFreeBusy := s.localFreeBusy.mid != 0
	s.mu.Unlock()
	nodes, err := hierarchyNodesForStore(objects, s.root, s.draftsReference, s.privateConfig, dynamic...)
	if err != nil {
		return nil, err
	}
	for i := range nodes {
		if nodes[i].key == (folderKey{role: freeBusyFolderRole}) && hasLocalFreeBusy {
			nodes[i].counts.values = [2]uint32{1, 1}
		}
		if data := metadata[nodes[i].key]; len(data) != 0 {
			nodes[i].fingerprint = metadataFingerprint(data, nodes[i].fingerprint)
			nodes[i].creator = 0 // The row includes more than the original creation.
			if value := data[folderHiddenTag>>16]; len(value) == 1 {
				hidden := value[0] != 0
				nodes[i].hidden = &hidden
			}
		}
	}
	return nodes, nil
}

func (s *ramStore) currentProjection(ctx context.Context, session core.Session) ([]hierarchyNode, error) {
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
	root, err := box.Get(ctx, s.root)
	if err != nil {
		return nil, err
	}
	if root.Kind != core.Folder || root.ID != s.root {
		return nil, core.ErrInvalid
	}
	catalog, objects, err := readPrivateFolderTreeWithSpecialFolders(ctx, session, s.calendarMetadata, s.contactsMetadata, s.tasksMetadata)
	if err != nil {
		return nil, err
	}
	if catalog.scope != s.scope || catalog.folders[IPMSubtree].NativeID != s.root {
		return nil, core.ErrConflict
	}
	return s.projectedNodes(objects)
}

func (s *ramStore) createFolder(ctx context.Context, session core.Session, parent uint64, request createFolderRequest) (uint64, uint32) {
	s.hierarchyMu.Lock()
	defer s.hierarchyMu.Unlock()
	parentFolder, err := s.folder(ctx, session, parent)
	if err != nil {
		return 0, storeError(err)
	}
	if parentFolder.search {
		return 0, NotSupported
	}
	nodes, err := s.currentProjection(ctx, session)
	if err != nil {
		return 0, storeError(err)
	}
	s.mu.Lock()
	key, exists := s.byFID[parent]
	s.mu.Unlock()
	if !exists {
		return 0, PropertyNotFound
	}
	found := false
	matches := 0
	var matching folderKey
	for _, n := range nodes {
		found = found || n.key == key
		if n.key != n.parent && n.parent == key && strings.EqualFold(n.name, request.name) {
			matches++
			matching = n.key
		}
	}
	if !found {
		return 0, PropertyNotFound
	}
	if matches > 1 {
		return 0, 0x80004005
	} // Ambiguous projection, never choose arbitrarily.
	if matches == 1 {
		if !request.existing {
			return 0, duplicateFolderName
		}
		ids, err := s.assign(ctx, []folderKey{matching})
		if err != nil {
			return 0, storeError(err)
		}
		if _, err := s.folder(ctx, session, ids[0]); err != nil {
			return 0, storeError(err)
		}
		return ids[0], 0
	}
	// A fresh authorization immediately precedes the RAM commit. Native create
	// rights are not inferred: this is an explicitly opted-in overlay operation.
	if _, err := s.folder(ctx, session, parent); err != nil {
		return 0, storeError(err)
	}
	if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
		return 0, storeError(err)
	}
	if err := ctx.Err(); err != nil {
		return 0, storeError(err)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, storeError(err)
	}
	depth := 1
	if key.dynamic != 0 {
		depth = s.dynamic[key.dynamic].depth + 1
	}
	budget := 128 + len(request.name) + len(request.comment) + len(key.native)
	if len(s.dynamic) >= maxDynamicFolders || depth > maxDynamicDepth || s.dynamicBytes+budget > maxDynamicBytes || s.mappingCountLocked() >= s.mappingLimit() || s.next >= maxGlobalCount {
		return 0, 0x8007000e
	}
	// One counter allocates both the private key discriminator and the FID;
	// keys have disjoint native/role/dynamic fields and never collide.
	next := s.next + 1
	newKey := folderKey{dynamic: next}
	fid := folderID(next)
	folder := ramFolder{id: next, parent: key, name: request.name, comment: request.comment, depth: depth, search: request.kind == searchFolderType, creator: request.origin}
	if s.durable.store != nil {
		binding, err := persistedFolderBinding(newKey, fid)
		if err != nil {
			return 0, storeError(err)
		}
		value, err := s.folderDefinitionValue(fid, folder)
		if err != nil {
			return 0, storeError(err)
		}
		if err := s.commitReplica(ctx, mapireplica.Mutation{
			HighWater: next, Bindings: []mapireplica.Binding{binding}, FolderValues: []mapireplica.FolderValue{value},
		}); err != nil {
			return 0, storeError(err)
		}
	}
	if s.dynamic == nil {
		s.dynamic = make(map[uint64]ramFolder)
	}
	s.dynamic[next] = folder
	s.byKey[newKey], s.byFID[fid] = fid, newKey
	s.next = next
	s.dynamicBytes += budget
	s.localNotifications.publish(
		objectNotification{flag: notifyCreated, fid: fid, parent: parent},
		objectNotification{flag: notifyModified, fid: parent},
	)
	return fid, 0
}

// Reauthorize the nearest non-dynamic ancestor on every access. Dynamic
// parents are immutable and bounded to eight levels, so missing/revoked
// native ancestors cannot be bypassed by a previously allocated RAM FID.
func (s *ramStore) dynamicFolder(ctx context.Context, session core.Session, fid uint64, key folderKey) (storeFolder, error) {
	s.mu.Lock()
	folder, exists := s.dynamic[key.dynamic]
	at := folder.parent
	parentFID := s.byKey[at]
	for steps := 0; at.dynamic != 0; steps++ {
		ancestor, ok := s.dynamic[at.dynamic]
		if !ok || steps >= maxDynamicDepth {
			s.mu.Unlock()
			return storeFolder{}, core.ErrInvalid
		}
		at = ancestor.parent
	}
	ancestorFID := s.byKey[at]
	s.mu.Unlock()
	if !exists || folder.softDeleted || parentFID == 0 || ancestorFID == 0 {
		return storeFolder{}, core.ErrNotFound
	}
	if _, err := s.folder(ctx, session, ancestorFID); err != nil {
		return storeFolder{}, err
	}
	return storeFolder{fid: fid, parent: parentFID, properties: emptyNormalFolderProperties(folder.name), comment: folder.comment, dynamic: true, search: folder.search}, nil
}

func (s *ramStore) folderChildCount(ctx context.Context, session core.Session, fid uint64) (int, error) {
	s.hierarchyMu.Lock()
	defer s.hierarchyMu.Unlock()
	if _, err := s.folder(ctx, session, fid); err != nil {
		return 0, err
	}
	nodes, err := s.currentProjection(ctx, session)
	if err != nil {
		return 0, err
	}
	s.mu.Lock()
	key, ok := s.byFID[fid]
	s.mu.Unlock()
	if !ok {
		return 0, core.ErrNotFound
	}
	count := 0
	for _, n := range nodes {
		if n.parent == key && n.key != key {
			count++
		}
	}
	return count, ctx.Err()
}
