// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func configureOptionsWire(c synchronizationConfigure, restriction []byte) []byte {
	out := []byte{0x70, 0, 1, 0, c.kind, c.sendOptions}
	out = binary.LittleEndian.AppendUint16(out, c.flags)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(restriction)))
	out = append(out, restriction...)
	out = binary.LittleEndian.AppendUint32(out, c.extraFlags)
	out = binary.LittleEndian.AppendUint16(out, uint16(len(c.tags)))
	for _, tag := range c.tags {
		out = binary.LittleEndian.AppendUint32(out, tag)
	}
	return out
}

func TestSynchronizationOptionVocabularyAndProfileParity(t *testing.T) {
	base := synchronizationConfigure{kind: 2, sendOptions: 1, flags: 0x101, extraFlags: 1}
	check := func(c synchronizationConfigure) {
		t.Helper()
		d := inspectSynchronizationOptions(c)
		_, _, _, rejected, _, _ := d.Fields()
		if (validateHierarchyConfigure(c) == nil) != reflect.DeepEqual(rejected, []string{"empty"}) {
			t.Fatal("diagnostic disagrees with actual profile")
		}
		if fmt.Sprintf("%v %+v %#v", d, d, d) != "SynchronizationOptionsDiagnostic{redacted} SynchronizationOptionsDiagnostic{redacted} SynchronizationOptionsDiagnostic{redacted}" {
			t.Fatal("format leak")
		}
	}
	for i := range 16 {
		c := base
		c.flags = 1 << i
		check(c)
	}
	for i := range 8 {
		c := base
		c.sendOptions = 1 << i
		check(c)
	}
	for i := range 32 {
		c := base
		c.extraFlags = 1 << i
		check(c)
	}
	d := inspectSynchronizationOptions(synchronizationConfigure{kind: 2, sendOptions: 0xff, flags: 0xffff, extraFlags: 0xffffffff, restrictionBytes: 42, tags: make([]uint32, 17)})
	flags, send, extra, reasons, restriction, tags := d.Fields()
	if len(flags) != 13 || len(send) != 8 || len(extra) != 5 || !reflect.DeepEqual(reasons, []string{"sync_flags", "send_options", "extra_flags", "restriction"}) || restriction != "present" || tags != "17_256" {
		t.Fatal("closed vocabulary")
	}
	for _, n := range []int{0, 1, 16, 17, 256} {
		c := base
		c.tags = make([]uint32, n)
		check(c)
		_, _, _, _, _, got := inspectSynchronizationOptions(c).Fields()
		want := "zero"
		if n > 16 {
			want = "17_256"
		} else if n > 0 {
			want = "1_16"
		}
		if got != want {
			t.Fatal("tag bucket")
		}
	}
	check(base)
	c := base
	c.kind = 1
	check(c)
	c = base
	c.restrictionBytes = 1
	check(c)
}

func TestSynchronizationOptionsNoPayloadRetentionOrMalformedPromotion(t *testing.T) {
	c := synchronizationConfigure{kind: 2, sendOptions: 1, flags: 0x103, extraFlags: 1, tags: []uint32{0xdeadbeef, 0xffffffff}}
	wire := configureOptionsWire(c, []byte("PRIVATE-filter-values"))
	buffer := ROPBuffer{Operations: wire, Handles: []uint32{1, 2}}
	_, err := buffer.decodeStoreOperations(true, true)
	var unsupported unsupportedOperation
	if !errors.As(err, &unsupported) {
		t.Fatal("missing unsupported profile")
	}
	a, b, x, r, restriction, tags := unsupported.synchronizationOptions.Fields()
	if !reflect.DeepEqual(a, []string{"Unicode", "NoDeletions", "NoForeignIdentifiers"}) || !reflect.DeepEqual(b, []string{"Unicode"}) || !reflect.DeepEqual(x, []string{"Eid"}) || !reflect.DeepEqual(r, []string{"sync_flags", "restriction"}) || restriction != "present" || tags != "1_16" {
		t.Fatal("options incorrect")
	}
	text := fmt.Sprint(a, b, x, r, restriction, tags, err, unsupported.synchronizationOptions)
	if strings.Contains(text, "PRIVATE") || strings.Contains(text, "dead") || strings.Contains(text, "429496") {
		t.Fatal("payload leak")
	}
	for n := range len(wire) {
		_, err := (ROPBuffer{Operations: wire[:n], Handles: buffer.Handles}).decodeStoreOperations(true, true)
		var u unsupportedOperation
		if errors.As(err, &u) && u.synchronizationOptions.parsed {
			t.Fatal("partial promoted")
		}
	}
	_, err = buffer.decodeStoreOperations(true, false)
	if !errors.As(err, &unsupported) || unsupported.synchronizationOptions.parsed {
		t.Fatal("disabled ICS parsed options")
	}
}

type syncOptionsRecorder struct {
	*httptest.ResponseRecorder
	calls      int
	diagnostic SynchronizationOptionsDiagnostic
}

func (w *syncOptionsRecorder) RecordSynchronizationOptionsDiagnostic(d SynchronizationOptionsDiagnostic) {
	w.calls++
	w.diagnostic = d
}

func TestSynchronizationOptionsTransportGateAndNoPrefixEffects(t *testing.T) {
	transport, session, _, cookies := storeFixture(t)
	transport.EnableHierarchyDownloadSpike()
	identity, _ := transport.namespace.Mailbox(session.Identity())
	body, cookies := storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 0, 0), []uint32{0xffffffff, 0xffffffff}, 4096)
	logon := storeResponse(t, body)
	fid := binary.LittleEndian.Uint64(logon.Operations[7+int(Inbox)*8:])
	body, cookies = storeExecute(t, transport, session, cookies, folderOperation(fid, 0, 0, 1), logon.Handles, 4096)
	opened := storeResponse(t, body)
	c := synchronizationConfigure{kind: 2, sendOptions: 1, flags: 0x103, extraFlags: 1}
	requestOps := append([]byte{1, 0, 1}, configureOptionsWire(c, nil)...)
	call := func(withCookies bool) ([]byte, *syncOptionsRecorder) {
		t.Helper()
		wire, _ := (ROPBuffer{Operations: requestOps, Handles: opened.Handles}).Marshal()
		request, _ := (Execute{ROP: wire, MaxOutput: 4096}).Marshal()
		r := httptest.NewRequest("POST", "https://gateway.invalid/mapi/emsmdb/?MailboxId="+identity.RoutingID, bytes.NewReader(request))
		r.Header.Set("Content-Type", "application/mapi-http")
		r.Header.Set("X-RequestId", "synthetic-request:1")
		r.Header.Set("X-RequestType", "Execute")
		if withCookies {
			for _, c := range cookies {
				r.AddCookie(c)
			}
		}
		w := &syncOptionsRecorder{ResponseRecorder: httptest.NewRecorder()}
		transport.Mailbox(w, r, session)
		if !withCookies {
			return nil, w
		}
		cookies = w.Result().Cookies()
		return requireTransportCode(t, w.ResponseRecorder, 0), w
	}
	off, w := call(true)
	if w.calls != 0 {
		t.Fatal("default gate")
	}
	transport.EnableSynchronizationOptionsTrace()
	_, w = call(false)
	if w.calls != 0 {
		t.Fatal("invalid context diagnostic")
	}
	on, w := call(true)
	_, _, _, reasons, _, _ := w.diagnostic.Fields()
	if w.calls != 1 || !bytes.Equal(off, on) || binary.LittleEndian.Uint32(on[4:]) != NotSupported || !reflect.DeepEqual(reasons, []string{"sync_flags"}) {
		t.Fatal("gate/wire changed")
	}
	for _, s := range transport.contexts.entries {
		if _, code := s.mailbox.object(opened.Handles[1], 0); code != 0 {
			t.Fatal("prefix Release executed")
		}
	}
}

func FuzzSynchronizationOptionVocabulary(f *testing.F) {
	f.Add(uint8(2), uint8(1), uint16(0x101), uint32(1), uint16(0), uint8(0))
	f.Fuzz(func(t *testing.T, kind, send uint8, flags uint16, extra uint32, restriction uint16, count uint8) {
		c := synchronizationConfigure{kind: kind, sendOptions: send, flags: flags, extraFlags: extra, restrictionBytes: restriction, tags: make([]uint32, int(count))}
		d := inspectSynchronizationOptions(c)
		a, b, x, r, p, n := d.Fields()
		if len(a) > 13 || len(b) > 8 || len(x) > 5 || len(r) > 8 || (p != "present" && p != "absent") || (n != "zero" && n != "1_16" && n != "17_256") {
			t.Fatal("unbounded vocabulary")
		}
		if (validateHierarchyConfigure(c) == nil) != reflect.DeepEqual(r, []string{"empty"}) {
			t.Fatal("profile parity")
		}
	})
}
