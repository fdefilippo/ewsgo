// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"net/http"
	"strconv"

	"ewsgo/internal/core"
	"ewsgo/internal/storage/sqlite"
)

type persistentSync struct {
	store           *sqlite.Store
	backendInstance string
	refresh         func(context.Context, core.Session) error
}

// NewPersistent serves EWS from the common mailbox journal while reading
// current objects and MIME through the authenticated backend session.
func NewPersistent(ctx context.Context, store *sqlite.Store, backendInstance string, refresh func(context.Context, core.Session) error, pageSize int) (*Handler, error) {
	if store == nil || backendInstance == "" || refresh == nil || pageSize < 1 || pageSize > 512 {
		return nil, core.ErrInvalid
	}
	key, err := store.EWSSigningKey(ctx)
	if err != nil {
		return nil, err
	}
	mac := hmac.New(sha256.New, key[:])
	mac.Write([]byte("EWS mailbox instance\x00" + backendInstance))
	handler := &Handler{pageSize: pageSize, journal: &persistentSync{store, backendInstance, refresh}}
	copy(handler.key[:], mac.Sum(nil))
	return handler, nil
}

type collectionState struct {
	Mailbox string
	Folder  core.ID
	Kind    core.Kind
	Cursor  sqlite.CollectionCursor
}

func (h *Handler) persistentChanges(r *http.Request, op node, session core.Session, folder core.ID, kind core.Kind, limit int, shape responseShape) []node {
	operation := op.XMLName.Local
	ctx := r.Context()
	scope := session.Identity().Scope
	mailbox, err := h.journal.store.BindMailbox(ctx, scope, h.journal.backendInstance)
	if err != nil {
		return syncError(operation, err)
	}
	state := collectionState{Mailbox: mailbox.MailboxID, Folder: folder, Kind: kind}
	if token := stateValue(op); token != "" {
		data, err := h.verify(token)
		if err != nil || json.Unmarshal(data, &state) != nil || state.Mailbox != mailbox.MailboxID || state.Folder != folder || state.Kind != kind || state.Cursor.Position.Generation == "" {
			return syncError(operation, core.ErrResync)
		}
	}
	box := session.Mailbox()
	if kind == core.Message {
		selected, err := box.Get(ctx, folder)
		if err != nil {
			return syncError(operation, err)
		}
		if selected.Kind != core.Folder {
			return syncError(operation, core.ErrNotFound)
		}
	}
	if err := h.journal.refresh(ctx, session); err != nil {
		return syncError(operation, err)
	}
	page, err := h.journal.store.Collection(ctx, mailbox.MailboxID, kind, folder, state.Cursor, limit)
	if err != nil {
		return syncError(operation, err)
	}
	get := box.Get
	if tree, ok := box.(core.OwnFolderTreeReader); ok && kind == core.Folder && len(page.Changes) != 0 {
		folders, err := tree.OwnFolderTree(ctx, nil)
		if err != nil {
			return syncError(operation, err)
		}
		byID := make(map[core.ID]core.Object, len(folders))
		for _, folder := range folders {
			byID[folder.ID] = folder
		}
		get = func(_ context.Context, id core.ID) (core.Object, error) {
			if folder, ok := byID[id]; ok {
				return folder, nil
			}
			return core.Object{}, core.ErrNotFound
		}
	}
	changes := make([]node, 0, len(page.Changes))
	for _, change := range page.Changes {
		wireChange, err := h.projectChange(ctx, get, scope, kind, folder, change, shape)
		if err != nil {
			return syncError(operation, err)
		}
		if wireChange.XMLName.Local != "" {
			changes = append(changes, wireChange)
		}
	}
	state.Cursor = page.Next
	data, _ := json.Marshal(state)
	last := "IncludesLastFolderInRange"
	if kind == core.Message {
		last = "IncludesLastItemInRange"
	}
	return []node{success(operation, el(messages, "SyncState", h.sign(data)),
		el(messages, last, strconv.FormatBool(!page.More)), el(messages, "Changes", "", changes...))}
}

func (h *Handler) projectChange(ctx context.Context, get func(context.Context, core.ID) (core.Object, error), scope core.Scope, kind core.Kind, folder core.ID, change core.Change, shape responseShape) (node, error) {
	if change.Kind == core.Deleted || kind == core.Message && change.Kind == core.Moved && change.Parent != folder {
		if kind == core.Folder {
			return el(types, "Delete", "", at(el(types, "FolderId", ""), "Id", h.encode(scope, change.ID))), nil
		}
		id := h.itemID(scope, core.Object{ID: change.ID, Parent: folder})
		return el(types, "Delete", "", at(el(types, "ItemId", ""), "Id", id)), nil
	}
	object, err := get(ctx, change.ID)
	if errors.Is(err, core.ErrNotFound) || err == nil && (object.Kind != kind || kind == core.Message && object.Parent != folder) {
		// A newer move or removal can win after the journal read. A later
		// refresh delivers the change in membership to its own collection.
		return node{}, nil
	}
	if err != nil {
		return node{}, err
	}
	operation := "Update"
	if change.Kind == core.Created || kind == core.Message && change.Kind == core.Moved {
		operation = "Create"
	}
	if kind == core.Folder {
		return el(types, operation, "", shape.apply(h.folder(scope, object))), nil
	}
	return el(types, operation, "", shape.apply(h.message(scope, object))), nil
}
