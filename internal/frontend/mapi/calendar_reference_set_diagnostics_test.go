// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"ewsgo/internal/core"
	"fmt"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestCalendarSetOutcomeClassificationPrivacyAndBounds(t *testing.T) {
	var d CalendarReferenceSetDiagnostic
	d.record([]byte{0xa, 1, 0, 0, 0, 0, 0, 0})
	d.record(append([]byte{0xa, 1, 0, 0, 0, 0, 1, 0}, []byte("PRIVATE-payload-must-not-survive")...))
	d.record(ropError(0xa, 1, accessDenied))
	outcomes, requests := d.Fields()
	if requests != "3" || !reflect.DeepEqual(outcomes, []string{"confirmed", "property_problem", "operation_error"}) || strings.Contains(fmt.Sprintf("%v %#v", d, d), "PRIVATE") {
		t.Fatal("classification/privacy")
	}
	for range 2000 {
		d.record([]byte{0xa, 1, 0, 0, 0, 0, 0, 0})
	}
	_, requests = d.Fields()
	if requests != "1024" {
		t.Fatal("unbounded count")
	}
	for _, invalid := range []CalendarReferenceSetDiagnostic{{Requests: -1}, {Requests: 1025}, {Requests: 1, Confirmed: 2}, {Requests: 1, PropertyProblems: -1}, {Requests: 1, OperationErrors: 2}, {Requests: 1}, {Confirmed: 1}, {Requests: 1, Confirmed: 1, PropertyProblems: 1}} {
		outcomes, n := invalid.Fields()
		if n != "other" || !reflect.DeepEqual(outcomes, []string{"other"}) {
			t.Fatal("invalid public counters escaped")
		}
	}
}

func TestCalendarSetOutcomesObserveActualHandlersOnly(t *testing.T) {
	f, b := newCalendarFixture(t)
	entry := calendarEntry(t, f)
	operation := calendarReferenceSetOperation(4, 1, entry)
	off, c := f.execute(operation, 65535)
	if c.calendarSet.Requests != 0 {
		t.Fatal("diagnostic default enabled")
	}
	f.transport.EnableCalendarReferenceSetTrace()
	on, c := f.execute(operation, 65535)
	outcomes, n := c.calendarSet.Fields()
	if !bytes.Equal(off, on) || n != "1" || !reflect.DeepEqual(outcomes, []string{"confirmed"}) {
		t.Fatal("trace changed accepted behavior")
	}
	// Distinguish PropertyProblem from ROP error, even though its ReturnValue is0.
	foreign := bytes.Clone(entry)
	foreign[22] ^= 1
	row, c := f.run(calendarReferenceSetOperation(4, 1, foreign))
	outcomes, n = c.calendarSet.Fields()
	if len(row) != 18 || c.errors != 0 || n != "1" || !reflect.DeepEqual(outcomes, []string{"property_problem"}) {
		t.Fatal("PropertyProblem hidden by ROP success")
	}
	b.calendarErr = core.ErrAccessDenied
	_, c = f.run(operation)
	outcomes, n = c.calendarSet.Fields()
	if c.errors != 1 || n != "1" || !reflect.DeepEqual(outcomes, []string{"operation_error"}) {
		t.Fatal("operation failure not classified")
	}
	b.calendarErr = nil
	prefix := append([]byte{1, 4, 1}, folderOperation(f.fids[Inbox], 4, 0, 1)...)
	mixed := append(append(prefix, operation...), calendarReferenceSetOperation(4, 1, foreign)...)
	_, c = f.run(mixed)
	outcomes, n = c.calendarSet.Fields()
	if n != "2" || !reflect.DeepEqual(outcomes, []string{"confirmed", "property_problem"}) {
		t.Fatal("prefix/mixed results not aggregated")
	}
	for _, request := range [][]byte{append(operation, 0xef, 4, 0), operation[:len(operation)-1]} {
		_, c = f.execute(request, 65535)
		if c.calendarSet.Requests != 0 {
			t.Fatal("unexecuted handler counted")
		}
	}
	_, c = f.execute(operation, 35)
	if c.calendarSet.Requests != 0 {
		t.Fatal("small output counted")
	}
}

type calendarOutcomeRecorder struct {
	*httptest.ResponseRecorder
	calls int
	d     CalendarReferenceSetDiagnostic
}

func (w *calendarOutcomeRecorder) RecordCalendarReferenceSetDiagnostic(d CalendarReferenceSetDiagnostic) {
	w.calls++
	w.d = d
}

func TestCalendarSetOutcomeTransportGateAndPrefix(t *testing.T) {
	f, _ := newCalendarFixture(t)
	entry := calendarEntry(t, f)
	connect, _ := (Connect{UserDN: f.identity.LegacyDN, CodePage: 1252}).Marshal()
	connected := transportRequest(t, f.transport, f.session, "Connect", connect, nil)
	requireTransportCode(t, connected, 0)
	cookies := connected.Result().Cookies()
	body, cookies := storeExecute(t, f.transport, f.session, cookies, logonOperation(f.identity.LegacyDN, 4, 0), []uint32{0xffffffff, 0xffffffff}, 4096)
	logon := storeResponse(t, body)
	body, cookies = storeExecute(t, f.transport, f.session, cookies, folderOperation(f.fids[Inbox], 4, 0, 1), logon.Handles, 4096)
	handles := storeResponse(t, body).Handles
	call := func(valid bool, ops []byte) *calendarOutcomeRecorder {
		rop, _ := (ROPBuffer{Operations: ops, Handles: handles}).Marshal()
		data, _ := (Execute{ROP: rop, MaxOutput: 4096}).Marshal()
		r := httptest.NewRequest("POST", "https://gateway.invalid/mapi/emsmdb/?MailboxId="+f.identity.RoutingID, bytes.NewReader(data))
		r.Header.Set("Content-Type", "application/mapi-http")
		r.Header.Set("X-RequestType", "Execute")
		r.Header.Set("X-RequestId", "synthetic-request:1")
		if valid {
			for _, cookie := range cookies {
				r.AddCookie(cookie)
			}
		}
		w := &calendarOutcomeRecorder{ResponseRecorder: httptest.NewRecorder()}
		f.transport.Mailbox(w, r, f.session)
		if valid {
			cookies = w.Result().Cookies()
			raw := requireTransportCode(t, w.ResponseRecorder, 0)
			if len(raw) >= 8 && binary.LittleEndian.Uint32(raw[4:]) == 0 {
				handles = storeResponse(t, raw).Handles
			}
		}
		return w
	}
	op := calendarReferenceSetOperation(4, 1, entry)
	if w := call(true, op); w.calls != 0 {
		t.Fatal("default recorder gate")
	}
	f.transport.EnableCalendarReferenceSetTrace()
	if w := call(false, op); w.calls != 0 {
		t.Fatal("invalid context recorded")
	}
	prefix := append([]byte{1, 4, 1}, folderOperation(f.fids[Inbox], 4, 0, 1)...)
	w := call(true, append(prefix, op...))
	outcomes, n := w.d.Fields()
	if w.calls != 1 || n != "1" || !reflect.DeepEqual(outcomes, []string{"confirmed"}) {
		t.Fatal("post-prefix handler result not forwarded")
	}
	w = call(true, append(op, 0xef, 4, 0))
	outcomes, n = w.d.Fields()
	if w.calls != 1 || n != "0" || !reflect.DeepEqual(outcomes, []string{"none"}) {
		t.Fatal("preflight rejection produced handler outcome")
	}
}
