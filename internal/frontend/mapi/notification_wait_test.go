// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bufio"
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"ewsgo/internal/core"
)

type notificationFixtureFeed struct {
	hints   chan struct{}
	started chan struct{}
	stopped chan struct{}
}

func (feed *notificationFixtureFeed) WatchMailbox(ctx context.Context, _ core.Session) (<-chan struct{}, func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, nil, err
	}
	feed.started <- struct{}{}
	return feed.hints, func() { feed.stopped <- struct{}{} }, nil
}

type notificationFixtureMailbox struct {
	*messageFixtureMailbox
	newItems atomic.Bool
}

type notificationWaitRecorder struct {
	http.ResponseWriter
	results chan<- bool
}

func (r notificationWaitRecorder) Unwrap() http.ResponseWriter { return r.ResponseWriter }
func (r notificationWaitRecorder) RecordNotificationWait(pending bool) {
	r.results <- pending
}

func (box *notificationFixtureMailbox) ReadMailContents(ctx context.Context, folder core.ID) (core.MailContents, error) {
	contents, err := box.messageFixtureMailbox.ReadMailContents(ctx, folder)
	if err == nil && box.newItems.Load() {
		contents.Messages = append(contents.Messages, messageTestObject("native-notification-test", folder))
	}
	return contents, err
}

func TestNotificationWaitHTTPWakesSubscribedTableAndPreservesExecuteSequence(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	table, _ := openTableTest(t, f, 0)
	f.run(tableColumnsWire(2, 0x674a0014))
	f.run(tableQueryWire(2, 0, 1, 1))
	if !table.subscribed {
		t.Fatal("table is not subscribed")
	}
	native := &notificationFixtureMailbox{messageFixtureMailbox: box}
	session := &testSession{native}
	contextOnly := &http.Cookie{Name: contextCookie, Value: f.state.id}
	cookies := []*http.Cookie{contextOnly, {Name: sequenceCookie, Value: f.state.sequence}}
	feed := &notificationFixtureFeed{make(chan struct{}, 1), make(chan struct{}, 1), make(chan struct{}, 1)}
	f.transport.EnableNotificationWait(feed)
	f.transport.EnableSynchronizationOptionsTrace()
	completions := make(chan bool, 1)
	host := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		f.transport.Mailbox(notificationWaitRecorder{w, completions}, r, session)
	}))
	host.Config.WriteTimeout = 2 * time.Second
	host.StartTLS()
	defer host.Close()
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()

	wait := func(ctx context.Context) (*http.Response, *bufio.Reader) {
		t.Helper()
		request, err := http.NewRequestWithContext(ctx, "POST", host.URL+"/mapi/emsmdb/?MailboxId="+f.identity.RoutingID, bytes.NewReader(make([]byte, 8)))
		if err != nil {
			t.Fatal(err)
		}
		request.Header.Set("Content-Type", "application/mapi-http")
		request.Header.Set("X-RequestType", "NotificationWait")
		request.Header.Set("X-RequestId", "notify-test:1")
		request.AddCookie(contextOnly)
		response, err := host.Client().Do(request)
		if err != nil {
			t.Fatal(err)
		}
		reader := bufio.NewReader(response.Body)
		if line, err := reader.ReadString('\n'); err != nil || line != "PROCESSING\r\n" {
			response.Body.Close()
			t.Fatalf("expected immediate PROCESSING, got %q (%v)", line, err)
		}
		if response.Header.Get("X-ResponseCode") != "0" || response.Header.Get("X-PendingPeriod") != "15000" ||
			len(response.TransferEncoding) != 1 || response.TransferEncoding[0] != "chunked" || len(response.Cookies()) != 1 ||
			response.Cookies()[0].Name != contextCookie {
			response.Body.Close()
			t.Fatal("notification framing or sequence cookie isolation")
		}
		select {
		case <-feed.started:
		case <-ctx.Done():
			response.Body.Close()
			t.Fatal("wait did not attach the mailbox watch")
		}
		return response, reader
	}
	first, reader := wait(ctx)
	defer first.Body.Close()
	// The wait is active when another request uses, then rotates, its sequence.
	emptyExecute, _ := (Execute{MaxOutput: 4096}).Marshal()
	concurrent := transportRequest(t, f.transport, session, "Execute", emptyExecute, cookies)
	if len(storeResponse(t, requireTransportCode(t, concurrent, 0)).Operations) != 0 {
		t.Fatal("unchanged mailbox queued a notification")
	}
	cookies = concurrent.Result().Cookies()
	requireTransportCode(t, transportRequest(t, f.transport, session, "NotificationWait", make([]byte, 8), cookies), 15)
	native.newItems.Store(true)
	feed.hints <- struct{}{}
	raw, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	meta, payload, ok := bytes.Cut(raw, []byte("\r\n\r\n"))
	if !ok || !strings.HasPrefix(string(meta), "DONE\r\nX-ResponseCode: 0\r\n") ||
		len(payload) != 16 || binary.LittleEndian.Uint32(payload[8:]) != 1 {
		t.Fatal("the changed view did not wake NotificationWait")
	}
	select {
	case pending := <-completions:
		if !pending {
			t.Fatal("EventPending missing from wait diagnostics")
		}
	case <-ctx.Done():
		t.Fatal("missing wait completion diagnostic")
	}
	<-feed.stopped
	delivered := transportRequest(t, f.transport, session, "Execute", emptyExecute, cookies)
	if len(requireTableChanged(t, storeResponse(t, requireTransportCode(t, delivered, 0)).Operations, f.handles[2])) != 0 {
		t.Fatal("expected a standalone TableChanged")
	}
	cookies = delivered.Result().Cookies()

	secondCtx, stopSecond := context.WithCancel(ctx)
	defer stopSecond()
	second, _ := wait(secondCtx)
	stopSecond()
	second.Body.Close()
	select {
	case <-feed.stopped:
	case <-ctx.Done():
		t.Fatal("client cancellation did not release the watch")
	}
	// Wait cleanup completes before context retirement. Revocation and scope
	// enforcement also apply when the previous request's client disappeared.
	foreign := session.Identity()
	foreign.Scope.Account = "foreign"
	requireTransportCode(t, transportRequest(t, f.transport, scopedSession{session, foreign}, "NotificationWait", make([]byte, 8), cookies), 10)
	requireTransportCode(t, transportRequest(t, f.transport, session, "Disconnect", make([]byte, 4), cookies), 0)
	requireTransportCode(t, transportRequest(t, f.transport, session, "NotificationWait", make([]byte, 8), cookies), 10)
}

func TestNotificationWaitKeepsContextAliveAndReconnectRevokesIt(t *testing.T) {
	states := newContexts()
	now := time.Unix(1700000000, 0)
	states.now = func() time.Time { return now }
	scope := core.Scope{Backend: "fixture", Account: "one"}
	state, code := states.create(scope, "/mapi/emsmdb/", "", Connect{})
	if code != 0 {
		t.Fatal("create context", code)
	}
	states.finish(state, false)
	_, revoked, code := states.beginWait(scope, state.id)
	if code != 0 {
		t.Fatal("start wait", code)
	}
	now = now.Add(sessionTTL + time.Second)
	states.mu.Lock()
	states.prune(now)
	states.mu.Unlock()
	got, code := states.acquire(scope, state.endpoint, state.id, state.sequence)
	if code != 0 || got != state {
		t.Fatal("context expired while NotificationWait was pending")
	}
	states.finish(got, false)
	states.endWait(state)
	if state.expires != now.Add(sessionTTL) || state.waiting != nil {
		t.Fatal("wait completion did not refresh the context")
	}
	select {
	case <-revoked:
		t.Fatal("finishing a wait revoked a valid context")
	default:
	}
	_, revoked, code = states.beginWait(scope, state.id)
	if code != 0 {
		t.Fatal("start second wait", code)
	}
	next, code := states.create(scope, state.endpoint, state.id, Connect{})
	if code != 0 || next == state {
		t.Fatal("reconnect did not replace the context")
	}
	select {
	case <-revoked:
	default:
		t.Fatal("reconnect did not terminate the previous context's wait")
	}
	states.endWait(state)
	if states.entries[next.id] != next {
		t.Fatal("old wait cleanup affected the new context")
	}
}
