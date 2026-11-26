// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package autodiscover contains opt-in laboratory POX handlers.
package autodiscover

import (
	"bytes"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"mime"
	"net/http"
	"strings"

	"ewsgo/internal/core"
)

const requestNS = "http://schemas.microsoft.com/exchange/autodiscover/outlook/requestschema/2006"
const responseNS = "http://schemas.microsoft.com/exchange/autodiscover/outlook/responseschema/2006a"

var errRequest = errors.New("invalid discovery request")

// Spike must be wrapped in core.BasicTLS. It deliberately cannot create an
// Outlook profile or advertise mailbox/directory endpoints that do not exist.
func Spike(w http.ResponseWriter, r *http.Request, session core.Session) {
	w.Header().Set("X-EWSGo-Profile", "autodiscover-authentication-spike")
	request, ok := readRequest(w, r)
	if !ok {
		return
	}
	if request.LegacyDN != "" {
		http.Error(w, "legacy DN unavailable in authentication probe", http.StatusBadRequest)
		return
	}
	if !strings.EqualFold(request.Email, session.Identity().Name) {
		http.Error(w, "mailbox denied", http.StatusForbidden)
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
		RequestedMailboxMatches bool   `json:"requestedMailboxMatches"`
		OutlookProfileCreated   bool   `json:"outlookProfileCreated"`
	}{"autodiscover-authentication-spike", true, true, false})
}

type poxRequest struct{ Email, LegacyDN string }

func readRequest(w http.ResponseWriter, r *http.Request) (poxRequest, bool) {
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		w.WriteHeader(http.StatusMethodNotAllowed)
		return poxRequest{}, false
	}
	content, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
	if err != nil || content != "text/xml" && content != "application/xml" {
		http.Error(w, "XML required", http.StatusUnsupportedMediaType)
		return poxRequest{}, false
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<10))
	if err != nil {
		http.Error(w, "request limit", http.StatusRequestEntityTooLarge)
		return poxRequest{}, false
	}
	request, err := parsePOX(raw)
	if err != nil {
		http.Error(w, "invalid discovery request", http.StatusBadRequest)
		return poxRequest{}, false
	}
	return request, true
}

// POX accepts one self-mailbox selector, with no directory lookup or redirects.
func parsePOX(raw []byte) (poxRequest, error) {
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	depth, roots, requests, nodes := 0, 0, 0, 0
	namespace, field := "", ""
	values := map[string]string{}
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return poxRequest{}, errRequest
		}
		switch token := token.(type) {
		case xml.StartElement:
			nodes++
			if nodes > 5 {
				return poxRequest{}, errRequest
			}
			switch depth {
			case 0:
				roots++
				if roots != 1 || token.Name.Local != "Autodiscover" || token.Name.Space != requestNS && token.Name.Space != strings.Replace(requestNS, "http:", "https:", 1) {
					return poxRequest{}, errRequest
				}
				namespace = token.Name.Space
			case 1:
				requests++
				if requests != 1 || token.Name.Local != "Request" || token.Name.Space != namespace {
					return poxRequest{}, errRequest
				}
			case 2:
				field = token.Name.Local
				if token.Name.Space != namespace || field != "EMailAddress" && field != "LegacyDN" && field != "AcceptableResponseSchema" {
					return poxRequest{}, errRequest
				}
				if _, exists := values[field]; exists {
					return poxRequest{}, errRequest
				}
				values[field] = ""
			default:
				return poxRequest{}, errRequest
			}
			depth++
		case xml.EndElement:
			depth--
		case xml.CharData:
			if depth == 3 {
				values[field] += string(token)
			} else if strings.TrimSpace(string(token)) != "" {
				return poxRequest{}, errRequest
			}
		case xml.Directive:
			return poxRequest{}, errRequest
		case xml.ProcInst:
			if token.Target != "xml" {
				return poxRequest{}, errRequest
			}
		}
	}
	address := strings.TrimSpace(values["EMailAddress"])
	dn := strings.TrimSpace(values["LegacyDN"])
	schema := strings.TrimSpace(values["AcceptableResponseSchema"])
	_, hasEmail := values["EMailAddress"]
	_, hasDN := values["LegacyDN"]
	if depth != 0 || roots != 1 || requests != 1 || hasEmail == hasDN || address == "" && dn == "" || len(address) > 320 || len(dn) > 4096 || schema != responseNS && schema != strings.Replace(responseNS, "http:", "https:", 1) {
		return poxRequest{}, errRequest
	}
	return poxRequest{Email: address, LegacyDN: dn}, nil
}
