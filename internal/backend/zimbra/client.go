// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package zimbra provides the native, read-only SOAP/REST access spike.
// It is not yet a complete implementation of the core.Mailbox contract.
package zimbra

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"encoding/xml"
	"errors"
	"ewsgo/internal/core"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"net/url"
	"regexp"
	"strconv"
	"sync"
	"sync/atomic"
	"time"
	"unicode/utf8"
)

const (
	soapNS    = "http://www.w3.org/2003/05/soap-envelope"
	contextNS = "urn:zimbra"
	accountNS = "urn:zimbraAccount"
	mailNS    = "urn:zimbraMail"
)

var (
	ErrAuth      = errors.New("authentication rejected")
	ErrExpired   = errors.New("session expired or revoked")
	ErrDenied    = errors.New("mailbox access denied")
	ErrRouting   = errors.New("redirect requires an explicitly configured trusted route")
	ErrLimit     = errors.New("response exceeds configured limit")
	ErrProtocol  = errors.New("unexpected Zimbra protocol response")
	ErrTransport = errors.New("Zimbra transport failed")
)

// Fault contains only a validated machine code. Server messages, request URLs,
// tracebacks and XML bodies are deliberately excluded from errors and logs.
type Fault struct {
	Code   string
	Status int
}

func (f *Fault) Error() string { return "Zimbra: " + f.Code }
func (f *Fault) Unwrap() error {
	switch f.Code {
	case "account.AUTH_FAILED", "service.AUTH_REQUIRED":
		return ErrAuth
	case "service.AUTH_EXPIRED":
		return ErrExpired
	case "service.PERM_DENIED", "account.PERMISSION_DENIED":
		return ErrDenied
	default:
		return ErrProtocol
	}
}

type Config struct {
	Origin                     string
	Timeout                    time.Duration
	MaxSOAPBytes, MaxMIMEBytes int64
	// RootCAs extends trust configuration without disabling certificate checks.
	RootCAs *x509.CertPool
}
type Client struct {
	origin               string
	http, waitHTTP       *http.Client
	soapLimit, mimeLimit int64
}

func (*Client) String() string { return "Zimbra client [configuration redacted]" }
func New(cfg Config) (*Client, error) {
	u, err := url.Parse(cfg.Origin)
	if err != nil || u.Scheme != "https" || u.Hostname() == "" || u.User != nil || u.RawQuery != "" || u.Fragment != "" || (u.Path != "" && u.Path != "/") {
		return nil, errors.New("Zimbra origin must be an HTTPS origin without credentials, path or query")
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 20 * time.Second
	}
	if cfg.Timeout < 0 {
		return nil, errors.New("Zimbra timeout must be positive")
	}
	if cfg.MaxSOAPBytes == 0 {
		cfg.MaxSOAPBytes = 2 << 20
	}
	if cfg.MaxMIMEBytes == 0 {
		cfg.MaxMIMEBytes = 10 << 20
	}
	if cfg.MaxSOAPBytes < 1 || cfg.MaxSOAPBytes > 64<<20 || cfg.MaxMIMEBytes < 1 || cfg.MaxMIMEBytes > 1<<30 {
		return nil, errors.New("invalid Zimbra response limits")
	}
	transport := &http.Transport{
		Proxy:               http.ProxyFromEnvironment,
		DialContext:         (&net.Dialer{Timeout: cfg.Timeout, KeepAlive: 30 * time.Second}).DialContext,
		TLSClientConfig:     &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: cfg.RootCAs},
		TLSHandshakeTimeout: cfg.Timeout, ResponseHeaderTimeout: cfg.Timeout,
		IdleConnTimeout: 60 * time.Second, MaxIdleConns: 16, MaxIdleConnsPerHost: 4,
	}
	// The native WaitSet can block beyond the ordinary SOAP timeout.
	waitTransport := transport.Clone()
	waitTransport.ResponseHeaderTimeout = 40 * time.Second
	noRedirect := func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &Client{
		origin:    u.Scheme + "://" + u.Host,
		http:      &http.Client{Transport: transport, Timeout: cfg.Timeout, CheckRedirect: noRedirect},
		waitHTTP:  &http.Client{Transport: waitTransport, Timeout: 45 * time.Second, CheckRedirect: noRedirect},
		soapLimit: cfg.MaxSOAPBytes, mimeLimit: cfg.MaxMIMEBytes,
	}, nil
}
func (c *Client) Close() {
	c.http.CloseIdleConnections()
	c.waitHTTP.CloseIdleConnections()
}

type node struct {
	XMLName  xml.Name
	Attrs    []xml.Attr `xml:",any,attr"`
	Text     string     `xml:",chardata"`
	Children []node     `xml:",any"`
}

func element(ns, name, text string, children ...node) node {
	return node{XMLName: xml.Name{Space: ns, Local: name}, Text: text, Children: children}
}
func attr(n node, k, v string) node {
	n.Attrs = append(n.Attrs, xml.Attr{Name: xml.Name{Local: k}, Value: v})
	return n
}
func (n node) get(key string) string {
	for _, a := range n.Attrs {
		if a.Name.Space == "" && a.Name.Local == key {
			return a.Value
		}
	}
	return ""
}
func (n node) child(ns, name string) *node {
	for i := range n.Children {
		if n.Children[i].XMLName == (xml.Name{Space: ns, Local: name}) {
			return &n.Children[i]
		}
	}
	return nil
}
func (n node) find(ns, name string) *node {
	if n.XMLName == (xml.Name{Space: ns, Local: name}) {
		return &n
	}
	for _, ch := range n.Children {
		if v := ch.find(ns, name); v != nil {
			return v
		}
	}
	return nil
}

var safeCode = regexp.MustCompile(`^(service|account|mail)\.[A-Z_]+$`)

func parseXML(raw []byte) (node, error) {
	decoder := xml.NewDecoder(bytes.NewReader(raw))
	depth, roots, nodes := 0, 0, 0
	for {
		token, err := decoder.Token()
		if err == io.EOF {
			break
		}
		if err != nil {
			return node{}, ErrProtocol
		}
		switch t := token.(type) {
		case xml.StartElement:
			if depth == 0 {
				roots++
			}
			depth++
			nodes++
			if depth > 64 || nodes > 65536 {
				return node{}, ErrLimit
			}
		case xml.EndElement:
			depth--
		case xml.Directive:
			return node{}, ErrProtocol
		case xml.CharData:
			if depth == 0 && len(bytes.TrimSpace(t)) != 0 {
				return node{}, ErrProtocol
			}
		}
	}
	if roots != 1 || depth != 0 {
		return node{}, ErrProtocol
	}
	var n node
	if xml.Unmarshal(raw, &n) != nil {
		return node{}, ErrProtocol
	}
	return n, nil
}

func (c *Client) exchange(ctx context.Context, req *http.Request, max int64) ([]byte, int, error) {
	return exchangeHTTP(ctx, c.http, req, max)
}

func exchangeHTTP(ctx context.Context, client *http.Client, req *http.Request, max int64) ([]byte, int, error) {
	response, err := client.Do(req)
	if err != nil {
		if ctx.Err() != nil {
			return nil, 0, ctx.Err()
		}
		if e, ok := err.(net.Error); ok && e.Timeout() {
			return nil, 0, context.DeadlineExceeded
		}
		return nil, 0, ErrTransport
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 && response.StatusCode < 400 {
		return nil, response.StatusCode, ErrRouting
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, max+1))
	if err != nil {
		return nil, response.StatusCode, ErrTransport
	}
	if int64(len(data)) > max {
		return nil, response.StatusCode, ErrLimit
	}
	return data, response.StatusCode, nil
}
func (c *Client) call(ctx context.Context, ns, operation, token, mailbox string, request node, contextElements ...node) (node, error) {
	body, _, err := c.callWithResponseContext(ctx, ns, operation, token, mailbox, request, contextElements...)
	return body, err
}

func validXMLContent(n node) bool {
	if !validXMLText(n.Text) {
		return false
	}
	for _, attribute := range n.Attrs {
		if !validXMLText(attribute.Value) {
			return false
		}
	}
	for _, child := range n.Children {
		if !validXMLContent(child) {
			return false
		}
	}
	return true
}

func validXMLText(value string) bool {
	if !utf8.ValidString(value) {
		return false
	}
	for _, r := range value {
		if r < 0x20 && r != '\t' && r != '\n' && r != '\r' || r == 0xfffe || r == 0xffff {
			return false
		}
	}
	return true
}

func (c *Client) callWithResponseContext(ctx context.Context, ns, operation, token, mailbox string, request node, contextElements ...node) (out node, responseContext *node, callErr error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: operation, Result: core.ActivityOutcome(semanticError(callErr)), Duration: time.Since(started)})
	}()
	header := element(contextNS, "context", "", attr(element(contextNS, "userAgent", ""), "name", "EWSGo-native-spike"))
	if token != "" {
		header.Children = append(header.Children, element(contextNS, "authToken", token))
	}
	if mailbox != "" {
		header.Children = append(header.Children, attr(element(contextNS, "account", mailbox), "by", "name"))
	}
	header.Children = append(header.Children, contextElements...)
	envelope := element(soapNS, "Envelope", "", element(soapNS, "Header", "", header), element(soapNS, "Body", "", request))
	if !validXMLContent(envelope) {
		return node{}, nil, core.ErrInvalid
	}
	data, err := xml.Marshal(envelope)
	if err != nil {
		return node{}, nil, ErrProtocol
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.origin+"/service/soap", bytes.NewReader(data))
	if err != nil {
		return node{}, nil, ErrProtocol
	}
	req.Header.Set("Content-Type", "application/soap+xml; charset=utf-8")
	client := c.http
	if ns == mailNS && operation == "WaitSet" {
		client = c.waitHTTP
	}
	raw, status, err := exchangeHTTP(ctx, client, req, c.soapLimit)
	if err != nil {
		return node{}, nil, err
	}
	root, err := parseXML(raw)
	if err != nil {
		return node{}, nil, err
	}
	if root.XMLName != (xml.Name{Space: soapNS, Local: "Envelope"}) {
		return node{}, nil, ErrProtocol
	}
	body := root.child(soapNS, "Body")
	if body == nil {
		return node{}, nil, ErrProtocol
	}
	if fault := body.child(soapNS, "Fault"); fault != nil {
		code := "UNKNOWN"
		if value := fault.find(contextNS, "Code"); value != nil && (safeCode.MatchString(value.Text) || value.Text == "admin.NO_SUCH_WAITSET") {
			code = value.Text
		}
		return node{}, nil, &Fault{Code: code, Status: status}
	}
	if status != http.StatusOK || len(body.Children) != 1 {
		return node{}, nil, ErrProtocol
	}
	response := body.child(ns, operation+"Response")
	if response == nil {
		return node{}, nil, ErrProtocol
	}
	if header := root.child(soapNS, "Header"); header != nil {
		responseContext = header.child(contextNS, "context")
	}
	return *response, responseContext, nil
}

type Session struct {
	mu      sync.Mutex
	client  *Client
	token   string
	expires time.Time
	closed  bool
}

func (*Session) String() string { return "Zimbra session [redacted]" }
func (c *Client) Login(ctx context.Context, user, password string) (*Session, error) {
	if user == "" || password == "" {
		return nil, errors.New("Zimbra user and password are required")
	}
	request := element(accountNS, "AuthRequest", "", attr(element(accountNS, "account", user), "by", "name"), element(accountNS, "password", password))
	response, err := c.call(ctx, accountNS, "Auth", "", "", request)
	if err != nil {
		return nil, err
	}
	token := response.child(accountNS, "authToken")
	lifetime := response.child(accountNS, "lifetime")
	if token == nil || token.Text == "" || lifetime == nil {
		return nil, ErrProtocol
	}
	ms, err := strconv.ParseInt(lifetime.Text, 10, 64)
	if err != nil || ms <= 0 || ms > int64((365*24*time.Hour)/time.Millisecond) {
		return nil, ErrProtocol
	}
	return &Session{client: c, token: token.Text, expires: time.Now().Add(time.Duration(ms) * time.Millisecond)}, nil
}
func (s *Session) credential() (string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed || !time.Now().Before(s.expires) {
		return "", ErrExpired
	}
	return s.token, nil
}
func (s *Session) request(ctx context.Context, ns, op, mailbox string, n node, contextElements ...node) (node, error) {
	token, err := s.credential()
	if err != nil {
		return node{}, err
	}
	return s.client.call(ctx, ns, op, token, mailbox, n, contextElements...)
}

func (s *Session) requestMutation(ctx context.Context, ns, op, mailbox string, n node) (node, error) {
	token, err := s.credential()
	if err != nil {
		return node{}, errors.Join(core.ErrWriteNotDispatched, err)
	}
	if err := ctx.Err(); err != nil {
		return node{}, errors.Join(core.ErrWriteNotDispatched, err)
	}
	// The standard transport cannot write the SOAP request before GotConn.
	// Once a connection has been acquired, treat transport failure as ambiguous.
	if _, ok := s.client.http.Transport.(*http.Transport); !ok {
		return s.client.call(ctx, ns, op, token, mailbox, n)
	}
	var connected atomic.Bool
	trace := &httptrace.ClientTrace{GotConn: func(httptrace.GotConnInfo) { connected.Store(true) }}
	response, err := s.client.call(httptrace.WithClientTrace(ctx, trace), ns, op, token, mailbox, n)
	if err != nil && !connected.Load() {
		err = errors.Join(core.ErrWriteNotDispatched, err)
	}
	return response, err
}

// Validate asks Zimbra to validate this exact token, even after Logout. This is
// useful to verify revocation; ordinary mailbox operations reject a closed session locally.
func (s *Session) Validate(ctx context.Context) error {
	s.mu.Lock()
	token := s.token
	s.mu.Unlock()
	_, err := s.client.call(ctx, accountNS, "Auth", "", "", element(accountNS, "AuthRequest", "", element(accountNS, "authToken", token)))
	return err
}
func (s *Session) Logout(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closed {
		return nil
	}
	_, err := s.client.call(ctx, accountNS, "EndSession", s.token, "", attr(element(accountNS, "EndSessionRequest", ""), "logoff", "1"))
	if err == nil {
		s.closed = true
	}
	return err
}

type Info struct{ Version, AccountID, Name string }

func (s *Session) Info(ctx context.Context) (Info, error) {
	r, err := s.request(ctx, accountNS, "GetInfo", "", attr(element(accountNS, "GetInfoRequest", ""), "sections", "mbox"))
	if err != nil {
		return Info{}, err
	}
	v := r.get("version")
	if v == "" {
		if n := r.child(accountNS, "version"); n != nil {
			v = n.Text
		}
	}
	id, name := r.get("id"), r.get("name")
	if n := r.child(accountNS, "id"); n != nil {
		id = n.Text
	}
	if n := r.child(accountNS, "name"); n != nil {
		name = n.Text
	}
	return Info{Version: v, AccountID: id, Name: name}, nil
}

type Folder struct {
	ID, Parent, Name, View string
	MetadataRevision       *uint64
	ContentsSequence       *uint64
	ModifiedAt             *time.Time
	ItemCount, UnreadCount *uint64
}

func (Folder) String() string { return "Zimbra folder [private metadata redacted]" }

func (s *Session) Folders(ctx context.Context, mailbox string) ([]Folder, error) {
	return s.folderCatalog(ctx, mailbox, false)
}

func (s *Session) folderCatalog(ctx context.Context, mailbox string, ownOnly bool) ([]Folder, error) {
	return s.folderCatalogWithLinks(ctx, mailbox, ownOnly, nil)
}

func (s *Session) folderCatalogWithLinks(ctx context.Context, mailbox string, ownOnly bool, links *[]node) ([]Folder, error) {
	request := attr(element(mailNS, "GetFolderRequest", "", attr(element(mailNS, "folder", ""), "path", "/")), "depth", "-1")
	if ownOnly {
		request = attr(request, "tr", "0")
	}
	r, err := s.request(ctx, mailNS, "GetFolder", mailbox, request)
	if err != nil {
		return nil, err
	}
	var folders []Folder
	var walk func(node) error
	walk = func(n node) error {
		if ownOnly && n.XMLName == (xml.Name{Space: mailNS, Local: "link"}) {
			if links != nil {
				*links = append(*links, n)
			}
			return nil
		}
		if n.XMLName == (xml.Name{Space: mailNS, Local: "folder"}) {
			folder, err := folderFromNode(n)
			if err != nil {
				return err
			}
			// Full GetFolder serializes real Folder nodes with n, but omits u
			// when unread is zero (Zimbra ToXML NOTIFY_FIELDS). A node without
			// n can be a structural placeholder: its counts remain unknown.
			if folder.ItemCount != nil && folder.UnreadCount == nil {
				zero := uint64(0)
				folder.UnreadCount = &zero
			}
			folders = append(folders, folder)
		}
		for _, ch := range n.Children {
			if err := walk(ch); err != nil {
				return err
			}
		}
		return nil
	}
	if err := walk(r); err != nil {
		return nil, err
	}
	if len(folders) == 0 {
		return nil, ErrProtocol
	}
	return folders, nil
}

type Message struct {
	ID, Folder, Flags                  string
	Subject, Preview, InternetID, From string
	MetadataRevision, ContentRevision  uint64
	Size, Date                         int64
	MIMETypes                          []string
}

func (s *Session) Inbox(ctx context.Context) ([]Message, error) {
	request := attr(attr(attr(element(mailNS, "SearchRequest", "", element(mailNS, "query", "in:inbox")), "types", "message"), "limit", "1"), "sortBy", "dateDesc")
	r, err := s.request(ctx, mailNS, "Search", "", request)
	if err != nil {
		return nil, err
	}
	var result []Message
	for _, n := range r.Children {
		if n.XMLName == (xml.Name{Space: mailNS, Local: "m"}) {
			if n.get("id") == "" {
				return nil, ErrProtocol
			}
			result = append(result, Message{ID: n.get("id"), Folder: n.get("l"), Flags: n.get("f")})
		}
	}
	return result, nil
}
func (s *Session) Message(ctx context.Context, id string) (Message, error) {
	n, err := s.getMessageNode(ctx, id, "8192")
	if err != nil {
		return Message{}, err
	}
	m := messageFromNode(n)
	var walk func(node)
	walk = func(n node) {
		if n.XMLName == (xml.Name{Space: mailNS, Local: "mp"}) && n.get("ct") != "" {
			m.MIMETypes = append(m.MIMETypes, n.get("ct"))
		}
		for _, ch := range n.Children {
			walk(ch)
		}
	}
	walk(n)
	return m, nil
}

func (s *Session) getMessageNode(ctx context.Context, id, maxBody string, headers ...string) (node, error) {
	if id == "" {
		return node{}, ErrProtocol
	}
	selector := attr(attr(attr(attr(attr(element(mailNS, "m", ""), "id", id), "read", "0"), "html", "1"), "max", maxBody), "wantModSeq", "1")
	for _, header := range headers {
		selector.Children = append(selector.Children, attr(element(mailNS, "header", ""), "n", header))
	}
	response, err := s.request(ctx, mailNS, "GetMsg", "", element(mailNS, "GetMsgRequest", "", selector))
	if err != nil {
		return node{}, err
	}
	return exactMailItem(response, "m", id)
}
func exactMailItem(response node, name, id string) (node, error) {
	var item node
	for _, child := range response.Children {
		if child.XMLName.Space == mailNS && child.XMLName.Local == name {
			if item.XMLName.Local != "" || child.get("id") != id {
				return node{}, ErrProtocol
			}
			item = child
		}
	}
	if item.XMLName.Local == "" {
		return node{}, ErrProtocol
	}
	return item, nil
}

func (s *Session) MIME(ctx context.Context, id string) ([]byte, error) {
	return s.mimeLimited(ctx, id, s.client.mimeLimit)
}

func (s *Session) mimeLimited(ctx context.Context, id string, limit int64) ([]byte, error) {
	return s.readMessageContent(ctx, "GetMIME", id, "", limit)
}

func (s *Session) readMessageContent(ctx context.Context, operation, id, part string, limit int64) (out []byte, callErr error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: operation, Result: core.ActivityOutcome(semanticError(callErr)), Duration: time.Since(started)})
	}()
	if id == "" {
		return nil, ErrProtocol
	}
	token, err := s.credential()
	if err != nil {
		return nil, err
	}
	q := url.Values{"id": {id}, "fmt": {"raw"}, "auth": {"co"}}
	if part != "" {
		q.Del("fmt")
		q.Set("part", part)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, s.client.origin+"/service/home/~/?"+q.Encode(), nil)
	if err != nil {
		return nil, ErrProtocol
	}
	req.AddCookie(&http.Cookie{Name: "ZM_AUTH_TOKEN", Value: token})
	if part != "" {
		req.Header.Set("Accept", "*/*")
	} else {
		req.Header.Set("Accept", "message/rfc822, application/octet-stream;q=0.9")
	}
	data, status, err := s.client.exchange(ctx, req, min(limit, s.client.mimeLimit))
	if err != nil {
		return nil, err
	}
	if status == 401 {
		return nil, ErrExpired
	}
	if status == 403 {
		return nil, ErrDenied
	}
	if status == 404 {
		return nil, core.ErrNotFound
	}
	if status != 200 || part == "" && len(data) == 0 {
		return nil, ErrProtocol
	}
	return data, nil
}
