// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/json"
	"errors"
	"ewsgo/internal/core"
	"strings"
	"time"
)

type accountSession struct {
	native   *Session
	identity core.Identity
}

func (s *accountSession) Identity() core.Identity { return s.identity }
func (s *accountSession) Mailbox() core.Mailbox {
	return &mailbox{native: s.native, scope: s.identity.Scope, owner: s.identity.Name}
}
func (s *accountSession) Close(ctx context.Context) error { return s.native.Logout(ctx) }

// Authenticate uses a fresh native session for every frontend request. No password
// or cross-request backend token is retained by the gateway.
func (c *Client) Authenticate(ctx context.Context, user, password string) (core.Session, error) {
	session, err := c.Login(ctx, user, password)
	if err != nil {
		return nil, semanticError(err)
	}
	info, err := session.Info(ctx)
	if err != nil || info.AccountID == "" || info.Name == "" {
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = session.Logout(cleanup)
		cancel()
		if err != nil {
			return nil, semanticError(err)
		}
		return nil, ErrProtocol
	}
	return &accountSession{session, core.Identity{Scope: core.Scope{Backend: "zimbra", Account: info.AccountID}, Name: info.Name}}, nil
}
func semanticError(err error) error {
	switch {
	case errors.Is(err, ErrAuth), errors.Is(err, ErrExpired):
		return core.ErrAuthentication
	case errors.Is(err, ErrDenied):
		return core.ErrAccessDenied
	default:
		var fault *Fault
		if errors.As(err, &fault) {
			switch fault.Code {
			case "mail.NO_SUCH_MSG", "mail.NO_SUCH_ITEM", "mail.NO_SUCH_FOLDER", "mail.NO_SUCH_CONTACT":
				return core.ErrNotFound
			case "mail.MUST_RESYNC", "mail.INVALID_SYNC_TOKEN":
				return core.ErrResync
			}
		}
		return err
	}
}

type mailbox struct {
	native *Session
	scope  core.Scope
	owner  string
}

func (*mailbox) Capabilities() core.Capabilities { return core.Capabilities{Attachments: true} }
func (*mailbox) Root() core.ID                   { return "f:1" }
func folderObject(f Folder) core.Object {
	known := map[string]string{"1": "msgfolderroot", "2": "inbox", "3": "deleteditems", "4": "junkemail", "5": "sentitems", "6": "drafts"}
	props := core.Properties{core.DisplayName: core.Text(clip(f.Name, 256))}
	var revision core.Revision
	if f.MetadataRevision != nil {
		revision = core.Revision(*f.MetadataRevision)
		props[core.MetadataRevision] = core.Integer(*f.MetadataRevision)
	}
	if f.ModifiedAt != nil {
		props[core.LastModifiedAt] = core.Timestamp(*f.ModifiedAt)
	}
	if f.ItemCount != nil {
		props[core.FolderItemCount] = core.Integer(*f.ItemCount)
	}
	if f.UnreadCount != nil {
		props[core.FolderUnreadCount] = core.Integer(*f.UnreadCount)
	}
	if name := known[f.ID]; name != "" {
		props[core.WellKnown] = core.Text(name)
	}
	parent := core.ID("")
	if f.ID != "1" && f.Parent != "" {
		parent = core.ID("f:" + f.Parent)
	}
	return core.Object{ID: core.ID("f:" + f.ID), Kind: core.Folder, Parent: parent, Revision: revision, Properties: props}
}
func (m *mailbox) Get(ctx context.Context, id core.ID) (core.Object, error) {
	if strings.HasPrefix(string(id), "m:") {
		native := strings.TrimPrefix(string(id), "m:")
		if !nativeID.MatchString(native) {
			return core.Object{}, core.ErrInvalid
		}
		return m.getMessageSummary(ctx, native)
	}
	if !strings.HasPrefix(string(id), "f:") {
		return core.Object{}, core.ErrInvalid
	}
	folders, err := m.native.Folders(ctx, "")
	if err != nil {
		return core.Object{}, semanticError(err)
	}
	for _, f := range folders {
		if f.View != "" && f.View != "message" {
			continue
		}
		o := folderObject(f)
		if o.ID == id {
			return o, nil
		}
	}
	return core.Object{}, core.ErrNotFound
}
func (m *mailbox) Query(ctx context.Context, q core.Query) (core.Page, error) {
	if q.Kind != core.Folder && q.Kind != core.Message || len(q.Where) > 0 || q.OrderBy != nil {
		return core.Page{}, core.ErrUnsupported
	}
	if q.Offset < 0 || q.Limit < 0 || q.Limit > 1000 {
		return core.Page{}, core.ErrInvalid
	}
	if q.Kind == core.Message {
		return m.queryMessages(ctx, q)
	}
	folders, err := m.native.Folders(ctx, "")
	if err != nil {
		return core.Page{}, semanticError(err)
	}
	var all []core.Object
	for _, f := range folders {
		if f.View != "" && f.View != "message" {
			continue
		}
		o := folderObject(f)
		if q.Parent == "" || o.Parent == q.Parent {
			all = append(all, o)
		}
	}
	start := min(q.Offset, len(all))
	limit := q.Limit
	if limit == 0 {
		limit = 100
	}
	end := min(start+limit, len(all))
	return core.Page{Objects: all[start:end], NextOffset: end, More: end < len(all)}, nil
}
func (*mailbox) Begin(context.Context, core.Target) (core.Edit, error) {
	return nil, core.ErrUnsupported
}
func (*mailbox) Delete(context.Context, core.ID, core.Revision) error { return core.ErrUnsupported }
func (*mailbox) Changes(context.Context, core.Cursor, int) (core.Changes, error) {
	return core.Changes{}, core.ErrUnsupported
}

func messageObject(m Message) core.Object {
	props := core.Properties{core.Subject: core.Text(clip(m.Subject, 998)), core.Preview: core.Text(clip(m.Preview, 256)), core.MessageID: core.Text(clip(m.InternetID, 998)), core.IsRead: core.Bool(!strings.Contains(m.Flags, "u")), core.MessageSize: core.Integer(m.Size), core.ReceivedAt: core.Timestamp(time.UnixMilli(m.Date)), core.FromAddress: core.Text(clip(m.From, 320)), core.ContentRevision: core.Integer(m.ContentRevision)}
	props[core.IsDraft] = core.Bool(strings.ContainsRune(m.Flags, 'd'))
	props[core.IsFlagged] = core.Bool(strings.ContainsRune(m.Flags, 'f'))
	props[core.HasAttachments] = core.Bool(strings.ContainsRune(m.Flags, 'a'))
	importance := core.Integer(1)
	switch {
	case strings.ContainsRune(m.Flags, '!'):
		importance = 2
	case strings.ContainsRune(m.Flags, '?'):
		importance = 0
	}
	props[core.MessageImportance] = importance
	return core.Object{ID: core.ID("m:" + m.ID), Kind: core.Message, Parent: core.ID("f:" + m.Folder), Revision: core.Revision(m.MetadataRevision), Properties: props}
}
func (m *mailbox) ReadMIME(ctx context.Context, id core.ID) ([]byte, error) {
	native := strings.TrimPrefix(string(id), "m:")
	if native == string(id) || !nativeID.MatchString(native) {
		return nil, core.ErrInvalid
	}
	data, err := m.native.MIME(ctx, native)
	return data, semanticError(err)
}
func (m *mailbox) SetRead(ctx context.Context, id core.ID, read bool) (core.Object, error) {
	native := strings.TrimPrefix(string(id), "m:")
	if native == string(id) || !nativeID.MatchString(native) {
		return core.Object{}, core.ErrInvalid
	}
	if err := m.native.SetRead(ctx, native, read); err != nil {
		return core.Object{}, semanticError(err)
	}
	object, err := m.Get(ctx, id)
	if err != nil {
		return core.Object{}, err
	}
	if object.Properties[core.IsRead] != core.Bool(read) {
		return core.Object{}, core.ErrConflict
	}
	return object, nil
}

type syncCursor struct {
	Scope  core.Scope
	Folder core.ID
	Token  string
}

func (m *mailbox) SyncFolder(ctx context.Context, folder core.ID, cursor core.Cursor, limit int) (core.SyncPage, error) {
	native := strings.TrimPrefix(string(folder), "f:")
	if native == string(folder) || !nativeID.MatchString(native) {
		return core.SyncPage{}, core.ErrInvalid
	}
	token := ""
	if cursor != "" {
		var c syncCursor
		if json.Unmarshal([]byte(cursor), &c) != nil || c.Scope != m.scope || c.Folder != folder {
			return core.SyncPage{}, core.ErrResync
		}
		token = c.Token
	}
	result, err := m.native.Sync(ctx, token, native, limit)
	if err != nil {
		return core.SyncPage{}, semanticError(err)
	}
	next, _ := json.Marshal(syncCursor{m.scope, folder, result.Token})
	out := core.SyncPage{Next: core.Cursor(next), More: result.More}
	for _, id := range result.Messages {
		object, err := m.Get(ctx, core.ID("m:"+id))
		if err != nil {
			return core.SyncPage{}, err
		}
		if object.Parent != folder {
			out.Deleted = append(out.Deleted, object.ID)
		} else {
			out.Objects = append(out.Objects, object)
		}
	}
	for _, id := range result.Deleted {
		out.Deleted = append(out.Deleted, core.ID("m:"+id))
	}
	return out, nil
}

func clip(value string, limit int) string {
	runes := []rune(value)
	return string(runes[:min(len(runes), limit)])
}
