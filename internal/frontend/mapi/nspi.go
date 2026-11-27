// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"errors"
	"net/http"
	"time"

	"ewsgo/internal/core"
)

// Directory establishes an authenticated NSPI context and optionally resolves
// native GAL DNs, names and properties.
func (t *SessionTransport) Directory(w http.ResponseWriter, r *http.Request, session core.Session) {
	started := time.Now()
	w.Header().Set("X-EWSGo-Profile", "nspi-bind-spike")
	if r.URL.Path != "/mapi/nspi/" {
		transportResponse(w, r, started, 3, nil, nil)
		return
	}
	data, code := readTransportRequest(w, r)
	if code != 0 {
		transportResponse(w, r, started, code, nil, nil)
		return
	}
	if _, code = t.route(r, session); code != 0 {
		transportResponse(w, r, started, code, nil, nil)
		return
	}
	op := r.Header.Get("X-RequestType")
	var bind Bind
	var names DNToMId
	var props GetProps
	var resolve resolveNamesRequest
	var tooManyProperties bool
	switch op {
	case "Bind":
		var err error
		bind, err = ParseBind(data)
		if err != nil {
			code = 12
		}
	case "Unbind":
		if parseUnbind(data) != nil {
			code = 12
		}
	case "DNToMId":
		if !t.nativeDirectory {
			code = 5
		} else if v, err := ParseDNToMId(data); err != nil {
			code = 12
		} else {
			names = v
		}
	case "GetProps":
		if !t.nativeDirectory {
			code = 5
		} else {
			var err error
			props, err = ParseGetProps(data)
			tooManyProperties = errors.Is(err, ErrTooManyProperties)
			if err != nil && !tooManyProperties {
				code = 12
			}
		}
	case "ResolveNames":
		if !t.nativeDirectory {
			code = 5
		} else if v, err := parseResolveNames(data); err != nil {
			code = 12
		} else {
			resolve = v
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
	if op == "Bind" {
		if bind.Flags&0x20 != 0 {
			transportResponse(w, r, started, 8, nil, nil)
			return
		}
		var methodError uint32
		if bind.State == nil {
			methodError = 0x80070057
		} else if bind.State.CodePage != 1252 && bind.State.CodePage != 65001 {
			methodError = 0x8004011e
		}
		if methodError != 0 {
			transportResponse(w, r, started, 0, nil, BindResponse(methodError, [16]byte{}))
			return
		}
	}
	id, sequence, code := requestCookies(r, op == "Bind", op == "PING")
	if code != 0 {
		transportResponse(w, r, started, code, nil, nil)
		return
	}
	if op == "PING" && id == "" {
		transportResponse(w, r, started, 0, nil, nil)
		return
	}
	var state *contextState
	if op == "Bind" {
		box := session.Mailbox()
		root, err := box.Get(r.Context(), box.Root())
		if err != nil || root.Kind != core.Folder {
			http.Error(w, "backend unavailable", http.StatusBadGateway)
			return
		}
		state, code = t.contexts.create(session.Identity().Scope, "/mapi/nspi/", id, Connect{CodePage: bind.State.CodePage, SortLCID: bind.State.SortLocale, StringLCID: bind.State.TemplateLocale})
	} else {
		state, code = t.contexts.acquire(session.Identity().Scope, "/mapi/nspi/", id, sequence)
	}
	if code != 0 {
		if code == 1 && op == "Bind" {
			transportResponse(w, r, started, 0, nil, BindResponse(0x80040111, [16]byte{}))
		} else {
			transportResponse(w, r, started, code, nil, nil)
		}
		return
	}
	revoke := op == "Unbind"
	defer func() { t.contexts.finish(state, revoke) }()
	var body []byte
	switch op {
	case "Bind":
		guid := t.namespace.ServerGUID()
		if t.nativeDirectory {
			state.directory = newDirectoryState()
			guid = state.directory.guid
		}
		body = BindResponse(0, guid)
	case "DNToMId":
		body = t.directoryIDs(r.Context(), session, state, names)
		recordDirectoryResult(w, body)
	case "GetProps":
		recordPropertyRequest(w, props, tooManyProperties)
		if tooManyProperties {
			body = getPropsResponse(TableTooBig, state.codePage, nil)
		} else {
			var values, missing int
			body, values, missing = t.directoryProperties(r.Context(), session, state, props)
			recordPropertyResult(w, values, missing)
		}
	case "ResolveNames":
		var matched int
		body, matched = t.resolveDirectoryNames(r.Context(), session, state, resolve)
		if recorder, ok := w.(interface{ RecordDirectoryResult(int, int) }); ok {
			recorder.RecordDirectoryResult(len(resolve.names), matched)
		}
	case "Unbind":
		body = append32(append32(append32(nil, 0), 1), 0) // NSPI UnbindSuccess (MS-OXCDATA 2.5).
	}
	if !transportResponse(w, r, started, 0, state, body) {
		revoke = true
	}
}
