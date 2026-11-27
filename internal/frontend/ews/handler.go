// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"encoding/xml"
	"errors"
	"io"
	"net/http"
	"os"
	"strconv"
	"strings"
	"sync"
	"time"

	"ewsgo/internal/core"
)

type Handler struct {
	key      [32]byte
	mu       sync.Mutex
	states   map[string]syncState
	pulls    map[string]pullSubscription
	pageSize int
	journal  *persistentSync
	sender   core.MailMIMESender
}

// EnableMIMESending installs the persistent submission coordinator before serving.
func (h *Handler) EnableMIMESending(sender core.MailMIMESender) {
	h.sender = sender
}

func New() *Handler {
	return NewWithPageSize(200)
}
func NewWithPageSize(size int) *Handler {
	if size < 1 || size > 512 {
		panic("invalid EWS page size")
	}
	h := &Handler{states: make(map[string]syncState), pageSize: size}
	if _, err := rand.Read(h.key[:]); err != nil {
		panic("cannot initialize opaque IDs")
	}
	return h
}

type reference struct {
	Scope  core.Scope
	ID     core.ID
	Parent core.ID `json:",omitempty"`
}

func (h *Handler) encode(scope core.Scope, id core.ID) string {
	return h.encodeRef(reference{Scope: scope, ID: id})
}
func (h *Handler) encodeRef(ref reference) string {
	data, _ := json.Marshal(ref)
	return h.sign(data)
}
func (h *Handler) sign(data []byte) string {
	mac := hmac.New(sha256.New, h.key[:])
	mac.Write(data)
	return base64.RawURLEncoding.EncodeToString(append(mac.Sum(nil), data...))
}
func (h *Handler) decode(scope core.Scope, value string) (core.ID, error) {
	ref, err := h.decodeRef(scope, value)
	return ref.ID, err
}
func (h *Handler) decodeRef(scope core.Scope, value string) (reference, error) {
	data, err := h.verify(value)
	if err != nil {
		return reference{}, err
	}
	var ref reference
	if json.Unmarshal(data, &ref) != nil || ref.Scope != scope || ref.ID == "" {
		return reference{}, core.ErrAccessDenied
	}
	return ref, nil
}
func (h *Handler) verify(value string) ([]byte, error) {
	if len(value) > 4096 {
		return nil, core.ErrInvalid
	}
	data, err := base64.RawURLEncoding.DecodeString(value)
	if err != nil || len(data) < 33 {
		return nil, core.ErrInvalid
	}
	mac := hmac.New(sha256.New, h.key[:])
	mac.Write(data[32:])
	if !hmac.Equal(data[:32], mac.Sum(nil)) {
		return nil, core.ErrAccessDenied
	}
	return data[32:], nil
}
func (h *Handler) Serve(w http.ResponseWriter, r *http.Request, session core.Session) {
	if r.Method != "POST" {
		w.WriteHeader(405)
		return
	}
	raw, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 16<<20))
	if err != nil {
		if errors.Is(err, os.ErrDeadlineExceeded) {
			http.Error(w, "request timed out", http.StatusRequestTimeout)
		} else {
			http.Error(w, "request limit", http.StatusRequestEntityTooLarge)
		}
		return
	}
	root, err := parse(raw)
	if err != nil || root.XMLName.Space != soap || root.XMLName.Local != "Envelope" {
		http.Error(w, "invalid SOAP", 400)
		return
	}
	body := root.child(soap, "Body")
	if body == nil || len(body.Children) != 1 || body.Children[0].XMLName.Space != messages {
		http.Error(w, "invalid SOAP body", 400)
		return
	}
	op := body.Children[0]
	started := time.Now()
	outcome := core.ActivitySuccess
	defer func() {
		core.RecordActivity(r.Context(), core.Activity{Layer: "ews", Operation: op.XMLName.Local, Result: outcome, Duration: time.Since(started)})
	}()
	// Self-only profile: never turn a client-supplied address into a backend scope.
	if !allowedIdentity(root, session.Identity().Name) {
		outcome = core.ActivityDenied
		if op.XMLName.Local == availabilityOperation {
			replyFault(w, "ErrorAccessDenied")
		} else {
			h.reply(w, op.XMLName.Local, []node{failure(op.XMLName.Local, "ErrorAccessDenied")})
		}
		return
	}

	modern, code := requestVersion(root)
	if code != "" {
		outcome = core.ActivityRejected
		replyFault(w, code)
		return
	}
	if op.XMLName.Local == availabilityOperation {
		outcome = serveAvailability(w, r.Context(), op, session)
		return
	}
	var shape responseShape
	switch op.XMLName.Local {
	case "FindFolder", "GetFolder", "SyncFolderHierarchy":
		shape, code = readShape(op, "FolderShape", folderFields, modern)
	case "GetItem", "SyncFolderItems":
		shape, code = readShape(op, "ItemShape", itemFields, modern)
	case "FindItem":
		shape, code = readShape(op, "ItemShape", itemFields, modern)
	}
	if code != "" {
		outcome = core.ActivityRejected
		h.reply(w, op.XMLName.Local, []node{failure(op.XMLName.Local, code)})
		return
	}

	var replies []node
	switch op.XMLName.Local {
	case "Subscribe", "GetEvents", "Unsubscribe":
		reply, code := h.notifications(r.Context(), op, session)
		if code != "" {
			reply = failure(op.XMLName.Local, code)
		}
		replies = []node{reply}
	case "GetFolder":
		replies = h.getFolders(r, op, session, shape)
	case "FindFolder":
		replies = h.findFolders(r.Context(), op, session, shape)
	case "SyncFolderHierarchy":
		replies = h.syncHierarchy(r, op, session, shape)
	case "SyncFolderItems":
		replies = h.syncItems(r, op, session, shape)
	case "GetItem":
		replies = h.getItems(r, op, session, shape)
	case "UpdateItem":
		replies = h.updateItems(r, op, session)
	case "CreateItem":
		items := op.child(messages, "Items")
		if containsItemType(items, "CancelCalendarItem") {
			replies = h.cancelMeetings(r.Context(), op, session)
		} else if hasMeetingReplies(items) {
			replies = h.replyToMeetings(r.Context(), op, session)
		} else if op.attr("MessageDisposition") == "SendAndSaveCopy" && !containsItemType(items, "CalendarItem") {
			replies = h.sendMIMEItems(r.Context(), op, session)
		} else {
			replies = h.createItems(r.Context(), op, session)
		}
	case "CreateAttachment":
		replies = h.createAttachments(r.Context(), op, session)
	case "DeleteAttachment":
		replies = h.deleteAttachments(r.Context(), op, session)
	case "FindItem":
		replies = h.findItems(r.Context(), op, session, shape)
	case "MoveItem", "CopyItem", "DeleteItem":
		replies = h.placeItems(r.Context(), op, session)
	case "CreateFolder", "UpdateFolder", "MoveFolder", "DeleteFolder":
		replies = h.writeFolders(r.Context(), op, session)
	default:
		outcome = core.ActivityRejected
		replyFault(w, "ErrorInvalidRequest")
		return
	}
	for _, reply := range replies {
		code := reply.child(messages, "ResponseCode")
		if code == nil {
			outcome = core.ActivityFailure
			break
		}
		switch code.Text {
		case "NoError":
		case "ErrorInternalServerError":
			outcome = core.ActivityFailure
		case "ErrorAccessDenied":
			if outcome != core.ActivityFailure {
				outcome = core.ActivityDenied
			}
		default:
			if outcome == core.ActivitySuccess {
				outcome = core.ActivityRejected
			}
		}
	}
	h.reply(w, op.XMLName.Local, replies)
}
func failure(op, code string) node {
	return at(el(messages, op+"ResponseMessage", "", el(messages, "ResponseCode", code)), "ResponseClass", "Error")
}
func success(op string, children ...node) node {
	return at(el(messages, op+"ResponseMessage", "", append([]node{el(messages, "ResponseCode", "NoError")}, children...)...), "ResponseClass", "Success")
}
func (h *Handler) reply(w http.ResponseWriter, op string, replies []node) {
	w.Header().Set("Content-Type", "text/xml; charset=utf-8")
	_, _ = w.Write(envelope(el(messages, op+"Response", "", el(messages, "ResponseMessages", "", replies...))))
}
func (h *Handler) folder(scope core.Scope, o core.Object) node {
	name := textProperty(o, core.DisplayName)
	children := []node{at(at(el(types, "FolderId", ""), "Id", h.encode(scope, o.ID)), "ChangeKey", h.changeKey(scope, o))}
	if o.Parent != "" {
		children = append(children, at(el(types, "ParentFolderId", ""), "Id", h.encode(scope, o.Parent)))
	}
	kind, class := "Folder", "IPF.Note"
	if o.Properties[core.WellKnown] == core.Text("contacts") || o.Properties[core.GroupwareFolderClass] == core.Text("IPF.Contact") {
		kind, class = "ContactsFolder", "IPF.Contact"
	}
	if o.Properties[core.WellKnown] == core.Text("calendar") || o.Properties[core.GroupwareFolderClass] == core.Text("IPF.Appointment") {
		kind, class = "CalendarFolder", "IPF.Appointment"
	}
	children = append(children, el(types, "FolderClass", class), el(types, "DisplayName", string(name)))
	for _, count := range []struct {
		name string
		key  core.Key
	}{
		{"TotalCount", core.FolderItemCount},
		{"UnreadCount", core.FolderUnreadCount},
	} {
		if value, ok := o.Properties[count.key].(core.Integer); ok && (kind == "Folder" || count.name != "UnreadCount") {
			children = append(children, el(types, count.name, strconv.FormatInt(int64(value), 10)))
		}
	}
	return el(types, kind, "", children...)
}

func (h *Handler) folderID(ctx context.Context, s core.Session, ref node) (core.ID, error) {
	if ref.XMLName.Space != types {
		return "", core.ErrInvalid
	}
	switch ref.XMLName.Local {
	case "FolderId":
		return h.decode(s.Identity().Scope, ref.attr("Id"))
	case "DistinguishedFolderId":
		name := ref.attr("Id")
		if name == "root" {
			name = "msgfolderroot"
		}
		if name == "msgfolderroot" {
			return s.Mailbox().Root(), nil
		}
		if name == "" {
			return "", core.ErrInvalid
		}
		for offset := 0; ; {
			page, err := s.Mailbox().Query(ctx, core.Query{Kind: core.Folder, Limit: 1000, Offset: offset})
			if err != nil {
				return "", err
			}
			for _, folder := range page.Objects {
				if folder.Properties[core.WellKnown] == core.Text(name) {
					return folder.ID, nil
				}
			}
			if !page.More {
				return "", core.ErrNotFound
			}
			if page.NextOffset <= offset {
				return "", errors.New("folder query made no progress")
			}
			offset = page.NextOffset
		}
	default:
		return "", core.ErrInvalid
	}
}

func (h *Handler) getFolders(r *http.Request, op node, s core.Session, shape responseShape) []node {
	ids := op.child(messages, "FolderIds")
	if ids == nil || len(ids.Children) == 0 || len(ids.Children) > 100 {
		return syncError("GetFolder", core.ErrInvalid)
	}
	var out []node
	for _, ref := range ids.Children {
		found, err := h.readFolder(r.Context(), s, ref)
		if err == nil && found.Kind != core.Folder {
			err = core.ErrNotFound
		}
		if err != nil {
			out = append(out, syncError("GetFolder", err)...)
		} else {
			out = append(out, success("GetFolder", el(messages, "Folders", "", shape.apply(h.folder(s.Identity().Scope, found)))))
		}
	}
	return out
}

func (h *Handler) readFolder(ctx context.Context, s core.Session, ref node) (core.Object, error) {
	if isCalendarFolderReference(ref) {
		return h.calendarFolder(ctx, s, ref)
	}
	if isContactFolderReference(ref) {
		folder, _, err := h.contactFolder(ctx, s, ref)
		return folder, err
	}
	id, err := h.folderID(ctx, s, ref)
	if err != nil {
		return core.Object{}, err
	}
	if strings.HasPrefix(string(id), core.SharedGroupwareFolderIDPrefix) {
		if shared, ok := s.Mailbox().(core.SharedGroupwareFolders); ok {
			return shared.ReadSharedGroupwareFolder(ctx, id, 0)
		}
		return core.Object{}, core.ErrNotFound
	}
	folder, err := s.Mailbox().Get(ctx, id)
	if errors.Is(err, core.ErrNotFound) && isElement(ref, types, "FolderId") {
		if contact, matched, contactErr := h.contactFolder(ctx, s, ref); matched {
			return contact, contactErr
		}
		if calendar, calendarErr := h.calendarFolder(ctx, s, ref); calendarErr == nil {
			return calendar, nil
		}
	}
	return folder, err
}

func allowedIdentity(n node, name string) bool {
	return checkIdentity(n, xml.Name{}, xml.Name{}, name)
}

func checkIdentity(n node, parent, grandparent xml.Name, name string) bool {
	if n.XMLName.Space == types && n.XMLName.Local == "ExchangeImpersonation" {
		return false
	}
	if n.XMLName.Space == types && n.XMLName.Local == "Mailbox" && !isRecipientList(parent, grandparent) {
		address := n.child(types, "EmailAddress")
		if address == nil || !strings.EqualFold(strings.TrimSpace(address.Text), name) {
			return false
		}
	}
	for _, child := range n.Children {
		if !checkIdentity(child, n.XMLName, parent, name) {
			return false
		}
	}
	return true
}

func isRecipientList(parent, grandparent xml.Name) bool {
	if parent == (xml.Name{Space: types, Local: "Attendee"}) && grandparent.Space == types {
		return grandparent.Local == "RequiredAttendees" || grandparent.Local == "OptionalAttendees"
	}
	if grandparent != (xml.Name{Space: types, Local: "Message"}) || parent.Space != types {
		return false
	}
	switch parent.Local {
	case "ToRecipients", "CcRecipients", "BccRecipients", "ReplyTo":
		return true
	}
	return false
}
