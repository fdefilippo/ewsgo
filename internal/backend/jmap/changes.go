// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package jmap

import (
	"context"
	"slices"
	"strings"

	"ewsgo/internal/core"
)

type changeSet struct {
	state     string
	touched   map[string]bool
	destroyed map[string]bool
}

func (s *session) changes(ctx context.Context, kind, since string, limit int) (changeSet, error) {
	result := changeSet{since, make(map[string]bool), make(map[string]bool)}
	states := map[string]bool{since: true}
	for count := 0; ; {
		var page struct {
			AccountID      string
			OldState       string
			NewState       string
			HasMoreChanges *bool
			Created        []string
			Updated        []string
			Destroyed      []string
		}
		err := s.call(ctx, kind+"/changes", map[string]any{"sinceState": result.state, "maxChanges": s.getLimit}, &page)
		if err != nil {
			return changeSet{}, err
		}
		if page.AccountID != s.account || page.OldState != result.state || !validState(page.NewState) ||
			page.HasMoreChanges == nil || page.Created == nil || page.Updated == nil || page.Destroyed == nil {
			return changeSet{}, errProtocol
		}
		pageSize := len(page.Created) + len(page.Updated) + len(page.Destroyed)
		if pageSize > s.getLimit || page.NewState == result.state && (*page.HasMoreChanges || pageSize > 0) ||
			page.NewState != result.state && states[page.NewState] {
			return changeSet{}, errProtocol
		}
		count += max(1, pageSize)
		if count > limit {
			return changeSet{}, core.ErrResync
		}
		for _, changed := range [][]string{page.Created, page.Updated, page.Destroyed} {
			for _, id := range changed {
				if !nativeID.MatchString(id) {
					return changeSet{}, errProtocol
				}
				result.touched[id] = true
			}
		}
		for _, id := range page.Destroyed {
			result.destroyed[id] = true
		}
		result.state = page.NewState
		if !*page.HasMoreChanges {
			return result, nil
		}
		states[result.state] = true
	}
}

func (s *session) ingestChanges(ctx context.Context, projection core.NativeProjection, checkpoint core.NativeCheckpoint, previous nativeState) error {
	mailboxes, err := s.changes(ctx, "Mailbox", previous.Mailboxes, maxFolders*3)
	if err != nil {
		return err
	}
	emails, err := s.changes(ctx, "Email", previous.Emails, maxEmails*3)
	if err != nil {
		return err
	}
	if mailboxes.state == previous.Mailboxes && emails.state == previous.Emails {
		return nil
	}

	state := nativeState{mailboxes.state, emails.state, previous.Readable}
	var folders []folder
	if state.Mailboxes != previous.Mailboxes {
		current, err := s.getFolders(ctx)
		if err != nil {
			return err
		}
		if current.State != state.Mailboxes {
			return core.ErrConflict
		}
		folders = current.List
		state.Readable = readPermissions(folders)
		for _, f := range folders {
			if mailboxes.destroyed[f.ID] {
				return core.ErrConflict
			}
			if f.Rights.Read && !previous.Readable[f.ID] && f.TotalEmails > 0 {
				// Existing Email objects can become readable without an
				// Email state change when a mailbox is exposed.
				return core.ErrResync
			}
		}
		for id := range previous.Readable {
			if _, present := state.Readable[id]; !present && !mailboxes.destroyed[id] {
				return core.ErrResync
			}
		}
	}

	baseline, err := projection.Baseline(ctx)
	if err != nil {
		return err
	}
	if baseline.Checkpoint != checkpoint {
		return core.ErrConflict
	}
	delta := newObservation(baseline)
	placements := 0
	for _, object := range baseline.Objects {
		if object.Kind == core.Folder {
			if state.Mailboxes != previous.Mailboxes {
				delta.before[object.ID] = object
			}
			continue
		}
		id, mailbox, ok := placementParts(object.ID)
		if !ok {
			return core.ErrResync
		}
		if emails.touched[id] || !state.Readable[mailbox] {
			delta.before[object.ID] = object
		} else {
			placements++
		}
	}
	for _, f := range folders {
		o := f.object()
		delta.observe(o.ID, o.Parent, o.Kind, f.digest())
	}

	ids := make([]string, 0, len(emails.touched))
	for id := range emails.touched {
		if !emails.destroyed[id] {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	for pageIDs := range slices.Chunk(ids, s.getLimit) {
		page, err := s.getEmails(ctx, pageIDs)
		if err != nil {
			return err
		}
		if page.State != state.Emails || len(page.NotFound) != 0 {
			return core.ErrConflict
		}
		for _, e := range page.List {
			if err := delta.observeEmail(e, state.Readable, &placements); err != nil {
				return err
			}
		}
	}
	if err := s.fenceStates(ctx, state); err != nil {
		return err
	}
	return projection.Apply(ctx, checkpoint, state.cursor(), delta.objects, delta.removed())
}

func placementParts(id core.ID) (emailID, mailbox string, valid bool) {
	value, prefix := strings.CutPrefix(string(id), "message:")
	emailID, mailbox, delimited := strings.Cut(value, ":")
	return emailID, mailbox, prefix && delimited && nativeID.MatchString(emailID) && nativeID.MatchString(mailbox)
}
