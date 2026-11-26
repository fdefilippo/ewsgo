// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"errors"

	"ewsgo/internal/core"
)

// ProjectMailboxDelta commits one page, or reconciles the mailbox if its
// projection is missing or the native token requires a resync.
func ProjectMailboxDelta(ctx context.Context, session core.Session, store core.NativeProjectionStore, backendInstance string, limit int) (bool, error) {
	account, ok := session.(*accountSession)
	if !ok || account == nil || limit < 1 || limit > 512 {
		return false, core.ErrInvalid
	}
	projection, err := store.OpenNativeProjection(ctx, account.identity.Scope, backendInstance)
	if err != nil {
		return false, err
	}
	more, err := account.native.projectMailboxDelta(ctx, projection, limit)
	if errors.Is(err, core.ErrResync) {
		return false, account.native.reconcileMailbox(ctx, projection, limit)
	}
	return more, err
}

func (s *Session) projectMailboxDelta(ctx context.Context, projection core.NativeProjection, limit int) (bool, error) {
	checkpoint, err := projection.Checkpoint(ctx)
	if err != nil {
		return false, err
	}
	page, err := s.readMailboxDelta(ctx, string(checkpoint.Cursor), limit)
	if err != nil {
		return false, err
	}
	observed, err := s.observeMailboxChanges(ctx, page)
	if err != nil {
		return false, err
	}
	hints := mailboxRemovalHints(page)
	var removed []core.ID
	if len(hints) != 0 {
		ids := make([]core.ID, 0, len(hints))
		for _, hint := range hints {
			ids = append(ids, hint.object.id())
		}
		tracked, err := projection.Members(ctx, checkpoint, ids)
		if err != nil {
			return false, err
		}
		removed, err = s.verifyMailboxRemovals(ctx, tracked, hints)
		if err != nil {
			return false, err
		}
	}
	if err := projection.Apply(ctx, checkpoint, core.Cursor(page.Next), observed, removed); err != nil {
		return false, err
	}
	return page.More, nil
}

type deltaObject struct {
	kind core.Kind
	ref  string
}

func (o deltaObject) id() core.ID {
	if o.kind == core.Folder {
		return core.ID("f:" + o.ref)
	}
	return core.ID("m:" + o.ref)
}

func (s *Session) observeMailboxChanges(ctx context.Context, page mailboxDelta) ([]core.NativeObject, error) {
	observations := make([]core.NativeObject, 0, len(page.ChangedFolders)+len(page.ChangedMessages))
	for _, group := range []struct {
		kind core.Kind
		refs []string
	}{
		{core.Folder, page.ChangedFolders},
		{core.Message, page.ChangedMessages},
	} {
		for _, ref := range group.refs {
			object, err := s.observeDeltaObject(ctx, deltaObject{group.kind, ref})
			if errors.Is(err, core.ErrNotFound) {
				// The page may be older than a concurrent delete. Re-read
				// from the committed token to obtain its tombstone.
				return nil, core.ErrConflict
			}
			if err != nil {
				return nil, err
			}
			observations = append(observations, object)
		}
	}
	return observations, nil
}

type removalHint struct {
	object deltaObject
	typed  bool
}

func mailboxRemovalHints(page mailboxDelta) []removalHint {
	hints := make([]removalHint, 0, len(page.DeletedMessages)+len(page.DeletedFolders)+2*len(page.OtherRemovals))
	for _, ref := range page.DeletedMessages {
		hints = append(hints, removalHint{deltaObject{core.Message, ref}, true})
	}
	for _, ref := range page.DeletedFolders {
		hints = append(hints, removalHint{deltaObject{core.Folder, ref}, true})
	}
	for _, ref := range page.OtherRemovals {
		hints = append(hints, removalHint{deltaObject{core.Message, ref}, false}, removalHint{deltaObject{core.Folder, ref}, false})
	}
	return hints
}

func (s *Session) verifyMailboxRemovals(ctx context.Context, tracked map[core.ID]core.NativeObject, hints []removalHint) ([]core.ID, error) {
	var removed []core.ID
	for _, hint := range hints {
		id := hint.object.id()
		if id == "f:1" {
			return nil, core.ErrResync
		}
		if _, exists := tracked[id]; !exists {
			continue
		}
		_, err := s.observeDeltaObject(ctx, hint.object)
		switch {
		case errors.Is(err, core.ErrNotFound):
			removed = append(removed, id)
		case err != nil:
			return nil, err
		case hint.typed:
			// Whole-mailbox typed tombstones should agree with a direct
			// lookup. An aggregate hint about a live object is inconclusive.
			return nil, core.ErrConflict
		}
	}
	return removed, nil
}

// Read current membership and ms without fetching raw MIME or changing flags.
// The observation may be newer than the page. Only the page token is committed;
// a later page with the same observation will be handled as a repeat.
func (s *Session) observeDeltaObject(ctx context.Context, object deltaObject) (core.NativeObject, error) {
	var item node
	var err error
	switch object.kind {
	case core.Message:
		item, err = s.getMessageNode(ctx, object.ref, "1")
	case core.Folder:
		selector := attr(element(mailNS, "folder", ""), "l", object.ref)
		request := attr(attr(attr(element(mailNS, "GetFolderRequest", "", selector), "depth", "0"), "tr", "0"), "needGranteeName", "0")
		var response node
		response, err = s.request(ctx, mailNS, "GetFolder", "", request)
		if err == nil {
			item, err = exactMailItem(response, "folder", object.ref)
		}
	default:
		return core.NativeObject{}, core.ErrInvalid
	}
	if err != nil {
		return core.NativeObject{}, semanticError(err)
	}
	revision, ok := syncNumber(item.get("ms"))
	if !ok {
		return core.NativeObject{}, ErrProtocol
	}
	var parent core.ID
	if object.id() != "f:1" {
		nativeParent := item.get("l")
		if !nativeID.MatchString(nativeParent) {
			return core.NativeObject{}, ErrProtocol
		}
		parent = (deltaObject{core.Folder, nativeParent}).id()
	}
	return core.NativeObject{ID: object.id(), Parent: parent, Kind: object.kind, Revision: revision}, nil
}
