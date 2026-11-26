// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/json"
	"errors"
	"io"
	"mime"
	"net/http"
	"net/url"
	"strings"

	"ewsgo/internal/core"
)

// Spike is an opt-in diagnostic transport, not a compatible EMSMDB service.
// It proves bounded decoding and authenticated native access, then explicitly
// refuses the unimplemented MAPI session/ROP semantics with HTTP 501.
func Spike(w http.ResponseWriter, r *http.Request, session core.Session) {
	spike(w, r, session, "")
}

// MailboxSpike binds routing and Connect DN to the authenticated scope. It
// still returns 501: no successful session or ROP execution is fabricated.
func (n Namespace) MailboxSpike(w http.ResponseWriter, r *http.Request, session core.Session) {
	identity, ok := n.route(w, r, session)
	if ok {
		spike(w, r, session, identity.LegacyDN)
	}
}

func (n Namespace) route(w http.ResponseWriter, r *http.Request, session core.Session) (MailboxIdentity, bool) {
	identity, err := n.Mailbox(session.Identity())
	if err != nil {
		http.Error(w, "backend identity unavailable", http.StatusBadGateway)
		return MailboxIdentity{}, false
	}
	query, err := url.ParseQuery(r.URL.RawQuery)
	if err != nil || len(query) != 1 || len(query["MailboxId"]) != 1 {
		http.Error(w, "one mailbox route required", http.StatusBadRequest)
		return MailboxIdentity{}, false
	}
	if !strings.EqualFold(query.Get("MailboxId"), identity.RoutingID) {
		http.Error(w, "mailbox denied", http.StatusForbidden)
		return MailboxIdentity{}, false
	}
	return identity, true
}

func spike(w http.ResponseWriter, r *http.Request, session core.Session, expectedDN string) {
	w.Header().Set("X-EWSGo-Profile", "technical-spike")
	data, ok := readSpikeRequest(w, r)
	if !ok {
		return
	}
	stage := r.Header.Get("X-RequestType")
	var err error
	switch stage {
	case "Connect":
		var connect Connect
		connect, err = ParseConnect(data)
		if err == nil && expectedDN != "" && !strings.EqualFold(connect.UserDN, expectedDN) {
			http.Error(w, "mailbox DN denied", http.StatusForbidden)
			return
		}
	case "Execute":
		var execute Execute
		execute, err = ParseExecute(data)
		if err == nil {
			var rop ROPBuffer
			rop, err = ParseROP(execute.ROP)
			if err == nil {
				_, err = rop.DecodeOperations()
			}
		}
	default:
		err = ErrUnsupported
	}
	if errors.Is(err, ErrMalformed) {
		http.Error(w, "malformed MAPI body", 400)
		return
	}
	if err != nil && !errors.Is(err, ErrUnsupported) {
		http.Error(w, "invalid MAPI body", 400)
		return
	}
	// A protocol DN never selects a Zimbra account. Scope came from authentication.
	root, backendErr := session.Mailbox().Get(r.Context(), session.Mailbox().Root())
	if backendErr != nil || root.Kind != core.Folder {
		http.Error(w, "backend unavailable", 502)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(struct {
		Profile                 string `json:"profile"`
		NativeMailboxAccessible bool   `json:"nativeMailboxAccessible"`
		MAPIMailboxOpened       bool   `json:"mapiMailboxOpened"`
		SessionContextCreated   bool   `json:"sessionContextCreated"`
		Error                   string `json:"error"`
	}{"technical-spike", true, false, false, "MAPI session context, ROP execution and NSPI are not implemented"})
}

// DirectorySpike records the first bounded NSPI request without claiming to
// decode a Bind or open a directory. Authentication is provided by BasicTLS.
func (n Namespace) DirectorySpike(w http.ResponseWriter, r *http.Request, session core.Session) {
	w.Header().Set("X-EWSGo-Profile", "nspi-directory-spike")
	if _, ok := n.route(w, r, session); !ok {
		return
	}
	if _, ok := readSpikeRequest(w, r); !ok {
		return
	}
	root, err := session.Mailbox().Get(r.Context(), session.Mailbox().Root())
	if err != nil || root.Kind != core.Folder {
		http.Error(w, "backend unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusNotImplemented)
	_ = json.NewEncoder(w).Encode(struct {
		Profile                 string `json:"profile"`
		NativeMailboxAccessible bool   `json:"nativeMailboxAccessible"`
		DirectoryOpened         bool   `json:"directoryOpened"`
		SessionContextCreated   bool   `json:"sessionContextCreated"`
	}{"nspi-directory-spike", true, false, false})
}

func readSpikeRequest(w http.ResponseWriter, r *http.Request) ([]byte, bool) {
	if r.Method != "POST" {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(405)
		return nil, false
	}
	content, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || content != "application/mapi-http" {
		http.Error(w, "MAPI content type required", 415)
		return nil, false
	}
	if r.Header.Get("X-RequestId") == "" {
		http.Error(w, "request ID required", 400)
		return nil, false
	}
	data, err := io.ReadAll(http.MaxBytesReader(w, r.Body, MaxBody))
	if err != nil {
		http.Error(w, "request limit", 413)
		return nil, false
	}
	return data, true
}
