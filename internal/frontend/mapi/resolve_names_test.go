// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"testing"

	"ewsgo/internal/core"
)

func TestResolveNamesNativeHTTP(t *testing.T) {
	transport, base, _ := transportFixture(t)
	transport.EnableNativeDirectory()
	owner := base.Identity()
	peer := owner
	peer.Scope.Account, peer.Name = "organizer", "organizer@example.invalid"
	source := &directoryFixture{entries: []core.DirectoryEntry{
		{Identity: owner, DisplayName: "Team Owner"},
		{Identity: peer, DisplayName: "Team Organizer", MailUser: true},
	}}
	session := directorySession{base, source}
	bind, _ := (Bind{State: &STAT{CodePage: 1252}}).Marshal()
	bound := directoryRequest(t, transport, session, "Bind", bind, nil)
	requireTransportCode(t, bound, 0)
	cookies := bound.Result().Cookies()

	tags := []uint32{0x0fff0102, 0x0ffe0003, 0x39000003, 0x30010000, 0x55550000}
	request := appendSTAT(append(append32(nil, 0x80000000), 0xff), &STAT{CodePage: 1252})
	request = append32(append(request, 0xff), uint32(len(tags)))
	for _, tag := range tags {
		request = append32(request, tag)
	}
	request = append32(append(request, 0xff), 4)
	for _, name := range []string{"=SMTP: Organizer@example.invalid", "Team", "absent@example.invalid", ""} {
		value, _ := propertyString(name, 0x001f, 1252)
		request = append(request, value...)
	}
	request = append32(request, 0)
	for i := range request {
		if _, err := parseResolveNames(request[:i]); err == nil {
			t.Fatalf("accepted truncated request at byte %d", i)
		}
	}
	w := directoryRequest(t, transport, session, "ResolveNames", request, cookies)
	body := requireTransportCode(t, w, 0)
	r := reader{data: body}
	if r.u32() != 0 || r.u32() != 0 || r.u32() != 1252 || !bytes.Equal(r.take(1), []byte{0xff}) || r.u32() != 4 {
		t.Fatal("ResolveNames method envelope")
	}
	for _, status := range []uint32{2, 1, 0, 0} {
		if r.u32() != status {
			t.Fatal("unique, ambiguous, absent or empty result is wrong")
		}
	}
	if !bytes.Equal(r.take(1), []byte{0xff}) || r.u32() != uint32(len(tags)) {
		t.Fatal("missing row columns")
	}
	for _, tag := range tags {
		if r.u32() != tag {
			t.Fatal("column order changed")
		}
	}
	entryID, err := transport.namespace.mailUserEntryID(peer)
	if err != nil {
		t.Fatal(err)
	}
	if r.u32() != 1 || !bytes.Equal(r.take(3), []byte{1, 0, 0xff}) || !bytes.Equal(r.blob(maxOwnAddressBookEntryID), entryID) {
		t.Fatal("the sole row must identify the visible organizer")
	}
	name, _ := addressBookString("Team Organizer", 0x001f, 1252)
	if !bytes.Equal(r.take(1), []byte{0}) || r.u32() != 6 || !bytes.Equal(r.take(1), []byte{0}) || r.u32() != 0 ||
		!bytes.Equal(r.take(3), []byte{0x1f, 0, 0}) || !bytes.Equal(r.take(len(name)), name) ||
		!bytes.Equal(r.take(3), []byte{0x0a, 0, 0x0a}) || r.u32() != PropertyNotFound || r.u32() != 0 || r.done() != nil {
		t.Fatal("flagged properties must preserve values and types")
	}
	if source.calls != 1 {
		t.Fatal("multiple directory reads for one lookup")
	}
}
