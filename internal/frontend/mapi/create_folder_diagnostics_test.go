// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestCreateFolderDiagnosticBoundaryAndClosedHeader(t *testing.T) {
	for _, prefix := range [][]byte{nil, {1, 4, 1}, folderOperation(123, 4, 0, 1), receiveFolderOperation(4, 0, "IPM.PRIVATE")} {
		for _, kind := range []byte{0, 1, 2, 255} {
			wire := append(bytes.Clone(prefix), 0x1c, 4, 1, 0, kind, 255, 2, 3)
			wire = append(wire, []byte("PRIVATE-NAME-COMMENT-UNTERMINATED")...)
			_, err := (ROPBuffer{Operations: wire, Handles: []uint32{1, 2}}).DecodeOperations()
			var u unsupportedOperation
			if !errors.As(err, &u) || u.label != "RopCreateFolder" {
				t.Fatal("unsupported diagnostic boundary")
			}
			inspection, got, encoding, existing, reserved := u.createFolder.Fields()
			want := "other"
			if kind == 1 {
				want = "generic"
			} else if kind == 2 {
				want = "search"
			}
			if inspection != "header_only" || got != want || encoding != "unicode" || existing != "true" || reserved != "nonzero" {
				t.Fatal("closed header enums")
			}
			text := fmt.Sprintf("%v %+v %#v %v", u.createFolder, u.createFolder, u.createFolder, err)
			if strings.Contains(text, "PRIVATE") || strings.Contains(text, "255") || !strings.Contains(text, "redacted") {
				t.Fatal("payload formatting leak")
			}
		}
	}
	for n := range 5 {
		d := inspectCreateFolderHeader([]byte{0, 1, 0, 0, 0}[:n], 2)
		a, b, c, x, y := d.Fields()
		if a != "malformed" || b != "none" || c != "none" || x != "none" || y != "none" {
			t.Fatal("short header promoted")
		}
	}
	if a, _, _, _, _ := inspectCreateFolderHeader([]byte{2, 1, 0, 0, 0}, 2).Fields(); a != "malformed" {
		t.Fatal("invalid output index")
	}
	a, b, c, x, y := inspectCreateFolderHeader([]byte{0, 2, 0, 0, 0}, 2).Fields()
	if a != "header_only" || b != "search" || c != "string8" || x != "false" || y != "zero" {
		t.Fatal("zero booleans")
	}
	// Do not scan a later opcode inside an unknown ROP payload.
	_, err := (ROPBuffer{Operations: []byte{0xaa, 4, 1, 0x1c, 4, 1, 0, 1, 1, 1, 0}, Handles: []uint32{1, 2}}).DecodeOperations()
	var u unsupportedOperation
	if !errors.As(err, &u) || u.createFolder.state != 0 {
		t.Fatal("scanned beyond first unsupported ROP")
	}
}

type createFolderRecorder struct {
	*httptest.ResponseRecorder
	calls      int
	diagnostic CreateFolderDiagnostic
}

func (w *createFolderRecorder) RecordCreateFolderDiagnostic(d CreateFolderDiagnostic) {
	w.calls++
	w.diagnostic = d
}

func TestCreateFolderTraceTransportGateAndAtomicRejection(t *testing.T) {
	transport, session, box, cookies := storeFixture(t)
	identity, _ := transport.namespace.Mailbox(session.Identity())
	body, cookies := storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 0, 0), []uint32{0xffffffff, 0xffffffff}, 4096)
	logon := storeResponse(t, body)
	fid := binary.LittleEndian.Uint64(logon.Operations[7+int(Inbox)*8:])
	body, cookies = storeExecute(t, transport, session, cookies, folderOperation(fid, 0, 0, 1), logon.Handles, 4096)
	opened := storeResponse(t, body)
	call := func(withCookies bool) ([]byte, *createFolderRecorder) {
		t.Helper()
		ops := append([]byte{1, 0, 1}, 0x1c, 0, 1, 0, 1, 1, 1, 0)
		ops = append(ops, []byte("PRIVATE-name-comment")...)
		wire, _ := (ROPBuffer{Operations: ops, Handles: opened.Handles}).Marshal()
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
		w := &createFolderRecorder{ResponseRecorder: httptest.NewRecorder()}
		transport.Mailbox(w, r, session)
		if !withCookies {
			return nil, w
		}
		cookies = w.Result().Cookies()
		return requireTransportCode(t, w.ResponseRecorder, 0), w
	}
	before := box.calls
	off, w := call(true)
	if w.calls != 0 || box.calls != before {
		t.Fatal("default or backend effects")
	}
	transport.EnableCreateFolderTrace()
	_, w = call(false)
	if w.calls != 0 {
		t.Fatal("unauthenticated context diagnostic")
	}
	on, w := call(true)
	a, b, c, x, y := w.diagnostic.Fields()
	if w.calls != 1 || !bytes.Equal(on, off) || binary.LittleEndian.Uint32(on[4:]) != NotSupported || a != "header_only" || b != "generic" || c != "unicode" || x != "true" || y != "zero" || box.calls != before {
		t.Fatal("diagnostic changed behavior")
	}
	for _, s := range transport.contexts.entries {
		if _, code := s.mailbox.object(opened.Handles[1], 0); code != 0 {
			t.Fatal("prefix Release executed")
		}
	}
}

func TestCreateFolderTraceExecutedFailure(t *testing.T) {
	f := newCreateFixture(t)
	f.transport.EnableCreateFolderTrace()
	name := "PRIVATE-duplicate"
	create := createOperation(4, 1, 2, name, "", false)
	_, result := f.run(append(bytes.Clone(create), create...))
	inspection, kind, _, existing, _ := result.createFolder.Fields()
	if inspection != "executed" || kind != "generic" || existing != "false" ||
		result.createFolder.Result() != "0x80040604" {
		t.Fatal("trace did not retain the duplicate result")
	}
	if strings.Contains(fmt.Sprintf("%+v %#v", result.createFolder, result.createFolder), name) {
		t.Fatal("folder name leaked")
	}
}

func FuzzCreateFolderDiagnosticHeader(f *testing.F) {
	f.Add([]byte{0, 1, 1, 1, 0}, uint8(2))
	f.Add([]byte{}, uint8(0))
	f.Fuzz(func(t *testing.T, raw []byte, handles uint8) {
		d := inspectCreateFolderHeader(raw, int(handles))
		a, b, c, x, y := d.Fields()
		if a == "malformed" {
			if b != "none" || c != "none" || x != "none" || y != "none" {
				t.Fatal("partial output")
			}
			return
		}
		if a != "header_only" || (b != "generic" && b != "search" && b != "other") || (c != "unicode" && c != "string8") || (x != "true" && x != "false") || (y != "zero" && y != "nonzero") {
			t.Fatal("vocabulary escaped")
		}
		copyRaw := bytes.Clone(raw[:5])
		copyRaw = append(copyRaw, []byte("different private payload")...)
		if inspectCreateFolderHeader(copyRaw, int(handles)) != d {
			t.Fatal("inspected variable strings")
		}
	})
}
