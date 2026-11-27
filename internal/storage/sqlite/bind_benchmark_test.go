// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package sqlite

import (
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func BenchmarkBindMailboxUnderWriter(b *testing.B) {
	ctx := b.Context()
	store, err := Open(ctx, filepath.Join(b.TempDir(), "bind.sqlite"))
	if err != nil {
		b.Fatal(err)
	}
	defer store.Close()
	store.db.SetMaxOpenConns(8)
	scope := core.Scope{Backend: "fixture", Account: "existing"}
	want, err := store.BindMailbox(ctx, scope, "primary")
	if err != nil {
		b.Fatal(err)
	}
	start, stop, writer := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		first := true
		for {
			tx, err := store.db.BeginTx(ctx, nil)
			if err != nil {
				writer <- err
				return
			}
			if first {
				close(start)
				first = false
			}
			time.Sleep(5 * time.Millisecond)
			if err := tx.Rollback(); err != nil {
				writer <- err
				return
			}
			select {
			case <-stop:
				writer <- nil
				return
			case <-time.After(time.Millisecond):
			}
		}
	}()
	select {
	case <-start:
	case err := <-writer:
		b.Fatal(err)
	}
	before := store.db.Stats()
	samples := make([]time.Duration, b.N)
	var next atomic.Int64
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		for pb.Next() {
			started := time.Now()
			got, err := store.BindMailbox(ctx, scope, "primary")
			elapsed := time.Since(started)
			if err != nil || got != want {
				b.Errorf("binding changed: %v, %v", got, err)
			}
			samples[next.Add(1)-1] = elapsed
		}
	})
	b.StopTimer()
	after := store.db.Stats()
	close(stop)
	if err := <-writer; err != nil {
		b.Fatal(err)
	}
	slices.Sort(samples)
	b.ReportMetric(float64(samples[(len(samples)-1)*95/100].Microseconds()), "p95-us")
	b.ReportMetric(float64(after.WaitCount-before.WaitCount)/float64(b.N), "pool-waits/op")
}
