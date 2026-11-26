// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

func (s *ramStore) loadDraftReplica(placements []mapireplica.DraftPlacement) error {
	for _, draft := range placements {
		fid, mid := folderID(draft.Folder), folderID(draft.Counter)
		folder, exists := s.byFID[fid]
		outbox := folder == (folderKey{role: Outbox})
		if !exists || !outbox && (folder.native != draft.NativeFolder || draft.CopyOf != "" || draft.NativeID != "") {
			return core.ErrInvalid
		}
		if draft.NativeID == "" {
			if s.byMID[mid].id != "" {
				return core.ErrInvalid
			}
			if s.pendingMailDrafts == nil {
				s.pendingMailDrafts = make(map[string]pendingMailDraft)
			}
			s.pendingMailDrafts[draft.Reference] = pendingMailDraft{
				fid: fid, mid: mid, folder: draft.NativeFolder, transportSource: draft.CopyOf, dispatched: true,
			}
			continue
		}
		key := messageKey{id: draft.NativeID}
		if s.byMessage[key] != mid || s.outboxDrafts[draft.NativeID].folder != "" {
			return core.ErrInvalid
		}
		if s.outboxDrafts == nil {
			s.outboxDrafts = make(map[core.ID]outboxDraft)
		}
		s.outboxDrafts[draft.NativeID] = outboxDraft{
			folder: draft.NativeFolder, transportSource: draft.CopyOf, reference: draft.Reference,
			revision: draft.SubmissionRevision, trashPending: draft.TrashPending,
		}
	}
	return nil
}

func draftPlacement(ref string, pending pendingMailDraft) mapireplica.DraftPlacement {
	return mapireplica.DraftPlacement{
		Counter: folderGlobalCount(pending.mid), Folder: folderGlobalCount(pending.fid),
		Reference: ref, NativeFolder: pending.folder, CopyOf: pending.transportSource,
	}
}

func (s *ramStore) setDraftDispatched(ctx context.Context, reference string, dispatched bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	pending, exists := s.pendingMailDrafts[reference]
	if !exists || pending.dispatched == dispatched {
		return nil
	}
	if s.durable.store != nil {
		var mutation mapireplica.Mutation
		if dispatched {
			mutation.Drafts = []mapireplica.DraftPlacement{draftPlacement(reference, pending)}
		} else {
			mutation.ForgetDrafts = []uint64{folderGlobalCount(pending.mid)}
		}
		if err := s.advanceReplicaLocked(ctx, mutation); err != nil {
			return err
		}
	}
	pending.dispatched = dispatched
	s.pendingMailDrafts[reference] = pending
	return nil
}

func (s *ramStore) addDraftAcknowledgment(mutation *mapireplica.Mutation, reference string, pending pendingMailDraft, native core.ID) {
	draft := draftPlacement(reference, pending)
	if s.byFID[pending.fid] == (folderKey{role: Outbox}) {
		draft.NativeID = native
		mutation.Drafts = append(mutation.Drafts, draft)
	} else {
		mutation.ForgetDrafts = append(mutation.ForgetDrafts, draft.Counter)
	}
}

func (s *ramStore) storeOutboxDispositionLocked(ctx context.Context, draft *mailDraft, entry outboxDraft, forget bool) error {
	if s.durable.store == nil {
		return nil
	}
	counter := folderGlobalCount(draft.mid)
	if forget {
		return s.advanceReplicaLocked(ctx, mapireplica.Mutation{ForgetDrafts: []uint64{counter}})
	}
	return s.advanceReplicaLocked(ctx, mapireplica.Mutation{Drafts: []mapireplica.DraftPlacement{{
		Counter: counter, Folder: folderGlobalCount(draft.fid), Reference: entry.reference, NativeFolder: entry.folder,
		NativeID: draft.staged.ID, CopyOf: entry.transportSource, SubmissionRevision: entry.revision, TrashPending: entry.trashPending,
	}}})
}

func (s *ramStore) resetRejectedDraft(ctx context.Context, reference string, failure error) error {
	commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.setDraftDispatched(commit, reference, false); err != nil {
		return errors.Join(core.ErrOutcomeUnknown, err)
	}
	return failure
}
