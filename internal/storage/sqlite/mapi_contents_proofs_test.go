// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"path/filepath"
	"testing"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

func TestMAPIContentsProofRetentionAndReopen(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "contents.sqlite")
	store, mailbox := openJournalFixture(t, path)
	snapshot, err := store.LoadMAPIReplica(ctx, mailbox)
	if err != nil {
		t.Fatal(err)
	}
	epoch := snapshot.Version.Epoch
	digest := func(i int) [32]byte {
		return sha256.Sum256(binary.LittleEndian.AppendUint64(nil, uint64(i)))
	}
	if err := store.ValidateMAPIContentsProof(ctx, mailbox, epoch, digest(0)); !errors.Is(err, core.ErrResync) {
		t.Fatalf("unpublished proof accepted: %v", err)
	}
	for i := range maxMAPIContentsProofs {
		if err := store.PublishMAPIContentsProof(ctx, mailbox, epoch, digest(i)); err != nil {
			t.Fatal(err)
		}
	}
	if err := store.PublishMAPIContentsProof(ctx, mailbox, epoch, digest(0)); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, _ := openJournalFixture(t, path)
	if err := reopened.PublishMAPIContentsProof(ctx, mailbox, epoch, digest(maxMAPIContentsProofs)); err != nil {
		t.Fatal(err)
	}
	for i := range maxMAPIContentsProofs + 1 {
		err := reopened.ValidateMAPIContentsProof(ctx, mailbox, epoch, digest(i))
		if i == 1 {
			if !errors.Is(err, core.ErrResync) {
				t.Fatalf("oldest proof retained: %v", err)
			}
		} else if err != nil {
			t.Fatalf("valid proof %d: %v", i, err)
		}
	}
	second, err := reopened.BindMailbox(ctx, core.Scope{Backend: "fixture", Account: "other"}, "one")
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = reopened.LoadMAPIReplica(ctx, second.MailboxID)
	if err != nil {
		t.Fatal(err)
	}
	otherEpoch := snapshot.Version.Epoch
	if err := reopened.ValidateMAPIContentsProof(ctx, second.MailboxID, otherEpoch, digest(0)); !errors.Is(err, core.ErrResync) {
		t.Fatalf("proof leaked across mailboxes: %v", err)
	}
	if err := reopened.PublishMAPIContentsProof(ctx, second.MailboxID, epoch, digest(0)); !errors.Is(err, core.ErrResync) {
		t.Fatalf("foreign epoch accepted for publication: %v", err)
	}
}

func TestMAPIUploadProofMigratesVersionTwenty(t *testing.T) {
	ctx := t.Context()
	path := filepath.Join(t.TempDir(), "upload-v20.sqlite")
	store, mailbox := openJournalFixture(t, path)
	replica, err := store.LoadMAPIReplica(ctx, mailbox)
	if err != nil {
		t.Fatal(err)
	}
	binding := MAPIObjectID{Namespace: MAPIFolder, Key: []byte("root"), Counter: 1}
	if err = store.AdvanceMAPIReplica(ctx, mailbox, replica.Version, 1, []MAPIObjectID{binding}); err != nil {
		t.Fatal(err)
	}
	legacy := mapireplica.UploadProof{Root: 1, After: sha256.Sum256([]byte("seen-fai"))}
	if err = store.PublishMAPIUploadProof(ctx, mailbox, replica.Version.Epoch, legacy); err != nil {
		t.Fatal(err)
	}
	if _, err = store.db.ExecContext(ctx, `
		DROP TABLE mapi_message_moves;
		DROP TABLE mapi_message_imports;
		ALTER TABLE mapi_upload_proofs DROP COLUMN normal_before;
		ALTER TABLE mapi_upload_proofs DROP COLUMN normal_after;
		DROP TABLE mapi_spooler;
		DROP TABLE mapi_public_read_state;
		PRAGMA user_version = 20;
	`); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, _ = openJournalFixture(t, path)
	if err = store.PublishMAPIUploadProof(ctx, mailbox, replica.Version.Epoch, legacy); err != nil {
		t.Fatal(err)
	}
	proofs, err := store.LoadMAPIUploadProofs(ctx, mailbox, replica.Version.Epoch, legacy.Root)
	if err != nil || len(proofs) != 1 || proofs[0].After != legacy.After ||
		proofs[0].NormalAfter != ([32]byte{}) || len(proofs[0].NormalBefore) != 0 {
		t.Fatal("migrated proof was lost, changed or duplicated", err)
	}
}
