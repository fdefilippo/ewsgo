// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const (
	syncObjectDeleted     = 0x80040800
	syncClientChangeNewer = 0x00040821
)

type importMessageMoveRequest struct {
	folder, source, destination [22]byte
	predecessors                faiPCL
	change                      changeXID
	digest                      [32]byte
}

func (importMessageMoveRequest) String() string   { return "importMessageMoveRequest{redacted}" }
func (importMessageMoveRequest) GoString() string { return "importMessageMoveRequest{redacted}" }

func parseImportMessageMove(r *reader, request *importMessageMoveRequest) error {
	start := r.pos
	sourceFolder := r.blob(22)
	sourceMessage := r.blob(22)
	predecessors := r.blob(25 * maxFAIPCLReplicas)
	destinationMessage := r.blob(22)
	change := r.blob(24)
	if r.err != nil || len(sourceFolder) != 22 || len(sourceMessage) != 22 || len(destinationMessage) != 22 ||
		!validFAIXID([22]byte(sourceFolder)) || !validFAIXID([22]byte(sourceMessage)) ||
		!validFAIXID([22]byte(destinationMessage)) || !changeXID(change).valid() || len(predecessors) == 0 {
		return ErrMalformed
	}
	pcl, err := parseFAIPCL(predecessors)
	if err != nil {
		return err
	}
	*request = importMessageMoveRequest{
		folder: [22]byte(sourceFolder), source: [22]byte(sourceMessage), destination: [22]byte(destinationMessage),
		predecessors: pcl, change: changeXID(change), digest: sha256.Sum256(r.data[start:r.pos]),
	}
	return nil
}

func executeImportMessageMove(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) []byte {
	reply := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if code != 0 {
		return reply(code)
	}
	if object.download == nil || object.download.collector == nil {
		return reply(NotSupported)
	}
	store := object.binding.store
	writer, _ := session.Mailbox().(core.MailMessagePlacementWriter)
	if store.durable.store == nil {
		return reply(ropNotImplemented)
	}
	c := object.download.collector
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.prepareNormalMutation(ctx, session); err != nil {
		return reply(faiError(err, false))
	}
	move, err := store.importMessageMove(ctx, session, object.fid, op.ImportMessageMove, writer)
	if err != nil {
		if errors.Is(err, core.ErrNotFound) {
			return reply(syncObjectDeleted)
		}
		return reply(faiError(err, false))
	}
	c.acknowledgeNormal(move.Destination, move.Result, true)
	if move.NewerClient {
		return reply(syncClientChangeNewer)
	}
	// MS-OXCFXICS 2.2.3.2.4.4.2 requires a zero MessageId on success.
	return binary.LittleEndian.AppendUint64(reply(0), 0)
}

func ownMoveCounter(replica [16]byte, source [22]byte) uint64 {
	if [16]byte(source[:16]) != replica {
		return 0
	}
	var counter uint64
	for _, b := range source[16:] {
		counter = counter<<8 | uint64(b)
	}
	return counter
}

func (s *ramStore) importMessageMove(ctx context.Context, session core.Session, destinationFID uint64, request importMessageMoveRequest, writer core.MailMessagePlacementWriter) (mapireplica.MessageMove, error) {
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	from := ownMoveCounter(s.replica, request.folder)
	source := ownMoveCounter(s.replica, request.source)
	destination := ownMoveCounter(s.replica, request.destination)
	to := folderGlobalCount(destinationFID)
	if from == 0 || source == 0 || destination == 0 || from == to || source == destination {
		return mapireplica.MessageMove{}, core.ErrInvalid
	}
	fid, mid, newMID := folderID(from), folderID(source), folderID(destination)
	s.mu.Lock()
	intent := s.messageMoves[mid]
	key := s.byMID[mid]
	version := s.messages[key]
	special := s.messageKeySpecialLocked(key)
	if intent.Source != 0 && (intent.Request != request.digest || intent.To != to) {
		s.mu.Unlock()
		return mapireplica.MessageMove{}, core.ErrConflict
	}
	_, boundMessage := s.byMID[newMID]
	_, boundFolder := s.byFID[newMID]
	_, pendingFolder := s.pendingCreations[newMID]
	free := s.clientReservedCounterLocked(destination) && !s.retiredCounterLocked(destination) && !boundMessage && !boundFolder && !pendingFolder
	s.mu.Unlock()
	if special != 0 && special != core.ContactsFolder || key.associated {
		return mapireplica.MessageMove{}, core.ErrUnsupported
	}
	fromFolder, err := s.scopedMessageAnchor(ctx, session, fid, false, special)
	if err != nil {
		return mapireplica.MessageMove{}, err
	}
	toFolder, err := s.messageAnchor(ctx, session, destinationFID, false)
	if err != nil {
		return mapireplica.MessageMove{}, err
	}
	if fromFolder.native == "" || toFolder.native == "" || fromFolder.native == toFolder.native {
		return mapireplica.MessageMove{}, core.ErrUnsupported
	}
	if special == core.ContactsFolder && (toFolder.native != s.trash || !s.contactsMetadata) {
		return mapireplica.MessageMove{}, core.ErrUnsupported
	}
	if intent.Result != 0 {
		return intent, nil
	}
	if key.id == "" || !version.present || version.parent != fid {
		return mapireplica.MessageMove{}, core.ErrNotFound
	}
	if !free {
		return mapireplica.MessageMove{}, core.ErrConflict
	}
	change, history, newer, err := s.messageMoveHistory(version, request)
	if err != nil {
		return mapireplica.MessageMove{}, err
	}
	if intent.Source != 0 && intent.NewerClient != newer {
		return mapireplica.MessageMove{}, core.ErrOutcomeUnknown
	}

	box := session.Mailbox()
	var current core.Object
	var moveNative func() (core.Object, error)
	if special == core.ContactsFolder {
		current, moveNative, err = contactMoveTarget(ctx, box, key.id, fromFolder.native, toFolder.native, intent.Source != 0)
	} else if writer != nil {
		current, err = box.Get(ctx, key.id)
		moveNative = func() (core.Object, error) {
			return writer.MoveMailMessage(ctx, key.id, intent.Revision, toFolder.native)
		}
	} else {
		return mapireplica.MessageMove{}, core.ErrUnsupported
	}
	if err != nil {
		if intent.Source != 0 {
			err = s.failMessageMove(ctx, mid, err)
		}
		return mapireplica.MessageMove{}, err
	}
	if current.ID != key.id || current.Kind != core.Message || special == 0 && current.Properties[core.IsDraft] != core.Bool(false) ||
		!validMoveMetadata(current) {
		return mapireplica.MessageMove{}, core.ErrUnsupported
	}
	if intent.Source == 0 {
		if current.Parent != fromFolder.native || current.Revision != version.nativeRevision {
			return mapireplica.MessageMove{}, core.ErrConflict
		}
		intent = mapireplica.MessageMove{
			Source: source, Destination: destination, From: from, To: to, NativeID: key.id,
			Revision: current.Revision, Request: request.digest, NewerClient: newer,
		}
		if intent.Special() != special {
			return mapireplica.MessageMove{}, core.ErrInvalid
		}
		if err := s.stageMessageMove(ctx, mid, intent); err != nil {
			return mapireplica.MessageMove{}, err
		}
	}
	placed := current
	switch current.Parent {
	case fromFolder.native:
		if current.Revision != intent.Revision {
			return mapireplica.MessageMove{}, s.failMessageMove(ctx, mid, core.ErrConflict)
		}
		placed, err = moveNative()
		if err != nil {
			return mapireplica.MessageMove{}, s.failMessageMove(ctx, mid, err)
		}
	case toFolder.native:
		// Lost SOAP or SQLite response. The exact native identity now occupies
		// the requested folder, so finish the old intent without another Move.
	default:
		return mapireplica.MessageMove{}, s.failMessageMove(ctx, mid, core.ErrConflict)
	}
	if placed.ID != key.id || placed.Kind != core.Message || placed.Parent != toFolder.native ||
		placed.Revision <= intent.Revision || !validMoveMetadata(placed) {
		return mapireplica.MessageMove{}, core.ErrOutcomeUnknown
	}
	cn, err := s.finishMessageMove(ctx, intent, placed, version, change, history)
	intent.Result = cn
	return intent, err
}

func contactMoveTarget(ctx context.Context, box core.Mailbox, id, from, to core.ID, pending bool) (core.Object, func() (core.Object, error), error) {
	reader, canRead := box.(core.ContactReader)
	writer, canWrite := box.(core.ContactWriter)
	trash, canReadTrash := box.(core.TrashedContactReader)
	if !canRead || !canWrite || !canReadTrash {
		return core.Object{}, nil, core.ErrUnsupported
	}
	readDestination := func() (core.Object, error) {
		item, err := trash.GetTrashedContact(ctx, to, id)
		if err != nil {
			return core.Object{}, err
		}
		return contactMessage(item, to)
	}
	before, err := reader.GetContact(ctx, from, id)
	if pending && errors.Is(err, core.ErrNotFound) {
		placed, err := readDestination()
		if err != nil {
			return core.Object{}, nil, errors.Join(core.ErrOutcomeUnknown, err)
		}
		return placed, nil, nil
	}
	if err != nil {
		return core.Object{}, nil, err
	}
	current, err := contactMessage(before, from)
	if err != nil {
		return core.Object{}, nil, err
	}
	return current, func() (core.Object, error) {
		if err := writer.TrashContact(ctx, before); err != nil {
			return core.Object{}, err
		}
		placed, err := readDestination()
		if err != nil {
			return core.Object{}, errors.Join(core.ErrOutcomeUnknown, err)
		}
		return placed, nil
	}, nil
}

func validMoveMetadata(object core.Object) bool {
	ms, ok := object.Properties[core.MetadataRevision].(core.Integer)
	_, hasRead := object.Properties[core.IsRead].(core.Bool)
	return ok && ms > 0 && uint64(ms) == uint64(object.Revision) && hasRead
}

func (s *ramStore) stageMessageMove(ctx context.Context, mid uint64, move mapireplica.MessageMove) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.retiredCounterLocked(move.Destination) {
		return core.ErrConflict
	}
	if s.mappingCountLocked() >= maxRAMMappings || s.next > maxGlobalCount-4 ||
		s.messageRegistryBytesLocked()+160+len(move.NativeID)+pendingMessageMoveBytes > maxMessageRegistryBytes {
		return core.ErrLimit
	}
	if err := s.advanceReplicaLocked(ctx, mapireplica.Mutation{MessageMove: &move}); err != nil {
		return err
	}
	if s.messageMoves == nil {
		s.messageMoves = map[uint64]mapireplica.MessageMove{}
	}
	s.messageMoves[mid] = move
	s.unboundMoves++
	return nil
}

func (s *ramStore) failMessageMove(ctx context.Context, mid uint64, cause error) error {
	if errors.Is(cause, core.ErrOutcomeUnknown) || !errors.Is(cause, core.ErrConflict) && !errors.Is(cause, core.ErrAccessDenied) &&
		!errors.Is(cause, core.ErrInvalid) && !errors.Is(cause, core.ErrNotFound) &&
		!errors.Is(cause, core.ErrUnsupported) {
		return errors.Join(core.ErrOutcomeUnknown, cause)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.advanceReplicaLocked(ctx, mapireplica.Mutation{CancelMessageMove: folderGlobalCount(mid)}); err != nil {
		return errors.Join(core.ErrOutcomeUnknown, err)
	}
	delete(s.messageMoves, mid)
	s.unboundMoves--
	return cause
}

func (s *ramStore) messageMoveHistory(version messageVersion, request importMessageMoveRequest) (changeXID, string, bool, error) {
	serverChange := s.messageChangeKey(version)
	serverHistory := s.messagePredecessors(version)
	previous, err := parseFAIPCL(serverHistory)
	if err != nil {
		return "", "", false, err
	}
	if !request.predecessors.includes(previous) {
		return "", "", false, core.ErrConflict
	}
	if !previous.includes(request.predecessors) {
		// A client with newer content must upload that content separately.
		return serverChange, string(serverHistory), true, nil
	}
	change := request.change
	merged, err := mergeFAIPCL(request.predecessors, faiPCL{change.namespace(): change.local()})
	if err != nil {
		return "", "", false, err
	}
	return change, string(merged.marshal()), false, nil
}

func (s *ramStore) finishMessageMove(ctx context.Context, intent mapireplica.MessageMove, placed core.Object, version messageVersion, change changeXID, history string) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next > maxGlobalCount-3 {
		return 0, core.ErrOutcomeUnknown
	}
	source, dest := folderID(intent.Source), folderID(intent.Destination)
	retired := messageKey{retired: intent.Source}
	key := messageKey{id: intent.NativeID, special: intent.Special()}
	tombstone := version
	tombstone.cn, tombstone.present, tombstone.change, tombstone.history = s.next+1, false, "", ""
	target := version
	target.parent, target.cn, target.nativeRevision = folderID(intent.To), s.next+2, placed.Revision
	target.change, target.history = change, history
	if modified, ok := placed.Properties[core.LastModifiedAt].(core.Timestamp); ok {
		target.modified = time.Time(modified).UTC()
	}
	read := bool(placed.Properties[core.IsRead].(core.Bool))
	if target.read != read {
		target.read, target.readCN = read, s.next+3
	}
	intent.Result = target.cn
	highWater := max(target.cn, target.readCN)
	if err := s.commitReplica(ctx, mapireplica.Mutation{
		HighWater: highWater, MessageMove: &intent,
		Projection: []mapireplica.Projection{s.persistedMessageProjection(source, tombstone), s.persistedMessageProjection(dest, target)},
	}); err != nil {
		return 0, errors.Join(core.ErrOutcomeUnknown, err)
	}
	s.byMID[source], s.byMID[dest] = retired, key
	s.byMessage[retired], s.byMessage[key] = source, dest
	s.messages[retired], s.messages[key] = tombstone, target
	s.messageMoves[source], s.next = intent, highWater
	s.unboundMoves--
	return target.cn, nil
}

func (s *ramStore) loadMessageMoves(moves []mapireplica.MessageMove) error {
	if len(moves) == 0 {
		return nil
	}
	s.messageMoves = make(map[uint64]mapireplica.MessageMove, len(moves))
	for _, move := range moves {
		source, destination := folderID(move.Source), folderID(move.Destination)
		key, mapped := s.byMID[source]
		v, observed := s.messages[key]
		if !mapped || !observed || v.parent != folderID(move.From) || s.byFID[folderID(move.To)] == (folderKey{}) ||
			!s.clientReservedCounterLocked(move.Destination) {
			return core.ErrInvalid
		}
		if move.Result == 0 {
			if key != (messageKey{id: move.NativeID, special: move.Special()}) || !v.present || s.byMID[destination] != (messageKey{}) {
				return core.ErrInvalid
			}
			s.unboundMoves++
		} else if key.retired != move.Source || v.present || s.byMID[destination] == (messageKey{}) {
			return core.ErrInvalid
		}
		s.messageMoves[source] = move
	}
	return nil
}
