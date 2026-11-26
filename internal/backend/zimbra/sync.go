// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"encoding/xml"
	"regexp"
	"strconv"
	"strings"
)

var nativeID = regexp.MustCompile(`^[1-9][0-9]{0,15}$`)

type SyncResult struct {
	Token             string
	More              bool
	Messages, Deleted []string
}

func (SyncResult) String() string { return "Zimbra sync result [private cursor and IDs redacted]" }
func (s *Session) requestSync(ctx context.Context, token, folder string, limit int) (node, error) {
	if !nativeID.MatchString(folder) || limit < 1 || limit > 512 || len(token) > 4096 {
		return node{}, ErrProtocol
	}
	request := attr(attr(attr(attr(element(mailNS, "SyncRequest", ""), "l", folder), "typed", "1"), "changeLimit", strconv.Itoa(limit)), "deleteLimit", strconv.Itoa(limit))
	if token != "" {
		request = attr(request, "token", token)
	}
	return s.request(ctx, mailNS, "Sync", "", request)
}

func (s *Session) Sync(ctx context.Context, token, folder string, limit int) (SyncResult, error) {
	response, err := s.requestSync(ctx, token, folder, limit)
	if err != nil {
		return SyncResult{}, err
	}
	out := SyncResult{Token: response.get("token"), More: response.get("more") == "1" || response.get("more") == "true"}
	if out.Token == "" {
		return out, ErrProtocol
	}
	seen := map[string]bool{}
	add := func(ids string, deleted bool) error {
		for _, id := range strings.Split(ids, ",") {
			id = strings.TrimSpace(id)
			if id == "" {
				continue
			}
			if !nativeID.MatchString(id) {
				return ErrProtocol
			}
			if deleted {
				out.Deleted = append(out.Deleted, id)
			} else if !seen[id] {
				out.Messages = append(out.Messages, id)
				seen[id] = true
			}
			if len(out.Messages)+len(out.Deleted) > 2000 {
				return ErrLimit
			}
		}
		return nil
	}
	var walk func(node, bool) error
	walk = func(n node, deleted bool) error {
		if n.XMLName.Space != mailNS {
			return nil
		}
		deleted = deleted || n.XMLName.Local == "deleted"
		if n.XMLName.Local == "m" {
			id := n.get("id")
			if id == "" {
				id = n.get("ids")
			}
			if err := add(id, deleted); err != nil {
				return err
			}
		}
		for _, child := range n.Children {
			if err := walk(child, deleted); err != nil {
				return err
			}
		}
		return nil
	}
	if err = walk(response, false); err != nil {
		return out, err
	}
	return out, nil
}
func (s *Session) SetRead(ctx context.Context, id string, read bool) error {
	op := "!read"
	if read {
		op = "read"
	}
	return s.messageAction(ctx, id, op, "")
}
func (s *Session) messageAction(ctx context.Context, id, op, folder string, contextElements ...node) error {
	if !nativeID.MatchString(id) || folder != "" && !nativeID.MatchString(folder) {
		return ErrProtocol
	}
	action := attr(attr(element(mailNS, "action", ""), "id", id), "op", op)
	if folder != "" {
		action = attr(action, "l", folder)
	}
	response, err := s.request(ctx, mailNS, "MsgAction", "", element(mailNS, "MsgActionRequest", "", action), contextElements...)
	if err != nil {
		return err
	}
	result := response.child(mailNS, "action")
	if result == nil || result.get("id") != id || result.get("op") != op {
		return ErrProtocol
	}
	return nil
}

// Laboratory fixture helpers are not exposed as frontend operations.
func (s *Session) CreateLabFolder(ctx context.Context, name string) (string, error) {
	if !strings.HasPrefix(name, "EWSGo-Lab-") || len(name) > 120 {
		return "", ErrProtocol
	}
	folder := attr(attr(attr(element(mailNS, "folder", ""), "name", name), "l", "1"), "view", "message")
	r, err := s.request(ctx, mailNS, "CreateFolder", "", element(mailNS, "CreateFolderRequest", "", folder))
	if err != nil {
		return "", err
	}
	n := r.child(mailNS, "folder")
	if n == nil || !nativeID.MatchString(n.get("id")) {
		return "", ErrProtocol
	}
	return n.get("id"), nil
}
func (s *Session) AddLabMessage(ctx context.Context, folder, mime string) (string, error) {
	if !nativeID.MatchString(folder) || len(mime) > 16384 || !strings.Contains(mime, "EWSGo laboratory fixture") {
		return "", ErrProtocol
	}
	m := attr(attr(element(mailNS, "m", "", element(mailNS, "content", mime)), "l", folder), "f", "u")
	r, err := s.request(ctx, mailNS, "AddMsg", "", element(mailNS, "AddMsgRequest", "", m))
	if err != nil {
		return "", err
	}
	n := r.child(mailNS, "m")
	if n == nil || !nativeID.MatchString(n.get("id")) {
		return "", ErrProtocol
	}
	return n.get("id"), nil
}
func (s *Session) MoveLabMessage(ctx context.Context, id, folder string) error {
	return s.messageAction(ctx, id, "move", folder)
}
func (s *Session) DeleteLabMessage(ctx context.Context, id string) error {
	return s.messageAction(ctx, id, "delete", "")
}
func (s *Session) SaveLabDraft(ctx context.Context, id, address, body string) (string, error) {
	if id != "" && !nativeID.MatchString(id) || !strings.HasPrefix(body, "EWSGo laboratory fixture") {
		return "", ErrProtocol
	}
	m := element(mailNS, "m", "", attr(attr(element(mailNS, "e", ""), "t", "f"), "a", address), element(mailNS, "su", "EWSGo laboratory fixture content revision"), attr(element(mailNS, "mp", "", element(mailNS, "content", body)), "ct", "text/plain"))
	if id != "" {
		m = attr(m, "id", id)
	}
	r, err := s.request(ctx, mailNS, "SaveDraft", "", element(mailNS, "SaveDraftRequest", "", m))
	if err != nil {
		return "", err
	}
	n := r.child(mailNS, "m")
	if n == nil || !nativeID.MatchString(n.get("id")) {
		return "", ErrProtocol
	}
	return n.get("id"), nil
}
func textChild(n node, name string) string {
	child := n.child(mailNS, name)
	if child != nil {
		return child.Text
	}
	return ""
}
func messageFromNode(n node) Message {
	m := Message{ID: n.get("id"), Folder: n.get("l"), Flags: n.get("f"), Subject: textChild(n, "su"), Preview: textChild(n, "fr"), InternetID: textChild(n, "mid")}
	m.MetadataRevision, _ = strconv.ParseUint(n.get("ms"), 10, 64)
	m.ContentRevision, _ = strconv.ParseUint(n.get("rev"), 10, 64)
	m.Size, _ = strconv.ParseInt(n.get("s"), 10, 64)
	m.Date, _ = strconv.ParseInt(n.get("d"), 10, 64)
	for _, child := range n.Children {
		if child.XMLName == (xml.Name{Space: mailNS, Local: "e"}) && child.get("t") == "f" {
			m.From = child.get("a")
		}
	}
	return m
}
