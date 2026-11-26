// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"testing"
)

func TestPrivateStorePerUserReads(t *testing.T) {
	f := newOSTFixture(t)
	guid := bytes.Repeat([]byte{0x23}, 16)
	longTermID := append(bytes.Clone(guid), 0, 0, 0, 0, 0, 3, 0, 0)
	ids := append([]byte{ropGetPerUserLongTermIDs, 4, 0}, guid...)
	getGUID := append([]byte{ropGetPerUserGUID, 4, 0}, longTermID...)
	read := append([]byte{ropReadPerUserInformation, 4, 0}, longTermID...)
	read = append(read, 0, 0, 0, 0, 0, 0, 2)
	ops := append(bytes.Clone(ids), getGUID...)
	ops = append(ops, read...)
	want := append(ropError(ropGetPerUserLongTermIDs, 0, 0), 0, 0)
	want = append(want, ropError(ropGetPerUserGUID, 0, PropertyNotFound)...)
	want = append(want, append(ropError(ropReadPerUserInformation, 0, 0), 1, 0, 0)...)
	if got, counts := f.run(ops); !bytes.Equal(got, want) || counts.errors != 1 {
		t.Fatalf("unexpected private-store responses %x", got)
	}
	for _, request := range [][]byte{ids, getGUID, read} {
		wrongHandle := bytes.Clone(request)
		wrongHandle[2] = 1
		if got, _ := f.run(wrongHandle); !bytes.Equal(got, ropError(request[0], 1, NotSupported)) {
			t.Fatal("PerUser read accepted a Folder handle")
		}
		wrongLogon := bytes.Clone(request)
		wrongLogon[1] = 5
		if got, _ := f.run(wrongLogon); !bytes.Equal(got, ropError(request[0], 0, accessDenied)) {
			t.Fatal("PerUser read accepted a handle from another logon")
		}
		truncated := append([]byte{1, 4, 1}, request[:len(request)-1]...)
		body, _ := f.execute(truncated, 65535)
		_, folderStillOpen := f.state.mailbox.objects[f.handles[1]]
		if !bytes.Equal(body, executeError(0x80070057, 65535)) || !folderStillOpen {
			t.Fatal("malformed PerUser read executed the Release prefix")
		}
	}
}

func TestPrivateStorePublicFolderRequests(t *testing.T) {
	f := newOSTFixture(t)
	// A private store answers IsGhosted=false even for an unrelated FID.
	owners := []byte{ropGetOwningServers, 4, 0, 1, 0, 0, 0, 0, 0, 0, 17}
	ghosted := bytes.Clone(owners)
	ghosted[0] = ropPublicFolderIsGhosted
	want := append(ropError(ropGetOwningServers, 0, NotSupported), append(ropError(ropPublicFolderIsGhosted, 0, 0), 0)...)
	prefix := append(bytes.Clone(owners), ghosted...)
	longTermID := bytes.Repeat([]byte{0x23}, 24)
	write := append([]byte{ropWritePerUserInformation, 4, 0}, longTermID...)
	first := append(bytes.Clone(write), 0, 0, 0, 0, 0, 3, 0, 4, 5, 6)
	first = append(first, longTermID[:16]...)
	last := append(bytes.Clone(write), 1, 3, 0, 0, 0, 1, 0, 7)
	batch := append(append(prefix, first...), last...)
	read := append([]byte{ropReadPerUserInformation, 4, 0}, longTermID...)
	batch = append(batch, append(read, make([]byte, 7)...)...)
	for range 2 {
		want = append(want, ropError(ropWritePerUserInformation, 0, NotSupported)...)
	}
	want = append(want, append(ropError(ropReadPerUserInformation, 0, 0), 1, 0, 0)...)
	if got, counts := f.run(batch); !bytes.Equal(got, want) || counts.errors != 3 {
		t.Fatalf("unexpected public-folder responses on private logon %x", got)
	}
	for _, request := range [][]byte{owners, ghosted, first, last} {
		truncated := append([]byte{1, 4, 1}, request[:len(request)-1]...)
		body, _ := f.execute(truncated, 65535)
		_, folderStillOpen := f.state.mailbox.objects[f.handles[1]]
		if !bytes.Equal(body, executeError(0x80070057, 65535)) || !folderStillOpen {
			t.Fatal("malformed public-folder request executed the Release prefix")
		}
	}
}
