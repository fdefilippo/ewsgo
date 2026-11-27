// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"errors"
	"io"
	"net/http"
	"reflect"
	"slices"
	"sort"
	"strconv"
	"time"

	"ewsgo/internal/core"
)

const (
	notificationTimeout = 5 * time.Minute
	notificationPeriod  = 15 * time.Second
	probeTimeout        = 5 * time.Second
	probeRetry          = time.Second
)

// EnableNotificationWait attaches mailbox hints to updating Table handles.
func (t *SessionTransport) EnableNotificationWait(wakeups core.MailboxWakeups) {
	t.wakeups = wakeups
}

func parseNotificationWait(data []byte) error {
	r := reader{data: data}
	r.u32() // Reserved flags are ignored (MS-OXCMAPIHTTP 2.2.4.4.1).
	r.blob(MaxAuxiliary)
	return r.done()
}

func (t *SessionTransport) notificationWait(w http.ResponseWriter, r *http.Request, started time.Time, session core.Session, id string) {
	state, revoked, code := t.contexts.beginWait(session.Identity().Scope, id)
	if code != 0 {
		transportResponse(w, r, started, code, nil, nil)
		return
	}
	defer t.contexts.endWait(state)
	outcome := core.ActivityFailure
	defer func() {
		core.RecordActivity(r.Context(), core.Activity{Layer: "mapi", Operation: "NotificationWait", Result: outcome, Duration: time.Since(started)})
	}()
	controller := http.NewResponseController(w)
	if err := controller.SetWriteDeadline(time.Now().Add(notificationTimeout + 10*time.Second)); err != nil && !errors.Is(err, http.ErrNotSupported) {
		return
	}
	h := transportHeaders(w, r, 0)
	h.Set("Content-Type", "application/mapi-http")
	h.Set("X-PendingPeriod", strconv.FormatInt(notificationPeriod.Milliseconds(), 10))
	h.Set("X-ExpirationInfo", strconv.FormatInt(sessionTTL.Milliseconds(), 10))
	// This channel never consumes or overwrites the Execute sequencing cookie.
	http.SetCookie(w, &http.Cookie{Name: contextCookie, Value: state.id, Path: state.endpoint, Secure: true, HttpOnly: true})
	if _, err := io.WriteString(w, "PROCESSING\r\n"); err != nil || controller.Flush() != nil {
		return
	}

	ctx, cancel := context.WithTimeout(r.Context(), notificationTimeout)
	type waitResult struct {
		changed bool
		err     error
	}
	result := make(chan waitResult, 1)
	go func() {
		changed, err := t.awaitNotification(ctx, session, state)
		result <- waitResult{changed, err}
		close(result)
	}()
	defer func() {
		cancel()
		// A canceled HTTP request must still join the active scan and finish
		// native cleanup before BasicTLS releases the session.
		for range result {
		}
	}()
	heartbeat := time.NewTicker(notificationPeriod)
	defer heartbeat.Stop()
	for {
		var pending uint32
		var code uint32
		select {
		case <-r.Context().Done():
			return
		case <-revoked:
			code = 10
		case completed := <-result:
			if completed.err != nil && ctx.Err() == nil {
				code = 1
			}
			if completed.changed {
				pending = 1
			}
		case <-ctx.Done():
			if r.Context().Err() != nil {
				return
			}
		case <-heartbeat.C:
			if _, err := io.WriteString(w, "PENDING\r\n"); err != nil || controller.Flush() != nil {
				return
			}
			continue
		}
		// The wait and a disconnect/reconnect can finish simultaneously.
		select {
		case <-revoked:
			code = 10
		default:
		}
		if writeTransportDone(w, started, code) != nil {
			return
		}
		if code != 0 {
			outcome = core.ActivityRejected
			return
		}
		payload := append32(append32(append32(append32(nil, 0), 0), pending), 0)
		if _, err := w.Write(payload); err == nil {
			outcome = core.ActivitySuccess
			if t.synchronizationOptionsTrace {
				if recorder, ok := w.(interface{ RecordNotificationWait(bool) }); ok {
					recorder.RecordNotificationWait(pending != 0)
				}
			}
		}
		return
	}
}

// Only the scan reads the backend. The waiting context is never acquired as an
// Execute: it borrows immutable table selectors and publishes under a short
// lock when no Execute is mutating the context.
func (t *SessionTransport) awaitNotification(ctx context.Context, session core.Session, state *contextState) (bool, error) {
	var after uint32
	for ctx.Err() == nil {
		hints, release, err := t.wakeups.WatchMailbox(ctx, session)
		if err != nil {
			return false, err
		}
		rejoin := false
		for !rejoin && ctx.Err() == nil {
			changed, retry, local, err := t.checkObjectNotifications(ctx, session, state)
			if err != nil || changed {
				release()
				return changed, err
			}
			changed, tableRetry := t.checkNotificationTables(ctx, session, state, &after)
			if changed {
				release()
				return true, nil
			}
			var again <-chan time.Time
			var timer *time.Timer
			if retry || tableRetry {
				timer = time.NewTimer(probeRetry)
				again = timer.C
			}
			select {
			case <-ctx.Done():
			case _, ok := <-hints:
				rejoin = !ok
			case <-again:
			case <-local:
			}
			if timer != nil {
				timer.Stop()
			}
		}
		release()
	}
	return false, ctx.Err()
}

func (t *SessionTransport) checkObjectNotifications(ctx context.Context, session core.Session, state *contextState) (bool, bool, <-chan struct{}, error) {
	targets, sequence, retry, release := t.contexts.objectNotificationTargets(state)
	if len(targets) == 0 {
		return false, retry, nil, nil
	}
	defer release()
	// All logons on this transport share the authenticated mailbox store.
	// Obtain the signal before scanning so a commit cannot be missed.
	local := targets[0].store.localNotificationSignal()
	scan, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	hasNative := hasNativeNotificationTarget(targets)
	deferredRefreshed := false
	if target := deferredActionNotificationTarget(targets); target != nil {
		_, err := target.store.observeDeferredActions(scan, session, target.sub.deferred)
		deferredRefreshed = err == nil
		retry = retry || err != nil
	}
	native := hasNative && t.notificationSource.Refresh(scan, session) == nil
	retry = retry || hasNative && !native
	for _, target := range targets {
		result, err := t.scanObjectNotifications(scan, session, target, 0, native)
		if errors.Is(err, core.ErrResync) {
			// The retained window was lost. The wait reports failure so the
			// client can reconnect or register a new subscription.
			return false, false, nil, err
		}
		if err != nil {
			retry = true
			continue
		}
		current := t.contexts.noteObjectNotificationScan(state, sequence, target, result, native || deferredRefreshed)
		if current && result.pending {
			return true, false, nil, nil
		}
		retry = retry || !current || result.more
	}
	return false, retry, local, nil
}

func (s *contexts) objectNotificationTargets(state *contextState) ([]objectNotificationTarget, string, bool, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries[state.id] != state || state.busy || state.mailbox == nil {
		return nil, "", state.busy, nil
	}
	targets := state.mailbox.notificationTargets()
	if len(targets) == 0 {
		return nil, "", false, nil
	}
	return targets, state.sequence, false, targets[0].store.pinForScan()
}

func (s *contexts) noteObjectNotificationScan(state *contextState, sequence string, target objectNotificationTarget, result notificationScan, refreshed bool) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries[state.id] != state || state.busy || state.mailbox == nil || state.sequence != sequence {
		return false
	}
	sub := state.mailbox.objects[target.handle].notification
	if sub == nil || sub.position != target.sub.position || sub.localAfter != target.sub.localAfter {
		return false
	}
	sub.advance(result)
	if refreshed {
		state.mailbox.notificationRefresh = time.Now()
	}
	return true
}

type notificationTable struct {
	handle     uint32
	binding    storeBinding
	fid        uint64
	flags      byte
	hierarchy  bool
	aggregates bool
}

func (s *contexts) notificationTables(state *contextState) ([]notificationTable, string, bool, func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries[state.id] != state || state.busy || state.mailbox == nil {
		return nil, "", state.busy, nil
	}
	var result []notificationTable
	for handle, obj := range state.mailbox.objects {
		if obj.binding.store == nil || !obj.table.updating() || !obj.table.subscribed {
			continue
		}
		result = append(result, notificationTable{handle, obj.binding, obj.fid, obj.table.flags, obj.table.hierarchy, slices.ContainsFunc(obj.table.columns, isFolderAggregateTag)})
	}
	if len(result) == 0 {
		return nil, "", false, nil
	}
	return result, state.sequence, false, result[0].binding.store.pinForScan()
}

func (t *SessionTransport) checkNotificationTables(ctx context.Context, session core.Session, state *contextState, after *uint32) (bool, bool) {
	tables, sequence, retry, release := t.contexts.notificationTables(state)
	if len(tables) == 0 {
		return false, retry
	}
	defer release()
	sort.Slice(tables, func(i, j int) bool { return tables[i].handle < tables[j].handle })
	next := sort.Search(len(tables), func(i int) bool { return tables[i].handle > *after })
	scan, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	for i := range tables {
		if scan.Err() != nil {
			return false, true
		}
		target := tables[(next+i)%len(tables)]
		var view *contentsTable
		var err error
		if target.hierarchy {
			view, err = captureHierarchyTable(scan, session, target.binding, target.fid, target.flags, t.folderAggregates && target.aggregates)
		} else {
			view, err = captureContentsTable(scan, session, target.binding, target.fid, target.flags)
		}
		*after = target.handle
		if err != nil {
			continue
		}
		changed, busy := t.contexts.noteTableChange(state, sequence, target.handle, view)
		view.close()
		if changed {
			return true, false
		}
		retry = retry || busy
	}
	return false, retry
}

func (s *contexts) noteTableChange(state *contextState, sequence string, handle uint32, view *contentsTable) (bool, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.entries[state.id] != state || state.busy || state.mailbox == nil {
		return false, state.busy
	}
	if state.sequence != sequence {
		// Execute may already have refreshed or released this view.
		return false, true
	}
	table := state.mailbox.objects[handle].table
	if !table.updating() || !table.subscribed {
		return false, false
	}
	if reflect.DeepEqual(table.baseRows, view.rows) {
		table.hierarchyCreations = view.hierarchyCreations
	} else if !table.suppressHierarchyCreations(view) {
		table.pending = true
	}
	return table.pending, false
}
