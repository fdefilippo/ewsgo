// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/xml"
	"errors"
	"net"
	"net/http"
	"net/mail"
	"net/url"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"ewsgo/internal/core"
)

// Capture buffers are bounded separately from caller-owned results. The MAPI
// frontend must reserve its aggregate storage budget before invoking this port.
// No frontend is wired to the capability by this prerequisite alone.
var mailContentCaptures = make(chan struct{}, 2)

// ReadMailContents deliberately uses a message-only, exact-folder Search rather
// than recursive Sync. Initial Sync is not paginated by changeLimit and can
// include non-mail descendants. There are no account selectors or native writes.
func (m *mailbox) ReadMailContents(ctx context.Context, folder core.ID) (core.MailContents, error) {
	native := strings.TrimPrefix(string(folder), "f:")
	if native == string(folder) || !nativeID.MatchString(native) {
		return core.MailContents{}, core.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return core.MailContents{}, err
	}
	select {
	case mailContentCaptures <- struct{}{}:
		defer func() { <-mailContentCaptures }()
	default:
		return core.MailContents{}, errors.Join(core.ErrLimit, ErrLimit)
	}
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	for attempt := 0; attempt < 3; attempt++ {
		result, err := m.readMailContents(ctx, native)
		if err == nil {
			return result, nil
		}
		if errors.Is(err, ErrLimit) {
			return core.MailContents{}, errors.Join(core.ErrLimit, err)
		}
		if !errors.Is(err, core.ErrConflict) {
			return core.MailContents{}, semanticError(err)
		}
	}
	return core.MailContents{}, core.ErrConflict
}

func (m *mailbox) ReadMailMessageEnvelope(ctx context.Context, folder, message core.ID, includeSoftDeleted bool) (core.Object, error) {
	fid, folderOK := strings.CutPrefix(string(folder), "f:")
	mid, messageOK := strings.CutPrefix(string(message), "m:")
	if !folderOK || !messageOK || !nativeID.MatchString(fid) || !nativeID.MatchString(mid) {
		return core.Object{}, core.ErrInvalid
	}
	if err := m.mailContentFolder(ctx, fid); err != nil {
		return core.Object{}, semanticError(err)
	}
	header, err := m.native.mailContentMetadata(ctx, mid)
	if err != nil {
		return core.Object{}, semanticError(err)
	}
	if header.message.Folder != fid || !includeSoftDeleted && strings.Contains(header.message.Flags, "x") {
		return core.Object{}, core.ErrNotFound
	}
	if header.bytes() > 64<<10 {
		return core.Object{}, ErrLimit
	}
	return header.object(), nil
}

func (m *mailbox) ReadMailMessageMIME(ctx context.Context, folder, message core.ID, includeSoftDeleted bool) (core.Object, error) {
	before, err := m.ReadMailMessageEnvelope(ctx, folder, message, includeSoftDeleted)
	if err != nil {
		return core.Object{}, err
	}
	size, ok := before.Properties[core.MessageSize].(core.Integer)
	if !ok || size > core.MaxMailContentMIMEBytes {
		return core.Object{}, core.ErrUnsupported
	}
	raw, err := m.native.mimeLimited(ctx, strings.TrimPrefix(string(message), "m:"), core.MaxMailContentMIMEBytes)
	if err != nil {
		return core.Object{}, semanticError(err)
	}
	after, err := m.ReadMailMessageEnvelope(ctx, folder, message, includeSoftDeleted)
	if err != nil {
		return core.Object{}, err
	}
	if before.Properties[core.ContentRevision] != after.Properties[core.ContentRevision] {
		return core.Object{}, core.ErrConflict
	}
	if len(raw) == 0 {
		return core.Object{}, core.ErrInvalid
	}
	after.MIME = raw
	return after, nil
}

func (m *mailbox) readMailContents(ctx context.Context, folder string) (core.MailContents, error) {
	if err := m.mailContentFolder(ctx, folder); err != nil {
		return core.MailContents{}, err
	}
	ids, err := m.native.mailContentIndex(ctx, folder)
	if err != nil {
		return core.MailContents{}, err
	}
	result := core.MailContents{Folder: core.ID("f:" + folder)}
	versions := make([]mailContentMetadata, 0, len(ids))
	retained := 0
	for _, id := range ids {
		first, err := m.native.mailContentMetadata(ctx, id)
		if err != nil {
			return core.MailContents{}, mailContentRace(err)
		}
		if first.message.Folder != folder || strings.ContainsRune(first.message.Flags, 'x') {
			return core.MailContents{}, core.ErrConflict
		}
		metadataBytes := first.bytes()
		// Reject before reading body if native size or metadata cannot fit. Content
		// lengths from HTTP are not trusted; the actual read is also hard bounded.
		if first.message.Size > core.MaxMailContentMIMEBytes || metadataBytes > 64<<10 || retained+metadataBytes > core.MaxMailContentViewBytes {
			return core.MailContents{}, ErrLimit
		}
		modified, flags, err := m.native.mailContentModified(ctx, id)
		if err != nil {
			return core.MailContents{}, mailContentRace(err)
		}
		if flags != first.message.Flags {
			return core.MailContents{}, core.ErrConflict
		}
		raw, err := m.native.mimeLimited(ctx, id, min(int64(core.MaxMailContentMIMEBytes), int64(core.MaxMailContentViewBytes-retained-metadataBytes)))
		if err != nil {
			return core.MailContents{}, mailContentRace(err)
		}
		if len(raw) == 0 {
			return core.MailContents{}, ErrProtocol
		}
		last, err := m.native.mailContentMetadata(ctx, id)
		if err != nil {
			return core.MailContents{}, mailContentRace(err)
		}
		if !reflect.DeepEqual(first, last) {
			return core.MailContents{}, core.ErrConflict
		}
		if last.modifiedSeconds != nil && uint64(modified.Unix()) != *last.modifiedSeconds {
			return core.MailContents{}, core.ErrConflict
		}
		object := last.object()
		object.Properties[core.LastModifiedAt] = core.Timestamp(modified)
		object.MIME = raw
		retained += metadataBytes + len(raw)
		result.Messages = append(result.Messages, object)
		versions = append(versions, last)
	}
	again, err := m.native.mailContentIndex(ctx, folder)
	if err != nil {
		return core.MailContents{}, err
	}
	if !slices.Equal(ids, again) {
		return core.MailContents{}, core.ErrConflict
	}
	for i, id := range ids {
		current, err := m.native.mailContentMetadata(ctx, id)
		if err != nil {
			return core.MailContents{}, mailContentRace(err)
		}
		if !reflect.DeepEqual(versions[i], current) {
			return core.MailContents{}, core.ErrConflict
		}
	}
	if err := m.mailContentFolder(ctx, folder); err != nil {
		return core.MailContents{}, err
	}
	if err := ctx.Err(); err != nil {
		return core.MailContents{}, err
	}
	return result, nil
}

func mailContentRace(err error) error {
	if errors.Is(semanticError(err), core.ErrNotFound) {
		return core.ErrConflict
	}
	return err
}

func (m *mailbox) mailContentFolder(ctx context.Context, folder string) error {
	tree, err := m.mailFolderTree(ctx)
	if err != nil {
		return err
	}
	_, err = tree.mailFolder(folder)
	return err
}

type nativeMailFolderTree map[string]Folder

func (m *mailbox) mailFolderTree(ctx context.Context) (nativeMailFolderTree, error) {
	tree, err := m.ownFolderTree(ctx)
	if err == nil && len(tree) > 1000 {
		return nil, ErrLimit
	}
	return tree, err
}

func (m *mailbox) ownFolderTree(ctx context.Context) (nativeMailFolderTree, error) {
	return m.ownFolderTreeWithLinks(ctx, nil)
}

func (m *mailbox) ownFolderTreeWithLinks(ctx context.Context, links *[]node) (nativeMailFolderTree, error) {
	catalog, err := m.native.folderCatalogWithLinks(ctx, "", true, links)
	if err != nil {
		return nil, err
	}
	byID := make(nativeMailFolderTree, len(catalog))
	for _, f := range catalog {
		if !nativeID.MatchString(f.ID) {
			return nil, ErrProtocol
		}
		if _, ok := byID[f.ID]; ok {
			return nil, core.ErrConflict
		}
		byID[f.ID] = f
	}
	root, ok := byID["1"]
	if !ok || (root.View != "" && root.View != "message") {
		return nil, ErrProtocol
	}
	return byID, nil
}

func (byID nativeMailFolderTree) mailFolder(folder string) (Folder, error) {
	target, ok := byID[folder]
	if !ok {
		return Folder{}, core.ErrNotFound
	}
	if target.View != "" && target.View != "message" {
		return Folder{}, core.ErrUnsupported
	}
	if err := byID.checkRooted(folder); err != nil {
		return Folder{}, err
	}
	return target, nil
}

func (byID nativeMailFolderTree) checkRooted(folder string) error {
	for at, steps := folder, 0; at != "1"; steps++ {
		f, ok := byID[at]
		if !ok || steps >= len(byID) {
			return ErrProtocol
		}
		at = f.Parent
	}
	return nil
}

func (s *Session) mailContentIndex(ctx context.Context, folder string) ([]string, error) {
	// Numeric ID syntax is validated before interpolation. INID is an exact
	// folder clause, unlike UNDERID (Zimbra 10.1.20 QueryParser). types=message
	// prevents appointment/task reads even for the native root or mixed trees.
	request := element(mailNS, "SearchRequest", "", element(mailNS, "query", "inid:"+folder))
	for name, value := range map[string]string{"types": "message", "limit": strconv.Itoa(core.MaxMailContentMessages + 1), "offset": "0", "sortBy": "dateDesc", "includeTagDeleted": "0"} {
		request = attr(request, name, value)
	}
	response, err := s.request(ctx, mailNS, "Search", "", request)
	if err != nil {
		return nil, err
	}
	switch response.get("more") {
	case "0", "false":
	case "1", "true":
		return nil, ErrLimit
	default:
		return nil, ErrProtocol
	}
	if response.get("offset") != "0" {
		return nil, ErrProtocol
	}
	seen := map[string]bool{}
	ids := make([]string, 0, len(response.Children))
	for _, n := range response.Children {
		if n.XMLName != (xml.Name{Space: mailNS, Local: "m"}) || !nativeID.MatchString(n.get("id")) || !nativeID.MatchString(n.get("l")) {
			return nil, ErrProtocol
		}
		if n.get("l") != folder {
			return nil, core.ErrConflict
		}
		if seen[n.get("id")] {
			return nil, ErrProtocol
		}
		seen[n.get("id")] = true
		if strings.ContainsRune(n.get("f"), 'x') {
			return nil, core.ErrConflict
		}
		if len(ids) >= core.MaxMailContentMessages {
			return nil, ErrLimit
		}
		ids = append(ids, n.get("id"))
	}
	slices.Sort(ids)
	return ids, nil
}

type mailContentMetadata struct {
	message         Message
	modifiedSeconds *uint64
	recipients      []core.Recipient
	categories      core.Strings
	hasCalendarPart bool
}

func (mailContentMetadata) String() string   { return "mailContentMetadata{private}" }
func (mailContentMetadata) GoString() string { return "mailContentMetadata{private}" }

func (m mailContentMetadata) bytes() int {
	h := m.message
	n := len(h.ID) + len(h.Folder) + len(h.Subject) + len(h.Preview) + len(h.InternetID) + len(h.From) + len(h.Flags) + 1024
	for _, recipient := range m.recipients {
		n += len(recipient.Address) + len(recipient.DisplayName) + 64
	}
	for _, category := range m.categories {
		n += len(category) + 32
	}
	return n
}

func (m mailContentMetadata) object() core.Object {
	h := m.message
	object := messageObject(h)
	object.Properties[core.Subject] = core.Text(h.Subject)
	object.Properties[core.Preview] = core.Text(h.Preview)
	object.Properties[core.MessageID] = core.Text(h.InternetID)
	object.Properties[core.FromAddress] = core.Text(h.From)
	object.Properties[core.MetadataRevision] = core.Integer(h.MetadataRevision)
	object.Properties[core.VersionedMIME] = core.Bool(true)
	object.Properties[core.MailCategories] = m.categories
	if m.hasCalendarPart {
		object.Properties[core.HasAttachments] = core.Bool(true)
	}
	if m.modifiedSeconds != nil {
		object.Properties[core.LastModifiedAt] = core.Timestamp(time.Unix(int64(*m.modifiedSeconds), 0).UTC())
	}
	object.Recipients = m.recipients
	return object
}

func (s *Session) mailContentMetadata(ctx context.Context, id string) (mailContentMetadata, error) {
	response, err := s.request(ctx, mailNS, "GetMsg", "", mailContentMetadataRequest(id))
	if err != nil {
		return mailContentMetadata{}, err
	}
	return parseMailContentMetadata(response, id)
}

func mailContentMetadataRequest(id string) node {
	selector := attr(attr(attr(attr(element(mailNS, "m", ""), "id", id), "read", "0"), "wantModSeq", "1"), "max", "0")
	return element(mailNS, "GetMsgRequest", "", selector)
}

func parseMailContentMetadata(response node, id string) (mailContentMetadata, error) {
	if response.XMLName != (xml.Name{Space: mailNS, Local: "GetMsgResponse"}) {
		return mailContentMetadata{}, ErrProtocol
	}
	if len(response.Children) != 1 {
		return mailContentMetadata{}, ErrProtocol
	}
	n := response.Children[0]
	if n.XMLName != (xml.Name{Space: mailNS, Local: "m"}) || n.get("id") != id || !nativeID.MatchString(n.get("l")) {
		return mailContentMetadata{}, ErrProtocol
	}
	values := map[string]*uint64{}
	for _, key := range []string{"ms", "rev", "md", "s", "d"} {
		value, err := optionalFolderNumber(n, key)
		if err != nil || (value == nil && key != "md") {
			return mailContentMetadata{}, ErrProtocol
		}
		values[key] = value
	}
	if *values["d"] > 253402300799999 || (values["md"] != nil && *values["md"] > 253402300799) {
		return mailContentMetadata{}, ErrProtocol
	}
	recipients, err := mailContentRecipients(n)
	if err != nil {
		return mailContentMetadata{}, err
	}
	return mailContentMetadata{
		message: messageFromNode(n), modifiedSeconds: values["md"],
		recipients: recipients, categories: zimbraTagNames(n.get("tn")),
		hasCalendarPart: hasCalendarMIMEPart(n),
	}, nil
}

// Zimbra omits the attachment flag for an inline calendar alternative.
// MAPI projects that part as iCalendar data attached to the open message.
func hasCalendarMIMEPart(parent node) bool {
	for _, child := range parent.Children {
		if child.XMLName == (xml.Name{Space: mailNS, Local: "mp"}) &&
			(strings.EqualFold(child.get("ct"), "text/calendar") || hasCalendarMIMEPart(child)) {
			return true
		}
	}
	return false
}

// GetMsg's addresses are derived from MIME headers by Zimbra ToXML, not from
// the SMTP envelope. Keep native ordering and repeated addresses across roles.
func mailContentRecipients(n node) ([]core.Recipient, error) {
	var recipients []core.Recipient
	used := 0
	for _, e := range n.Children {
		if e.XMLName.Local != "e" {
			continue
		}
		if e.XMLName.Space != mailNS || len(e.Children) != 0 || strings.TrimSpace(e.Text) != "" {
			return nil, ErrProtocol
		}
		seen := map[string]bool{}
		for _, a := range e.Attrs {
			switch a.Name.Local {
			case "t", "a", "p", "d", "isGroup", "exp":
				if a.Name.Space != "" || seen[a.Name.Local] {
					return nil, ErrProtocol
				}
				seen[a.Name.Local] = true
			}
		}
		var kind core.RecipientKind
		switch e.get("t") {
		case "t":
			kind = core.RecipientTo
		case "c":
			kind = core.RecipientCc
		case "b":
			kind = core.RecipientBcc
		case "f", "s", "r", "n", "rf":
			continue // These are separate message header identities, not recipients.
		default:
			return nil, core.ErrUnsupported
		}
		for _, name := range []string{"isGroup", "exp"} {
			switch e.get(name) {
			case "", "0", "false":
			case "1", "true":
				return nil, core.ErrUnsupported
			default:
				return nil, ErrProtocol
			}
		}
		address, personal := e.get("a"), e.get("p")
		if personal == "" {
			personal = e.get("d")
		}
		used += len(address) + len(personal) + 64
		if len(recipients) >= 256 || used > 64<<10 {
			return nil, ErrLimit
		}
		parsed, err := mail.ParseAddress(address)
		if err != nil || parsed.Name != "" || parsed.Address != address || !utf8.ValidString(address) || !utf8.ValidString(personal) || strings.ContainsAny(address+personal, "\x00\r\n") {
			return nil, ErrProtocol
		}
		recipients = append(recipients, core.Recipient{Address: address, DisplayName: personal, Kind: kind})
	}
	return recipients, nil
}

// Zimbra 10 ContentServlet sync=1 emits native X-Zimbra-Modified in milliseconds
// on HEAD. The original REST MIME is fetched independently and bracketed by ms/rev.
func (s *Session) mailContentModified(ctx context.Context, id string) (time.Time, string, error) {
	return s.mailContentInfo(ctx, http.MethodHead, url.Values{"id": {id}, "sync": {"1"}})
}

func (s *Session) mailContentInfo(ctx context.Context, method string, query url.Values) (time.Time, string, error) {
	token, err := s.credential()
	if err != nil {
		return time.Time{}, "", err
	}
	request, err := http.NewRequestWithContext(ctx, method, s.client.origin+"/service/content/get?"+query.Encode(), nil)
	if err != nil {
		return time.Time{}, "", ErrProtocol
	}
	request.AddCookie(&http.Cookie{Name: "ZM_AUTH_TOKEN", Value: token})
	response, err := s.client.http.Do(request)
	if err != nil {
		if ctx.Err() != nil {
			return time.Time{}, "", ctx.Err()
		}
		if e, ok := err.(net.Error); ok && e.Timeout() {
			return time.Time{}, "", context.DeadlineExceeded
		}
		return time.Time{}, "", ErrTransport
	}
	defer response.Body.Close()
	switch {
	case response.StatusCode == 401:
		return time.Time{}, "", ErrExpired
	case response.StatusCode == 403:
		return time.Time{}, "", ErrDenied
	case response.StatusCode >= 300 && response.StatusCode < 400:
		return time.Time{}, "", ErrRouting
	case response.StatusCode != 200:
		return time.Time{}, "", ErrProtocol
	}
	dates, flags := response.Header.Values("X-Zimbra-Modified"), response.Header.Values("X-Zimbra-Flags")
	if len(dates) != 1 || len(flags) != 1 || len(dates[0]) > 15 || len(flags[0]) > 128 {
		return time.Time{}, "", ErrProtocol
	}
	value, err := strconv.ParseUint(dates[0], 10, 63)
	if err != nil || value > 253402300799999 {
		return time.Time{}, "", ErrProtocol
	}
	return time.UnixMilli(int64(value)).UTC(), flags[0], nil
}
