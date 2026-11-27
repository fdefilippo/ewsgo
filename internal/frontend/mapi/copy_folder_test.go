// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"strconv"
	"testing"
	"testing/synctest"
	"time"

	"ewsgo/internal/core"
)

type copyFolderMailbox struct {
	*placementTestMailbox
	core.MailMessagePlacementWriter
	messages    map[core.ID]core.Object
	subtree     []core.Object
	observeHook func()
	createError error
	creates     int
	copies      int
}

func (b *copyFolderMailbox) Get(ctx context.Context, id core.ID) (core.Object, error) {
	if message, ok := b.messages[id]; ok {
		return message, nil
	}
	return b.stateMailbox.Get(ctx, id)
}

func (b *copyFolderMailbox) ReadMailFolderCopyTree(ctx context.Context, source, _ core.ID, recursive bool) ([]core.Object, error) {
	folder, err := b.Get(ctx, source)
	if recursive {
		return append([]core.Object{folder}, b.subtree...), err
	}
	return []core.Object{folder}, err
}

func (b *copyFolderMailbox) ObserveMailFolder(context.Context, core.ID, *uint64) (core.MailFolderObservation, error) {
	if b.observeHook != nil {
		b.observeHook()
	}
	if len(b.messages) == 0 {
		return core.MailFolderObservation{}, nil
	}
	return core.MailFolderObservation{MessageCount: 2, MessageIDs: []core.ID{"first", "second"}}, nil
}

func (b *copyFolderMailbox) CreateMailFolder(_ context.Context, placement core.MailFolderPlacement) (core.Object, error) {
	b.creates++
	if b.createError != nil {
		return core.Object{}, b.createError
	}
	folder := core.Object{ID: "created", Kind: core.Folder, Parent: placement.Parent,
		Properties: core.Properties{core.DisplayName: core.Text(placement.Name)}}
	if b.creates > 1 {
		folder.ID = core.ID("created-" + strconv.Itoa(b.creates))
	}
	b.page.Objects = append(b.page.Objects, folder)
	return folder, nil
}

func TestCopyFolderProgress(t *testing.T) {
	for _, action := range []string{"complete", "cancel", "release", "move destination"} {
		t.Run(action, func(t *testing.T) {
			f := newCreateFixture(t)
			folder := f.state.mailbox.objects[f.handles[1]]
			box := &copyFolderMailbox{placementTestMailbox: &placementTestMailbox{stateMailbox: f.box}}
			for _, entry := range [][2]core.ID{{"child", folder.binding.store.inbox}, {"leaf", "child"}} {
				object := core.Object{ID: entry[0], Kind: core.Folder, Parent: entry[1],
					Properties: core.Properties{core.DisplayName: core.Text(entry[0])}}
				box.subtree = append(box.subtree, object)
				box.page.Objects = append(box.page.Objects, object)
			}
			f.session = &testSession{box}
			faiRunOK(t, f, folderOperation(f.fids[SentItems], 4, 0, 2))
			wire := binary.LittleEndian.AppendUint64([]byte{ropCopyFolder, 4, 1, 2, 1, 1, 0}, f.fids[Inbox])
			wire = append(wire, "Replica\x00"...)
			poll := []byte{ropProgress, 4, 1, 0}
			progress := func(done byte) []byte {
				return []byte{0x50, 1, 0, 0, 0, 0, 4, done, 0, 0, 0, 3, 0, 0, 0}
			}
			require := func(request, want []byte) {
				t.Helper()
				if got, _ := f.run(request); !bytes.Equal(got, want) {
					t.Fatalf("got %x, want %x", got, want)
				}
			}
			baseline := folder.binding.store.allocator.contents.used
			require(wire, progress(1))
			if box.creates != 1 || folder.binding.store.allocator.contents.used <= baseline {
				t.Fatal("first response did not retain only the confirmed root and remaining plan")
			}
			require(wire, append(ropError(ropCopyFolder, 1, 0x8004010b), 0))
			require([]byte{ropProgress, 4, 2, 0}, ropError(ropProgress, 2, NotSupported))
			require([]byte{ropProgress, 5, 1, 0}, ropError(ropProgress, 1, accessDenied))
			body, _ := f.execute(append(bytes.Clone(poll), 0x50), 65535)
			if !bytes.Equal(body, executeError(0x80070057, 65535)) || box.creates != 1 {
				t.Fatal("malformed batch advanced a copy")
			}
			switch action {
			case "complete":
				require(poll, progress(2))
				require(poll, append(ropError(ropCopyFolder, 1, 0), 0))
				leaf, err := box.Get(t.Context(), "created-3")
				if err != nil || leaf.Parent != "created-2" || box.creates != 3 {
					t.Fatal("nested placement was not preserved")
				}
			case "cancel":
				require([]byte{ropProgress, 4, 1, 1}, append(ropError(ropCopyFolder, 1, 0x80040113), 1))
			case "release":
				require([]byte{1, 4, 1}, nil)
			case "move destination":
				box.page.Objects[len(box.page.Objects)-1].Parent = folder.binding.store.root
				require(poll, append(ropError(ropCopyFolder, 1, 0x80004005), 1))
			}
			if action != "complete" && box.creates != 1 {
				t.Fatal("copy advanced after cancellation or after unrelated polls")
			}
			if folder.binding.store.allocator.contents.used != baseline {
				t.Fatal("completed or canceled job leaked its plan lease")
			}
			if action == "release" {
				require(poll, ropError(ropProgress, 1, invalidObject))
			} else {
				require(poll, ropError(ropProgress, 1, NotSupported))
			}
		})
	}
}

func (b *copyFolderMailbox) CopyMailMessage(_ context.Context, id core.ID, revision core.Revision, folder core.ID) (core.Object, error) {
	b.copies++
	object, ok := b.messages[id]
	if b.copies != 1 || !ok || object.Revision != revision {
		return core.Object{}, core.ErrConflict
	}
	object.ID, object.Parent = "copied", folder
	b.messages[object.ID] = object
	return object, nil
}

func TestCopyFolderPartialCompletion(t *testing.T) {
	for _, tc := range []struct {
		name    string
		create  error
		code    uint32
		partial byte
		copies  int
	}{
		{name: "second copy rejected", partial: 1, copies: 2},
		{name: "create rejected", create: core.ErrFolderNameConflict, code: duplicateFolderName},
		{name: "create response lost", create: core.ErrOutcomeUnknown, code: 0x80004005, partial: 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newCreateFixture(t)
			inbox := f.state.mailbox.objects[f.handles[1]].binding.store.inbox
			box := &copyFolderMailbox{
				placementTestMailbox: &placementTestMailbox{stateMailbox: f.box},
				createError:          tc.create,
				messages:             make(map[core.ID]core.Object),
			}
			for _, id := range []core.ID{"first", "second"} {
				box.messages[id] = core.Object{ID: id, Kind: core.Message, Parent: inbox,
					Revision: 1, Properties: core.Properties{core.MetadataRevision: core.Integer(1)}}
			}
			f.session = &testSession{box}
			faiRunOK(t, f, folderOperation(f.fids[SentItems], 4, 0, 2))
			wire := binary.LittleEndian.AppendUint64([]byte{ropCopyFolder, 4, 1, 2, 0, 0, 0}, f.fids[Inbox])
			wire = append(wire, 'x', 0)
			wire = append(wire, folderOperation(f.fids[Inbox], 4, 0, 3)...)
			out, _ := f.run(wire)
			want := append(ropError(ropCopyFolder, 1, tc.code), tc.partial, 2, 3, 0, 0, 0, 0, 0, 0)
			if !bytes.Equal(out, want) {
				t.Fatalf("CopyFolder and trailing OpenFolder = %x, want %x", out, want)
			}
			if box.creates != 1 || box.copies != tc.copies || box.updates != 0 ||
				box.messages["first"].Parent != inbox || box.messages["second"].Parent != inbox {
				t.Fatal("unexpected writes or source mutation")
			}
			if tc.copies > 0 && box.messages["copied"].Parent != "created" {
				t.Fatal("confirmed copy was lost")
			}
		})
	}
}

func TestCopyFolderPlanningDoesNotHoldHierarchyLock(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		f := newCreateFixture(t)
		store := f.state.mailbox.objects[f.handles[1]].binding.store
		started, resume := make(chan struct{}), make(chan struct{})
		box := &copyFolderMailbox{
			placementTestMailbox: &placementTestMailbox{stateMailbox: f.box},
			observeHook: func() {
				close(started)
				<-resume
			},
		}
		f.session = &testSession{box}
		faiRunOK(t, f, folderOperation(f.fids[SentItems], 4, 0, 2))
		wire := binary.LittleEndian.AppendUint64([]byte{ropCopyFolder, 4, 1, 2, 0, 0, 0}, f.fids[Inbox])
		wire = append(wire, "Replica\x00"...)
		result := make(chan []byte, 1)
		go func() {
			out, _ := f.run(wire)
			result <- out
		}()
		<-started
		start := time.Now()
		if !store.hierarchyMu.TryLock() {
			close(resume)
			<-result
			t.Fatal("folder scanning still blocks hierarchy mutations")
		}
		for i, entry := range box.page.Objects {
			if entry.ID == store.inbox {
				box.page.Objects[i].Properties = core.Properties{core.DisplayName: core.Text("New Inbox")}
				break
			}
		}
		store.hierarchyMu.Unlock()
		mutationLatency := time.Since(start)
		time.Sleep(time.Second) // Model one slow native read while copying.
		close(resume)
		out := <-result
		if !bytes.Equal(out, append(ropError(ropCopyFolder, 1, folderPlacementCode(core.ErrNotFound)), 0)) || box.creates != 0 {
			t.Fatalf("copy ignored changed source: %x, writes %d", out, box.creates)
		}
		t.Logf("hierarchy mutation completed in %v during a %v planning stall", mutationLatency, time.Since(start))
	})
}
