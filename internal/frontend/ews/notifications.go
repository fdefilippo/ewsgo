// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"strconv"
	"strings"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/storage/sqlite"
)

const (
	maxPullSubscriptions = 1024
	maxPullPerScope      = 16
	maxPullFolders       = 64
	maxPullEvents        = 50
	maxPullScan          = 512
)

type pullSubscription struct {
	scope   core.Scope
	mailbox string
	folders map[core.ID]bool // nil means all supported folders
	events  map[core.ChangeKind]bool
	timeout time.Duration
	expires time.Time
}

type eventWatermark struct {
	ID     string
	Cursor sqlite.JournalCursor
}

var eventNames = map[core.ChangeKind]string{
	core.Created: "CreatedEvent",
	core.Updated: "ModifiedEvent",
	core.Deleted: "DeletedEvent",
	core.Moved:   "MovedEvent",
}

func (h *Handler) notifications(ctx context.Context, op node, session core.Session) (node, string) {
	if h.journal == nil {
		return node{}, "ErrorNotSupported"
	}
	if op.XMLName.Local == "Subscribe" {
		return h.subscribe(ctx, op, session)
	}
	scope := session.Identity().Scope
	id := nodeText(op.child(messages, "SubscriptionId"))
	now := time.Now()
	h.mu.Lock()
	subscription, ok := h.pulls[id]
	switch {
	case !ok || !now.Before(subscription.expires):
		delete(h.pulls, id)
		h.mu.Unlock()
		return node{}, "ErrorSubscriptionNotFound"
	case subscription.scope != scope:
		h.mu.Unlock()
		return node{}, "ErrorSubscriptionAccessDenied"
	case op.XMLName.Local == "Unsubscribe":
		delete(h.pulls, id)
		h.mu.Unlock()
		return success("Unsubscribe"), ""
	}
	h.mu.Unlock()
	return h.getEvents(ctx, op, session, id, subscription)
}

func (h *Handler) subscribe(ctx context.Context, op node, session core.Session) (node, string) {
	request := op.child(messages, "PullSubscriptionRequest")
	if request == nil || len(op.Children) != 1 {
		return node{}, "ErrorNotSupported"
	}
	if request.child(types, "Watermark") != nil {
		return node{}, "ErrorInvalidWatermark"
	}
	minutes, err := strconv.Atoi(nodeText(request.child(types, "Timeout")))
	if err != nil || minutes < 1 || minutes > 1440 {
		return node{}, "ErrorInvalidRequest"
	}
	eventTypes := request.child(types, "EventTypes")
	if eventTypes == nil || len(eventTypes.Children) == 0 || len(eventTypes.Children) > len(eventNames) {
		return node{}, "ErrorInvalidRequest"
	}
	s := pullSubscription{scope: session.Identity().Scope, events: make(map[core.ChangeKind]bool), timeout: time.Duration(minutes) * time.Minute}
	for _, event := range eventTypes.Children {
		if event.XMLName.Space != types || event.XMLName.Local != "EventType" {
			return node{}, "ErrorInvalidRequest"
		}
		for kind, name := range eventNames {
			if name == strings.TrimSpace(event.Text) {
				s.events[kind] = true
			}
		}
	}
	if len(s.events) != len(eventTypes.Children) {
		return node{}, "ErrorInvalidEvent"
	}
	folders := request.child(types, "FolderIds")
	switch request.attr("SubscribeToAllFolders") {
	case "true", "1":
		if folders != nil {
			return node{}, "ErrorInvalidSubscriptionRequest"
		}
	case "", "false", "0":
		if folders == nil || len(folders.Children) == 0 || len(folders.Children) > maxPullFolders {
			return node{}, "ErrorInvalidSubscriptionRequest"
		}
		s.folders = make(map[core.ID]bool, len(folders.Children))
		for _, ref := range folders.Children {
			id, err := h.folderID(ctx, session, ref)
			if err != nil {
				return node{}, notificationError(err)
			}
			if s.folders[id] {
				continue
			}
			object, err := session.Mailbox().Get(ctx, id)
			if err != nil {
				return node{}, notificationError(err)
			}
			if object.Kind != core.Folder {
				return node{}, "ErrorFolderNotFound"
			}
			s.folders[id] = true
		}
	default:
		return node{}, "ErrorInvalidSubscriptionRequest"
	}

	binding, err := h.journal.store.BindMailbox(ctx, s.scope, h.journal.backendInstance)
	if err != nil {
		return node{}, notificationError(err)
	}
	s.mailbox = binding.MailboxID
	if err := h.journal.refresh(ctx, session); err != nil {
		return node{}, notificationError(err)
	}
	head, err := h.journal.store.JournalHead(ctx, s.mailbox)
	if err != nil {
		return node{}, notificationError(err)
	}
	// Final admission and expiry are serialized. Database and backend work
	// happen outside this lock, so a refresh never delays other subscriptions.
	var random [24]byte
	_, _ = rand.Read(random[:])
	id := hex.EncodeToString(random[:])
	now := time.Now()
	s.expires = now.Add(s.timeout)
	h.mu.Lock()
	count := 0
	for oldID, old := range h.pulls {
		if !now.Before(old.expires) {
			delete(h.pulls, oldID)
		} else if old.scope == s.scope {
			count++
		}
	}
	if len(h.pulls) >= maxPullSubscriptions || count >= maxPullPerScope {
		h.mu.Unlock()
		return node{}, "ErrorExceededSubscriptionCount"
	}
	if h.pulls == nil {
		h.pulls = make(map[string]pullSubscription)
	}
	h.pulls[id] = s
	h.mu.Unlock()
	return success("Subscribe", el(messages, "SubscriptionId", id), el(messages, "Watermark", h.watermark(id, head))), ""
}

func (h *Handler) getEvents(ctx context.Context, op node, session core.Session, id string, s pullSubscription) (node, string) {
	token := nodeText(op.child(messages, "Watermark"))
	data, err := h.verify(token)
	var from eventWatermark
	if err != nil || json.Unmarshal(data, &from) != nil || from.ID != id || from.Cursor.Generation == "" {
		return node{}, "ErrorInvalidWatermark"
	}
	if err := h.journal.refresh(ctx, session); err != nil {
		return node{}, notificationError(err)
	}
	entries, next, err := h.journal.store.JournalEntries(ctx, s.mailbox, from.Cursor, maxPullScan)
	if err != nil {
		return node{}, notificationError(err)
	}
	events := make([]node, 0, maxPullEvents)
	more := len(entries) == maxPullScan
	for _, entry := range entries {
		if !s.matches(entry) {
			continue
		}
		if len(events) == maxPullEvents {
			more = true
			break
		}
		if entry.ObservedAt.IsZero() {
			return node{}, "ErrorMissedNotificationEvents"
		}
		position := sqlite.JournalCursor{Generation: next.Generation, Revision: entry.Revision}
		events = append(events, h.notificationEvent(id, s.scope, position, entry))
	}
	if len(events) == 0 {
		events = append(events, el(types, "StatusEvent", "", el(types, "Watermark", h.watermark(id, next))))
	}
	children := []node{
		el(types, "SubscriptionId", id),
		el(types, "PreviousWatermark", token),
		el(types, "MoreEvents", strconv.FormatBool(more)),
	}

	h.mu.Lock()
	current, ok := h.pulls[id]
	now := time.Now()
	if !ok || !now.Before(current.expires) {
		delete(h.pulls, id)
		h.mu.Unlock()
		return node{}, "ErrorSubscriptionNotFound"
	}
	current.expires = now.Add(current.timeout)
	h.pulls[id] = current
	h.mu.Unlock()

	return success("GetEvents", el(messages, "Notification", "", append(children, events...)...)), ""
}

func (s pullSubscription) matches(entry sqlite.JournalEntry) bool {
	if !s.events[entry.Kind] || (entry.ObjectKind != core.Folder && entry.ObjectKind != core.Message) || entry.Parent == "" {
		return false
	}
	return s.folders == nil || s.folders[entry.Parent] || s.folders[entry.PreviousParent] ||
		(entry.ObjectKind == core.Folder && s.folders[entry.ID])
}

func (h *Handler) notificationEvent(id string, scope core.Scope, pos sqlite.JournalCursor, entry sqlite.JournalEntry) node {
	objectID := func(parent core.ID, old bool) node {
		name, value := "FolderId", h.encode(scope, entry.ID)
		if entry.ObjectKind == core.Message {
			name = "ItemId"
			value = h.itemID(scope, core.Object{ID: entry.ID, Parent: parent})
		}
		if old {
			name = "Old" + name
		}
		return at(el(types, name, ""), "Id", value)
	}
	children := []node{
		el(types, "Watermark", h.watermark(id, pos)),
		el(types, "TimeStamp", entry.ObservedAt.Format(time.RFC3339Nano)),
		objectID(entry.Parent, false),
		at(el(types, "ParentFolderId", ""), "Id", h.encode(scope, entry.Parent)),
	}
	if entry.Kind == core.Moved {
		children = append(children, objectID(entry.PreviousParent, true),
			at(el(types, "OldParentFolderId", ""), "Id", h.encode(scope, entry.PreviousParent)))
	}
	return el(types, eventNames[entry.Kind], "", children...)
}

func (h *Handler) watermark(id string, position sqlite.JournalCursor) string {
	raw, _ := json.Marshal(eventWatermark{ID: id, Cursor: position})
	return h.sign(raw)
}

func nodeText(n *node) string {
	if n == nil {
		return ""
	}
	return strings.TrimSpace(n.Text)
}

func notificationError(err error) string {
	switch {
	case errors.Is(err, core.ErrResync):
		return "ErrorMissedNotificationEvents"
	case errors.Is(err, core.ErrAccessDenied):
		return "ErrorAccessDenied"
	case errors.Is(err, core.ErrNotFound):
		return "ErrorFolderNotFound"
	case errors.Is(err, core.ErrInvalid):
		return "ErrorInvalidRequest"
	default:
		return "ErrorInternalServerError"
	}
}
