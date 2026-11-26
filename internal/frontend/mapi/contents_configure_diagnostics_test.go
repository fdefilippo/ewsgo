// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestContentsConfigureFailurePreservesFirstFailureAcrossBatch(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	c := fullContentsConfig()
	fai := c
	fai.flags &^= 0x20
	// A valid Configure before the failure must not hide it. A second failed
	// Configure and a later successful one must not replace the first failure.
	ops := contentsConfigureOperation(c, nil)
	ops = append(ops, 1, 4, 2)
	ops = append(ops, contentsConfigureOperation(fai, filterWire(t, "06 01 03004523 08000000"))...)
	ops = append(ops, contentsConfigureOperation(fai, filterWire(t, "06 01 03003412 08000000"))...)
	ops = append(ops, contentsConfigureOperation(c, nil)...)
	body, counts := f.run(ops)
	_, _, _, reasons, _, _ := counts.synchronizationOptions.Fields()
	want := []string{"configure_restriction_binding", "configure_not_supported", "binding_other_fixed_property"}
	if counts.errors != 2 || !reflect.DeepEqual(reasons, want) || counts.unsupported != "" || len(body) != 24 {
		t.Fatal("first handler failure not preserved", reasons)
	}
	if binary.LittleEndian.Uint32(body[2:]) != 0 || binary.LittleEndian.Uint32(body[8:]) != NotSupported || binary.LittleEndian.Uint32(body[14:]) != NotSupported || binary.LittleEndian.Uint32(body[20:]) != 0 {
		t.Fatal("ROP results changed")
	}
	if box.sourceCalls != 0 {
		t.Fatal("diagnostic caused native capture")
	}
	object, code := f.state.mailbox.object(f.handles[2], 4)
	if code != 0 || object.download == nil {
		t.Fatal("later success lost its handle")
	}
}

func TestContentsConfigureFailureStagesAndWireCode(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	c := contentsConfigure{frame: fullContentsConfig()}
	op := Operation{ID: 0x70, Logon: 4, Input: 1, Output: 2, Contents: &c, Configure: &c.frame}
	check := func(op Operation, handles []uint32, stage contentsConfigureStage, code uint32) {
		t.Helper()
		wire, d := configureContents(context.Background(), f.session, f.state.mailbox, op, handles)
		if d.stage != stage || d.code != code || binary.LittleEndian.Uint32(wire[2:]) != code {
			t.Fatal("wrong Configure stage/code", d)
		}
	}
	missing := append([]uint32(nil), f.handles...)
	missing[1] = 0xffffffff
	check(op, missing, contentsInputStage, nullObject)
	logon := op
	logon.Input = 0
	check(logon, f.handles, contentsObjectStage, NotSupported)
	c.frame.flags = 0
	check(op, f.handles, contentsOptionsStage, NotSupported)
	c.frame = fullContentsConfig()
	c.frame.tags = []uint32{0x8001001f}
	check(op, f.handles, contentsSelectionStage, NotSupported)
	c.frame = fullContentsConfig()
	_, diagnostic := configureContents(context.Background(), f.session, f.state.mailbox, op, f.handles)
	if diagnostic != (contentsConfigureFailure{}) {
		t.Fatal("success reported as failure")
	}
	calendar, _ := newCalendarFixture(t)
	calendar.transport.EnableContentsDownloadSpike()
	calendar.handles = append(calendar.handles, 0xffffffff)
	entry := calendarEntry(t, calendar)
	resolved, _ := calendar.run(conversionOperation(0x44, entry[22:]))
	calendar.run(folderOperation(binary.LittleEndian.Uint64(resolved[6:]), 4, 0, 1))
	wire, d := configureContents(context.Background(), calendar.session, calendar.state.mailbox, op, calendar.handles)
	if d.stage != contentsStateStage || d.code != NotSupported || binary.LittleEndian.Uint32(wire[2:]) != NotSupported {
		t.Fatal("Calendar Normal diagnostic wrong")
	}
	if !reflect.DeepEqual(d.labels(), []string{"configure_state", "configure_not_supported", "configure_normal_metadata_only"}) {
		t.Fatal("metadata-only cause not distinguished", d.labels())
	}
	calendar.box.state.Owner = false
	wire, d = configureContents(context.Background(), calendar.session, calendar.state.mailbox, op, calendar.handles)
	if d.metadataOnly || binary.LittleEndian.Uint32(wire[2:]) != accessDenied {
		t.Fatal("denied access misclassified as metadata policy")
	}
	calendar.state.mailbox.close()
}

func TestContentsConfigureFailureTransportExplicitGate(t *testing.T) {
	transport, session, _, cookies := storeFixture(t)
	transport.EnableHierarchyDownloadSpike()
	transport.EnableContentsDownloadSpike()
	identity, _ := transport.namespace.Mailbox(session.Identity())
	body, cookies := storeExecute(t, transport, session, cookies, logonOperation(identity.LegacyDN, 0, 0), []uint32{0xffffffff, 0xffffffff, 0xffffffff}, 4096)
	response := storeResponse(t, body)
	fid := binary.LittleEndian.Uint64(response.Operations[7+int(Inbox)*8:])
	body, cookies = storeExecute(t, transport, session, cookies, folderOperation(fid, 0, 0, 1), response.Handles, 4096)
	opened := storeResponse(t, body)
	c := fullContentsConfig()
	c.flags &^= 0x20
	op := contentsConfigureOperation(c, filterWire(t, "06 01 03004523 08000000"))
	op[1] = 0
	wire, _ := (ROPBuffer{Operations: op, Handles: opened.Handles}).Marshal()
	request, _ := (Execute{ROP: wire, MaxOutput: 4096}).Marshal()
	var first []byte
	for _, enabled := range []bool{false, true} {
		transport.synchronizationOptionsTrace = enabled
		r := httptest.NewRequest("POST", "https://gateway.invalid/mapi/emsmdb/?MailboxId="+identity.RoutingID, bytes.NewReader(request))
		r.Header.Set("Content-Type", "application/mapi-http")
		r.Header.Set("X-RequestId", "synthetic-request:1")
		r.Header.Set("X-RequestType", "Execute")
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		w := &syncOptionsRecorder{ResponseRecorder: httptest.NewRecorder()}
		transport.Mailbox(w, r, session)
		cookies = w.Result().Cookies()
		got := requireTransportCode(t, w.ResponseRecorder, 0)
		if first == nil {
			first = got
		} else if !bytes.Equal(first, got) {
			t.Fatal("diagnostic changed response")
		}
		if w.calls != boolInt(enabled) {
			t.Fatal("diagnostic escaped gate")
		}
		if enabled {
			_, _, _, reasons, _, _ := w.diagnostic.Fields()
			if !reflect.DeepEqual(reasons, []string{"configure_restriction_binding", "configure_not_supported", "binding_other_fixed_property"}) {
				t.Fatal("wrong handler reason", reasons)
			}
		}
	}
}

func FuzzContentsConfigureFailureVocabulary(f *testing.F) {
	f.Add(byte(contentsRestrictionStage), uint32(NotSupported), byte(1), uint32(0x0e070003))
	f.Fuzz(func(t *testing.T, stage byte, code uint32, binding byte, tag uint32) {
		d := contentsConfigureFailure{stage: contentsConfigureStage(stage), code: code, binding: binding}
		labels := d.labels()
		if len(labels) > 3 {
			t.Fatal("unbounded labels")
		}
		for _, s := range labels {
			if len(s) > 40 || strings.ContainsAny(s, "0123456789") || (!strings.HasPrefix(s, "configure_") && !strings.HasPrefix(s, "binding_")) {
				t.Fatal("private raw data exposed")
			}
		}
		for _, format := range []string{"%v", "%+v", "%#v"} {
			if fmt.Sprintf(format, d) != "contentsConfigureFailure{redacted}" {
				t.Fatal("unredacted diagnostic")
			}
		}
		if c := contentsBindingClass(tag); c < 1 || c > 4 {
			t.Fatal("unbounded binding class")
		}
	})
}
