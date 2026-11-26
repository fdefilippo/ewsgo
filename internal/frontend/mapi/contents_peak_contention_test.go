// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"ewsgo/internal/core"
)

// Signal actual admission waiting without relying on a scheduler delay.
type contentsWaitContext struct {
	context.Context
	waiting chan struct{}
}

func (c contentsWaitContext) Done() <-chan struct{} {
	select {
	case c.waiting <- struct{}{}:
	default:
	}
	return c.Context.Done()
}

func TestContentsInboxSurvivesConcurrentPermissionsCapture(t *testing.T) {
	f, source := newContentsDownloadFixture(t)
	configureContentsTest(t, f, fullContentsConfig())
	object, _ := f.state.mailbox.object(f.handles[2], 4)
	store := object.binding.store
	entered, proceed := make(chan struct{}), make(chan struct{})
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	access := &permissionsMailbox{Mailbox: f.box, state: f.box, access: core.FolderAccess{Scope: f.session.Identity().Scope, Source: f.box.Root(), CanViewList: true, Completeness: core.AccessComplete, Provenance: core.AccessInherited}}
	access.hook = func(call int) {
		if call == 1 {
			close(entered)
			select {
			case <-proceed:
			case <-ctx.Done():
			}
		}
	}
	type permissionResult struct {
		table *contentsTable
		err   error
	}
	permissions := make(chan permissionResult, 1)
	go func() {
		table, err := f.transport.capturePermissionsTable(ctx, &testSession{box: access}, store, f.fids[Inbox], 0)
		permissions <- permissionResult{table, err}
	}()
	<-entered
	// The actual ACL reader holds its 8 MiB peak, not a fabricated blocker.
	waiting := make(chan struct{}, 1)
	result := make(chan []byte, 1)
	go func() {
		result <- getContentsBuffer(contentsWaitContext{ctx, waiting}, f.session, object, Operation{Input: 2, TransferLimit: 4096})
	}()
	select {
	case <-waiting:
		if source.sourceCalls != 0 || len(store.contentsProofs) != 0 {
			t.Error("waiting capture read/published prematurely")
		}
	case wire := <-result:
		t.Errorf("Inbox failed instead of waiting for the concurrent ACL peak: code %08x", binary.LittleEndian.Uint32(wire[2:]))
	case <-ctx.Done():
		t.Error("peak admission failed to progress")
	}
	close(proceed)
	p := <-permissions
	if p.table != nil {
		defer p.table.close()
	}
	if p.err != nil {
		t.Fatal("ACL capture failed", p.err)
	}
	if t.Failed() {
		return
	}
	var wire []byte
	select {
	case wire = <-result:
	case <-ctx.Done():
		t.Fatal("Inbox did not resume after ACL capture")
	}
	if binary.LittleEndian.Uint32(wire[2:]) != 0 || binary.LittleEndian.Uint16(wire[6:]) != 3 {
		t.Fatal("resumed Inbox failed")
	}
	changes := 0
	for _, token := range decodeTestContents(t, wire[15:]) {
		if token.tag == 0x65e00102 {
			changes++
		}
	}
	if changes != 1 {
		t.Fatal("resumed Inbox lost native fixture message")
	}
	if err := store.validateContentsCheckpoint(t.Context(), object.download.contents.state.scope, object.download.contents.final); err != nil {
		t.Fatal("resumed Inbox checkpoint missing", err)
	}
}

func TestContentsPeakAdmissionCancellationAndRetainedQuotas(t *testing.T) {
	for _, global := range []bool{false, true} {
		t.Run(map[bool]string{false: "store", true: "global"}[global], func(t *testing.T) {
			stores := newRAMStores()
			a, b := &ramStore{allocator: stores}, &ramStore{allocator: stores}
			retained, err := a.reserveContentsMemory(1 << 20)
			if err != nil {
				t.Fatal(err)
			}
			defer retained.release()
			if global {
				other, err := b.reserveContentsMemory(maxStoreContentsMemory)
				if err != nil {
					t.Fatal(err)
				}
				defer other.release()
				// Both quotas now have the same remaining room for a's workspaces.
			}
			peak, err := a.reserveContentsPeak(context.Background(), 8<<20)
			if err != nil {
				t.Fatal(err)
			}
			defer peak.release()
			ctx, cancel := context.WithCancel(context.Background())
			waiting := make(chan struct{}, 1)
			result := make(chan error, 1)
			go func() {
				l, e := a.reserveContentsPeak(contentsWaitContext{ctx, waiting}, 8<<20)
				l.release()
				result <- e
			}()
			<-waiting
			cancel()
			if err := <-result; !errors.Is(err, context.Canceled) {
				t.Fatal("cancellation lost", err)
			}
			if stores.contents.used != peak.size()+(1<<20)+map[bool]int{false: 0, true: maxStoreContentsMemory}[global] {
				t.Fatal("canceled waiter charged memory")
			}
			// Converting a peak to retained bytes must wake waiters even if the size
			// stays the same: this is now a real quota failure, not transient pressure.
			ctx, cancel = context.WithCancel(context.Background())
			defer cancel()
			waiting = make(chan struct{}, 1)
			go func() {
				l, e := a.reserveContentsPeak(contentsWaitContext{ctx, waiting}, 8<<20)
				l.release()
				result <- e
			}()
			<-waiting
			if err := peak.shrink(peak.size() + 1); err == nil {
				t.Fatal("invalid shrink accepted")
			}
			if err := peak.shrink(peak.size()); err != nil {
				t.Fatal(err)
			}
			if err := <-result; !errors.Is(err, contentsBudgetError{}) {
				t.Fatal("retained quota should fail promptly", err)
			}
			if stores.contents.peaks != 0 || len(stores.contents.storePeaks) != 0 {
				t.Fatal("ended peak retained transient accounting")
			}
		})
	}
}

func TestContentsPeakAdmissionWaitsForOtherStoreGlobalWorkspace(t *testing.T) {
	stores := newRAMStores()
	a, b, c := &ramStore{allocator: stores}, &ramStore{allocator: stores}, &ramStore{allocator: stores}
	retained, err := a.reserveContentsMemory(maxStoreContentsMemory)
	if err != nil {
		t.Fatal(err)
	}
	defer retained.release()
	retainedB, err := b.reserveContentsMemory(1 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer retainedB.release()
	peak, err := c.reserveContentsPeak(context.Background(), maxStoreContentsMemory)
	// 16+1+16 cannot fit globally; reserve a smaller actual workspace.
	if err == nil {
		peak.release()
		t.Fatal("global quota bypassed")
	}
	peak, err = c.reserveContentsPeak(context.Background(), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer peak.release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waiting := make(chan struct{}, 1)
	type reservation struct {
		lease *contentsLease
		err   error
	}
	result := make(chan reservation, 1)
	go func() {
		l, e := b.reserveContentsPeak(contentsWaitContext{ctx, waiting}, 8<<20)
		result <- reservation{l, e}
	}()
	<-waiting
	if err := peak.shrink(1024); err != nil {
		t.Fatal(err)
	}
	r := <-result
	if r.err != nil {
		t.Fatal("global workspace did not resume", r.err)
	}
	r.lease.release()
	if stores.contents.used > maxGlobalContentsMemory {
		t.Fatal("global cap exceeded")
	}
}

func TestContentsImmutableStateRechecksAccessAfterPeakWait(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	configureContentsTest(t, f, fullContentsConfig())
	object, _ := f.state.mailbox.object(f.handles[2], 4)
	store := object.binding.store
	retained, err := store.reserveContentsMemory(4 << 20)
	if err != nil {
		t.Fatal(err)
	}
	defer retained.release()
	peak, err := store.reserveContentsPeak(context.Background(), 8<<20)
	if err != nil {
		t.Fatal(err)
	}
	defer peak.release()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	waiting := make(chan struct{}, 1)
	result := make(chan []byte, 1)
	baseline := store.allocator.contents.used
	before := f.handles[3]
	go func() {
		result <- getContentsTransferState(contentsWaitContext{ctx, waiting}, f.session, f.state.mailbox, object, Operation{Output: 3}, f.handles, &transferStateFailure{})
	}()
	select {
	case <-waiting:
	case <-ctx.Done():
		t.Fatal("immutable state did not reach waiting admission")
	}
	f.box.state.Owner = false
	peak.release()
	wire := <-result
	if binary.LittleEndian.Uint32(wire[2:]) != accessDenied || f.handles[3] != before {
		t.Fatal("revoked state source published after waiting")
	}
	if store.allocator.contents.used != baseline-(8<<20) {
		t.Fatal("revoked state source leaked memory")
	}
}
