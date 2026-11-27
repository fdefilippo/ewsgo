// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"context"
	"log/slog"
	"time"

	"ewsgo/internal/storage/sqlite"
)

func startJournalMaintenance(ctx context.Context, store *sqlite.Store, logger *slog.Logger) func() {
	ctx, cancel := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() {
		defer close(done)
		ticker := time.NewTicker(time.Minute)
		defer ticker.Stop()
		for ctx.Err() == nil {
			pass, finish := context.WithTimeout(ctx, 30*time.Second)
			err := store.MaintainJournal(pass)
			finish()
			if err != nil && ctx.Err() == nil {
				logger.Warn("SQLite journal maintenance failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()
	return func() {
		cancel()
		<-done
	}
}
