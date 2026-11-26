// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestServiceNotificationScopeOutputAndWait(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	f.transport.EnableCreateFolderSpike()
	f.open(int(Views))
	f.handles = append(f.handles, 0xffffffff)
	store := f.state.mailbox.objects[f.handles[0]].binding.store
	views := f.fids[Views]
	journalIDs, err := store.assign(t.Context(), []folderKey{{role: journalFolderRole}})
	if err != nil {
		t.Fatal(err)
	}
	register := func(slot byte, mask uint16, fid uint64) {
		wire := binary.LittleEndian.AppendUint16([]byte{0x29, 4, 0, slot}, mask)
		wire = binary.LittleEndian.AppendUint64(append(wire, 0), fid)
		resp, _ := f.run(binary.LittleEndian.AppendUint64(wire, 0))
		if !bytes.Equal(resp, []byte{0x29, slot, 0, 0, 0, 0}) {
			t.Fatalf("registration failed: %x", resp)
		}
	}
	register(2, 0x00f8, views)
	register(3, 0x00f8, journalIDs[0])
	register(4, notifyCreated, views)
	feed := &notificationFixtureFeed{make(chan struct{}), make(chan struct{}, 1), make(chan struct{}, 1)}
	f.transport.EnableNotificationWait(feed)
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	wait := make(chan error, 1)
	go func() {
		changed, err := f.transport.awaitNotification(ctx, f.session, f.state)
		if !changed && err == nil {
			err = errors.New("wait returned without a change")
		}
		wait <- err
	}()
	select {
	case <-feed.started:
	case <-ctx.Done():
		t.Fatal("wait never subscribed")
	}
	const name = "Nested Views"
	child, code := store.createFolder(ctx, f.session, views, createFolderRequest{name: name})
	if code != 0 {
		t.Fatalf("create folder: 0x%x", code)
	}
	if err := <-wait; err != nil {
		t.Fatal("local commit did not wake NotificationWait", err)
	}
	body, _ := f.execute(nil, uint32(10+len(f.handles)*4+17))
	if got := storeResponse(t, body).Operations; !bytes.Equal(got, []byte{0x6e, 0, 0}) {
		t.Fatalf("undersized response: %x", got)
	}
	ops, _ := f.run(nil)
	want := (objectNotification{flag: notifyModified, fid: views}).appendTo(nil, objectNotificationTarget{handle: f.handles[2], logon: 4})
	want = (objectNotification{flag: notifyCreated, fid: child, parent: views}).appendTo(want, objectNotificationTarget{handle: f.handles[4], logon: 4})
	if !bytes.Equal(ops, want) {
		t.Fatalf("misrouted, lost or duplicate notification: %x", ops)
	}
	if id, code := store.createFolder(ctx, f.session, views, createFolderRequest{name: name, existing: true}); code != 0 || id != child {
		t.Fatal("could not reopen the created folder")
	}
	if ops, _ := f.run(nil); len(ops) != 0 {
		t.Fatal("a folder reopen emitted a notification")
	}
	f.run([]byte{1, 4, 2, 1, 4, 4})
	if _, code := store.createFolder(ctx, f.session, views, createFolderRequest{name: "Another child"}); code != 0 {
		t.Fatal("could not create another child")
	}
	if ops, _ := f.run(nil); len(ops) != 0 {
		t.Fatal("released subscription or unrelated folder received a notification")
	}
}

func TestFAINotificationsFollowCommittedVersions(t *testing.T) {
	f, _ := messageFixture(t)
	request := syntheticFAIImport()
	draft := newFAIDraftForTest(t, f, request)
	store := f.store
	folder := draft.root
	require := func(after uint64, want ...objectNotification) {
		t.Helper()
		events, more, err := store.localNotificationPage(after)
		if err != nil || more || len(events) != len(want) {
			t.Fatalf("unexpected notification page: events=%v more=%v err=%v", events, more, err)
		}
		for i, event := range events {
			if event != want[i] {
				t.Fatalf("event %d = %+v, want %+v", i, event, want[i])
			}
		}
	}
	require(0)
	row := saveFAIDraftForTest(t, f, draft, 2)
	parent := objectNotification{flag: notifyModified, fid: folder}
	require(0, objectNotification{flag: notifyCreated | notifyMessage, fid: folder, mid: row.mid}, parent)
	saveFAIDraftForTest(t, f, draft, 2)
	require(2)
	_, err := draft.set(t.Context(), f.session, faiSetRequest{{0x7006001f, core.Text("Updated view")}})
	if err != nil {
		t.Fatal(err)
	}
	saveFAIDraftForTest(t, f, draft, 0)
	require(2, objectNotification{flag: notifyModified | notifyMessage, fid: folder, mid: row.mid}, parent)
	if err := store.deleteFAI(t.Context(), f.session, folder, faiDeleteRequest{sources: [][22]byte{request.source}}); err != nil {
		t.Fatal(err)
	}
	require(4, objectNotification{flag: notifyDeleted | notifyMessage, fid: folder, mid: row.mid}, parent)
}

func TestServiceNotificationExpiredCursorRequestsResync(t *testing.T) {
	var store ramStore
	for range localNotificationLimit + 1 {
		store.localNotifications.publish(objectNotification{flag: notifyModified, fid: 1})
	}
	if _, _, err := store.localNotificationPage(0); !errors.Is(err, core.ErrResync) {
		t.Fatal("an expired cursor did not require resync")
	}
	if events, more, err := store.localNotificationPage(1); err != nil || !more || len(events) != notificationPageSize {
		t.Fatal("the oldest retained cursor did not return a bounded page")
	}
}
