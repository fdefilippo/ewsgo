// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"testing"
)

// Independent literal MS-OXCROPS fixture: RopId, input slot, success, count=2,
// size=8, ASCII EX/NUL and SMTP/NUL. No property values or native identities.
func addressTypesFixture(input byte) []byte {
	return []byte{0x49, input, 0, 0, 0, 0, 2, 0, 8, 0, 'E', 'X', 0, 'S', 'M', 'T', 'P', 0}
}

func TestAddressTypesCodecThreeByteRequestAndNoPartialBatch(t *testing.T) {
	for _, tc := range []struct {
		data []byte
		err  error
	}{
		{[]byte{0x49, 0xa1, 1}, nil},
		{[]byte{0x49}, ErrMalformed},
		{[]byte{0x49, 0}, ErrMalformed},
		{[]byte{0x49, 0, 2}, ErrMalformed},
		{[]byte{0x49, 0, 0, 2, 0, 0}, ErrMalformed},
		{[]byte{0x49, 0, 0, ropFastTransferSourceCopyFolder, 0, 0}, ErrUnsupported},
	} {
		ops, err := (ROPBuffer{Operations: tc.data, Handles: []uint32{0xffffffff, 0xffffffff}}).DecodeOperations()
		if tc.err != nil {
			if !errors.Is(err, tc.err) || ops != nil {
				t.Fatal("invalid suffix returned a partial batch", err)
			}
		} else if err != nil || len(ops) != 1 || ops[0].ID != 0x49 || ops[0].Logon != 0xa1 || ops[0].Input != 1 {
			t.Fatal("three-byte request changed", err)
		}
	}
}

func TestAddressTypesIgnoresObjectButPreservesContextAndOptIn(t *testing.T) {
	transport, session, box, cookies := storeFixture(t)
	// No Logon object exists. Its numeric value and LogonId are ignored as
	// specified by MS-OXOMSG, not interpreted as authority to another mailbox.
	body, cookies := storeExecute(t, transport, session, cookies, []byte{0x49, 0x91, 1}, []uint32{0xffffffff, 0x12345678}, 4096)
	response := storeResponse(t, body)
	if !bytes.Equal(response.Operations, addressTypesFixture(1)) || response.Handles[1] != 0x12345678 || len(transport.stores.entries) != 0 || box.calls != 0 || box.stateCalls != 0 {
		t.Fatal("address types created objects, performed native queries or changed ignored handle")
	}
	rop, _ := (ROPBuffer{Operations: []byte{0x49, 0, 0}, Handles: []uint32{0xffffffff}}).Marshal()
	execute, _ := (Execute{ROP: rop, MaxOutput: 4096}).Marshal()
	foreign := session.Identity()
	foreign.Scope.Account += "/another"
	requireTransportCode(t, transportRequest(t, transport, scopedSession{session, foreign}, "Execute", execute, cookies), 10)
	requireTransportCode(t, transportRequest(t, transport, session, "Execute", execute, nil), 13)
	// The non-store session experiment still refuses even this metadata ROP.
	disabled, otherSession, connect := transportFixture(t)
	w := transportRequest(t, disabled, otherSession, "Connect", connect, nil)
	requireTransportCode(t, w, 0)
	body, _ = storeExecute(t, disabled, otherSession, w.Result().Cookies(), []byte{0x49, 0, 0}, []uint32{0xffffffff}, 4096)
	if binary.LittleEndian.Uint32(body[4:]) != NotSupported {
		t.Fatal("address types escaped the store opt-in")
	}
}

func TestAddressTypesOutputReservationAndLogonRevision(t *testing.T) {
	transport, session, box, cookies := storeFixture(t)
	body, cookies := storeExecute(t, transport, session, cookies, []byte{0x49, 0, 0}, []uint32{0xffffffff}, 31)
	tooSmall := storeResponse(t, body)
	if tooSmall.Operations[0] != 0xff || binary.LittleEndian.Uint16(tooSmall.Operations[1:]) != 32 || tooSmall.Handles[0] != 0xffffffff {
		t.Fatal("exact address-types output bound")
	}
	body, cookies = storeExecute(t, transport, session, cookies, []byte{0x49, 0, 0}, []uint32{0xffffffff}, 32)
	if !bytes.Equal(storeResponse(t, body).Operations, addressTypesFixture(0)) {
		t.Fatal("exact-fit retry failed")
	}
	identity, _ := transport.namespace.Mailbox(session.Identity())
	ops := append(logonOperation(identity.LegacyDN, 0, 0), 0x49, 0, 0)
	body, cookies = storeExecute(t, transport, session, cookies, ops, []uint32{0xffffffff}, 197)
	if storeResponse(t, body).Operations[0] != 0xff || box.calls != 0 || box.stateCalls != 0 || len(transport.stores.entries) != 0 {
		t.Fatal("address-types response was omitted from preflight")
	}
	body, _ = storeExecute(t, transport, session, cookies, ops, []uint32{0xffffffff}, 198)
	response := storeResponse(t, body)
	if len(response.Operations) != 184 || binary.LittleEndian.Uint64(response.Operations[154:162]) != 1 || !bytes.Equal(response.Operations[166:], addressTypesFixture(0)) {
		t.Fatal("Logon/AddressTypes batch or configuration revision")
	}
}
