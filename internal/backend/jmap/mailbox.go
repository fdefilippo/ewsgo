// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package jmap

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"errors"
	"math"
	"sort"
	"strings"
	"time"

	"ewsgo/internal/core"
)

const (
	rootID        core.ID = "jmap-root"
	maxFolders            = 1000
	maxEmails             = 10000
	maxPlacements         = 20000
)

type folder struct {
	ID          string  `json:"id"`
	Name        string  `json:"name"`
	ParentID    *string `json:"parentId"`
	Role        string  `json:"role"`
	TotalEmails int64   `json:"totalEmails"`
	Unread      int64   `json:"unreadEmails"`
	Rights      struct {
		Read   bool `json:"mayReadItems"`
		Add    bool `json:"mayAddItems"`
		Remove bool `json:"mayRemoveItems"`
		Submit bool `json:"maySubmit"`
	} `json:"myRights"`
}

type address struct{ Name, Email string }

type email struct {
	ID             string          `json:"id"`
	BlobID         string          `json:"blobId"`
	ThreadID       string          `json:"threadId"`
	MailboxIDs     map[string]bool `json:"mailboxIds"`
	Keywords       map[string]bool `json:"keywords"`
	Size           int64           `json:"size"`
	ReceivedAt     time.Time       `json:"receivedAt"`
	Subject        string          `json:"subject"`
	Preview        string          `json:"preview"`
	HasAttachments bool            `json:"hasAttachment"`
	MessageIDs     []string        `json:"messageId"`
	From           []address       `json:"from"`
	To             []address       `json:"to"`
	Cc             []address       `json:"cc"`
	Bcc            []address       `json:"bcc"`
}

type getResponse[T any] struct {
	AccountID string   `json:"accountId"`
	State     string   `json:"state"`
	List      []T      `json:"list"`
	NotFound  []string `json:"notFound"`
}

func (s *session) readFolders(ctx context.Context, ids []string) (getResponse[folder], error) {
	var result getResponse[folder]
	err := s.call(ctx, "Mailbox/get", map[string]any{"ids": ids, "properties": []string{
		"id", "name", "parentId", "role", "totalEmails", "unreadEmails", "myRights",
	}}, &result)
	if err != nil {
		return result, err
	}
	if result.AccountID != s.account || !validState(result.State) || result.List == nil {
		return result, errProtocol
	}
	if len(result.List) > maxFolders {
		return result, core.ErrLimit
	}
	return result, nil
}

func (s *session) getFolders(ctx context.Context) (getResponse[folder], error) {
	result, err := s.readFolders(ctx, nil)
	if errors.Is(err, core.ErrLimit) {
		result, err = s.scanFolders(ctx)
	}
	if err != nil {
		return result, err
	}
	if len(result.NotFound) != 0 {
		return result, errProtocol
	}
	parents := make(map[string]string, len(result.List))
	for _, f := range result.List {
		if _, seen := parents[f.ID]; seen || !nativeID.MatchString(f.ID) || f.Name == "" ||
			f.TotalEmails < 0 || f.Unread < 0 || f.Unread > f.TotalEmails {
			return result, errProtocol
		}
		parents[f.ID] = ""
		if f.ParentID != nil {
			if !nativeID.MatchString(*f.ParentID) {
				return result, errProtocol
			}
			parents[f.ID] = *f.ParentID
		}
	}
	depths := make(map[string]int, len(parents))
	for id := range parents {
		for parent, depth := id, 0; ; depth++ {
			if parent == "" {
				depths[id] = depth
				break
			}
			next, exists := parents[parent]
			if !exists || depth >= len(parents) {
				return result, errProtocol
			}
			parent = next
		}
	}
	sort.Slice(result.List, func(i, j int) bool {
		left, right := result.List[i].ID, result.List[j].ID
		if depths[left] != depths[right] {
			return depths[left] < depths[right]
		}
		return left < right
	})
	return result, nil
}

func (s *session) scanFolders(ctx context.Context) (getResponse[folder], error) {
	result, err := s.readFolders(ctx, []string{})
	if err != nil {
		return result, err
	}
	if len(result.List)+len(result.NotFound) != 0 {
		return result, errProtocol
	}
	err = s.scanIDs(ctx, "Mailbox", maxFolders, func(ids []string) error {
		page, err := s.readFolders(ctx, ids)
		if err != nil {
			return err
		}
		if page.State != result.State || len(page.NotFound) != 0 {
			return core.ErrConflict
		}
		if len(ids) != len(page.List) {
			return errProtocol
		}
		requested := make(map[string]bool, len(ids))
		for _, id := range ids {
			requested[id] = true
		}
		for _, f := range page.List {
			if !requested[f.ID] {
				return errProtocol
			}
			delete(requested, f.ID)
		}
		result.List = append(result.List, page.List...)
		return nil
	})
	if err != nil {
		return result, err
	}
	fence, err := s.readFolders(ctx, []string{})
	if err == nil && (fence.State != result.State || len(fence.List)+len(fence.NotFound) != 0) {
		return result, core.ErrConflict
	}
	return result, err
}

func (s *session) getEmails(ctx context.Context, ids []string) (getResponse[email], error) {
	var result getResponse[email]
	err := s.call(ctx, "Email/get", map[string]any{"ids": ids, "properties": []string{
		"id", "blobId", "threadId", "mailboxIds", "keywords", "size", "receivedAt", "subject", "preview",
		"hasAttachment", "messageId", "from", "to", "cc", "bcc",
	}}, &result)
	if err != nil {
		return result, err
	}
	if result.AccountID != s.account || !validState(result.State) || result.List == nil ||
		len(result.List)+len(result.NotFound) != len(ids) {
		return result, errProtocol
	}
	requested := make(map[string]bool, len(ids))
	for _, id := range ids {
		requested[id] = true
	}
	for _, e := range result.List {
		if !requested[e.ID] || !nativeID.MatchString(e.BlobID) || !nativeID.MatchString(e.ThreadID) ||
			e.Size < 0 || e.ReceivedAt.IsZero() || len(e.MailboxIDs) == 0 || e.Keywords == nil {
			return result, errProtocol
		}
		delete(requested, e.ID)
		for id, present := range e.MailboxIDs {
			if !nativeID.MatchString(id) || !present {
				return result, errProtocol
			}
		}
	}
	for _, id := range result.NotFound {
		if !requested[id] {
			return result, errProtocol
		}
		delete(requested, id)
	}
	return result, nil
}

func validState(state string) bool { return state != "" && len(state) <= 4096 }

func folderID(id string) core.ID { return core.ID("folder:" + id) }
func placementID(message, mailbox string) core.ID {
	return core.ID("message:" + message + ":" + mailbox)
}

func folderRole(role string) string {
	switch role {
	case "archive", "drafts", "inbox":
		return role
	case "junk":
		return "junkemail"
	case "sent":
		return "sentitems"
	case "trash":
		return "deleteditems"
	default:
		return ""
	}
}

func (f folder) object() core.Object {
	parent := rootID
	if f.ParentID != nil {
		parent = folderID(*f.ParentID)
	}
	properties := core.Properties{
		core.DisplayName:       core.Text(f.Name),
		core.FolderItemCount:   core.Integer(f.TotalEmails),
		core.FolderUnreadCount: core.Integer(f.Unread),
	}
	if role := folderRole(f.Role); role != "" {
		properties[core.WellKnown] = core.Text(role)
	}
	return core.Object{ID: folderID(f.ID), Kind: core.Folder, Parent: parent,
		Revision: objectRevision(f.digest()), Properties: properties}
}

func (e email) object(mailbox string) core.Object {
	properties := core.Properties{
		core.Subject: core.Text(e.Subject), core.Preview: core.Text(e.Preview),
		core.IsRead: core.Bool(e.Keywords["$seen"]), core.IsDraft: core.Bool(e.Keywords["$draft"]),
		core.IsFlagged: core.Bool(e.Keywords["$flagged"]), core.MessageSize: core.Integer(e.Size),
		core.ReceivedAt: core.Timestamp(e.ReceivedAt), core.HasAttachments: core.Bool(e.HasAttachments),
		core.ConversationID: core.Text(e.ThreadID),
	}
	if len(e.MessageIDs) != 0 {
		properties[core.MessageID] = core.Text("<" + e.MessageIDs[0] + ">")
	}
	if len(e.From) != 0 {
		properties[core.FromAddress] = core.Text(e.From[0].Email)
	}
	var recipients []core.Recipient
	for _, list := range []struct {
		kind   core.RecipientKind
		values []address
	}{{core.RecipientTo, e.To}, {core.RecipientCc, e.Cc}, {core.RecipientBcc, e.Bcc}} {
		for _, a := range list.values {
			recipients = append(recipients, core.Recipient{Address: a.Email, DisplayName: a.Name, Kind: list.kind})
		}
	}
	return core.Object{ID: placementID(e.ID, mailbox), Kind: core.Message, Parent: folderID(mailbox),
		Revision: objectRevision(e.digest()), Properties: properties, Recipients: recipients}
}

func fingerprint(value any) [sha256.Size]byte {
	// All call sites use wire structs composed of JSON values already decoded
	// successfully. encoding/json orders map keys deterministically.
	data, _ := json.Marshal(value)
	return sha256.Sum256(data)
}

func (f folder) digest() [sha256.Size]byte { return fingerprint(f) }
func (e email) digest() [sha256.Size]byte {
	// Membership changes belong to the affected placement alone.
	e.MailboxIDs = nil
	return fingerprint(e)
}

func objectRevision(digest [sha256.Size]byte) core.Revision {
	return core.Revision(max(1, binary.BigEndian.Uint64(digest[:8])&math.MaxInt64))
}

func (s *session) Capabilities() core.Capabilities { return core.Capabilities{Attachments: true} }
func (s *session) Root() core.ID                   { return rootID }
func (s *session) Begin(context.Context, core.Target) (core.Edit, error) {
	return nil, core.ErrUnsupported
}
func (s *session) Delete(context.Context, core.ID, core.Revision) error { return core.ErrUnsupported }
func (s *session) Changes(context.Context, core.Cursor, int) (core.Changes, error) {
	return core.Changes{}, core.ErrUnsupported
}

func (s *session) OwnFolderTree(ctx context.Context, _ []core.SpecialFolderKind) ([]core.Object, error) {
	folders, err := s.getFolders(ctx)
	if err != nil {
		return nil, err
	}
	objects := make([]core.Object, 0, len(folders.List))
	for _, f := range folders.List {
		objects = append(objects, f.object())
	}
	return objects, nil
}

func (s *session) Query(ctx context.Context, q core.Query) (core.Page, error) {
	if (q.Kind != core.Folder && q.Kind != core.Message) || len(q.Where) != 0 || q.OrderBy != nil {
		return core.Page{}, core.ErrUnsupported
	}
	if q.Offset < 0 || q.Limit < 0 || q.Limit > maxFolders {
		return core.Page{}, core.ErrInvalid
	}
	if q.Limit == 0 {
		q.Limit = 100
	}
	if q.Kind == core.Message {
		return s.queryMessages(ctx, q)
	}
	folders, err := s.OwnFolderTree(ctx, nil)
	if err != nil {
		return core.Page{}, err
	}
	matches := folders[:0]
	for _, f := range folders {
		if q.Parent == "" || q.Parent == f.Parent {
			matches = append(matches, f)
		}
	}
	start := min(q.Offset, len(matches))
	end := min(start+q.Limit, len(matches))
	return core.Page{Objects: matches[start:end], NextOffset: end, More: end < len(matches)}, nil
}

func (s *session) queryMessages(ctx context.Context, q core.Query) (core.Page, error) {
	id, ok := strings.CutPrefix(string(q.Parent), "folder:")
	if !ok || !nativeID.MatchString(id) {
		return core.Page{}, core.ErrInvalid
	}
	folders, err := s.getFolders(ctx)
	if err != nil {
		return core.Page{}, err
	}
	readable := false
	for _, f := range folders.List {
		if f.ID == id {
			readable = f.Rights.Read
			break
		}
	}
	if !readable {
		return core.Page{}, core.ErrNotFound
	}
	var page struct {
		AccountID  string
		QueryState string
		IDs        []string
		Position   *int
		Total      *int
	}
	limit := min(q.Limit, s.getLimit)
	if err := s.call(ctx, "Email/query", map[string]any{"filter": map[string]string{"inMailbox": id},
		"position": q.Offset, "limit": limit, "calculateTotal": true}, &page); err != nil {
		return core.Page{}, err
	}
	if page.AccountID != s.account || !validState(page.QueryState) || page.IDs == nil ||
		page.Total == nil || *page.Total < 0 || page.Position == nil ||
		len(page.IDs) > limit || *page.Position != min(q.Offset, *page.Total) && *page.Position != q.Offset ||
		len(page.IDs) > max(0, *page.Total-*page.Position) {
		return core.Page{}, errProtocol
	}
	out := core.Page{NextOffset: *page.Position + len(page.IDs)}
	out.More = out.NextOffset < *page.Total
	if len(page.IDs) == 0 {
		if out.More {
			return core.Page{}, errProtocol
		}
		return out, nil
	}
	for _, id := range page.IDs {
		if !nativeID.MatchString(id) {
			return core.Page{}, errProtocol
		}
	}
	result, err := s.getEmails(ctx, page.IDs)
	if err != nil {
		return core.Page{}, err
	}
	objects := make(map[string]core.Object, len(result.List))
	for _, e := range result.List {
		if !e.MailboxIDs[id] {
			return core.Page{}, core.ErrConflict
		}
		objects[e.ID] = e.object(id)
	}
	for _, emailID := range page.IDs {
		object, ok := objects[emailID]
		if !ok {
			return core.Page{}, core.ErrConflict
		}
		out.Objects = append(out.Objects, object)
	}
	return out, nil
}

func (s *session) Get(ctx context.Context, id core.ID) (core.Object, error) {
	if id == rootID {
		return core.Object{ID: rootID, Kind: core.Folder, Revision: 1,
			Properties: core.Properties{core.DisplayName: core.Text("Mailbox")}}, nil
	}
	if strings.HasPrefix(string(id), "folder:") {
		folders, err := s.OwnFolderTree(ctx, nil)
		if err != nil {
			return core.Object{}, err
		}
		for _, f := range folders {
			if f.ID == id {
				return f, nil
			}
		}
		return core.Object{}, core.ErrNotFound
	}
	e, mailbox, err := s.readPlacement(ctx, id)
	if err != nil {
		return core.Object{}, err
	}
	return e.object(mailbox), nil
}

func (s *session) readPlacement(ctx context.Context, id core.ID) (email, string, error) {
	emailID, mailbox, ok := placementParts(id)
	if !ok {
		return email{}, "", core.ErrNotFound
	}
	result, err := s.getEmails(ctx, []string{emailID})
	if err != nil {
		return email{}, "", err
	}
	if len(result.List) == 0 || !result.List[0].MailboxIDs[mailbox] {
		return email{}, "", core.ErrNotFound
	}
	folders, err := s.getFolders(ctx)
	if err != nil {
		return email{}, "", err
	}
	for _, f := range folders.List {
		if f.ID == mailbox && f.Rights.Read {
			return result.List[0], mailbox, nil
		}
	}
	return email{}, "", core.ErrNotFound
}

func (s *session) ReadMIME(ctx context.Context, id core.ID) ([]byte, error) {
	e, _, err := s.readPlacement(ctx, id)
	if err != nil {
		return nil, err
	}
	return s.downloadBlob(ctx, e.BlobID, "message.eml", "message/rfc822", e.Size)
}

func (s *session) downloadBlob(ctx context.Context, blob, name, mediaType string, size int64) (_ []byte, err error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: "JMAPDownload", Result: core.ActivityOutcome(err), Duration: time.Since(started)})
	}()
	if size > core.MaxMailContentMIMEBytes {
		return nil, core.ErrLimit
	}
	endpoint, err := s.blobURL(blob, name, mediaType)
	if err != nil {
		return nil, err
	}
	raw, err := s.client.request(ctx, s.auth, "GET", endpoint, nil, mediaType, core.MaxMailContentMIMEBytes)
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) != size {
		return nil, errProtocol
	}
	return raw, nil
}
