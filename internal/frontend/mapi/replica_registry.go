// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const maxForeignReplicas = 256
const foreignReplicaRegistryBytes = 512 + maxForeignReplicas*16
const parameterOverflow uint32 = 0x00000450

// Replica translation is not object lookup. Foreign namespaces can be converted
// without creating a Folder/Message mapping or granting any native access.
// The local replica is always 1. Foreign REPLIDs are never evicted or reused.
func (s *ramStore) replicaID(ctx context.Context, guid [16]byte) (uint16, uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return 0, storeError(err)
	}
	if guid == ([16]byte{}) {
		return 0, 0x80070057
	}
	if guid == s.replica {
		return 1, 0
	}
	for i, known := range s.foreignReplicas {
		if guid == known {
			return uint16(i + 2), 0
		}
	}
	if len(s.foreignReplicas) == maxForeignReplicas {
		return 0, parameterOverflow
	}
	lease := s.foreignReplicaLease
	if lease == nil {
		var err error
		lease, err = s.reserveContentsMemory(foreignReplicaRegistryBytes)
		if err != nil {
			return 0, 0x8007000e
		}
	}
	if err := ctx.Err(); err != nil {
		if s.foreignReplicaLease == nil {
			lease.release()
		}
		return 0, storeError(err)
	}
	if err := s.advanceReplicaLocked(ctx, mapireplica.Mutation{ForeignReplica: guid}); err != nil {
		if s.foreignReplicaLease == nil {
			lease.release()
		}
		return 0, storeError(err)
	}
	if s.foreignReplicaLease == nil {
		s.foreignReplicaLease = lease
		s.foreignReplicas = make([][16]byte, 0, maxForeignReplicas)
	}
	s.foreignReplicas = append(s.foreignReplicas, guid)
	return uint16(len(s.foreignReplicas) + 1), 0
}

func (s *ramStore) replicaGUID(ctx context.Context, id uint16) ([16]byte, uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return [16]byte{}, storeError(err)
	}
	if id == 1 {
		return s.replica, 0
	}
	if id >= 2 && int(id)-2 < len(s.foreignReplicas) {
		return s.foreignReplicas[int(id)-2], 0
	}
	return [16]byte{}, PropertyNotFound
}

func (s *ramStore) loadForeignReplicas(epoch [16]byte, foreign [][16]byte) error {
	if len(foreign) == 0 {
		return nil
	}
	if len(foreign) > maxForeignReplicas {
		return core.ErrLimit
	}
	seen := make(map[[16]byte]bool, len(foreign)+1)
	seen[epoch] = true
	for _, guid := range foreign {
		if guid == ([16]byte{}) || seen[guid] {
			return core.ErrInvalid
		}
		seen[guid] = true
	}
	lease, err := s.reserveContentsMemory(foreignReplicaRegistryBytes)
	if err != nil {
		return err
	}
	s.foreignReplicaLease = lease
	s.foreignReplicas = append(make([][16]byte, 0, maxForeignReplicas), foreign...)
	return nil
}
