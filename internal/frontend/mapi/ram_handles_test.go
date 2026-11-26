// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"testing"

	"ewsgo/internal/core"
)

func TestRAMHandlesLogonFolderReleaseAndReplacement(t *testing.T) {
	stores, binding, session := ramFixture(t)
	objects := newMailboxObjects(stores)
	handle, err := objects.addLogon(0x9a, binding)
	if err != nil || handle == 0 || handle == 0xffffffff {
		t.Fatal("Logon handle", err)
	}
	folder, value, code := objects.openFolder(context.Background(), session, 0x9a, handle, binding.fids[Inbox])
	if code != 0 || folder == handle || value.native != "any/not/zimbra/inbox" {
		t.Fatal("native folder object", code)
	}
	virtual, _, code := objects.openFolder(context.Background(), session, 0x9a, handle, binding.fids[Outbox])
	if code != 0 || virtual == folder {
		t.Fatal("service folder object", code)
	}
	if _, code := objects.object(folder, 0x9b); code != accessDenied {
		t.Fatal("cross-logon handle accepted")
	}
	other := newMailboxObjects(stores)
	second, err := other.addLogon(0x9a, binding)
	if err != nil || second == handle {
		t.Fatal("handle reused in another context")
	}
	if _, code := other.object(folder, 0x9a); code != nullObject {
		t.Fatal("cross-context handle accepted")
	}
	objects.release(folder)
	if _, code := objects.object(folder, 0x9a); code != invalidObject {
		t.Fatal("released handle accepted")
	}
	if _, code := objects.object(0xffffffff, 0x9a); code != nullObject {
		t.Fatal("unallocated sentinel accepted")
	}
	replacement, err := objects.addLogon(0x9a, binding)
	if err != nil || replacement == handle {
		t.Fatal("relogon replacement", err)
	}
	for _, old := range []uint32{handle, folder, virtual} {
		if _, code := objects.object(old, 0x9a); code != invalidObject {
			t.Fatal("old Logon child survived replacement")
		}
	}
	objects.release(replacement)
	objects.release(replacement)
	if len(objects.objects) != 0 || len(objects.logons) != 0 {
		t.Fatal("Logon release retained children")
	}
}

func TestRAMHandleLimitsAndRevocationDoNotAllocate(t *testing.T) {
	stores, binding, session := ramFixture(t)
	objects := newMailboxObjects(stores)
	handle, _ := objects.addLogon(1, binding)
	box := session.box.(*catalogMailbox)
	box.getErr = core.ErrAccessDenied
	before := stores.nextHandle.Load()
	if _, _, code := objects.openFolder(context.Background(), session, 1, handle, binding.fids[Outbox]); code != accessDenied || stores.nextHandle.Load() != before {
		t.Fatal("revoked service object allocated a handle")
	}
	box.getErr = nil
	for len(objects.objects) < maxLiveStoreHandles {
		if _, _, code := objects.openFolder(context.Background(), session, 1, handle, binding.fids[Inbox]); code != 0 {
			t.Fatal("handle allocation failed before quota", code)
		}
	}
	before = stores.nextHandle.Load()
	if _, _, code := objects.openFolder(context.Background(), session, 1, handle, binding.fids[Inbox]); code != 0x8007000e || stores.nextHandle.Load() != before {
		t.Fatal("live-handle quota changed allocator")
	}
	if _, err := objects.addLogon(2, binding); err != core.ErrUnsupported || objects.logons[1] != handle {
		t.Fatal("unrelated logon evicted quota-limited context")
	}
	replacement, err := objects.addLogon(1, binding)
	if err != nil || replacement == handle || len(objects.objects) != 1 {
		t.Fatal("replacement at live quota failed", err)
	}
	objects.allocated = maxContextHandleAllocations
	if _, err := objects.addLogon(1, binding); err != core.ErrUnsupported || objects.logons[1] != replacement {
		t.Fatal("failed replacement destroyed active logon")
	}
	if _, _, code := objects.openFolder(context.Background(), session, 1, replacement, binding.fids[Inbox]); code != 0x8007000e {
		t.Fatal("lifetime allocation quota bypassed")
	}
}
