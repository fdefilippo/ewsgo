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

const (
	needsSpooler    byte   = 0x02
	notInQueue      uint32 = 0x80040601
	spoolerLock     byte   = 0
	spoolerUnlock   byte   = 1
	spoolerFinished byte   = 2
)

type spooledDraft struct {
	mapireplica.SpoolerEntry
	owner uint32 // Local to the live, registered spooler logon.
}

func (s *ramStore) loadSpoolerQueue(entries []mapireplica.SpoolerEntry) error {
	if len(entries) == 0 {
		return nil
	}
	s.spoolerQueue = make(map[uint64]spooledDraft, len(entries))
	for _, entry := range entries {
		mid := folderID(entry.Counter)
		key := s.byMID[mid]
		if key.id == "" || key.associated || key.special != 0 || key.retired != 0 ||
			s.byFID[folderID(entry.Folder)] == (folderKey{}) || entry.Revision == 0 ||
			entry.State < mapireplica.SpoolerQueued || entry.State > mapireplica.SpoolerAccepted {
			return core.ErrInvalid
		}
		s.spoolerQueue[mid] = spooledDraft{SpoolerEntry: entry}
	}
	return nil
}

func (s *ramStore) unlockSpoolerOwner(owner uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for mid, entry := range s.spoolerQueue {
		if entry.owner == owner {
			entry.owner = 0
			s.spoolerQueue[mid] = entry
		}
	}
}

func (s *ramStore) writeSpoolerLocked(ctx context.Context, entry spooledDraft) error {
	if err := s.advanceReplicaLocked(ctx, mapireplica.Mutation{Spooler: &entry.SpoolerEntry}); err != nil {
		return err
	}
	if s.spoolerQueue == nil {
		s.spoolerQueue = make(map[uint64]spooledDraft)
	}
	s.spoolerQueue[folderID(entry.Counter)] = entry
	return nil
}

func (s *ramStore) removeSpoolerLocked(ctx context.Context, mid uint64) error {
	if err := s.advanceReplicaLocked(ctx, mapireplica.Mutation{ForgetSpooler: folderGlobalCount(mid)}); err != nil {
		return err
	}
	delete(s.spoolerQueue, mid)
	return nil
}

func (t *SessionTransport) executeSpoolerQueue(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	reply := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	handle := handles[op.Input]
	logon, code := objects.logonObject(handle, op.Logon)
	if code != 0 {
		return reply(code)
	}
	s := logon.binding.store
	if s.durable.store == nil || t.mailDraftSender == nil {
		return rejectLegacySubmission(objects, op, handles)
	}
	if session == nil || session.Identity().Scope != s.scope || session.Mailbox() == nil || session.Mailbox().Root() != s.root {
		return reply(accessDenied)
	}
	if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
		return reply(storeError(err))
	}
	if op.ID == ropSpoolerLockMessage && !logon.spooler {
		return reply(accessDenied)
	}
	if op.ID == ropSpoolerLockMessage && op.Flags > spoolerFinished {
		return reply(0x80070057)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	mid := op.SubmissionMID
	key := s.byMID[mid]
	if key.id == "" || key.associated || key.special != 0 || key.retired != 0 {
		return reply(PropertyNotFound)
	}
	entry := s.spoolerQueue[mid]
	if op.ID == ropAbortSubmit {
		if _, exists := s.byFID[op.Folder]; !exists {
			return reply(PropertyNotFound)
		}
		if entry.Counter == 0 {
			if v := s.messages[key]; v.parent != 0 && v.parent != op.Folder {
				return reply(PropertyNotFound)
			}
			return reply(notInQueue)
		}
		if folderID(entry.Folder) != op.Folder {
			return reply(PropertyNotFound)
		}
		if entry.State != mapireplica.SpoolerQueued || entry.owner != 0 {
			return reply(unableToAbort)
		}
		return reply(storeErrorOrSuccess(s.removeSpoolerLocked(ctx, mid)))
	}
	if entry.Counter == 0 {
		return reply(notInQueue)
	}
	if entry.State == mapireplica.SpoolerDispatching || entry.owner != 0 && entry.owner != handle {
		return reply(unableToAbort)
	}
	switch op.Flags {
	case spoolerLock:
		entry.owner = handle
	case spoolerUnlock:
		entry.owner = 0
	case spoolerFinished:
		if entry.owner != handle || entry.State != mapireplica.SpoolerAccepted {
			return reply(unableToAbort)
		}
		return reply(storeErrorOrSuccess(s.removeSpoolerLocked(ctx, mid)))
	}
	s.spoolerQueue[mid] = entry
	return reply(0)
}

func storeErrorOrSuccess(err error) uint32 {
	if err == nil {
		return 0
	}
	return storeError(err)
}

func (d *mailDraft) enqueue(ctx context.Context, session core.Session) error {
	s := d.store
	if s.durable.store == nil {
		return core.ErrUnsupported
	}
	if d.submitted {
		return core.ErrConflict
	}
	if err := d.checkSendable(); err != nil {
		return err
	}
	if _, err := d.save(ctx, session, 2); err != nil {
		return err
	}
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	current, err := session.Mailbox().Get(ctx, d.staged.ID)
	if err != nil {
		return err
	}
	if !validSavedMailDraft(current, d.folder) || current.ID != d.staged.ID || current.Revision != d.staged.Revision {
		return core.ErrConflict
	}
	entry := spooledDraft{SpoolerEntry: mapireplica.SpoolerEntry{
		Counter: folderGlobalCount(d.mid), Folder: folderGlobalCount(d.fid),
		Revision: d.staged.Revision, State: mapireplica.SpoolerQueued,
	}}
	s.mu.Lock()
	defer s.mu.Unlock()
	if previous := s.spoolerQueue[d.mid]; previous.Counter != 0 {
		if previous.SpoolerEntry == entry.SpoolerEntry {
			return nil
		}
		return core.ErrConflict
	}
	if len(s.spoolerQueue) >= mapireplica.MaxSpoolerEntries {
		return core.ErrLimit
	}
	return s.writeSpoolerLocked(ctx, entry)
}

func (d *mailDraft) claimSpoolerDispatch(ctx context.Context, owner uint32) (mapireplica.SpoolerState, error) {
	s := d.store
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.spoolerQueue[d.mid]
	if entry.Counter == 0 {
		return 0, nil
	}
	if owner == 0 || entry.owner != owner {
		return entry.State, core.ErrAccessDenied
	}
	switch entry.State {
	case mapireplica.SpoolerAccepted:
		return entry.State, nil
	case mapireplica.SpoolerDispatching:
		return entry.State, core.ErrOutcomeUnknown
	}
	if entry.Revision != d.staged.Revision || folderID(entry.Folder) != d.fid || d.dirty {
		return entry.State, core.ErrConflict
	}
	entry.State = mapireplica.SpoolerDispatching
	return entry.State, s.writeSpoolerLocked(ctx, entry)
}

func (d *mailDraft) recordSpoolerDispatch(ctx context.Context, submitErr error) error {
	if errors.Is(submitErr, core.ErrOutcomeUnknown) {
		return submitErr // Sending is durable and never retried after an indeterminate SOAP outcome.
	}
	s := d.store
	s.mu.Lock()
	defer s.mu.Unlock()
	entry := s.spoolerQueue[d.mid]
	if entry.State != mapireplica.SpoolerDispatching {
		return errors.Join(core.ErrOutcomeUnknown, submitErr)
	}
	entry.State = mapireplica.SpoolerAccepted
	if submitErr != nil {
		entry.State = mapireplica.SpoolerQueued
	}
	commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), 5*time.Second)
	defer cancel()
	if err := s.writeSpoolerLocked(commit, entry); err != nil {
		return errors.Join(core.ErrOutcomeUnknown, submitErr, err)
	}
	return submitErr
}

func submitSuccess(op Operation) []byte {
	reply := ropError(op.ID, op.Input, 0)
	if op.ID == ropTransportSend {
		return append(reply, 1) // NoPropertiesReturned.
	}
	return reply
}

func submissionError(op Operation, err error) []byte {
	if errors.Is(err, core.ErrInvalidSendRecipients) {
		return ropError(op.ID, op.Input, 0x00000467)
	}
	return ropError(op.ID, op.Input, faiError(err, true))
}

func (t *SessionTransport) submitViaSpooler(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, d *mailDraft) ([]byte, bool) {
	if op.ID == ropSubmitMessage && op.Flags == needsSpooler {
		if err := d.enqueue(ctx, session); err != nil {
			return submissionError(op, err), true
		}
		return submitSuccess(op), true
	}
	owner := uint32(0)
	if op.ID == ropTransportSend {
		if handle := objects.logons[op.Logon]; objects.objects[handle].spooler {
			owner = handle
		}
	}
	state, err := d.claimSpoolerDispatch(ctx, owner)
	if err != nil {
		return submissionError(op, err), true
	}
	if state == 0 {
		return nil, false
	}
	if state == mapireplica.SpoolerAccepted {
		return submitSuccess(op), true
	}
	_, err = d.submit(ctx, session, t.mailDraftSender)
	err = d.recordSpoolerDispatch(ctx, err)
	core.RecordActivity(ctx, core.Activity{Layer: "mapi", Operation: "SpoolerDispatch", Result: core.ActivityOutcome(err)})
	if err != nil {
		return submissionError(op, err), true
	}
	return submitSuccess(op), true
}

type spoolerLogonKey struct{}

func (entry spooledDraft) deniesAccess(ctx context.Context) bool {
	if entry.owner == 0 {
		return false
	}
	owner, _ := ctx.Value(spoolerLogonKey{}).(uint32)
	return entry.owner != owner
}

func (s *ramStore) isQueuedSubmission(mid uint64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.spoolerQueue[mid].Counter != 0
}

func (s *ramStore) isSpoolerQueue(fid uint64) bool {
	if s.durable.store == nil || fid == 0 {
		return false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.byFID[fid] == (folderKey{role: SpoolerQueue})
}

func (s *ramStore) spoolerMessageParent(ctx context.Context, session core.Session, mid uint64) (uint64, error) {
	owner, _ := ctx.Value(spoolerLogonKey{}).(uint32)
	if owner == 0 || session == nil || session.Identity().Scope != s.scope {
		return 0, core.ErrAccessDenied
	}
	s.mu.Lock()
	entry := s.spoolerQueue[mid]
	s.mu.Unlock()
	if entry.deniesAccess(ctx) {
		return 0, core.ErrAccessDenied
	}
	if entry.State != mapireplica.SpoolerQueued {
		return 0, core.ErrNotFound
	}
	return folderID(entry.Folder), nil
}

func (s *ramStore) observeSpoolerQueue(ctx context.Context, session core.Session, anchor uint64) (messageSnapshot, error) {
	owner, _ := ctx.Value(spoolerLogonKey{}).(uint32)
	if owner == 0 {
		return messageSnapshot{}, core.ErrAccessDenied
	}
	if _, err := s.messageAnchor(ctx, session, anchor, false); err != nil {
		return messageSnapshot{}, err
	}
	s.mu.Lock()
	view := messageSnapshot{replica: s.replica, root: anchor, highWater: s.next}
	entries := make([]spooledDraft, 0, len(s.spoolerQueue))
	for _, entry := range s.spoolerQueue {
		if entry.State == mapireplica.SpoolerQueued && !entry.deniesAccess(ctx) {
			entries = append(entries, entry)
		}
	}
	s.mu.Unlock()
	if len(entries) == 0 {
		return view, nil
	}
	reader, ok := session.Mailbox().(core.MailMessageMIMEReader)
	if !ok {
		return messageSnapshot{}, core.ErrUnsupported
	}
	for _, entry := range entries {
		mid, fid := folderID(entry.Counter), folderID(entry.Folder)
		key, folder, err := s.openedMessageScope(ctx, session, fid, openedMessage{mid: mid})
		if errors.Is(err, core.ErrNotFound) || errors.Is(err, core.ErrAccessDenied) {
			continue // The draft moved or was locked during the scan.
		}
		if err != nil {
			return messageSnapshot{}, err
		}
		object, err := reader.ReadMailMessageMIME(ctx, folder.native, key.id, false)
		if errors.Is(err, core.ErrNotFound) {
			continue
		}
		if err != nil {
			return messageSnapshot{}, err
		}
		if !validSavedMailDraft(object, folder.native) || object.ID != key.id || object.Revision != entry.Revision {
			continue
		}
		row := messageRow{parent: fid, mid: mid, object: object, sourceKey: s.hierarchyXID(mid)}
		read, _ := object.Properties[core.IsRead].(core.Bool)
		row.read = bool(read)
		view.retainedBytes += len(object.MIME) + 2048
		if view.retainedBytes > core.MaxMailContentViewBytes {
			return messageSnapshot{}, core.ErrLimit
		}
		view.messages = append(view.messages, row)
	}
	return view, nil
}
