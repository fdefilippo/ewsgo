// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package autodiscover

import (
	"encoding/xml"
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"ewsgo/internal/core"
	"ewsgo/internal/frontend/mapi"
)

// Profile advertises an experimental MAPI/HTTP route, not a working Exchange
// mailbox. Both advertised endpoints must be mounted by the caller. URLs are
// configuration, never derived from untrusted Host or forwarding headers.
type Profile struct {
	origin    string
	namespace mapi.Namespace
}

func NewProfile(origin string, namespace mapi.Namespace) (*Profile, error) {
	u, err := url.Parse(origin)
	if err != nil || u.Scheme != "https" || u.Host == "" || u.Hostname() == "" || u.User != nil || u.Opaque != "" || u.RawQuery != "" || u.ForceQuery || u.Fragment != "" || u.RawFragment != "" || u.RawPath != "" || u.Path != "" && u.Path != "/" || namespace.DeploymentID() == "" {
		return nil, errors.New("profile requires an HTTPS origin and deployment UUID")
	}
	for _, c := range u.Host {
		if c <= 32 || c >= 127 {
			return nil, errors.New("public origin host must be ASCII")
		}
	}
	if port := u.Port(); port != "" {
		n, err := strconv.Atoi(port)
		if err != nil || n < 1 || n > 65535 {
			return nil, errors.New("invalid public origin port")
		}
	}
	return &Profile{origin: "https://" + u.Host, namespace: namespace}, nil
}

func (p *Profile) Serve(w http.ResponseWriter, r *http.Request, session core.Session) {
	w.Header().Set("X-EWSGo-Profile", "autodiscover-mapi-profile-spike")
	request, ok := readRequest(w, r)
	if !ok {
		return
	}
	identity := session.Identity()
	mailbox, err := p.namespace.Mailbox(identity)
	if err != nil {
		http.Error(w, "backend identity unavailable", http.StatusBadGateway)
		return
	}
	if request.Email != "" && !strings.EqualFold(request.Email, identity.Name) || request.LegacyDN != "" && !strings.EqualFold(request.LegacyDN, mailbox.LegacyDN) {
		http.Error(w, "mailbox denied", http.StatusForbidden)
		return
	}
	if !supportsMAPIHTTPv1(r.Header.Values("X-MapiHttpCapability")) {
		http.Error(w, "positive MAPI/HTTP capability required; legacy profile unavailable", http.StatusNotImplemented)
		return
	}
	root, err := session.Mailbox().Get(r.Context(), session.Mailbox().Root())
	if err != nil || root.Kind != core.Folder {
		http.Error(w, "backend unavailable", http.StatusBadGateway)
		return
	}
	endpoint := func(path string) endpointURLs {
		u := p.origin + path + "?" + url.Values{"MailboxId": {mailbox.RoutingID}}.Encode()
		return endpointURLs{Internal: u, External: u}
	}
	response := profileResponse{XMLNS: "http://schemas.microsoft.com/exchange/autodiscover/responseschema/2006"}
	response.Response.XMLNS = responseNS
	response.Response.User = profileUser{identity.Name, mailbox.LegacyDN, identity.Name, p.namespace.DeploymentID()}
	response.Response.Account = profileAccount{Type: "email", Action: "settings", Protocol: profileProtocol{
		Type: "mapiHttp", Version: "1", MailStore: endpoint("/mapi/emsmdb/"), AddressBook: endpoint("/mapi/nspi/"),
	}}
	body, err := xml.Marshal(response)
	if err != nil {
		http.Error(w, "profile unavailable", http.StatusBadGateway)
		return
	}
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	_, _ = w.Write(append([]byte(xml.Header), body...))
}

// MS-OXDSCLI 3.2.5.1 selects the highest supported version <= the client's
// capability. We support only v1, so every positive decimal integer suffices.
// Check digits directly so a future version cannot overflow a machine integer.
func supportsMAPIHTTPv1(values []string) bool {
	if len(values) != 1 {
		return false
	}
	value := strings.TrimPrefix(strings.Trim(values[0], " \t"), "+")
	positive := false
	for _, c := range value {
		if c < '0' || c > '9' {
			return false
		}
		positive = positive || c != '0'
	}
	return positive
}

type profileResponse struct {
	XMLName  xml.Name `xml:"Autodiscover"`
	XMLNS    string   `xml:"xmlns,attr"`
	Response struct {
		XMLNS   string         `xml:"xmlns,attr"`
		User    profileUser    `xml:"User"`
		Account profileAccount `xml:"Account"`
	} `xml:"Response"`
}
type profileUser struct {
	DisplayName string `xml:"DisplayName"`
	LegacyDN    string `xml:"LegacyDN"`
	SMTP        string `xml:"AutoDiscoverSMTPAddress"`
	Deployment  string `xml:"DeploymentId"`
}
type profileAccount struct {
	Type     string          `xml:"AccountType"`
	Action   string          `xml:"Action"`
	Protocol profileProtocol `xml:"Protocol"`
}
type profileProtocol struct {
	Type        string       `xml:"Type,attr"`
	Version     string       `xml:"Version,attr"`
	MailStore   endpointURLs `xml:"MailStore"`
	AddressBook endpointURLs `xml:"AddressBook"`
}
type endpointURLs struct {
	Internal string `xml:"InternalUrl"`
	External string `xml:"ExternalUrl"`
}
