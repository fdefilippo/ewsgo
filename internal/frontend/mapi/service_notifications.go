// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"sort"

	"ewsgo/internal/core"
)

const localNotificationLimit = 256

// The journal shares the lifetime of the RAM store and publishes only after a
// local commit. An expired cursor requires a new subscription.
type localNotificationJournal struct {
	events  []objectNotification
	head    uint64
	changed chan struct{}
}

func (j *localNotificationJournal) publish(events ...objectNotification) {
	if len(events) == 0 {
		return
	}
	if j.events == nil {
		j.events = make([]objectNotification, localNotificationLimit)
	}
	for _, event := range events {
		j.events[j.head%localNotificationLimit] = event
		j.head++
	}
	j.signal()
}

func (j *localNotificationJournal) signal() {
	if j.changed != nil {
		close(j.changed)
		j.changed = nil
	}
}

func (s *ramStore) localNotificationSignal() <-chan struct{} {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.localNotifications.changed == nil {
		s.localNotifications.changed = make(chan struct{})
	}
	return s.localNotifications.changed
}

func (s *ramStore) localNotificationPage(after uint64) ([]objectNotification, bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	j := &s.localNotifications
	if after > j.head || j.head-after > localNotificationLimit {
		return nil, false, core.ErrResync
	}
	count := min(j.head-after, notificationPageSize)
	if count == 0 {
		return nil, false, nil
	}
	events := make([]objectNotification, count)
	for i := range events {
		events[i] = j.events[(after+uint64(i))%localNotificationLimit]
	}
	return events, j.head-after > count, nil
}

func (s notificationSubscription) matchesLocal(n objectNotification) bool {
	if s.mask&(n.flag&^notifyMessage) == 0 {
		return false
	}
	if s.mid != 0 {
		return s.fid == n.fid && s.mid == n.mid
	}
	return s.fid == 0 || s.fid == n.fid || s.fid == n.parent
}

func (s *ramStore) scanLocalNotifications(ctx context.Context, session core.Session, target objectNotificationTarget, available int, result *notificationScan) error {
	events, more, err := s.localNotificationPage(target.sub.localAfter)
	if err != nil {
		return err
	}
	checked := map[uint64]bool{}
	authorized := false
	for _, event := range events {
		if target.sub.matchesLocal(event) {
			if !authorized {
				if session == nil || session.Identity().Scope != s.scope || session.Mailbox() == nil || session.Mailbox().Root() != s.root {
					return core.ErrAccessDenied
				}
				if err := hierarchyOwner(ctx, session.Mailbox()); err != nil {
					return err
				}
				authorized = true
			}
			visible, known := checked[event.fid]
			if !known {
				_, err := s.folder(ctx, session, event.fid)
				if err != nil && !errors.Is(err, core.ErrNotFound) {
					return err
				}
				visible = err == nil
				checked[event.fid] = visible
			}
			if visible {
				if len(event.appendTo(nil, target)) > available-len(result.output) {
					result.pending = true
					return nil
				}
				result.emit(event, target)
			}
		}
		result.localAfter++
	}
	result.more = more
	return nil
}

func (s *ramStore) publishFAIChanges(folder uint64, before, after map[core.ID]faiRecord) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var events []objectNotification
	for id, record := range after {
		if record.folder != folder {
			continue
		}
		mid := s.byMessage[messageKey{id: id, associated: true}]
		if mid == 0 {
			continue
		}
		previous, existed := before[id]
		var flag uint16
		switch {
		case record.deleted && existed && !previous.deleted:
			flag = notifyDeleted
		case !record.deleted && (!existed || previous.deleted):
			flag = notifyCreated
		case !record.deleted && previous.object.Revision != record.object.Revision:
			flag = notifyModified
		default:
			continue
		}
		events = append(events, objectNotification{flag: flag | notifyMessage, fid: folder, mid: mid})
	}
	if len(events) == 0 {
		return
	}
	sort.Slice(events, func(i, j int) bool { return events[i].mid < events[j].mid })
	events = append(events, objectNotification{flag: notifyModified, fid: folder})
	s.localNotifications.publish(events...)
}
