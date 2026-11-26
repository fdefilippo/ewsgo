// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package main

import (
	"bytes"
	"path/filepath"
	"testing"

	"ewsgo/internal/storage/sqlite"
)

func TestStoreBackupRestoreCommands(t *testing.T) {
	ctx := t.Context()
	dir := t.TempDir()
	source := filepath.Join(dir, "live.sqlite")
	archive := filepath.Join(dir, "copy's archive.sqlite")
	restored := filepath.Join(dir, "restored.sqlite")
	store, err := sqlite.Open(ctx, source)
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()

	var stdout, stderr bytes.Buffer
	for _, args := range [][]string{
		{"backup", "-from", source, "-to", archive},
		{"restore", "-from", archive, "-to", restored},
	} {
		if code := runStoreCommand(ctx, args, &stdout, &stderr); code != 0 {
			t.Fatalf("%s exited %d: %s", args[0], code, stderr.String())
		}
	}
	if code := runStoreCommand(ctx, []string{"restore", "-from", archive, "-to", restored}, &stdout, &stderr); code != 1 {
		t.Fatalf("restore should refuse to replace a file: exit %d", code)
	}
}
