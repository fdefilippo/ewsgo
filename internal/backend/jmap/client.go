// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package jmap adapts a personal JMAP Mail account to the gateway's mailbox model.
package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"ewsgo/internal/core"
)

const (
	coreCapability       = "urn:ietf:params:jmap:core"
	mailCapability       = "urn:ietf:params:jmap:mail"
	submissionCapability = "urn:ietf:params:jmap:submission"
	maxJSONBytes         = 4 << 20
)

var (
	errProtocol = errors.New("invalid JMAP server response")
	errBackend  = errors.New("JMAP request failed")
	nativeID    = regexp.MustCompile(`^[A-Za-z0-9_-]{1,255}$`)
	addressID   = regexp.MustCompile(`^[A-Za-z0-9_-][A-Za-z0-9_.+-]*@[A-Za-z0-9][A-Za-z0-9.-]*$`)
)

// Some servers advertise address-shaped account and identity IDs. Keep the
// compatibility limited to these identifiers. Email and Mailbox IDs remain
// RFC 8620 Ids, including where they are composed into a placement key.
func validPrincipalID(id string) bool {
	return len(id) <= 255 && (nativeID.MatchString(id) || addressID.MatchString(id))
}

func hasCapability(capabilities map[string]json.RawMessage, name string) bool {
	return bytes.HasPrefix(bytes.TrimSpace(capabilities[name]), []byte("{"))
}

type Client struct {
	endpoint *url.URL
	http     *http.Client
}

// New connects to an HTTPS JMAP Session URL. HTTP testing and private PKIs
// may supply their own trusted transport. The default uses system trust roots.
func New(sessionURL string, transport http.RoundTripper) (*Client, error) {
	endpoint, err := parseHTTPS(sessionURL)
	if err != nil {
		return nil, err
	}
	return &Client{
		endpoint: endpoint,
		http: &http.Client{
			Transport: transport,
			Timeout:   20 * time.Second,
			CheckRedirect: func(r *http.Request, via []*http.Request) error {
				if len(via) > 4 || r.Method != http.MethodGet || via[0].Method != http.MethodGet ||
					!sameOrigin(endpoint, r.URL) {
					return http.ErrUseLastResponse
				}
				return nil
			},
		},
	}, nil
}

func parseHTTPS(raw string) (*url.URL, error) {
	u, err := url.Parse(raw)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.Fragment != "" {
		return nil, core.ErrInvalid
	}
	return u, nil
}

func sameOrigin(want, actual *url.URL) bool {
	return actual.Scheme == "https" && actual.User == nil && actual.Fragment == "" &&
		strings.EqualFold(want.Hostname(), actual.Hostname()) && httpsPort(want) == httpsPort(actual)
}

func httpsPort(u *url.URL) string {
	if u.Port() == "" {
		return "443"
	}
	return u.Port()
}

type credentials struct{ user, password string }

type discovery struct {
	Username        string            `json:"username"`
	APIURL          string            `json:"apiUrl"`
	DownloadURL     string            `json:"downloadUrl"`
	UploadURL       string            `json:"uploadUrl"`
	PrimaryAccounts map[string]string `json:"primaryAccounts"`
	Accounts        map[string]struct {
		IsPersonal   bool                       `json:"isPersonal"`
		IsReadOnly   bool                       `json:"isReadOnly"`
		Capabilities map[string]json.RawMessage `json:"accountCapabilities"`
	} `json:"accounts"`
	Capabilities map[string]json.RawMessage `json:"capabilities"`
}

type session struct {
	client       *Client
	auth         credentials
	identity     core.Identity
	account      string
	discoveryURL *url.URL
	api          *url.URL
	download     string
	getLimit     int
	upload       *url.URL
	maxUpload    int64
}

func (c *Client) Authenticate(ctx context.Context, username, password string) (core.Session, error) {
	auth := credentials{username, password}
	data, discoveredAt, err := c.fetch(ctx, auth, http.MethodGet, c.endpoint, nil, "application/json", "", maxJSONBytes)
	if err != nil {
		return nil, err
	}
	var info discovery
	if json.Unmarshal(data, &info) != nil || info.Username == "" || len(info.Username) > 512 {
		return nil, errProtocol
	}
	accountID := info.PrimaryAccounts[mailCapability]
	account := info.Accounts[accountID]
	if !validPrincipalID(accountID) || !account.IsPersonal || !hasCapability(account.Capabilities, mailCapability) {
		return nil, core.ErrAccessDenied
	}
	var limits struct {
		MaxObjectsInGet int   `json:"maxObjectsInGet"`
		MaxSizeUpload   int64 `json:"maxSizeUpload"`
	}
	if !hasCapability(info.Capabilities, mailCapability) || json.Unmarshal(info.Capabilities[coreCapability], &limits) != nil ||
		limits.MaxObjectsInGet < 1 {
		return nil, errProtocol
	}
	api, err := c.resolve(discoveredAt, info.APIURL)
	if err != nil {
		return nil, errProtocol
	}
	s := &session{
		client: c, auth: auth, account: accountID, api: api, discoveryURL: discoveredAt,
		download: info.DownloadURL, getLimit: min(limits.MaxObjectsInGet, 128),
		identity: core.Identity{Scope: core.Scope{Backend: "jmap", Account: accountID + ":" + info.Username}, Name: info.Username},
	}
	if _, err := s.blobURL("validation", "message.eml", "message/rfc822"); err != nil {
		return nil, err
	}
	if !account.IsReadOnly && limits.MaxSizeUpload > 0 &&
		info.PrimaryAccounts[submissionCapability] == accountID &&
		hasCapability(info.Capabilities, submissionCapability) && hasCapability(account.Capabilities, submissionCapability) &&
		strings.Contains(info.UploadURL, "{accountId}") {
		s.upload, err = c.resolve(discoveredAt, strings.ReplaceAll(info.UploadURL, "{accountId}", url.QueryEscape(accountID)))
		if err != nil {
			return nil, err
		}
		s.maxUpload = limits.MaxSizeUpload
	}
	return s, nil
}

func (s *session) Identity() core.Identity { return s.identity }
func (s *session) Mailbox() core.Mailbox {
	if s.upload != nil {
		return &submissionMailbox{s}
	}
	return s
}
func (s *session) Close(context.Context) error {
	s.auth = credentials{}
	return nil
}

func (c *Client) request(ctx context.Context, auth credentials, method string, endpoint *url.URL, body []byte, accept string, maxBytes int) ([]byte, error) {
	data, _, err := c.fetch(ctx, auth, method, endpoint, body, accept, "application/json", maxBytes)
	return data, err
}

func (c *Client) fetch(ctx context.Context, auth credentials, method string, endpoint *url.URL, body []byte, accept, contentType string, maxBytes int) ([]byte, *url.URL, error) {
	request, err := http.NewRequestWithContext(ctx, method, endpoint.String(), bytes.NewReader(body))
	if err != nil {
		return nil, nil, errProtocol
	}
	request.SetBasicAuth(auth.user, auth.password)
	request.Header.Set("Accept", accept)
	if body != nil {
		request.Header.Set("Content-Type", contentType)
	}
	response, err := c.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		return nil, nil, errBackend
	}
	defer response.Body.Close()
	switch response.StatusCode {
	case http.StatusOK, http.StatusCreated:
	case http.StatusUnauthorized:
		return nil, nil, core.ErrAuthentication
	case http.StatusForbidden:
		return nil, nil, core.ErrAccessDenied
	case http.StatusRequestEntityTooLarge:
		return nil, nil, core.ErrLimit
	default:
		return nil, nil, errBackend
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, int64(maxBytes)+1))
	if err != nil {
		return nil, nil, errBackend
	}
	if len(data) > maxBytes {
		return nil, nil, core.ErrLimit
	}
	return data, response.Request.URL, nil
}

func (c *Client) resolve(base *url.URL, raw string) (*url.URL, error) {
	ref, err := url.Parse(raw)
	if err != nil || raw == "" || strings.ContainsAny(raw, "{}") {
		return nil, errProtocol
	}
	target := base.ResolveReference(ref)
	if !sameOrigin(c.endpoint, target) {
		return nil, errProtocol
	}
	return target, nil
}

func (s *session) call(ctx context.Context, method string, args map[string]any, result any) error {
	return s.callUsing(ctx, method, args, result, false)
}

func (s *session) callUsing(ctx context.Context, method string, args map[string]any, result any, submission bool) (err error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: method, Result: core.ActivityOutcome(err), Duration: time.Since(started)})
	}()
	methods, err := s.invoke(ctx, method, args, submission)
	if err != nil {
		return err
	}
	if len(methods) != 1 {
		return errProtocol
	}
	return methods[0].decode(method, result)
}

type invocation struct {
	name string
	args json.RawMessage
}

func (s *session) invoke(ctx context.Context, method string, args map[string]any, submission bool) ([]invocation, error) {
	args["accountId"] = s.account
	using := []string{coreCapability, mailCapability}
	if submission {
		using = append(using, submissionCapability)
	}
	payload, err := json.Marshal(struct {
		Using       []string `json:"using"`
		MethodCalls [][3]any `json:"methodCalls"`
	}{using, [][3]any{{method, args, "0"}}})
	if err != nil {
		return nil, errProtocol
	}
	data, err := s.client.request(ctx, s.auth, http.MethodPost, s.api, payload, "application/json", maxJSONBytes)
	if err != nil {
		return nil, err
	}
	var response struct {
		Methods [][]json.RawMessage `json:"methodResponses"`
	}
	if json.Unmarshal(data, &response) != nil || len(response.Methods) == 0 || len(response.Methods) > 2 {
		return nil, errProtocol
	}
	methods := make([]invocation, len(response.Methods))
	for i, fields := range response.Methods {
		var name, tag string
		if len(fields) != 3 || json.Unmarshal(fields[0], &name) != nil || json.Unmarshal(fields[2], &tag) != nil || tag != "0" {
			return nil, errProtocol
		}
		methods[i] = invocation{name, fields[1]}
	}
	return methods, nil
}

func (i invocation) decode(method string, result any) error {
	if i.name == "error" {
		var fault struct {
			Type string `json:"type"`
		}
		if json.Unmarshal(i.args, &fault) != nil {
			return errProtocol
		}
		switch fault.Type {
		case "forbidden", "accountNotFound", "accountNotSupportedByMethod":
			return core.ErrAccessDenied
		case "cannotCalculateChanges":
			return core.ErrResync
		case "requestTooLarge", "tooManyObjects":
			return core.ErrLimit
		default:
			return errBackend
		}
	}
	if i.name != method || json.Unmarshal(i.args, result) != nil {
		return errProtocol
	}
	return nil
}

func (s *session) blobURL(blob, name, mediaType string) (*url.URL, error) {
	for _, name := range []string{"{accountId}", "{blobId}", "{name}", "{type}"} {
		if !strings.Contains(s.download, name) {
			return nil, errProtocol
		}
	}
	return s.client.resolve(s.discoveryURL, strings.NewReplacer(
		"{accountId}", url.QueryEscape(s.account), "{blobId}", blob,
		"{name}", url.PathEscape(name), "{type}", url.QueryEscape(mediaType),
	).Replace(s.download))
}
