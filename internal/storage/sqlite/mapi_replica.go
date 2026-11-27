// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"crypto/rand"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const (
	maxMAPICounter   = 1<<48 - 1
	maxMAPIObjectID  = 4096
	maxMAPIObjectKey = 4104 // Native key up to 4096 bytes, plus frontend discriminators.
)

type MAPIReplicaVersion = mapireplica.Version
type MAPIObjectNamespace = mapireplica.Namespace
type MAPIObjectID = mapireplica.Binding
type MAPIReplicaSnapshot = mapireplica.Snapshot

const (
	MAPIFolder             = mapireplica.Folder
	MAPIMessage            = mapireplica.Message
	MAPIAssociatedMessage  = mapireplica.AssociatedMessage
	MAPIRecoverableMessage = mapireplica.RecoverableMessage
)

func validMAPIObjectID(binding MAPIObjectID) bool {
	return binding.Namespace >= MAPIFolder && binding.Namespace <= MAPIRecoverableMessage &&
		len(binding.Key) > 0 && len(binding.Key) <= maxMAPIObjectKey &&
		binding.Counter > 0 && binding.Counter <= maxMAPICounter
}

// LoadMAPIReplica opens the wire namespace and returns a consistent snapshot.
// mailboxID must be the authenticated backend instance binding from BindMailbox.
func (s *Store) LoadMAPIReplica(ctx context.Context, mailboxID string) (_ MAPIReplicaSnapshot, err error) {
	start := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "sqlite", Operation: "LoadMAPIReplica", Result: core.ActivityOutcome(err), Duration: time.Since(start)})
	}()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return MAPIReplicaSnapshot{}, fmt.Errorf("begin MAPI replica load: %w", err)
	}
	defer tx.Rollback()
	version, err := readMAPIReplica(ctx, tx, mailboxID)
	if errors.Is(err, core.ErrNotFound) {
		if err := requireMailbox(ctx, tx, mailboxID); err != nil {
			return MAPIReplicaSnapshot{}, err
		}
		version.Epoch = newMAPIReplicaEpoch()
		if _, err = tx.ExecContext(ctx, "INSERT INTO mapi_replicas VALUES (?, ?, 0)", mailboxID, version.Epoch[:]); err != nil {
			return MAPIReplicaSnapshot{}, fmt.Errorf("create MAPI replica: %w", err)
		}
	}
	if err != nil {
		return MAPIReplicaSnapshot{}, err
	}
	mappings, err := loadMAPIObjectIDs(ctx, tx, mailboxID, version.HighWater)
	if err != nil {
		return MAPIReplicaSnapshot{}, err
	}
	reserved, err := loadMAPIIDRanges(ctx, tx, mailboxID, version.HighWater)
	if err != nil {
		return MAPIReplicaSnapshot{}, err
	}
	projection, err := loadMAPIProjection(ctx, tx, mailboxID, version.HighWater)
	if err != nil {
		return MAPIReplicaSnapshot{}, err
	}
	fai, err := loadMAPIFAI(ctx, tx, mailboxID)
	if err != nil {
		return MAPIReplicaSnapshot{}, err
	}
	drafts, err := loadMAPIDraftPlacements(ctx, tx, mailboxID, version.HighWater)
	if err != nil {
		return MAPIReplicaSnapshot{}, err
	}
	values, err := loadMAPIFolderValues(ctx, tx, mailboxID)
	if err != nil {
		return MAPIReplicaSnapshot{}, err
	}
	creations, err := loadMAPIFolderCreations(ctx, tx, mailboxID, version.HighWater)
	if err != nil {
		return MAPIReplicaSnapshot{}, err
	}
	foreign, err := loadMAPIForeignReplicas(ctx, tx, mailboxID)
	if err != nil {
		return MAPIReplicaSnapshot{}, err
	}
	moves, err := loadMAPIMessageMoves(ctx, tx, mailboxID, version.HighWater)
	if err != nil {
		return MAPIReplicaSnapshot{}, err
	}
	imports, err := loadMAPIMessageImports(ctx, tx, mailboxID, version.HighWater)
	if err != nil {
		return MAPIReplicaSnapshot{}, err
	}
	spooler, err := loadMAPISpooler(ctx, tx, mailboxID)
	if err != nil {
		return MAPIReplicaSnapshot{}, err
	}
	if err := tx.Commit(); err != nil {
		return MAPIReplicaSnapshot{}, fmt.Errorf("commit MAPI replica load: %w", err)
	}
	return MAPIReplicaSnapshot{
		Version: version, Bindings: mappings, Reserved: reserved, Projection: projection, FAI: fai, Drafts: drafts, FolderValues: values, FolderCreations: creations, ForeignReplicas: foreign, MessageMoves: moves,
		MessageImports: imports, Spooler: spooler,
	}, nil
}

func newMAPIReplicaEpoch() (epoch [16]byte) {
	_, _ = rand.Read(epoch[:])
	epoch[7] = epoch[7]&0x0f | 0x40
	epoch[8] = epoch[8]&0x3f | 0x80
	return epoch
}

func readMAPIReplica(ctx context.Context, tx *sql.Tx, mailboxID string) (MAPIReplicaVersion, error) {
	var version MAPIReplicaVersion
	var epoch []byte
	err := tx.QueryRowContext(ctx, "SELECT epoch, high_water FROM mapi_replicas WHERE mailbox_id = ?", mailboxID).Scan(&epoch, &version.HighWater)
	if errors.Is(err, sql.ErrNoRows) {
		return MAPIReplicaVersion{}, core.ErrNotFound
	}
	if err != nil {
		return MAPIReplicaVersion{}, fmt.Errorf("read MAPI replica: %w", err)
	}
	if len(epoch) != len(version.Epoch) || version.HighWater > maxMAPICounter {
		return MAPIReplicaVersion{}, core.ErrInvalid
	}
	copy(version.Epoch[:], epoch)
	return version, nil
}

func loadMAPIObjectIDs(ctx context.Context, tx *sql.Tx, mailboxID string, highWater uint64) ([]MAPIObjectID, error) {
	rows, err := tx.QueryContext(ctx, `
		SELECT namespace, object_key, counter FROM mapi_object_ids
		WHERE mailbox_id = ? ORDER BY counter LIMIT ?
	`, mailboxID, mapireplica.MaxBindings+1)
	if err != nil {
		return nil, fmt.Errorf("read MAPI object IDs: %w", err)
	}
	defer rows.Close()
	var mappings []MAPIObjectID
	for rows.Next() {
		var binding MAPIObjectID
		if err := rows.Scan(&binding.Namespace, &binding.Key, &binding.Counter); err != nil {
			return nil, fmt.Errorf("decode MAPI object ID: %w", err)
		}
		if binding.Counter > highWater {
			return nil, core.ErrInvalid
		}
		mappings = append(mappings, binding)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("iterate MAPI object IDs: %w", err)
	}
	if len(mappings) > mapireplica.MaxBindings {
		return nil, core.ErrUnsupported
	}
	return mappings, nil
}

// AdvanceMAPIReplica commits new mappings and the allocation position together.
// Gaps are permitted for counters without a binding, such as change numbers.
// Existing assignments are immutable within the epoch.
func (s *Store) AdvanceMAPIReplica(ctx context.Context, mailboxID string, expected MAPIReplicaVersion, highWater uint64, mappings []MAPIObjectID) error {
	return s.CommitMAPIProjection(ctx, mailboxID, expected, highWater, mappings, nil)
}

// CommitMAPIProjection publishes observed versions, newly bound IDs and the
// replica position together. It also advances the replica when only counters
// have been allocated.
func (s *Store) CommitMAPIProjection(ctx context.Context, mailboxID string, expected MAPIReplicaVersion, highWater uint64, mappings []MAPIObjectID, projection []MAPIProjectionVersion) error {
	return s.CommitMAPIReplica(ctx, mailboxID, expected, MAPIReplicaMutation{
		HighWater: highWater, Bindings: mappings, Projection: projection,
	})
}

type MAPIReplicaMutation = mapireplica.Mutation

// CommitMAPIReplica commits imported IDs, projections and private data before
// any of them can be returned to the client.
func (s *Store) CommitMAPIReplica(ctx context.Context, mailboxID string, expected MAPIReplicaVersion, mutation MAPIReplicaMutation) (err error) {
	start := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "sqlite", Operation: "CommitMAPIReplica", Result: core.ActivityOutcome(err), Duration: time.Since(start)})
	}()
	bindingCount := len(mutation.Bindings) + len(mutation.ReservedBindings) + len(mutation.StagedBindings)
	if expected.Epoch == ([16]byte{}) || mutation.HighWater <= expected.HighWater || mutation.HighWater > maxMAPICounter ||
		bindingCount > maxMAPIObjectID || len(mutation.Drafts)+len(mutation.ForgetDrafts) > maxMAPIObjectID ||
		len(mutation.FolderValues) > maxMAPIObjectID {
		return core.ErrInvalid
	}
	if mutation.Stage != 0 && (!validStagedNamespace(mutation.Stage) || mutation.HighWater != expected.HighWater+1 ||
		bindingCount != 0 || len(mutation.Projection) != 0 || mutation.FAI != nil ||
		len(mutation.Drafts)+len(mutation.ForgetDrafts)+len(mutation.FolderValues) != 0 ||
		mutation.FolderCreation != nil || mutation.ForgetFolderCreation != 0 || mutation.ForeignReplica != ([16]byte{}) ||
		mutation.Spooler != nil || mutation.ForgetSpooler != 0 || mutation.ResolveSpooler != 0 ||
		mutation.MessageMove != nil || mutation.CancelMessageMove != 0 || mutation.MessageImport != nil || mutation.ImportAction != 0) {
		return core.ErrInvalid
	}
	for _, binding := range mutation.Bindings {
		if !validMAPIObjectID(binding) || binding.Counter <= expected.HighWater || binding.Counter > mutation.HighWater {
			return core.ErrInvalid
		}
	}
	for _, binding := range mutation.ReservedBindings {
		if !validMAPIReservedBinding(binding) || binding.Counter > expected.HighWater {
			return core.ErrInvalid
		}
	}
	for _, binding := range mutation.StagedBindings {
		if !validMAPIObjectID(binding) || !validStagedNamespace(binding.Namespace) || binding.Counter > expected.HighWater {
			return core.ErrInvalid
		}
	}
	if mutation.FAI != nil && !boundedMAPIFAIChanges(*mutation.FAI) {
		return core.ErrUnsupported
	}
	if err := validateMAPIProjectionBatch(mutation.Projection, expected.HighWater, mutation.HighWater); err != nil {
		return err
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin MAPI allocation: %w", err)
	}
	defer tx.Rollback()
	if err := expectMAPIReplica(ctx, tx, mailboxID, expected); err != nil {
		return err
	}
	if err := applyMAPIStaging(ctx, tx, mailboxID, mutation); err != nil {
		return err
	}
	if err := checkMAPIFolderClaims(ctx, tx, mailboxID, mutation.ReservedBindings, mutation.FolderCreation); err != nil {
		return err
	}
	for _, binding := range mutation.ReservedBindings {
		if err := requireMAPIReservedID(ctx, tx, mailboxID, expected.HighWater, binding.Counter); err != nil {
			return err
		}
		if err := checkMAPIMessageMoveReservation(ctx, tx, mailboxID, binding.Counter); err != nil {
			return err
		}
		if err := checkMAPIMessageImportReservation(ctx, tx, mailboxID, binding.Counter); err != nil {
			return err
		}
	}
	for _, batch := range [][]MAPIObjectID{mutation.Bindings, mutation.ReservedBindings, mutation.StagedBindings} {
		if len(batch) > 0 {
			if err := insertMAPIObjectIDs(ctx, tx, mailboxID, batch); err != nil {
				return err
			}
		}
	}
	if mutation.MessageMove != nil {
		if err := writeMAPIMessageMove(ctx, tx, mailboxID, expected.HighWater, mutation); err != nil {
			return err
		}
	}
	if mutation.CancelMessageMove != 0 {
		if mutation.MessageMove != nil {
			return core.ErrInvalid
		}
		if _, err := tx.ExecContext(ctx, "DELETE FROM mapi_message_moves WHERE mailbox_id = ? AND source = ? AND result = 0",
			mailboxID, mutation.CancelMessageMove); err != nil {
			return fmt.Errorf("cancel MAPI message move: %w", err)
		}
	}
	if mutation.MessageImport != nil || mutation.ImportAction != 0 {
		if err := writeMAPIMessageImport(ctx, tx, mailboxID, expected.HighWater, mutation); err != nil {
			return err
		}
	}
	if err := writeMAPIProjection(ctx, tx, mailboxID, mutation.Projection); err != nil {
		return err
	}
	if mutation.FAI != nil {
		if err := writeMAPIFAI(ctx, tx, mailboxID, mutation.Projection, *mutation.FAI); err != nil {
			return err
		}
	}
	if err := writeMAPIDraftPlacements(ctx, tx, mailboxID, mutation); err != nil {
		return err
	}
	if err := writeMAPISpooler(ctx, tx, mailboxID, mutation); err != nil {
		return err
	}
	if err := writeMAPIFolderValues(ctx, tx, mailboxID, mutation.FolderValues); err != nil {
		return err
	}
	if err := writeMAPIFolderCreation(ctx, tx, mailboxID, expected.HighWater, mutation); err != nil {
		return err
	}
	if mutation.ForeignReplica != ([16]byte{}) {
		if mutation.ForeignReplica == expected.Epoch {
			return core.ErrInvalid
		}
		if err := writeMAPIForeignReplica(ctx, tx, mailboxID, mutation.ForeignReplica); err != nil {
			return err
		}
	}
	if err := updateMAPICounter(ctx, tx, mailboxID, mutation.HighWater); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("commit MAPI allocation: %w", err)
	}
	return nil
}

func expectMAPIReplica(ctx context.Context, tx *sql.Tx, mailboxID string, expected MAPIReplicaVersion) error {
	current, err := readMAPIReplica(ctx, tx, mailboxID)
	if errors.Is(err, core.ErrNotFound) || err == nil && current.Epoch != expected.Epoch {
		return core.ErrResync
	}
	if err != nil {
		return err
	}
	if current.HighWater != expected.HighWater {
		return core.ErrConflict
	}
	return nil
}

func updateMAPICounter(ctx context.Context, tx *sql.Tx, mailboxID string, highWater uint64) error {
	if _, err := tx.ExecContext(ctx, "UPDATE mapi_replicas SET high_water = ? WHERE mailbox_id = ?", highWater, mailboxID); err != nil {
		return fmt.Errorf("advance MAPI replica: %w", err)
	}
	return nil
}

func insertMAPIObjectIDs(ctx context.Context, tx *sql.Tx, mailboxID string, mappings []MAPIObjectID) error {
	var count int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM mapi_object_ids WHERE mailbox_id = ?", mailboxID).Scan(&count); err != nil {
		return fmt.Errorf("count MAPI object IDs: %w", err)
	}
	if len(mappings) > mapireplica.MaxBindings-count {
		return core.ErrUnsupported
	}
	for _, binding := range mappings {
		result, err := tx.ExecContext(ctx, `
			INSERT INTO mapi_object_ids (mailbox_id, namespace, object_key, counter) VALUES (?, ?, ?, ?)
			ON CONFLICT DO NOTHING
		`, mailboxID, binding.Namespace, binding.Key, binding.Counter)
		if err != nil {
			return fmt.Errorf("write MAPI object ID: %w", err)
		}
		n, err := result.RowsAffected()
		if err != nil {
			return fmt.Errorf("check MAPI object ID binding: %w", err)
		}
		if n != 1 {
			return core.ErrConflict
		}
	}
	return nil
}
