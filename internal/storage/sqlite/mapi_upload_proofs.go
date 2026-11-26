// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"fmt"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const maxMAPIUploadProofs = 64
const maxMAPIUploadCreated = 128

func (s *Store) LoadMAPIUploadProofs(ctx context.Context, mailbox string, epoch [16]byte, root uint64) ([]mapireplica.UploadProof, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT seen_before, seen_after, read_before, read_after, normal_before, normal_after, created
		FROM mapi_upload_proofs JOIN mapi_replicas USING (mailbox_id)
		WHERE mailbox_id = ? AND epoch = ? AND root = ?
		ORDER BY sequence DESC LIMIT ?
	`, mailbox, epoch[:], root, maxMAPIUploadProofs)
	if err != nil {
		return nil, fmt.Errorf("read MAPI collector proofs: %w", err)
	}
	defer rows.Close()
	var result []mapireplica.UploadProof
	for rows.Next() {
		p := mapireplica.UploadProof{Root: root}
		var seen, read, normal, ids []byte
		if err := rows.Scan(&p.Before, &seen, &p.ReadBefore, &read, &p.NormalBefore, &normal, &ids); err != nil {
			return nil, fmt.Errorf("decode MAPI collector proof: %w", err)
		}
		if len(seen) != 32 || len(read) != 32 || len(normal) != 32 || len(ids)%6 != 0 || len(ids)/6 > maxMAPIUploadCreated {
			return nil, core.ErrInvalid
		}
		copy(p.After[:], seen)
		copy(p.ReadAfter[:], read)
		copy(p.NormalAfter[:], normal)
		if p.NormalAfter == ([32]byte{}) && len(p.NormalBefore) != 0 {
			return nil, core.ErrInvalid
		}
		for len(ids) != 0 {
			p.Created = append(p.Created, binary.BigEndian.Uint64(append([]byte{0, 0}, ids[:6]...)))
			ids = ids[6:]
		}
		result = append(result, p)
	}
	return result, rows.Err()
}

func (s *Store) PublishMAPIUploadProof(ctx context.Context, mailbox string, epoch [16]byte, proof mapireplica.UploadProof) error {
	if proof.Root == 0 || proof.After == ([32]byte{}) || len(proof.Before) > 65536 || len(proof.ReadBefore) > 65536 ||
		len(proof.NormalBefore) > 65536 || proof.NormalAfter == ([32]byte{}) && len(proof.NormalBefore) != 0 ||
		len(proof.Created) > maxMAPIUploadCreated {
		return core.ErrInvalid
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("begin MAPI collector proof: %w", err)
	}
	defer tx.Rollback()
	version, err := readMAPIReplica(ctx, tx, mailbox)
	if errors.Is(err, core.ErrNotFound) || err == nil && version.Epoch != epoch {
		return core.ErrResync
	}
	if err != nil {
		return err
	}
	created := make([]byte, 0, len(proof.Created)*6)
	for _, id := range proof.Created {
		if id == 0 || id > version.HighWater {
			return core.ErrInvalid
		}
		counter := binary.BigEndian.AppendUint64(nil, id)
		created = append(created, counter[2:]...)
	}
	hash := sha256.New()
	hash.Write(binary.BigEndian.AppendUint64(nil, proof.Root))
	parts := [][]byte{proof.Before, proof.After[:], proof.ReadBefore, proof.ReadAfter[:], created}
	if proof.NormalAfter != ([32]byte{}) {
		parts = append(parts, proof.NormalBefore, proof.NormalAfter[:])
	}
	for _, part := range parts {
		hash.Write(binary.BigEndian.AppendUint32(nil, uint32(len(part))))
		hash.Write(part)
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT OR REPLACE INTO mapi_upload_proofs (
			mailbox_id, root, digest, seen_before, seen_after, read_before, read_after, created, normal_before, normal_after
		)
		VALUES (?, ?, ?, COALESCE(?, x''), ?, COALESCE(?, x''), ?, COALESCE(?, x''), COALESCE(?, x''), ?)
	`, mailbox, proof.Root, hash.Sum(nil), proof.Before, proof.After[:], proof.ReadBefore, proof.ReadAfter[:], created, proof.NormalBefore, proof.NormalAfter[:]); err != nil {
		return fmt.Errorf("write MAPI collector proof: %w", err)
	}
	if _, err := tx.ExecContext(ctx, `
		DELETE FROM mapi_upload_proofs WHERE mailbox_id = ? AND sequence <= (
			SELECT sequence FROM mapi_upload_proofs WHERE mailbox_id = ? ORDER BY sequence DESC LIMIT 1 OFFSET ?
		)
	`, mailbox, mailbox, maxMAPIUploadProofs); err != nil {
		return fmt.Errorf("compact MAPI collector proofs: %w", err)
	}
	return tx.Commit()
}
