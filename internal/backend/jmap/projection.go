// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package jmap

import (
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"time"

	"ewsgo/internal/core"
)

type nativeState struct {
	Mailboxes string
	Emails    string
	Readable  map[string]bool
}

func (s nativeState) cursor() core.Cursor {
	data, _ := json.Marshal(s)
	return core.Cursor(data)
}

// Refresh advances the JMAP cursors and the shared journal together. A
// complete fenced snapshot seeds the projection or replaces expired cursors.
func Refresh(ctx context.Context, account core.Session, store core.NativeProjectionStore, instance string) error {
	s, ok := account.(*session)
	if !ok {
		return core.ErrInvalid
	}
	ctx, cancel := context.WithTimeout(ctx, 25*time.Second)
	defer cancel()
	projection, err := store.OpenNativeProjection(ctx, s.identity.Scope, instance)
	if err != nil {
		return err
	}
	for attempt := 0; attempt < 3; attempt++ {
		err = s.refresh(ctx, projection)
		if !errors.Is(err, core.ErrConflict) {
			return err
		}
	}
	return err
}

func (s *session) refresh(ctx context.Context, projection core.NativeProjection) error {
	checkpoint, err := projection.Checkpoint(ctx)
	if err != nil {
		return err
	}
	var state nativeState
	if checkpoint.Cursor != "" && json.Unmarshal([]byte(checkpoint.Cursor), &state) == nil &&
		validState(state.Mailboxes) && validState(state.Emails) && state.Readable != nil {
		err = s.ingestChanges(ctx, projection, checkpoint, state)
		if !errors.Is(err, core.ErrResync) {
			return err
		}
	}
	return s.reconcile(ctx, projection, checkpoint)
}

type observation struct {
	revision uint64
	before   map[core.ID]core.NativeObject
	objects  []core.NativeObject
}

func newObservation(baseline core.NativeBaseline) observation {
	return observation{
		revision: uint64(baseline.Checkpoint.Revision) + 1,
		before:   make(map[core.ID]core.NativeObject, len(baseline.Objects)),
	}
}

func (p *observation) observe(id, parent core.ID, kind core.Kind, digest [32]byte) {
	fingerprint := hex.EncodeToString(digest[:])
	before := p.before[id]
	if before.Fingerprint != fingerprint || before.Parent != parent {
		p.objects = append(p.objects, core.NativeObject{
			ID: id, Parent: parent, Kind: kind, Revision: p.revision, Fingerprint: fingerprint,
		})
	}
	delete(p.before, id)
}

func (p *observation) observeEmail(e email, readable map[string]bool, count *int) error {
	digest := e.digest()
	for id := range e.MailboxIDs {
		canRead, exists := readable[id]
		if !exists {
			return core.ErrConflict
		}
		if canRead {
			*count = *count + 1
			if *count > maxPlacements {
				return core.ErrLimit
			}
			p.observe(placementID(e.ID, id), folderID(id), core.Message, digest)
		}
	}
	return nil
}

func (p observation) removed() []core.ID {
	removed := make([]core.ID, 0, len(p.before))
	for id := range p.before {
		removed = append(removed, id)
	}
	return removed
}

func readPermissions(folders []folder) map[string]bool {
	result := make(map[string]bool, len(folders))
	for _, f := range folders {
		result[f.ID] = f.Rights.Read
	}
	return result
}

func (s *session) reconcile(ctx context.Context, projection core.NativeProjection, checkpoint core.NativeCheckpoint) error {
	baseline, err := projection.Baseline(ctx)
	if err != nil {
		return err
	}
	if baseline.Checkpoint != checkpoint {
		return core.ErrConflict
	}
	folders, err := s.getFolders(ctx)
	if err != nil {
		return err
	}
	emails, err := s.getEmails(ctx, []string{})
	if err != nil {
		return err
	}
	state := nativeState{folders.State, emails.State, readPermissions(folders.List)}
	changes := newObservation(baseline)
	for _, object := range baseline.Objects {
		changes.before[object.ID] = object
	}
	for _, f := range folders.List {
		o := f.object()
		changes.observe(o.ID, o.Parent, o.Kind, f.digest())
	}
	placements := 0
	err = s.scanIDs(ctx, "Email", maxEmails, func(ids []string) error {
		page, err := s.getEmails(ctx, ids)
		if err != nil {
			return err
		}
		if page.State != state.Emails || len(page.NotFound) != 0 {
			return core.ErrConflict
		}
		for _, e := range page.List {
			if err := changes.observeEmail(e, state.Readable, &placements); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	if err := s.fenceStates(ctx, state); err != nil {
		return err
	}
	return projection.Reconcile(ctx, baseline, state.cursor(), changes.objects, changes.removed())
}

func (s *session) fenceStates(ctx context.Context, state nativeState) error {
	folders, err := s.readFolders(ctx, []string{})
	if err != nil {
		return err
	}
	emails, err := s.getEmails(ctx, []string{})
	if err != nil {
		return err
	}
	if len(folders.List)+len(folders.NotFound) != 0 {
		return errProtocol
	}
	if folders.State != state.Mailboxes || emails.State != state.Emails {
		return core.ErrConflict
	}
	return nil
}

func (s *session) scanIDs(ctx context.Context, kind string, maxObjects int, visit func([]string) error) error {
	seen := make(map[string]bool)
	var queryState string
	total := -1
	for position := 0; ; {
		var result struct {
			AccountID  string   `json:"accountId"`
			QueryState string   `json:"queryState"`
			Position   *int     `json:"position"`
			IDs        []string `json:"ids"`
			Total      *int     `json:"total"`
		}
		args := map[string]any{"position": position, "limit": s.getLimit, "calculateTotal": true}
		if kind == "Email" {
			args["collapseThreads"] = false
		}
		err := s.call(ctx, kind+"/query", args, &result)
		if err != nil {
			return err
		}
		if result.AccountID != s.account || !validState(result.QueryState) || result.IDs == nil ||
			result.Total == nil || *result.Total < 0 || result.Position == nil ||
			len(result.IDs) > s.getLimit {
			return errProtocol
		}
		if *result.Total > maxObjects {
			return core.ErrLimit
		}
		if position == 0 {
			queryState, total = result.QueryState, *result.Total
		}
		if result.QueryState != queryState || *result.Total != total || *result.Position != position {
			return core.ErrConflict
		}
		if position+len(result.IDs) > total || len(result.IDs) == 0 && position < total {
			return errProtocol
		}
		for _, id := range result.IDs {
			if !nativeID.MatchString(id) || seen[id] {
				return errProtocol
			}
			seen[id] = true
		}
		if len(result.IDs) != 0 {
			if err := visit(result.IDs); err != nil {
				return err
			}
		}
		position += len(result.IDs)
		if position == total {
			return nil
		}
	}
}
