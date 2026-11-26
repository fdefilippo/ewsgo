// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"math"
	"slices"
	"strings"
	"time"

	"ewsgo/internal/core"
)

const localCommitTimeMaxID = 0x670a
const deletedCountTotalID = 0x670b

type folderAggregateValues struct {
	committed uint64 // EWSGo observation in FILETIME units.
	removed   uint32 // Verified removals since the first observation.
	count     uint32
}

type folderAggregateState struct {
	folderAggregateValues
	sequence uint64
	children [32]byte
	messages []core.ID // Sorted direct membership.
	lease    *contentsLease
}

func (t *SessionTransport) EnableFolderAggregates() { t.folderAggregates = true }

func isFolderAggregateTag(tag uint32) bool {
	id, kind := uint16(tag>>16), uint16(tag)
	return id == localCommitTimeMaxID && (kind == 0 || kind == 0x0040) ||
		id == deletedCountTotalID && (kind == 0 || kind == 0x0003)
}

func (s *ramStore) observeFolderAggregates(ctx context.Context, session core.Session, folder core.ID) (folderAggregateValues, error) {
	return s.observeFolderAggregatesAt(ctx, session, folder, nil)
}

func (s *ramStore) observeFolderAggregatesAt(ctx context.Context, session core.Session, folder core.ID, marker *core.MailFolderMarker) (folderAggregateValues, error) {
	if folder == "" {
		return folderAggregateValues{}, core.ErrUnsupported
	}
	if session == nil || session.Identity().Scope != s.scope {
		return folderAggregateValues{}, core.ErrAccessDenied
	}
	box := session.Mailbox()
	if box == nil || box.Root() != s.root {
		return folderAggregateValues{}, core.ErrAccessDenied
	}
	var observe func(context.Context, core.ID, *uint64) (core.MailFolderObservation, error)
	shared := strings.HasPrefix(string(folder), core.SharedGroupwareFolderIDPrefix)
	if observer, ok := box.(core.FolderContentsObserver); (s.contactsMetadata || s.calendarMetadata) && ok {
		observe = observer.ObserveFolderContents
	} else if observer, ok := box.(core.MailFolderObserver); ok {
		observe = observer.ObserveMailFolder
	} else {
		return folderAggregateValues{}, core.ErrUnsupported
	}
	s.folderAggregatesMu.Lock()
	defer s.folderAggregatesMu.Unlock()
	if err := ctx.Err(); err != nil {
		return folderAggregateValues{}, err
	}
	old, exists := s.folderAggregates[folder]
	if marker != nil && exists && marker.Sequence == old.sequence && marker.Children == old.children &&
		marker.MessageCount == uint64(len(old.messages)) {
		return old.folderAggregateValues, nil
	}
	var prior *uint64
	if exists {
		prior = &old.sequence
	}
	// Allow one detached membership and the retained map entry during capture.
	const maxMessageIDBytes = 256
	const entryBytes = 512
	lease, err := s.reserveContentsMemory(entryBytes + core.MaxMailContentIndexItems*(maxMessageIDBytes+32))
	if err != nil {
		return folderAggregateValues{}, err
	}
	defer func() { lease.release() }()
	next, err := observe(ctx, folder, prior)
	// A hierarchy row may still report the last verified aggregate. Do
	// not turn a conflicted refresh into a removal or a notification.
	if err != nil {
		if marker != nil && exists && errors.Is(err, core.ErrConflict) && ctx.Err() == nil {
			return old.folderAggregateValues, nil
		}
		return folderAggregateValues{}, err
	}
	if err := ctx.Err(); err != nil {
		return folderAggregateValues{}, err
	}
	if marker != nil && (marker.Sequence != next.Sequence || marker.Children != next.Children || marker.MessageCount != next.MessageCount) {
		if exists {
			return old.folderAggregateValues, nil
		}
		return folderAggregateValues{}, core.ErrConflict
	}
	if exists && next.Sequence < old.sequence {
		return folderAggregateValues{}, core.ErrConflict
	}
	if exists && next.Sequence == old.sequence && !shared {
		if next.MessageCount != uint64(len(old.messages)) {
			return folderAggregateValues{}, core.ErrConflict
		}
		if next.Children != old.children {
			old.children = next.Children
			old.committed = max(old.committed+1, fileTime(time.Now()))
			s.folderAggregates[folder] = old
		}
		return old.folderAggregateValues, nil
	}
	if next.MessageCount > core.MaxMailContentIndexItems || next.MessageCount != uint64(len(next.MessageIDs)) {
		return folderAggregateValues{}, core.ErrInvalid
	}
	slices.Sort(next.MessageIDs)
	retained := entryBytes
	for i, id := range next.MessageIDs {
		if id == "" || len(id) > maxMessageIDBytes || i > 0 && id == next.MessageIDs[i-1] {
			return folderAggregateValues{}, core.ErrInvalid
		}
		retained += len(id) + 32
	}
	// A grant can change the visible membership of a mount without advancing
	// the owner's contents revision.
	if shared && exists && next.Sequence == old.sequence && slices.Equal(old.messages, next.MessageIDs) {
		return old.folderAggregateValues, nil
	}
	removed := old.removed
	for _, id := range old.messages {
		if _, present := slices.BinarySearch(next.MessageIDs, id); !present {
			if removed == math.MaxInt32 {
				return folderAggregateValues{}, core.ErrUnsupported
			}
			removed++
		}
	}
	if err := lease.shrink(retained); err != nil {
		return folderAggregateValues{}, err
	}
	if s.folderAggregates == nil {
		s.folderAggregates = make(map[core.ID]folderAggregateState)
	}
	values := folderAggregateValues{committed: max(old.committed+1, fileTime(time.Now())), removed: removed, count: uint32(next.MessageCount)}
	s.folderAggregates[folder] = folderAggregateState{
		folderAggregateValues: values, sequence: next.Sequence, children: next.Children, messages: next.MessageIDs, lease: lease,
	}
	lease = nil // Ownership moved to the store.
	old.lease.release()
	return values, nil
}
