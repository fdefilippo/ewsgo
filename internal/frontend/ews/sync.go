// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package ews

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"net/http"
	"strconv"
	"time"

	"ewsgo/internal/core"
)

type syncChange struct {
	kind    string
	object  core.Object
	deleted core.ID
}
type syncState struct {
	scope     core.Scope
	folder    core.ID
	hierarchy bool
	native    core.Cursor
	known     map[core.ID]string
	pending   []syncChange
	more      bool
	expires   time.Time
}

func (h *Handler) loadState(value string, scope core.Scope, folder core.ID, hierarchy bool) (syncState, error) {
	if value == "" {
		return syncState{scope: scope, folder: folder, hierarchy: hierarchy, known: map[core.ID]string{}}, nil
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	state, ok := h.states[value]
	if !ok || state.scope != scope || state.folder != folder || state.hierarchy != hierarchy || time.Now().After(state.expires) {
		return syncState{}, core.ErrResync
	}
	known := make(map[core.ID]string, len(state.known))
	for k, v := range state.known {
		known[k] = v
	}
	state.known = known
	state.pending = append([]syncChange(nil), state.pending...)
	return state, nil
}
func (h *Handler) saveState(state syncState) string {
	var random [24]byte
	if _, err := rand.Read(random[:]); err != nil {
		panic("cannot create sync cursor")
	}
	value := base64.RawURLEncoding.EncodeToString(random[:])
	state.expires = time.Now().Add(10 * time.Minute)
	h.mu.Lock()
	defer h.mu.Unlock()
	for key, old := range h.states {
		if time.Now().After(old.expires) {
			delete(h.states, key)
		}
	}
	pending := len(state.pending)
	for _, old := range h.states {
		pending += len(old.pending)
	}
	for len(h.states) >= 128 || pending > 4096 {
		oldest, key := time.Now().Add(time.Hour), ""
		for k, old := range h.states {
			if old.expires.Before(oldest) {
				oldest, key = old.expires, k
			}
		}
		pending -= len(h.states[key].pending)
		delete(h.states, key)
	}
	h.states[value] = state
	return value
}
func syncError(op string, err error) []node {
	code := "ErrorInternalServerError"
	switch {
	case errors.Is(err, core.ErrResync):
		code = "ErrorInvalidSyncStateData"
	case errors.Is(err, core.ErrAccessDenied):
		code = "ErrorAccessDenied"
	case errors.Is(err, core.ErrNotFound):
		code = "ErrorFolderNotFound"
	case errors.Is(err, core.ErrUnsupported):
		code = "ErrorInvalidPropertyForOperation"
	case errors.Is(err, core.ErrInvalid):
		code = "ErrorInvalidRequest"
	}
	return []node{failure(op, code)}
}
func stateValue(op node) string {
	if n := op.child(messages, "SyncState"); n != nil {
		return n.Text
	}
	return ""
}
func (h *Handler) syncHierarchy(r *http.Request, op node, s core.Session, shape responseShape) []node {
	if selector := op.child(messages, "SyncFolderId"); selector != nil {
		if len(selector.Children) != 1 || selector.Children[0].XMLName.Space != types {
			return syncError("SyncFolderHierarchy", core.ErrInvalid)
		}
		ref := selector.Children[0]
		switch ref.XMLName.Local {
		case "DistinguishedFolderId":
			if id := ref.attr("Id"); id != "msgfolderroot" && id != "root" {
				return syncError("SyncFolderHierarchy", core.ErrUnsupported)
			}
		case "FolderId":
			id, err := h.decode(s.Identity().Scope, ref.attr("Id"))
			if err != nil {
				return syncError("SyncFolderHierarchy", err)
			}
			if id != s.Mailbox().Root() {
				return syncError("SyncFolderHierarchy", core.ErrUnsupported)
			}
		default:
			return syncError("SyncFolderHierarchy", core.ErrUnsupported)
		}
	}
	if h.journal != nil {
		return h.persistentChanges(r, op, s, "", core.Folder, h.pageSize, shape)
	}
	state, err := h.loadState(stateValue(op), s.Identity().Scope, s.Mailbox().Root(), true)
	if err != nil {
		return syncError("SyncFolderHierarchy", err)
	}
	if len(state.pending) == 0 {
		page, err := s.Mailbox().Query(r.Context(), core.Query{Kind: core.Folder, Limit: 1000})
		if err != nil {
			return syncError("SyncFolderHierarchy", err)
		}
		if page.More || len(page.Objects) > 1000 {
			return syncError("SyncFolderHierarchy", core.ErrUnsupported)
		}
		known := map[core.ID]string{}
		for _, o := range page.Objects {
			if o.ID == s.Mailbox().Root() {
				continue
			}
			fingerprint := sha256.Sum256([]byte(string(o.Parent) + "\x00" + string(textProperty(o, core.DisplayName))))
			hash := base64.RawURLEncoding.EncodeToString(fingerprint[:])
			known[o.ID] = hash
			if state.known[o.ID] != hash {
				kind := "Update"
				if _, ok := state.known[o.ID]; !ok {
					kind = "Create"
				}
				state.pending = append(state.pending, syncChange{kind: kind, object: o})
			}
		}
		for id := range state.known {
			if _, ok := known[id]; !ok {
				state.pending = append(state.pending, syncChange{kind: "Delete", deleted: id})
			}
		}
		state.known = known
	}
	n := min(h.pageSize, len(state.pending))
	var changes []node
	for _, change := range state.pending[:n] {
		if change.kind == "Delete" {
			changes = append(changes, el(types, "Delete", "", at(el(types, "FolderId", ""), "Id", h.encode(s.Identity().Scope, change.deleted))))
		} else {
			changes = append(changes, el(types, change.kind, "", shape.apply(h.folder(s.Identity().Scope, change.object))))
		}
	}
	state.pending = append([]syncChange(nil), state.pending[n:]...)
	cursor := h.saveState(state)
	return []node{success("SyncFolderHierarchy", el(messages, "SyncState", cursor), el(messages, "IncludesLastFolderInRange", strconv.FormatBool(len(state.pending) == 0)), el(messages, "Changes", "", changes...))}
}
func (h *Handler) syncItems(r *http.Request, op node, s core.Session, shape responseShape) []node {
	if scope := op.child(messages, "SyncScope"); scope != nil && scope.Text != "NormalItems" {
		return syncError("SyncFolderItems", core.ErrUnsupported)
	}
	selector := op.child(messages, "SyncFolderId")
	if selector == nil || len(selector.Children) != 1 {
		return syncError("SyncFolderItems", core.ErrInvalid)
	}
	if op.child(messages, "Ignore") != nil {
		return syncError("SyncFolderItems", core.ErrUnsupported)
	}
	folder, err := h.folderID(r.Context(), s, selector.Children[0])
	if err != nil {
		return syncError("SyncFolderItems", err)
	}
	count := op.child(messages, "MaxChangesReturned")
	if count == nil {
		return syncError("SyncFolderItems", core.ErrInvalid)
	}
	limit, err := strconv.Atoi(count.Text)
	if err != nil || limit < 1 || limit > 512 {
		return syncError("SyncFolderItems", core.ErrInvalid)
	}
	limit = min(limit, h.pageSize)
	if h.journal != nil {
		return h.persistentChanges(r, op, s, folder, core.Message, limit, shape)
	}
	source, ok := s.Mailbox().(core.FolderSync)
	if !ok {
		return syncError("SyncFolderItems", core.ErrUnsupported)
	}
	state, err := h.loadState(stateValue(op), s.Identity().Scope, folder, false)
	if err != nil {
		return syncError("SyncFolderItems", err)
	}
	if len(state.pending) == 0 {
		page, err := source.SyncFolder(r.Context(), folder, state.native, limit)
		if err != nil {
			return syncError("SyncFolderItems", err)
		}
		if len(page.Objects)+len(page.Deleted) > 2000 || page.Next == "" || page.More && page.Next == state.native {
			return syncError("SyncFolderItems", core.ErrResync)
		}
		state.native = page.Next
		state.more = page.More
		for _, o := range page.Objects {
			kind := "Update"
			if _, exists := state.known[o.ID]; !exists {
				kind = "Create"
			}
			state.known[o.ID] = strconv.FormatUint(uint64(o.Revision), 10)
			state.pending = append(state.pending, syncChange{kind: kind, object: o})
		}
		for _, id := range page.Deleted {
			delete(state.known, id)
			state.pending = append(state.pending, syncChange{kind: "Delete", deleted: id})
		}
		if len(state.known) > 2000 {
			return syncError("SyncFolderItems", core.ErrUnsupported)
		}
	}
	n := min(limit, len(state.pending))
	var changes []node
	for _, change := range state.pending[:n] {
		if change.kind == "Delete" {
			id := h.itemID(s.Identity().Scope, core.Object{ID: change.deleted, Parent: folder})
			changes = append(changes, el(types, "Delete", "", at(el(types, "ItemId", ""), "Id", id)))
		} else {
			changes = append(changes, el(types, change.kind, "", shape.apply(h.message(s.Identity().Scope, change.object))))
		}
	}
	state.pending = append([]syncChange(nil), state.pending[n:]...)
	cursor := h.saveState(state)
	return []node{success("SyncFolderItems", el(messages, "SyncState", cursor), el(messages, "IncludesLastItemInRange", strconv.FormatBool(len(state.pending) == 0 && !state.more)), el(messages, "Changes", "", changes...))}
}
