// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"

	"ewsgo/internal/core"
	"ewsgo/internal/mailmime"
)

// Only the placement and submission gate are local. Zimbra owns the MIME,
// revision and attachments, and the sender owns the durable submission intent.
type outboxDraft struct {
	folder          core.ID
	transportSource core.ID
	reference       string
	revision        core.Revision
	sending         bool
	trashPending    bool
}

type outboxSubmissionResult uint8

const (
	outboxUnconfirmed outboxSubmissionResult = iota
	outboxUncertain
	outboxRetained
	outboxTrashPending
	outboxTrashed
)

func (s *ramStore) outboxFID() uint64 {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byKey[folderKey{role: Outbox}]
}

func (s *ramStore) outboxNativeFolder(id core.ID) core.ID {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.outboxDrafts[id].folder
}

func (s *ramStore) linkOutboxDraftLocked(id core.ID, reference string, pending pendingMailDraft) {
	if s.outboxDrafts == nil {
		s.outboxDrafts = make(map[core.ID]outboxDraft)
	}
	if s.outboxDrafts[id].folder == "" {
		s.outboxDrafts[id] = outboxDraft{folder: pending.folder, transportSource: pending.transportSource, reference: reference}
		s.localNotifications.publish(objectNotification{flag: notifyCreated | notifyMessage, fid: pending.fid, mid: pending.mid})
	}
}

func (s *ramStore) nativeOutboxDraftsFolder(ctx context.Context, session core.Session) (core.ID, error) {
	objects, err := s.metadataFolderObjects(ctx, session)
	if err != nil {
		return "", err
	}
	var folder core.ID
	for _, object := range objects {
		if object.Properties[core.WellKnown] != core.Text("drafts") {
			continue
		}
		if folder != "" || object.ID == s.root {
			return "", core.ErrConflict
		}
		folder = object.ID
	}
	if folder == "" {
		return "", core.ErrUnsupported
	}
	return folder, nil
}

func (s *ramStore) hasOutboxDrafts() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.outboxDrafts) != 0 {
		return true
	}
	fid := s.byKey[folderKey{role: Outbox}]
	for _, pending := range s.pendingMailDrafts {
		if pending.fid == fid && pending.dispatched {
			return true
		}
	}
	return false
}

// Called in the complete observation transaction. Drafts and Outbox expose
// mutually exclusive placements for a native ID within this MAPI replica.
func (s *ramStore) filterOutboxDrafts(view []core.Object, folder core.ID, outbox bool) []core.Object {
	s.mu.Lock()
	defer s.mu.Unlock()
	fid := s.byKey[folderKey{role: Outbox}]
	pendingInFolder := false
	for _, pending := range s.pendingMailDrafts {
		if pending.dispatched && pending.fid == fid && pending.folder == folder {
			pendingInFolder = true
			break
		}
	}
	if !outbox && !pendingInFolder && len(s.outboxDrafts) == 0 {
		return view
	}
	result := make([]core.Object, 0, len(view))
	for _, object := range view {
		virtual := false
		if object.Kind == core.Message && object.Parent == folder && object.Properties[core.IsDraft] == core.Bool(true) {
			virtual = s.outboxDrafts[object.ID].folder == folder
			if !virtual && pendingInFolder {
				pending := s.pendingMailDrafts[mailmime.DraftReference(object.MIME)]
				virtual = pending.dispatched && pending.fid == fid && pending.folder == folder
			}
		}
		if outbox != virtual {
			continue
		}
		if outbox {
			object.Parent = "" // The parent on MAPI is the virtual FID.
		}
		result = append(result, object)
	}
	return result
}

func (s *ramStore) editableOutboxDraft(id, folder core.ID) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.outboxDrafts[id]
	switch {
	case !ok || entry.folder != folder:
		return core.ErrNotFound
	case entry.sending:
		return core.ErrConflict
	case entry.revision != 0:
		return core.ErrOutcomeUnknown
	default:
		return nil
	}
}

func (s *ramStore) startOutboxSubmission(ctx context.Context, d *mailDraft) (outboxDraft, error) {
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	entry, ok := s.outboxDrafts[d.staged.ID]
	if !ok || entry.folder != d.folder {
		return outboxDraft{}, core.ErrNotFound
	}
	if entry.sending || entry.revision != 0 && entry.revision != d.staged.Revision {
		return outboxDraft{}, core.ErrConflict
	}
	claim := entry
	claim.revision, claim.sending = d.staged.Revision, true
	if entry.revision == 0 {
		if err := s.storeOutboxDispositionLocked(ctx, d, claim, false); err != nil {
			return outboxDraft{}, err
		}
	}
	s.outboxDrafts[d.staged.ID] = claim
	return entry, nil
}

func (s *ramStore) finishOutboxSubmission(ctx context.Context, d *mailDraft, previous outboxDraft, result outboxSubmissionResult) error {
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	current := s.outboxDrafts[d.staged.ID]
	current.sending = false
	s.outboxDrafts[d.staged.ID] = current
	if result == outboxRetained || result == outboxTrashed {
		if err := s.storeOutboxDispositionLocked(ctx, d, current, true); err != nil {
			return err
		}
		delete(s.outboxDrafts, d.staged.ID)
		s.localNotifications.publish(objectNotification{flag: notifyDeleted | notifyMessage, fid: d.fid, mid: d.mid})
		if nativeFID := s.byKey[folderKey{native: d.folder}]; result == outboxRetained && nativeFID != 0 {
			s.localNotifications.publish(objectNotification{flag: notifyCreated | notifyMessage, fid: nativeFID, mid: d.mid})
		}
		d.submitted = true
		return nil
	}
	entry := current
	if result == outboxUncertain || result == outboxTrashPending {
		entry.revision = d.staged.Revision
		entry.trashPending = result == outboxTrashPending
	} else {
		entry.revision, entry.trashPending = previous.revision, previous.trashPending
	}
	if entry != current {
		if err := s.storeOutboxDispositionLocked(ctx, d, entry, false); err != nil {
			return err
		}
	}
	s.outboxDrafts[d.staged.ID] = entry
	return nil
}
