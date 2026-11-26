// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"net/http"
	"testing"

	"ewsgo/internal/core"
)

type stateMailbox struct {
	*catalogMailbox
	state      core.MailboxState
	stateErr   error
	stateCalls int
}

func (b *stateMailbox) MailboxState(context.Context) (core.MailboxState, error) {
	b.stateCalls++
	return b.state, b.stateErr
}

func storeFixture(t *testing.T) (*SessionTransport, core.Session, *stateMailbox, []*http.Cookie) {
	t.Helper()
	transport, _, _ := transportFixture(t)
	transport.EnableRAMStore()
	box := &stateMailbox{catalogMailbox: catalogFixture(), state: core.MailboxState{Owner: true, OutOfOffice: true}}
	session := &testSession{box}
	identity, _ := transport.namespace.Mailbox(session.Identity())
	data, _ := (Connect{UserDN: identity.LegacyDN, CodePage: 1252}).Marshal()
	w := transportRequest(t, transport, session, "Connect", data, nil)
	requireTransportCode(t, w, 0)
	return transport, session, box, w.Result().Cookies()
}

func logonOperation(dn string, logon, slot byte) []byte {
	out := append32(append32([]byte{0xfe, logon, slot, 9}, 0x2100040c), 0)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(dn)+1))
	return append(append(out, dn...), 0)
}

func folderOperation(fid uint64, logon, input, output byte) []byte {
	out := binary.LittleEndian.AppendUint64([]byte{2, logon, input, output}, fid)
	return append(out, 0)
}

func storeExecute(t *testing.T, transport *SessionTransport, session core.Session, cookies []*http.Cookie, ops []byte, handles []uint32, maximum uint32) ([]byte, []*http.Cookie) {
	t.Helper()
	rop, err := (ROPBuffer{Operations: ops, Handles: handles}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	request, err := (Execute{ROP: rop, MaxOutput: maximum}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	w := transportRequest(t, transport, session, "Execute", request, cookies)
	return requireTransportCode(t, w, 0), w.Result().Cookies()
}

func storeResponse(t *testing.T, body []byte) ROPBuffer {
	t.Helper()
	if len(body) < 20 || binary.LittleEndian.Uint32(body) != 0 || binary.LittleEndian.Uint32(body[4:]) != 0 {
		t.Fatal("Execute failed", len(body))
	}
	r := reader{data: body[12:]}
	rop := r.blob(MaxBody)
	aux := r.blob(MaxAuxiliary)
	if r.done() != nil || len(aux) != 0 {
		t.Fatal("Execute response framing")
	}
	buffer, err := ParseROP(rop)
	if err != nil {
		t.Fatal("ROP response framing", err)
	}
	return buffer
}

func TestStoreExecuteLogonOpenAllRolesReleaseAndReconnect(t *testing.T) {
	transport, session, _, cookies := storeFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	body, cookies := storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 0x9a, 0), []uint32{0xffffffff, 0xffffffff}, 4096)
	response := storeResponse(t, body)
	if len(response.Operations) != 166 || response.Operations[0] != 0xfe || response.Operations[6] != 1 || response.Operations[111] != 0x13 || binary.LittleEndian.Uint32(response.Operations[2:]) != 0 || response.Handles[0] == 0xffffffff {
		t.Fatal("Logon roles/rights/handle")
	}
	first := bytes.Clone(response.Operations)
	logonHandle := response.Handles[0]
	for role := range specialFolderCount {
		fid := binary.LittleEndian.Uint64(first[7+role*8:])
		body, cookies = storeExecute(t, transport, session, cookies, folderOperation(fid, 0x9a, 0, 1), []uint32{logonHandle, 0xffffffff}, 4096)
		opened := storeResponse(t, body)
		if !bytes.Equal(opened.Operations, []byte{2, 1, 0, 0, 0, 0, 0, 0}) || opened.Handles[1] == 0xffffffff {
			t.Fatal("OpenFolder failed", role)
		}
		body, cookies = storeExecute(t, transport, session, cookies, []byte{1, 0x9a, 1}, opened.Handles, 4096)
		if len(storeResponse(t, body).Operations) != 0 {
			t.Fatal("Release returned a ROP")
		}
		body, cookies = storeExecute(t, transport, session, cookies, folderOperation(fid, 0x9a, 1, 1), opened.Handles, 4096)
		if binary.LittleEndian.Uint32(storeResponse(t, body).Operations[2:]) != invalidObject {
			t.Fatal("released folder reopened")
		}
	}
	connect, _ := (Connect{UserDN: identity.LegacyDN}).Marshal()
	w := transportRequest(t, transport, session, "Connect", connect, cookies)
	requireTransportCode(t, w, 0)
	cookies = w.Result().Cookies()
	body, cookies = storeExecute(t, transport, session, cookies, folderOperation(binary.LittleEndian.Uint64(first[7:]), 0x9a, 0, 1), []uint32{logonHandle, 0xffffffff}, 4096)
	if binary.LittleEndian.Uint32(storeResponse(t, body).Operations[2:]) != nullObject {
		t.Fatal("old context handle accepted")
	}
	body, cookies = storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 0x9a, 0), []uint32{0xffffffff}, 4096)
	reconnected := storeResponse(t, body)
	if !bytes.Equal(first[7:111], reconnected.Operations[7:111]) || !bytes.Equal(first[130:146], reconnected.Operations[130:146]) || reconnected.Handles[0] == logonHandle {
		t.Fatal("RAM reconnect identity/replica continuity or handle isolation")
	}
	w = transportRequest(t, transport, session, "Disconnect", make([]byte, 4), cookies)
	requireTransportCode(t, w, 0)
	if len(transport.contexts.entries) != 0 {
		t.Fatal("disconnect retained store context")
	}
}

func TestStoreExecuteValidatesWholeBatchAndReservesOutputBeforeEffects(t *testing.T) {
	for _, tc := range []struct {
		name     string
		tail     []byte
		maximum  uint32
		method   uint32
		tooSmall bool
	}{
		{"unknown tail", []byte{0xff, 0, 0}, 4096, NotSupported, false},
		{"truncated tail", []byte{2, 0, 0}, 4096, 0x80070057, false},
		{"output limit", nil, 80, 0, true},
		{"no output space", nil, 0, 0x47d, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport, session, box, cookies := storeFixture(t)
			identity, _ := transport.namespace.Mailbox(session.Identity())
			ops := append(logonOperation(identity.LegacyDN, 1, 0), tc.tail...)
			body, _ := storeExecute(t, transport, session, cookies, ops, []uint32{0xffffffff}, tc.maximum)
			if box.calls != 0 || box.stateCalls != 0 || len(transport.stores.entries) != 0 {
				t.Fatal("invalid batch had native/allocation effects")
			}
			if tc.tooSmall {
				response := storeResponse(t, body)
				if response.Operations[0] != 0xff || binary.LittleEndian.Uint16(response.Operations[1:]) != 180 || response.Handles[0] != 0xffffffff {
					t.Fatal("BufferTooSmall framing")
				}
			} else if tc.maximum == 0 {
				if len(body) != 8 || binary.LittleEndian.Uint32(body) != tc.method {
					t.Fatal("failure framing")
				}
			} else if binary.LittleEndian.Uint32(body[4:]) != tc.method {
				t.Fatal("method failure code")
			}
		})
	}
	transport, session, _, cookies := storeFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	body, cookies := storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 1, 0), []uint32{0xffffffff}, 4096)
	handle := storeResponse(t, body).Handles[0]
	// Even Release must wait for full validation and output reservation.
	body, _ = storeExecute(t, transport, session, cookies, append([]byte{1, 1, 0}, logonOperation(identity.LegacyDN, 1, 0)...), []uint32{handle}, 50)
	storeResponse(t, body)
	for _, state := range transport.contexts.entries {
		if _, code := state.mailbox.object(handle, 1); code != 0 {
			t.Fatal("Release executed before output preflight")
		}
	}
}

func TestStoreLogonAuthorizationAndProviderErrors(t *testing.T) {
	for _, tc := range []struct {
		name   string
		alter  func([]byte, *stateMailbox)
		code   uint32
		noRead bool
	}{
		{"foreign target", func(b []byte, _ *stateMailbox) { b[15] ^= 1 }, accessDenied, true},
		{"admin", func(b []byte, _ *stateMailbox) { b[4] |= 1 }, accessDenied, true},
		{"public", func(b []byte, _ *stateMailbox) { b[3] &^= 1; b[4] |= 2 }, NotSupported, true},
		{"unknown flag", func(b []byte, _ *stateMailbox) { b[3] |= 0x80 }, NotSupported, true},
		{"extra spooler bit on ordinary logon", func(b []byte, _ *stateMailbox) { b[3] &^= 8; b[4] |= 0x40 }, NotSupported, true},
		{"spooler cannot request admin access", func(b []byte, _ *stateMailbox) { b[4] |= 0x41 }, accessDenied, true},
		{"spooler cannot target a foreign mailbox", func(b []byte, _ *stateMailbox) { b[4] |= 0x40; b[15] ^= 1 }, accessDenied, true},
		{"state denied", func(_ []byte, b *stateMailbox) { b.stateErr = core.ErrAccessDenied }, accessDenied, false},
		{"not owner", func(_ []byte, b *stateMailbox) { b.state.Owner = false }, accessDenied, false},
		{"catalog denied", func(_ []byte, b *stateMailbox) { b.err = core.ErrAccessDenied }, accessDenied, false},
		{"catalog truncated", func(_ []byte, b *stateMailbox) { b.page.More = true }, NotSupported, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			transport, session, box, cookies := storeFixture(t)
			identity, _ := transport.namespace.Mailbox(session.Identity())
			ops := logonOperation(identity.LegacyDN, 3, 0)
			tc.alter(ops, box)
			body, _ := storeExecute(t, transport, session, cookies, ops, []uint32{0xffffffff}, 4096)
			response := storeResponse(t, body)
			if len(response.Operations) != 6 || binary.LittleEndian.Uint32(response.Operations[2:]) != tc.code || response.Handles[0] != 0xffffffff || len(transport.stores.entries) != 0 {
				t.Fatal("target or backend failure accepted")
			}
			if tc.noRead && (box.stateCalls != 0 || box.calls != 0) {
				t.Fatal("foreign/unsupported logon queried native state")
			}
		})
	}
}

func TestStoreSpoolerLogonWithOutlookOpenFlags(t *testing.T) {
	transport, session, _, cookies := storeFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	ops := logonOperation(identity.LegacyDN, 3, 0)
	ops[4] |= 0x40
	body, _ := storeExecute(t, transport, session, cookies, ops, []uint32{0xffffffff}, 4096)
	response := storeResponse(t, body)
	if len(response.Operations) != 166 || binary.LittleEndian.Uint32(response.Operations[2:]) != 0 || response.Handles[0] == 0xffffffff {
		t.Fatal("Outlook spooler logon did not open the authenticated private store")
	}
}

func TestStoreExecuteBatchAndRevokedAccess(t *testing.T) {
	transport, session, box, cookies := storeFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	// Simulate an earlier context's known FIDs, then use a freshly created
	// Logon output handle as input to OpenFolder in the very same Execute.
	catalog, _ := privateFolders(context.Background(), session)
	binding, _ := transport.stores.bind(context.Background(), catalog)
	ops := append(logonOperation(identity.LegacyDN, 4, 0), folderOperation(binding.fids[Inbox], 4, 0, 1)...)
	body, cookies := storeExecute(t, transport, session, cookies, ops, []uint32{0xffffffff, 0xffffffff}, 4096)
	response := storeResponse(t, body)
	if len(response.Operations) != 174 || binary.LittleEndian.Uint32(response.Operations[168:]) != 0 {
		t.Fatal("same-batch logon handle unavailable")
	}
	box.getErr = core.ErrAccessDenied
	body, cookies = storeExecute(t, transport, session, cookies, folderOperation(binding.fids[Outbox], 4, 0, 1), response.Handles, 4096)
	if binary.LittleEndian.Uint32(storeResponse(t, body).Operations[2:]) != accessDenied {
		t.Fatal("virtual folder bypassed revoked native access")
	}
	box.getErr = nil
	// A forged Release in another LogonId cannot revoke a live object.
	storeExecute(t, transport, session, cookies, []byte{1, 5, 0}, response.Handles, 4096)
	for _, state := range transport.contexts.entries {
		if _, code := state.mailbox.object(response.Handles[0], 4); code != 0 {
			t.Fatal("cross-logon Release accepted")
		}
	}
}

func TestStoreExecuteFolderErrorsAndQuotasLeaveHandlesUsable(t *testing.T) {
	transport, session, _, cookies := storeFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	body, cookies := storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 7, 0), []uint32{0xffffffff}, 4096)
	response := storeResponse(t, body)
	handle := response.Handles[0]
	fid := binary.LittleEndian.Uint64(response.Operations[7+int(Inbox)*8:])
	var objects *mailboxObjects
	for _, state := range transport.contexts.entries {
		objects = state.mailbox
	}
	for _, tc := range []struct {
		name string
		ops  []byte
		code uint32
	}{
		{"foreign logon", folderOperation(fid, 8, 0, 1), accessDenied},
		{"unknown FID", folderOperation(0xffffffffffffffff, 7, 0, 1), 0x8004010f},
		{"soft-deleted unsupported", append(folderOperation(fid, 7, 0, 1)[:12], 4), NotSupported},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body, cookies = storeExecute(t, transport, session, cookies, tc.ops, []uint32{handle, 0xffffffff}, 4096)
			output := storeResponse(t, body)
			if binary.LittleEndian.Uint32(output.Operations[2:]) != tc.code || output.Handles[1] != 0xffffffff || len(objects.objects) != 1 {
				t.Fatal("failed folder operation allocated a handle")
			}
		})
	}
	objects.allocated = maxContextHandleAllocations
	body, cookies = storeExecute(t, transport, session, cookies, folderOperation(fid, 7, 0, 1), []uint32{handle, 0xffffffff}, 4096)
	if binary.LittleEndian.Uint32(storeResponse(t, body).Operations[2:]) != 0x8007000e {
		t.Fatal("folder quota not enforced")
	}
	body, _ = storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 7, 0), []uint32{handle}, 4096)
	if binary.LittleEndian.Uint32(storeResponse(t, body).Operations[2:]) != NotSupported {
		t.Fatal("logon quota not enforced")
	}
	if _, code := objects.object(handle, 7); code != 0 {
		t.Fatal("failed replacement invalidated existing Logon")
	}
}

func TestStoreExecuteUnknownFlagsAndOversizedOutputAreRejected(t *testing.T) {
	for _, tc := range []struct{ flags, maximum, status, method uint32 }{
		{8, 4096, 0, NotSupported}, {3, 0x40001, 0x6f7, 0},
	} {
		transport, session, box, cookies := storeFixture(t)
		identity, _ := transport.namespace.Mailbox(session.Identity())
		rop, _ := (ROPBuffer{Operations: logonOperation(identity.LegacyDN, 0, 0), Handles: []uint32{0xffffffff}}).Marshal()
		data, _ := (Execute{Flags: tc.flags, ROP: rop, MaxOutput: tc.maximum}).Marshal()
		body := requireTransportCode(t, transportRequest(t, transport, session, "Execute", data, cookies), 0)
		if binary.LittleEndian.Uint32(body) != tc.status || binary.LittleEndian.Uint32(body[4:]) != tc.method || box.stateCalls != 0 || len(transport.stores.entries) != 0 {
			t.Fatal("unsupported Execute options had side effects")
		}
	}
}
