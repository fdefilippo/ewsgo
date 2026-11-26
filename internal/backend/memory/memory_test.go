// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package memory_test

import (
	"context"
	"errors"
	"sync"
	"testing"

	"ewsgo/internal/backend/memory"
	"ewsgo/internal/core"
)

var ctx = context.Background()
var subject = core.Key{ID: 0x0037}
var named = core.Key{Set: "00020329-0000-0000-c000-000000000046", Name: "ewsgo-test"}

func box(t *testing.T, p core.Provider, account string) core.Mailbox {
	t.Helper()
	b, err := p.Open(ctx, core.Scope{Backend: "memory-test", Account: account})
	if err != nil {
		t.Fatal(err)
	}
	return b
}
func create(t *testing.T, b core.Mailbox, kind core.Kind, parent core.ID) core.Object {
	t.Helper()
	e, err := b.Begin(ctx, core.Target{Kind: kind, Parent: parent})
	if err != nil {
		t.Fatal(err)
	}
	if err = e.Apply(core.Patch{Set: core.Properties{subject: core.Text("initial"), named: core.Binary{1, 2}}}); err != nil {
		t.Fatal(err)
	}
	o, err := e.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return o
}
func edit(t *testing.T, b core.Mailbox, o core.Object) core.Edit {
	t.Helper()
	e, err := b.Begin(ctx, core.Target{ID: o.ID, Expected: o.Revision})
	if err != nil {
		t.Fatal(err)
	}
	return e
}
func TestScopeIsolationAndStableIdentity(t *testing.T) {
	p := memory.New()
	a := box(t, p, "a")
	b := box(t, p, "b")
	o := create(t, a, core.Message, a.Root())
	if _, err := b.Get(ctx, o.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("cross-mailbox read: %v", err)
	}
	other, err := p.Open(ctx, core.Scope{Backend: "different", Account: "a"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = other.Get(ctx, o.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("cross-backend read: %v", err)
	}
	got, err := box(t, p, "a").Get(ctx, o.ID)
	if err != nil || got.ID != o.ID || got.Revision != o.Revision {
		t.Fatal("identity changed on reopen")
	}
}
func TestPartialUpdateAndDefensiveCopies(t *testing.T) {
	b := box(t, memory.New(), "a")
	o := create(t, b, core.Message, b.Root())
	o.Properties[named].(core.Binary)[0] = 99
	o, _ = b.Get(ctx, o.ID)
	if o.Properties[named].(core.Binary)[0] != 1 {
		t.Fatal("read leaked mutable state")
	}
	e := edit(t, b, o)
	buf := core.Binary{3, 4}
	mime := []byte("MIME")
	rcpts := []core.Recipient{{Address: "test@example.invalid"}}
	atts := []core.Attachment{{ID: "a", MediaType: "text/plain", Data: []byte("bytes")}}
	if err := e.Apply(core.Patch{Set: core.Properties{named: buf}, MIME: &mime, Recipients: &rcpts, Attachments: &atts}); err != nil {
		t.Fatal(err)
	}
	buf[0] = 9
	mime[0] = 'X'
	atts[0].Data[0] = 'X'
	before, _ := b.Get(ctx, o.ID)
	if len(before.MIME) != 0 {
		t.Fatal("uncommitted data visible")
	}
	updated, err := e.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if updated.ID != o.ID || updated.Revision <= o.Revision || updated.Properties[subject] != core.Text("initial") || updated.Properties[named].(core.Binary)[0] != 3 || string(updated.MIME) != "MIME" || string(updated.Attachments[0].Data) != "bytes" {
		t.Fatal("partial update or copy invariant failed")
	}
	if _, err = e.Commit(ctx); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("second commit: %v", err)
	}
	e = edit(t, b, updated)
	if err = e.Apply(core.Patch{Remove: []core.Key{named}}); err != nil {
		t.Fatal(err)
	}
	updated, err = e.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := updated.Properties[named]; ok {
		t.Fatal("property not removed")
	}
}
func TestConcurrentWritersConflictAtCommit(t *testing.T) {
	b := box(t, memory.New(), "a")
	o := create(t, b, core.Message, b.Root())
	a := edit(t, b, o)
	c := edit(t, b, o)
	var wg sync.WaitGroup
	results := make(chan error, 2)
	for _, e := range []core.Edit{a, c} {
		wg.Add(1)
		go func(e core.Edit) { defer wg.Done(); _, err := e.Commit(ctx); results <- err }(e)
	}
	wg.Wait()
	close(results)
	success, conflict := 0, 0
	for err := range results {
		switch {
		case err == nil:
			success++
		case errors.Is(err, core.ErrConflict):
			conflict++
		default:
			t.Fatal(err)
		}
	}
	if success != 1 || conflict != 1 {
		t.Fatalf("success=%d conflict=%d", success, conflict)
	}
}
func TestMoveDeleteAndCursorIsolation(t *testing.T) {
	p := memory.New()
	b := box(t, p, "a")
	baseline, err := b.Changes(ctx, "", 100)
	if err != nil {
		t.Fatal(err)
	}
	folder := create(t, b, core.Folder, b.Root())
	o := create(t, b, core.Message, b.Root())
	e := edit(t, b, o)
	if err = e.Apply(core.Patch{Parent: &folder.ID}); err != nil {
		t.Fatal(err)
	}
	o, err = e.Commit(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = b.Delete(ctx, o.ID, o.Revision); err != nil {
		t.Fatal(err)
	}
	changes, err := b.Changes(ctx, baseline.Next, 2)
	if err != nil || !changes.More || len(changes.Events) != 2 {
		t.Fatalf("first page: %v %+v", err, changes)
	}
	changes, err = b.Changes(ctx, changes.Next, 2)
	if err != nil || changes.More || len(changes.Events) != 2 || changes.Events[0].Kind != core.Moved || changes.Events[1].Kind != core.Deleted {
		t.Fatalf("move/delete: %v %+v", err, changes)
	}
	if _, err = box(t, p, "b").Changes(ctx, changes.Next, 100); !errors.Is(err, core.ErrResync) {
		t.Fatalf("foreign cursor: %v", err)
	}
	if _, err = box(t, memory.New(), "a").Changes(ctx, changes.Next, 100); !errors.Is(err, core.ErrResync) {
		t.Fatalf("restarted provider cursor: %v", err)
	}
}
func TestUnsupportedQueriesAndTypedPatches(t *testing.T) {
	b := box(t, memory.New(), "a")
	o := create(t, b, core.Message, b.Root())
	if b.Capabilities().Directory {
		t.Fatal("directory incorrectly advertised")
	}
	if _, err := b.Query(ctx, core.Query{Parent: b.Root(), OrderBy: &subject}); !errors.Is(err, core.ErrUnsupported) {
		t.Fatalf("unsupported sort: %v", err)
	}
	page, err := b.Query(ctx, core.Query{Parent: b.Root(), Where: []core.Predicate{{Key: subject, Equal: core.Text("initial")}}})
	if err != nil || len(page.Objects) != 1 {
		t.Fatalf("query: %v %+v", err, page)
	}
	e := edit(t, b, o)
	for _, patch := range []core.Patch{
		{Set: core.Properties{subject: core.Integer(7)}},
		{Set: core.Properties{{Name: "not named"}: core.Text("bad")}},
		{Set: core.Properties{named: nil}},
		{Set: core.Properties{named: core.Text("x")}, Remove: []core.Key{named}},
	} {
		if err = e.Apply(patch); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("invalid patch accepted: %v", err)
		}
	}
	e.Discard()
	if err = e.Apply(core.Patch{}); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("discarded edit: %v", err)
	}
}
func TestHierarchyValidationAndCanceledCommit(t *testing.T) {
	b := box(t, memory.New(), "a")
	folder := create(t, b, core.Folder, b.Root())
	child := create(t, b, core.Folder, folder.ID)
	e := edit(t, b, folder)
	if err := e.Apply(core.Patch{Parent: &child.ID}); err != nil {
		t.Fatal(err)
	}
	if _, err := e.Commit(ctx); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("folder cycle: %v", err)
	}
	if err := b.Delete(ctx, folder.ID, folder.Revision); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("nonempty folder: %v", err)
	}
	e, err := b.Begin(ctx, core.Target{Kind: core.Message, Parent: b.Root()})
	if err != nil {
		t.Fatal(err)
	}
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = e.Commit(canceled); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled commit: %v", err)
	}
	page, err := b.Query(ctx, core.Query{Parent: b.Root(), Kind: core.Message})
	if err != nil || len(page.Objects) != 0 {
		t.Fatal("canceled commit published an object")
	}
}
