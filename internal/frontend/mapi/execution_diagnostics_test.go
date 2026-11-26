// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"slices"
	"strings"
	"testing"

	"ewsgo/internal/core"
)

func TestExecutionDiagnosticPreflightAndActualOperations(t *testing.T) {
	f := newOSTFixture(t)
	op := folderPropertiesOperation(4, 1, 1, 0x67480014)
	off, _ := f.run(op)
	f.transport.EnableSynchronizationOptionsTrace()
	on, c := f.run(op)
	stage, ops, first, buffers, n := c.execution.Fields()
	if !bytes.Equal(off, on) || stage != "executed" || !slices.Equal(ops, []string{"get_properties"}) || first != "none" || n != "0" || !slices.Equal(buffers, []string{"none"}) {
		t.Fatal("trace changed behavior or execution evidence")
	}
	for _, tc := range []struct {
		op      []byte
		maximum uint32
		stage   string
	}{
		{append(bytes.Clone(op), 0xef, 4, 0), 65535, "batch_rejected"},
		{op[:len(op)-1], 65535, "batch_rejected"},
		{append([]byte{1, 4, 1}, op...), 20, "output_too_small"},
	} {
		_, c := f.execute(tc.op, tc.maximum)
		stage, ops, first, buffers, n := c.execution.Fields()
		if stage != tc.stage || !slices.Equal(ops, []string{"none"}) || first != "none" || n != "0" || !slices.Equal(buffers, []string{"none"}) {
			t.Fatal("unexecuted operations reported", stage, ops, first, buffers, n)
		}
	}
	_, c = f.run(op)
	if c.errors != 0 {
		t.Fatal("preflight released handle")
	}
	f.box.getErr = core.ErrAccessDenied
	_, c = f.run(op)
	_, _, first, _, _ = c.execution.Fields()
	if first != "get_properties:access_denied" {
		t.Fatal("first actual handler failure", first)
	}
	f.box.getErr = nil
	wire, _ := (ROPBuffer{Operations: op, Handles: f.handles}).Marshal()
	for _, request := range []Execute{{ROP: []byte{1}, MaxOutput: 4096}, {ROP: wire, Flags: 8, MaxOutput: 4096}, {ROP: wire, MaxOutput: 0x40001}} {
		_, c = f.transport.executeStore(context.Background(), f.session, f.state, f.identity, request)
		stage, ops, _, _, _ = c.execution.Fields()
		if !slices.Equal(ops, []string{"none"}) || stage == "executed" {
			t.Fatal("outer rejection executed")
		}
	}
}

type executionActivity []core.Activity

func (*executionActivity) Authenticated(string) {}
func (a *executionActivity) Record(event core.Activity) {
	*a = append(*a, event)
}

func TestExecutionDiagnosticsNameRecipientAndAttachmentOperations(t *testing.T) {
	f := newOSTFixture(t)
	f.transport.EnableContentsDownloadSpike()
	requests := []struct {
		wire []byte
		name string
	}{
		{[]byte{0x0d, 4, 0, 0, 0, 0, 0}, "remove_all_recipients"},
		{[]byte{0x0e, 4, 0, 0, 0, 0, 0}, "modify_recipients"},
		{[]byte{0x0f, 4, 0, 0, 0, 0, 0, 0, 0}, "read_recipients"},
		{[]byte{0x52, 4, 0}, "get_valid_attachments"},
	}
	var operations, expectedResponse []byte
	var names []string
	for _, request := range requests {
		operations = append(operations, request.wire...)
		expectedResponse = append(expectedResponse, ropError(request.wire[0], 0, nullObject)...)
		names = append(names, request.name)
	}
	wire, err := (ROPBuffer{Operations: operations, Handles: []uint32{0xffffffff}}).Marshal()
	if err != nil {
		t.Fatal(err)
	}
	var events executionActivity
	ctx := core.WithActivityObserver(context.Background(), &events)
	body, counts := f.transport.executeStore(ctx, f.session, f.state, f.identity, Execute{ROP: wire, MaxOutput: 65535})
	response := storeResponse(t, body)
	if counts.errors != len(requests) || !bytes.Equal(response.Operations, expectedResponse) {
		t.Fatal("unexpected responses to requests with null handles")
	}
	_, operationsLogged, _, _, _ := counts.execution.Fields()
	if !slices.Equal(operationsLogged, names) || len(events) != len(requests) {
		t.Fatalf("wrong batch activity: %v, %v", operationsLogged, events)
	}
	for i, event := range events {
		if event.Operation != names[i] || !IsExecutionOperationLabel(event.Operation) ||
			event.ROPID != requests[i].wire[0] || event.ROPReturnValue != nullObject || event.Result != core.ActivityRejected {
			t.Fatalf("wrong operation activity: %+v", event)
		}
	}
}

func TestExecutionGetBufferClassifiesActualHierarchyContentsAndState(t *testing.T) {
	f := newHierarchyDownloadFixture(t)
	f.transport.EnableSynchronizationOptionsTrace()
	seen := map[string]bool{}
	for range 10000 {
		response, c := f.run(hierarchyBufferOperation(4, 2, 23))
		_, ops, first, outcomes, n := c.execution.Fields()
		if !slices.Equal(ops, []string{"get_buffer"}) || first != "none" || n != "1" || len(outcomes) != 1 {
			t.Fatal("buffer evidence")
		}
		seen[outcomes[0]] = true
		if binary.LittleEndian.Uint16(response[6:]) == 3 {
			break
		}
	}
	if !seen["hierarchy:partial"] || !seen["hierarchy:done"] {
		t.Fatal("missing hierarchy progress", seen)
	}
	f.run([]byte{0x82, 4, 2, 3})
	_, c := f.run(hierarchyBufferOperation(4, 3, 4096))
	_, _, _, outcomes, _ := c.execution.Fields()
	if !slices.Equal(outcomes, []string{"state:done"}) {
		t.Fatal("state source confused with hierarchy", outcomes)
	}
	// Invalid source handle is a real handler error, without a guessed scope.
	_, c = f.run(hierarchyBufferOperation(4, 0, 4096))
	_, _, first, outcomes, _ := c.execution.Fields()
	if first != "get_buffer:not_supported" || !slices.Equal(outcomes, []string{"unknown:error"}) {
		t.Fatal("invalid source result", first, outcomes)
	}
	g, _ := newContentsDownloadFixture(t)
	g.transport.EnableSynchronizationOptionsTrace()
	configureContentsTest(t, g, fullContentsConfig())
	_, c = g.run(hierarchyBufferOperation(4, 2, 1))
	_, _, _, outcomes, _ = c.execution.Fields()
	if !slices.Equal(outcomes, []string{"contents:no_room"}) {
		t.Fatal("no-room not distinguished", outcomes)
	}
	seen = map[string]bool{}
	for range 10000 {
		response, c := g.run(hierarchyBufferOperation(4, 2, 73))
		_, _, _, outcomes, _ := c.execution.Fields()
		seen[outcomes[0]] = true
		if binary.LittleEndian.Uint16(response[6:]) == 3 {
			break
		}
	}
	if !seen["contents:partial"] || !seen["contents:done"] {
		t.Fatal("missing contents progress", seen)
	}
	g.run([]byte{0x82, 4, 2, 3})
	_, c = g.run(hierarchyBufferOperation(4, 3, 4096))
	_, _, _, outcomes, _ = c.execution.Fields()
	if !slices.Equal(outcomes, []string{"state:done"}) {
		t.Fatal("Contents state source confused with message stream", outcomes)
	}
}

func TestExecutionDiagnosticBoundsPrivacyAndFirstFailure(t *testing.T) {
	d := ExecutionDiagnostic{enabled: true}
	d.at(6)
	d.record(2, ropError(2, 1, accessDenied))
	d.record(0xfe, ropError(0xfe, 1, NotSupported))
	r := append(ropError(0x4e, 1, 0), 3, 0, 0, 0, 0, 0, 0, 0, 0)
	for range 2000 {
		d.recordBuffer(1, r)
	}
	_, ops, first, outcomes, n := d.Fields()
	if n != "1024" || first != "open_folder:access_denied" || !slices.Equal(ops, []string{"open_folder", "logon"}) || !slices.Equal(outcomes, []string{"hierarchy:done"}) {
		t.Fatal("bounds/first failure", n, first, outcomes)
	}
	d.recordBuffer(1, []byte("PRIVATE-response-payload"))
	if strings.Contains(fmt.Sprintf("%v %+v %#v", d, d, d), "PRIVATE") {
		t.Fatal("formatter retained bytes")
	}
	bad := ExecutionDiagnostic{stage: 255, firstErrorOp: 255, firstErrorKind: 255, buffers: 0xffffffff, bufferCalls: 65535}
	stage, _, first, _, n := bad.Fields()
	if stage != "other" || first != "other" || n != "other" {
		t.Fatal("invalid enum escaped")
	}
	var off ExecutionDiagnostic
	off.at(6)
	off.record(2, ropError(2, 1, 0))
	off.recordBuffer(1, r)
	if off != (ExecutionDiagnostic{}) {
		t.Fatal("default gate")
	}
}

func TestExecutionDiagnosticDistinguishesMissingAndReleasedHandles(t *testing.T) {
	for _, tc := range []struct {
		name string
		code uint32
		kind string
	}{
		{"missing", nullObject, "null_object"},
		{"released", invalidObject, "invalid_object"},
		{"unclassified", 0xdeadbeef, "other"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := ExecutionDiagnostic{enabled: true}
			d.record(7, append(ropError(7, 3, tc.code), []byte("PRIVATE-payload")...))
			d.record(0x0c, ropError(0x0c, 1, NotSupported))
			_, _, first, _, _ := d.Fields()
			if first != "get_properties:"+tc.kind || strings.Contains(fmt.Sprintf("%v %+v %#v", d, d, d), "PRIVATE") {
				t.Fatal("handle error classification/first-error/redaction")
			}
		})
	}
	// Actual object lookup, not a fabricated status: a fresh missing slot and
	// an explicitly released draft yield distinct errors on a ChangeKey request.
	for _, closed := range []bool{false, true} {
		f, _, _ := newFAIExecuteFixture(t)
		t.Cleanup(f.state.mailbox.close)
		code, kind := uint32(nullObject), "null_object"
		if closed {
			faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
			faiRunOK(t, f, faiSetOperation(3, faiCreateProperties()))
			faiRunOK(t, f, []byte{0x0c, 4, 1, 3, 0})
			faiRunOK(t, f, []byte{1, 4, 3})
			code, kind = invalidObject, "invalid_object"
		}
		op := folderPropertiesOperation(4, 3, 1, 0x65e20102)
		off, _ := f.run(op)
		f.transport.EnableSynchronizationOptionsTrace()
		on, counts := f.run(op)
		_, _, first, _, _ := counts.execution.Fields()
		if !bytes.Equal(off, on) || !bytes.Equal(on, ropError(7, 3, code)) || first != "get_properties:"+kind {
			t.Fatal("trace changed missing/released ChangeKey read")
		}
	}
}

type executionRecorder struct {
	*httptest.ResponseRecorder
	d     ExecutionDiagnostic
	calls int
}

func (w *executionRecorder) RecordExecutionDiagnostic(d ExecutionDiagnostic) { w.d = d; w.calls++ }

func TestExecutionDiagnosticTransportExplicitGate(t *testing.T) {
	f := newOSTFixture(t)
	// Use a real Connect for valid context/sequence cookies.
	connect, _ := (Connect{UserDN: f.identity.LegacyDN, CodePage: 1252}).Marshal()
	w := transportRequest(t, f.transport, f.session, "Connect", connect, nil)
	var cookies []*http.Cookie = w.Result().Cookies()
	for _, enabled := range []bool{false, true} {
		if enabled {
			f.transport.EnableSynchronizationOptionsTrace()
		}
		rop, _ := (ROPBuffer{Operations: []byte{1, 4, 0}, Handles: []uint32{0xffffffff}}).Marshal()
		body, _ := (Execute{ROP: rop, MaxOutput: 4096}).Marshal()
		r := httptest.NewRequest("POST", "https://gateway.invalid/mapi/emsmdb/?MailboxId="+f.identity.RoutingID, bytes.NewReader(body))
		r.Header.Set("Content-Type", "application/mapi-http")
		r.Header.Set("X-RequestType", "Execute")
		r.Header.Set("X-RequestId", "synthetic-request:1")
		for _, cookie := range cookies {
			r.AddCookie(cookie)
		}
		recorder := &executionRecorder{ResponseRecorder: httptest.NewRecorder()}
		f.transport.Mailbox(recorder, r, f.session)
		requireTransportCode(t, recorder.ResponseRecorder, 0)
		cookies = recorder.Result().Cookies()
		stage, ops, _, _, _ := recorder.d.Fields()
		if enabled && (recorder.calls != 1 || stage != "executed" || !reflect.DeepEqual(ops, []string{"release"})) || !enabled && recorder.calls != 0 {
			t.Fatal("transport gate/evidence", recorder.calls, stage, ops)
		}
	}
}
