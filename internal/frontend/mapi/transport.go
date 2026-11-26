// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
)

// SessionTransport is an opt-in MAPI/HTTP context experiment. A successful
// Connect establishes transport state. Mailbox ROPs require EnableRAMStore.
// Callers MUST wrap each entry point with authenticated TLS (core.BasicTLS).
type SessionTransport struct {
	namespace                   Namespace
	contexts                    *contexts
	wakeups                     core.MailboxWakeups
	notificationSource          NotificationSource
	nativeDirectory             bool
	stores                      *ramStores
	publicStore                 *publicStore
	unknownPropertyTrace        bool
	setPropertyTrace            bool
	setListTrace                bool
	synchronizationTrace        bool
	synchronizationOptionsTrace bool
	createFolderTrace           bool
	createFolderSpike           bool
	ostPropertySpike            bool
	folderMetadataSpike         bool
	calendarMetadataSpike       bool
	contactsMetadataSpike       bool
	tasksMetadataSpike          bool
	draftsReferenceSpike        bool
	calendarReferenceSetTrace   bool
	hierarchyDownloadSpike      bool
	hierarchyImport             bool
	folderAggregates            bool
	contentsDownloadSpike       bool
	contentsTableSpike          bool
	faiCollectorSpike           bool
	mailDraftCreator            mailDraftCreator
	mailDraftSender             mailDraftSender
	readStateImport             bool
}

func NewSessionTransport(namespace Namespace) *SessionTransport {
	return &SessionTransport{namespace: namespace, contexts: newContexts()}
}

func (t *SessionTransport) Mailbox(w http.ResponseWriter, r *http.Request, session core.Session) {
	started := time.Now()
	w.Header().Set("X-EWSGo-Profile", "mapi-session-spike")
	if t.stores != nil {
		w.Header().Set("X-EWSGo-Profile", "mapi-store-spike")
	}
	if r.URL.Path != "/mapi/emsmdb/" {
		transportResponse(w, r, started, 3, nil, nil)
		return
	}
	data, code := readTransportRequest(w, r)
	if code != 0 {
		transportResponse(w, r, started, code, nil, nil)
		return
	}
	identity, code := t.route(r, session)
	if code != 0 {
		transportResponse(w, r, started, code, nil, nil)
		return
	}
	op := r.Header.Get("X-RequestType")
	var connect Connect
	var execute Execute
	switch op {
	case "Connect":
		var err error
		connect, err = ParseConnect(data)
		if err != nil {
			code = 12
		} else if !strings.EqualFold(connect.UserDN, identity.LegacyDN) {
			code = 11
		}
	case "Execute":
		var err error
		execute, err = ParseExecute(data)
		if err != nil {
			code = 12
		}
	case "NotificationWait":
		if t.wakeups == nil {
			code = 5
		} else if parseNotificationWait(data) != nil {
			code = 12
		}
	case "Disconnect":
		if parseDisconnect(data) != nil {
			code = 12
		}
	case "PING":
		if len(data) != 0 {
			code = 12
		}
	default:
		code = 5
	}
	if code != 0 {
		transportResponse(w, r, started, code, nil, nil)
		return
	}

	// No administrative access is available through this self-mailbox profile.
	// 0x8000 is the documented public-folder/client-version connection bit.
	if op == "Connect" && connect.Flags & ^uint32(0x8000) != 0 {
		errCode := NotSupported
		if connect.Flags&1 != 0 {
			errCode = 0x80070005
		}
		body, _ := ConnectResponse(errCode, "", "")
		transportResponse(w, r, started, 0, nil, body)
		return
	}

	id, sequence, code := requestCookies(r, op == "Connect" || op == "NotificationWait", op == "PING")
	if code != 0 {
		transportResponse(w, r, started, code, nil, nil)
		return
	}
	if op == "NotificationWait" {
		if id == "" {
			transportResponse(w, r, started, 13, nil, nil)
		} else {
			t.notificationWait(w, r, started, session, id)
		}
		return
	}
	if op == "PING" && id == "" {
		transportResponse(w, r, started, 0, nil, nil)
		return
	}
	var state *contextState
	if op == "Connect" {
		// Native authorization precedes context creation. The protocol DN and
		// query never choose a different Zimbra account.
		box := session.Mailbox()
		root, err := box.Get(r.Context(), box.Root())
		if err != nil || root.Kind != core.Folder {
			http.Error(w, "backend unavailable", http.StatusBadGateway)
			return
		}
		state, code = t.contexts.create(session.Identity().Scope, "/mapi/emsmdb/", id, connect)
	} else {
		state, code = t.contexts.acquire(session.Identity().Scope, "/mapi/emsmdb/", id, sequence)
	}
	if code != 0 {
		transportResponse(w, r, started, code, nil, nil)
		return
	}
	revoke := op == "Disconnect"
	defer func() { t.contexts.finish(state, revoke) }()
	var body []byte
	switch op {
	case "Connect":
		if t.stores != nil {
			state.mailbox = newMailboxObjects(t.stores)
		}
		body, _ = ConnectResponse(0, "", session.Identity().Name)
	case "Execute":
		// Only closed labels describing framing/the first ROP leave the codec.
		// This is not validation or execution of the full ROP batch.
		if recorder, ok := w.(interface {
			RecordROPDiagnostic(string, string, []string)
		}); ok {
			diagnostic := InspectROP(execute.ROP)
			recorder.RecordROPDiagnostic(diagnostic.Layout, diagnostic.First, diagnostic.Flags)
		}
		if t.stores != nil {
			if recorder, ok := w.(interface{ RecordSetPropertyDiagnostic(SetPropertyDiagnostic) }); ok && t.setPropertyTrace {
				recorder.RecordSetPropertyDiagnostic(InspectSetProperty(execute.ROP))
			}
			var counts storeResults
			executeStarted := time.Now()
			body, counts = t.executeStore(r.Context(), session, state, identity, execute)
			stage, ops, firstError, _, _ := counts.execution.Fields()
			outcome := core.ActivitySuccess
			if stage != "executed" || counts.errors > 0 || firstError != "none" {
				outcome = core.ActivityRejected
			}
			core.RecordActivity(r.Context(), core.Activity{Layer: "rop", Operation: "Execute", Result: outcome, Duration: time.Since(executeStarted), Operations: ops})
			if recorder, ok := w.(interface{ RecordExecutionDiagnostic(ExecutionDiagnostic) }); ok && t.synchronizationOptionsTrace {
				recorder.RecordExecutionDiagnostic(counts.execution)
			}
			if recorder, ok := w.(interface{ RecordSetListDiagnostic(SetListDiagnostic) }); ok && t.setListTrace {
				diagnostic := counts.setList
				if diagnostic.state == 0 {
					diagnostic = InspectSetList(execute.ROP)
				}
				recorder.RecordSetListDiagnostic(diagnostic)
			}
			if recorder, ok := w.(interface {
				RecordSynchronizationDiagnostic(SynchronizationDiagnostic)
			}); ok && t.synchronizationTrace {
				recorder.RecordSynchronizationDiagnostic(counts.synchronization)
			}
			if recorder, ok := w.(interface {
				RecordSynchronizationOptionsDiagnostic(SynchronizationOptionsDiagnostic)
			}); ok && t.synchronizationOptionsTrace {
				recorder.RecordSynchronizationOptionsDiagnostic(counts.synchronizationOptions)
			}
			if recorder, ok := w.(interface{ RecordCreateFolderDiagnostic(CreateFolderDiagnostic) }); ok && t.createFolderTrace {
				recorder.RecordCreateFolderDiagnostic(counts.createFolder)
			}
			if recorder, ok := w.(interface {
				RecordCalendarReferenceSetDiagnostic(CalendarReferenceSetDiagnostic)
			}); ok && t.calendarReferenceSetTrace {
				recorder.RecordCalendarReferenceSetDiagnostic(counts.calendarSet)
			}
			if recorder, ok := w.(interface{ RecordStoreResult(int, int, int) }); ok {
				recorder.RecordStoreResult(counts.logons, counts.folders, counts.errors)
			}
			if recorder, ok := w.(interface{ RecordUnsupportedROP(string) }); ok && counts.unsupported != "" {
				recorder.RecordUnsupportedROP(counts.unsupported)
			}
			if recorder, ok := w.(interface{ RecordStoreProperties(StorePropertyDiagnostic) }); ok && counts.properties.Requests > 0 {
				recorder.RecordStoreProperties(counts.properties)
			}
			if recorder, ok := w.(interface{ RecordContentsBindingDiagnostic(StorePropertyDiagnostic) }); ok && t.unknownPropertyTrace && t.synchronizationOptionsTrace && counts.bindingProperties {
				recorder.RecordContentsBindingDiagnostic(counts.properties)
			}
		} else {
			body, _ = ExecuteResponse(NotSupported, nil, nil, execute.MaxOutput)
		}
	case "Disconnect":
		body = append32(append32(append32(nil, 0), 0), 0)
	}
	if !transportResponse(w, r, started, 0, state, body) {
		revoke = true
	}
}

func (t *SessionTransport) route(r *http.Request, session core.Session) (MailboxIdentity, uint32) {
	identity, err := t.namespace.Mailbox(session.Identity())
	if err != nil {
		return MailboxIdentity{}, 1
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) != 1 || len(query["MailboxId"]) != 1 {
		return MailboxIdentity{}, 4
	}
	if !strings.EqualFold(query.Get("MailboxId"), identity.RoutingID) {
		return MailboxIdentity{}, 11
	}
	return identity, 0
}

func parseDisconnect(data []byte) error {
	if len(data) > MaxBody {
		return ErrMalformed
	}
	r := reader{data: data}
	r.blob(MaxAuxiliary)
	return r.done()
}

func readTransportRequest(w http.ResponseWriter, r *http.Request) ([]byte, uint32) {
	if r.Method != http.MethodPost {
		return nil, 2
	}
	if r.ContentLength < 0 || len(r.TransferEncoding) != 0 {
		return nil, 7
	}
	for _, header := range []string{"Content-Type", "X-RequestType", "X-RequestId"} {
		values := r.Header.Values(header)
		if len(values) == 0 || values[0] == "" {
			return nil, 7
		}
		if len(values) != 1 || !printableHeader(values[0]) {
			return nil, 4
		}
	}
	content, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || content != "application/mapi-http" {
		return nil, 4
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		var limit *http.MaxBytesError
		if errors.As(err, &limit) {
			return nil, 9
		}
		return nil, 12
	}
	return data, 0
}

func printableHeader(value string) bool {
	if len(value) == 0 || len(value) > 256 {
		return false
	}
	for _, b := range []byte(value) {
		if b < 32 || b > 126 {
			return false
		}
	}
	return true
}

func requestCookies(r *http.Request, reconnect, optional bool) (id, sequence string, code uint32) {
	contextCount, sequenceCount := 0, 0
	for _, cookie := range r.Cookies() {
		switch cookie.Name {
		case contextCookie:
			id = cookie.Value
			contextCount++
		case sequenceCookie:
			sequence = cookie.Value
			sequenceCount++
		}
	}
	if contextCount == 0 {
		if reconnect || optional {
			return "", "", 0
		}
		return "", "", 13
	}
	if contextCount != 1 || !validCookie(id) {
		return "", "", 6
	}
	if !reconnect {
		if sequenceCount == 0 {
			return "", "", 13
		}
		if sequenceCount != 1 || !validCookie(sequence) {
			return "", "", 15
		}
	}
	return id, sequence, 0
}

// Short responses are buffered and length-delimited.
func transportResponse(w http.ResponseWriter, r *http.Request, started time.Time, code uint32, state *contextState, payload []byte) bool {
	outcome := core.ActivitySuccess
	if code != 0 {
		outcome = core.ActivityRejected
	} else if len(payload) >= 8 {
		status, result := binary.LittleEndian.Uint32(payload), binary.LittleEndian.Uint32(payload[4:])
		if status != 0 || (result != 0 && !(r.Header.Get("X-RequestType") == "Unbind" && result == 1)) {
			outcome = core.ActivityRejected
		}
	}
	core.RecordActivity(r.Context(), core.Activity{Layer: "mapi", Operation: r.Header.Get("X-RequestType"), Result: outcome, Duration: time.Since(started)})
	if code == 0 && len(payload) >= 8 {
		if recorder, ok := w.(interface{ RecordMAPIResult(uint32, uint32) }); ok {
			recorder.RecordMAPIResult(binary.LittleEndian.Uint32(payload), binary.LittleEndian.Uint32(payload[4:]))
		}
	}
	h := transportHeaders(w, r, code)
	var body []byte
	if code != 0 {
		h.Set("Content-Type", "text/html")
		body = []byte("<html><body>MAPI transport request rejected.</body></html>")
	} else {
		h.Set("Content-Type", "application/mapi-http")
		h.Set("X-PendingPeriod", "15000")
		if state != nil {
			h.Set("X-ExpirationInfo", strconv.FormatInt(sessionTTL.Milliseconds(), 10))
			for _, cookie := range []*http.Cookie{
				{Name: contextCookie, Value: state.id}, {Name: sequenceCookie, Value: state.sequence},
			} {
				cookie.Path, cookie.Secure, cookie.HttpOnly = state.endpoint, true, true
				http.SetCookie(w, cookie)
			}
		}
		var meta bytes.Buffer
		meta.WriteString("PROCESSING\r\n")
		writeTransportDone(&meta, started, code)
		body = append(meta.Bytes(), payload...)
	}
	h.Set("Content-Length", strconv.Itoa(len(body)))
	w.WriteHeader(http.StatusOK)
	written, err := w.Write(body)
	return err == nil && written == len(body)
}

func transportHeaders(w http.ResponseWriter, r *http.Request, code uint32) http.Header {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-ResponseCode", strconv.FormatUint(uint64(code), 10))
	for _, name := range []string{"X-RequestType", "X-RequestId"} {
		value := r.Header.Get(name)
		if printableHeader(value) {
			h.Set(name, value)
		}
	}
	// MS-OXCMAPIHTTP requires this protocol-shaped identifier. It is a format
	// marker, not the version of an installed Exchange server or a claim of
	// Exchange feature parity. X-EWSGo-Profile identifies the experiment.
	h.Set("X-ServerApplication", "Exchange/15.00.0000.000")
	h.Set("X-ExpirationInfo", "0")
	return h
}

func writeTransportDone(w io.Writer, started time.Time, code uint32) error {
	_, err := fmt.Fprintf(w, "DONE\r\nX-ResponseCode: %d\r\nX-ElapsedTime: %d\r\nX-StartTime: %s\r\n\r\n", code, time.Since(started).Milliseconds(), started.UTC().Format(time.RFC3339))
	return err
}

// Configure before serving; service validates all laboratory prerequisites.
func (t *SessionTransport) EnableSynchronizationOptionsTrace() { t.synchronizationOptionsTrace = true }
