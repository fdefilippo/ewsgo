// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"slices"
	"strings"

	"ewsgo/internal/core"
)

const folderCopyProgressSize = 15

type folderCopyPlan struct {
	folder   core.Object
	messages []core.ID
	fai      []folderCopyFAI
	metadata folderMetadata
}

type folderCopyFAI struct {
	id       core.ID
	revision core.Revision
}

type folderCopyJob struct {
	plan      []folderCopyPlan
	root      folderCopyPlacement
	placed    map[core.ID]folderCopyPlacement
	target    core.ID
	name      string
	completed int
	lease     *contentsLease
}

type folderCopyPlacement struct {
	id, parent core.ID
	name       core.Text
}

func copyStoreFolder(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32, codePage uint32) []byte {
	request := op.FolderPlacement
	input := handles[op.Input]
	source, code := objects.object(input, op.Logon)
	if code != 0 {
		return folderPlacementResponse(op, code, false)
	}
	destination, code := objects.object(handles[request.destination], op.Logon)
	if code == nullObject {
		code = moveCopyNullDestination
	}
	if code != 0 {
		return folderPlacementResponse(op, code, false)
	}
	if source.isLogon || destination.isLogon || source.download != nil || destination.download != nil ||
		destination.binding.store != source.binding.store {
		return folderPlacementResponse(op, NotSupported, false)
	}
	name, code := folderPlacementName(request, codePage)
	if code != 0 {
		return folderPlacementResponse(op, code, false)
	}
	if source.folderCopy != nil {
		return folderPlacementResponse(op, 0x8004010b, false) // Busy.
	}
	s := source.binding.store
	job, err := s.prepareFolderCopy(ctx, session, source.fid, destination.fid, request, name)
	if err != nil {
		return folderPlacementResponse(op, folderPlacementCode(err), false)
	}
	if request.async && len(job.plan) > 1 {
		job.lease, err = s.reserveContentsMemory(job.memoryBytes())
		if err != nil {
			return folderPlacementResponse(op, storeError(err), false)
		}
		done, partial, err := s.advanceFolderCopyStep(ctx, session, job)
		if done {
			job.lease.release()
			return folderPlacementResponse(op, folderPlacementCode(err), partial)
		}
		source.folderCopy = job
		objects.objects[input] = source
		return folderCopyProgressResponse(op, job)
	}
	for {
		done, partial, err := s.advanceFolderCopyStep(ctx, session, job)
		if done {
			return folderPlacementResponse(op, folderPlacementCode(err), partial)
		}
	}
}

func (s *ramStore) prepareFolderCopy(ctx context.Context, session core.Session, source, destination uint64, request folderPlacementRequest, name string) (*folderCopyJob, error) {
	from, err := s.folder(ctx, session, source)
	if err != nil {
		return nil, err
	}
	to, err := s.folder(ctx, session, destination)
	if err != nil {
		return nil, err
	}
	if from.native == "" || to.native == "" || from.search || to.search {
		return nil, core.ErrUnsupported
	}
	s.mu.Lock()
	key := s.byFID[request.fid]
	s.mu.Unlock()
	if key.native == "" {
		return nil, core.ErrNotFound
	}
	if key.native == s.root {
		return nil, core.ErrAccessDenied
	}
	box := session.Mailbox()
	reader, readOK := box.(core.MailFolderCopyReader)
	observer, observeOK := box.(core.MailFolderObserver)
	_, folderOK := box.(core.MailFolderHierarchyWriter)
	_, messageOK := box.(core.MailMessagePlacementWriter)
	if !readOK || !observeOK || !folderOK || !messageOK {
		return nil, core.ErrUnsupported
	}
	if err := hierarchyOwner(ctx, box); err != nil {
		return nil, err
	}
	tree, err := reader.ReadMailFolderCopyTree(ctx, key.native, to.native, request.recursive)
	if err != nil {
		return nil, err
	}
	if len(tree) == 0 || tree[0].ID != key.native || tree[0].Parent != from.native {
		return nil, core.ErrNotFound
	}
	plan, err := s.planFolderCopy(ctx, observer, tree, request.recursive)
	if err != nil {
		return nil, err
	}
	return &folderCopyJob{plan: plan, target: to.native, name: name, placed: make(map[core.ID]folderCopyPlacement, len(plan))}, nil
}

func (job *folderCopyJob) memoryBytes() int {
	bytes := 512 + len(job.name) + len(job.target)
	for _, entry := range job.plan {
		name, _ := entry.folder.Properties[core.DisplayName].(core.Text)
		bytes += 768 + len(entry.folder.ID) + len(entry.folder.Parent) + len(name)
		for _, mid := range entry.messages {
			bytes += 24 + len(mid)
		}
		for _, item := range entry.fai {
			bytes += 32 + len(item.id)
		}
		for _, value := range entry.metadata {
			bytes += 48 + len(value)
		}
	}
	return bytes
}

func folderCopyProgressResponse(op Operation, job *folderCopyJob) []byte {
	out := append(ropError(ropProgress, op.Input, 0), op.Logon)
	return append32(append32(out, uint32(job.completed)), uint32(len(job.plan)))
}

func pollFolderCopy(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	input := handles[op.Input]
	object, code := objects.lookup(input, op.Logon)
	if code != 0 {
		return ropError(ropProgress, op.Input, code)
	}
	job := object.folderCopy
	if job == nil {
		return ropError(ropProgress, op.Input, NotSupported)
	}
	s := object.binding.store
	if session == nil || session.Identity().Scope != s.scope || session.Mailbox() == nil || session.Mailbox().Root() != s.root {
		return ropError(ropProgress, op.Input, accessDenied)
	}
	if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
		return ropError(ropProgress, op.Input, storeError(err))
	}
	code, partial := uint32(0x80040113), true // MAPI_E_USER_CANCEL.
	if op.Flags == 0 {
		done, partialCopy, err := s.advanceFolderCopyStep(ctx, session, job)
		if !done {
			return folderCopyProgressResponse(op, job)
		}
		code, partial = folderPlacementCode(err), partialCopy
	}
	job.lease.release()
	object.folderCopy = nil
	objects.objects[input] = object
	op.ID = ropCopyFolder
	return folderPlacementResponse(op, code, partial)
}

// A step confirms one folder and its contents while holding the hierarchy
// lock. Planning and the intervals between steps allow other mutations.
func (s *ramStore) advanceFolderCopyStep(ctx context.Context, session core.Session, job *folderCopyJob) (done, partial bool, err error) {
	s.hierarchyMu.Lock()
	defer s.hierarchyMu.Unlock()
	box := session.Mailbox()
	folders, folderOK := box.(core.MailFolderHierarchyWriter)
	messages, messageOK := box.(core.MailMessagePlacementWriter)
	if !folderOK || !messageOK {
		return true, job.completed > 0, core.ErrUnsupported
	}
	entry := job.plan[job.completed]
	parent, name := job.target, job.name
	if job.completed == 0 {
		// Refresh the native destination and cycle check after planning.
		reader, ok := box.(core.MailFolderCopyReader)
		if !ok {
			return true, false, core.ErrUnsupported
		}
		if _, err := reader.ReadMailFolderCopyTree(ctx, entry.folder.ID, job.target, false); err != nil {
			return true, false, err
		}
	} else {
		target := job.placed[entry.folder.Parent]
		parent, name = target.id, string(entry.folder.Properties[core.DisplayName].(core.Text))
		if !job.root.matches(ctx, box) || target.id != job.root.id && !target.matches(ctx, box) {
			return true, true, core.ErrConflict
		}
	}
	created, err := s.copyFolderEntry(ctx, session, folders, messages, entry, parent, name)
	if err != nil {
		if job.completed != 0 || created.ID != "" {
			return true, true, nil // Confirmed writes remain visible.
		}
		// A lost acknowledgment might have created a root. Never bind a
		// same-name folder or retry individual SOAP writes here.
		partial := errors.Is(err, core.ErrOutcomeUnknown)
		if !partial && errors.Is(err, core.ErrConflict) {
			err = core.ErrFolderNameConflict
		}
		return true, partial, err
	}
	placement := folderCopyPlacement{id: created.ID, parent: parent, name: core.Text(name)}
	if job.completed == 0 {
		job.root = placement
	}
	job.placed[entry.folder.ID] = placement
	job.completed++
	return job.completed == len(job.plan), false, nil
}

func (expected folderCopyPlacement) matches(ctx context.Context, box core.Mailbox) bool {
	actual, err := box.Get(ctx, expected.id)
	return err == nil && actual.ID == expected.id && actual.Kind == core.Folder && actual.Parent == expected.parent &&
		actual.Properties[core.DisplayName] == expected.name
}

func (s *ramStore) planFolderCopy(ctx context.Context, observer core.MailFolderObserver, tree []core.Object, recursive bool) ([]folderCopyPlan, error) {
	plan := make([]folderCopyPlan, len(tree))
	index := make(map[core.ID]int, len(tree))
	for i, folder := range tree {
		_, duplicate := index[folder.ID]
		_, parentSeen := index[folder.Parent]
		name, named := folder.Properties[core.DisplayName].(core.Text)
		if folder.ID == "" || folder.Kind != core.Folder || duplicate || i > 0 && !parentSeen || !named || name == "" {
			return nil, core.ErrInvalid
		}
		observation, err := observer.ObserveMailFolder(ctx, folder.ID, nil)
		if err != nil {
			return nil, err
		}
		if observation.MessageCount > core.MaxMailContentMessages {
			return nil, core.ErrUnsupported
		}
		if uint64(len(observation.MessageIDs)) != observation.MessageCount {
			return nil, core.ErrConflict
		}
		plan[i] = folderCopyPlan{folder: core.Object{ID: folder.ID, Parent: folder.Parent,
			Properties: core.Properties{core.DisplayName: name}}, messages: observation.MessageIDs}
		index[folder.ID] = i
	}
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	if recursive {
		for _, folder := range s.dynamic {
			if _, included := index[folder.parent.native]; included && !folder.softDeleted {
				return nil, core.ErrUnsupported
			}
		}
	}
	for i := range plan {
		fid := s.byKey[folderKey{native: plan[i].folder.ID}]
		for id, value := range s.metadata[fid] {
			if id != folderHiddenTag>>16 && (id != 0x3613 || !strings.EqualFold(string(value), "IPF.Note")) {
				// Folder references and other container types need their own projection.
				return nil, core.ErrUnsupported
			}
		}
		plan[i].metadata = cloneMetadata(s.metadata[fid])
	}
	for id, record := range s.fai.records {
		if i, included := index[record.object.Parent]; included && !record.deleted {
			plan[i].fai = append(plan[i].fai, folderCopyFAI{id: id, revision: record.object.Revision})
		}
	}
	for i := range plan {
		slices.SortFunc(plan[i].fai, func(a, b folderCopyFAI) int { return strings.Compare(string(a.id), string(b.id)) })
	}
	return plan, nil
}

func (s *ramStore) copyFolderEntry(ctx context.Context, session core.Session, folders core.MailFolderHierarchyWriter, messages core.MailMessagePlacementWriter, entry folderCopyPlan, parent core.ID, name string) (core.Object, error) {
	if err := ctx.Err(); err != nil {
		return core.Object{}, err
	}
	box := session.Mailbox()
	current, err := box.Get(ctx, entry.folder.ID)
	if err != nil {
		return core.Object{}, err
	}
	if current.ID != entry.folder.ID || current.Kind != core.Folder || current.Parent != entry.folder.Parent ||
		current.Properties[core.DisplayName] != entry.folder.Properties[core.DisplayName] {
		return core.Object{}, core.ErrNotFound
	}
	created, err := folders.CreateMailFolder(ctx, core.MailFolderPlacement{Parent: parent, Name: name})
	if err != nil {
		if created.ID != "" {
			err = errors.Join(core.ErrOutcomeUnknown, err)
		}
		return core.Object{}, err
	}
	if created.ID == "" || created.ID == current.ID || created.Kind != core.Folder || created.Parent != parent ||
		created.Properties[core.DisplayName] != core.Text(name) {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	for _, id := range entry.messages {
		current, err := box.Get(ctx, id)
		if err != nil {
			return created, err
		}
		if current.ID != id || current.Parent != entry.folder.ID || current.Kind != core.Message {
			return created, core.ErrNotFound
		}
		if current.Properties[core.MetadataRevision] == nil {
			return created, core.ErrUnsupported
		}
		copied, err := messages.CopyMailMessage(ctx, id, current.Revision, created.ID)
		if err != nil {
			return created, err
		}
		if copied.ID == "" || copied.ID == id || copied.Kind != core.Message || copied.Parent != created.ID {
			return created, core.ErrOutcomeUnknown
		}
	}
	if len(entry.metadata)+len(entry.fai) == 0 {
		return created, nil
	}
	fids, err := s.assign(ctx, []folderKey{{native: created.ID}})
	if err != nil {
		return created, err
	}
	if err := s.copyFolderMetadata(ctx, fids[0], entry.metadata); err != nil {
		return created, err
	}
	for _, item := range entry.fai {
		if err := s.copyFolderFAI(ctx, session, entry.folder.ID, fids[0], item); err != nil {
			return created, err
		}
	}
	return created, nil
}

func (s *ramStore) copyFolderMetadata(ctx context.Context, fid uint64, metadata folderMetadata) error {
	if len(metadata) == 0 {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	used := s.metadataBytes + metadata.accountedBytes()
	if used > maxStoreMetadataBytes || len(s.metadata[fid]) != 0 {
		return core.ErrConflict
	}
	if err := s.persistMetadataLocked(ctx, map[uint64]folderMetadata{fid: metadata}); err != nil {
		return err
	}
	if s.metadata == nil {
		s.metadata = make(map[uint64]folderMetadata)
	}
	s.metadata[fid], s.metadataBytes = metadata, used
	return nil
}

func (s *ramStore) copyFolderFAI(ctx context.Context, session core.Session, source core.ID, destination uint64, item folderCopyFAI) error {
	draft, err := s.stageLocalMessage(ctx, session, destination, false)
	if err != nil {
		return err
	}
	defer draft.close()
	s.contentsMu.Lock()
	record, ok := s.fai.records[item.id]
	if !ok || record.deleted || record.object.Parent != source || record.object.Revision != item.revision {
		s.contentsMu.Unlock()
		return core.ErrConflict
	}
	draft.properties, err = core.CloneProperties(record.object.Properties)
	s.contentsMu.Unlock()
	if err != nil {
		return err
	}
	_, err = draft.save(ctx, session, 0)
	return err
}
