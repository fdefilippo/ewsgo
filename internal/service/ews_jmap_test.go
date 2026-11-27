// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"encoding/base64"
	"encoding/json"
	"encoding/pem"
	"encoding/xml"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"

	"ewsgo/internal/backend/jmap"
	"ewsgo/internal/storage/sqlite"
)

const (
	jmapFixtureUser = "jmap@example.invalid"
	jmapFixturePass = "synthetic-password"
	jmapFixtureMIME = "From: sender@example.invalid\r\nTo: jmap@example.invalid\r\nSubject: JMAP in two folders\r\n" +
		"Message-ID: <shared@example.invalid>\r\nMIME-Version: 1.0\r\nContent-Type: text/plain; charset=utf-8\r\n\r\nSynthetic JMAP body.\r\n"
)

var jmapFixtureSecondMIME = strings.NewReplacer("JMAP in two folders", "JMAP second message", "shared@", "second@").Replace(jmapFixtureMIME)

type jmapFixture struct {
	mu            sync.Mutex
	removedFromA  bool
	incomplete    bool
	markRead      bool
	destroyed     bool
	losePage      bool
	snapshotCalls int
	deltaPages    int
}

func (f *jmapFixture) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	user, pass, ok := r.BasicAuth()
	if !ok || user != jmapFixtureUser || pass != jmapFixturePass {
		w.WriteHeader(http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()

	const coreURN, mailURN = "urn:ietf:params:jmap:core", "urn:ietf:params:jmap:mail"
	type obj = map[string]any
	switch r.URL.Path {
	case "/.well-known/jmap":
		http.Redirect(w, r, "/jmap/session", http.StatusTemporaryRedirect)
	case "/jmap/session":
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(obj{
			"username": jmapFixtureUser, "state": "session",
			"apiUrl": "api", "downloadUrl": "/jmap/blob/{accountId}/{blobId}/{name}?type={type}",
			"capabilities":    obj{coreURN: obj{"maxObjectsInGet": 1}, mailURN: obj{}},
			"primaryAccounts": obj{mailURN: "personal"},
			"accounts":        obj{"personal": obj{"isPersonal": true, "accountCapabilities": obj{mailURN: obj{}}}},
		})
	case "/jmap/blob/personal/one-blob/message.eml", "/jmap/blob/personal/two-blob/message.eml":
		if r.Method != http.MethodGet || r.URL.Query().Get("type") != "message/rfc822" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "message/rfc822")
		raw := jmapFixtureMIME
		if strings.Contains(r.URL.Path, "two-blob") {
			raw = jmapFixtureSecondMIME
		}
		_, _ = io.WriteString(w, raw)
	case "/jmap/api":
		var request struct {
			Using       []string            `json:"using"`
			MethodCalls [][]json.RawMessage `json:"methodCalls"`
		}
		if r.Method != http.MethodPost || json.NewDecoder(r.Body).Decode(&request) != nil ||
			!reflect.DeepEqual(request.Using, []string{coreURN, mailURN}) ||
			len(request.MethodCalls) != 1 || len(request.MethodCalls[0]) != 3 {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		call := request.MethodCalls[0]
		var method, tag string
		var args struct {
			AccountID string `json:"accountId"`
			IDs       []string
			Position  int
			Limit     int
			Since     string `json:"sinceState"`
			Max       int    `json:"maxChanges"`
		}
		if json.Unmarshal(call[0], &method) != nil || json.Unmarshal(call[1], &args) != nil ||
			json.Unmarshal(call[2], &tag) != nil || args.AccountID != "personal" {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		nativeState := "first"
		inA := 1
		membership := obj{"A": true, "B": true}
		if f.removedFromA {
			nativeState, inA = "second", 0
			delete(membership, "A")
		}
		keywords := obj{}
		unreadInB := 2
		if f.markRead {
			nativeState = "third"
			keywords["$seen"] = true
			unreadInB = 1
		}
		totalInB := 2
		if f.destroyed {
			nativeState, totalInB = "fourth", 1
		}
		result := obj{"accountId": "personal"}
		switch method {
		case "Mailbox/changes", "Email/changes":
			if args.Max != 1 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			resync := func() { method, result = "error", obj{"type": "cannotCalculateChanges"} }
			next, more := nativeState, false
			updated, destroyed := []string{}, []string{}
			switch {
			case f.incomplete:
				resync()
			case args.Since == nativeState:
			case args.Since == "first" && nativeState == "second" && method == "Email/changes":
				next, more, updated = "email-page", true, []string{"one"}
			case args.Since == "email-page" && nativeState == "second" && method == "Email/changes":
				if f.losePage {
					panic(http.ErrAbortHandler)
				}
				updated = []string{"two"}
			case args.Since == "first" && nativeState == "second":
				updated = []string{"A"}
			case method == "Mailbox/changes" && (args.Since == "second" && nativeState == "third" ||
				args.Since == "third" && nativeState == "fourth"):
				updated = []string{"B"}
			case args.Since == "second" && nativeState == "third":
				updated = []string{"one"}
			case args.Since == "third" && nativeState == "fourth":
				destroyed = []string{"one"}
			default:
				resync()
			}
			if method != "error" {
				f.deltaPages++
				result["oldState"], result["newState"], result["hasMoreChanges"] = args.Since, next, more
				result["created"], result["updated"], result["destroyed"] = []string{}, updated, destroyed
			}
		case "Mailbox/get":
			if args.IDs == nil || len(args.IDs) > 1 {
				method, result = "error", obj{"type": "requestTooLarge"}
				break
			}
			all := map[string]obj{
				"A": {"id": "A", "name": "Inbox", "role": "inbox", "parentId": nil, "totalEmails": inA, "unreadEmails": inA, "myRights": obj{"mayReadItems": true}},
				"B": {"id": "B", "name": "Project", "role": nil, "parentId": nil, "totalEmails": totalInB, "unreadEmails": unreadInB, "myRights": obj{"mayReadItems": true}},
			}
			list, missing := []obj{}, []string{}
			for _, id := range args.IDs {
				if folder, ok := all[id]; ok {
					list = append(list, folder)
				} else {
					missing = append(missing, id)
				}
			}
			result["state"], result["list"], result["notFound"] = nativeState, list, missing
		case "Mailbox/query", "Email/query":
			if args.Limit != 1 || args.Position < 0 || args.Position > 1 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			ids := []string{"A", "B"}
			if method == "Email/query" {
				f.snapshotCalls++
				ids = []string{"one", "two"}
			}
			if method == "Email/query" && f.destroyed {
				ids = ids[1:]
			}
			result["queryState"], result["total"], result["position"] = nativeState, len(ids), args.Position
			result["ids"] = ids[args.Position : args.Position+1]
		case "Email/get":
			if args.IDs == nil || len(args.IDs) > 1 {
				w.WriteHeader(http.StatusBadRequest)
				return
			}
			list, missing := []obj{}, []string{}
			for _, id := range args.IDs {
				switch {
				case id == "one" && !f.incomplete && !f.destroyed:
					list = append(list, obj{"id": id, "blobId": "one-blob", "threadId": "thread-one", "mailboxIds": membership,
						"keywords": keywords, "size": len(jmapFixtureMIME), "receivedAt": "2026-11-27T14:00:00Z",
						"messageId": []string{"shared@example.invalid"}, "subject": "JMAP in two folders",
						"preview": "Synthetic JMAP body.", "from": []obj{{"email": "sender@example.invalid"}},
						"to": []obj{{"email": jmapFixtureUser}}})
				case id == "two":
					list = append(list, obj{"id": id, "blobId": "two-blob", "threadId": "thread-two",
						"mailboxIds": obj{"B": true}, "keywords": obj{}, "size": len(jmapFixtureSecondMIME),
						"receivedAt": "2026-11-27T13:00:00Z", "messageId": []string{"second@example.invalid"},
						"subject": "JMAP second message"})
				default:
					missing = append(missing, id)
				}
			}
			result["state"], result["list"], result["notFound"] = nativeState, list, missing
		default:
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(obj{"methodResponses": [][3]any{{method, result, tag}}, "sessionState": "session"})
	default:
		w.WriteHeader(http.StatusNotFound)
	}
}

type jmapSOAPID struct {
	ID        string `xml:"Id,attr"`
	ChangeKey string `xml:"ChangeKey,attr"`
}

type jmapSOAPResponse struct {
	XMLName xml.Name
	Code    string `xml:"ResponseCode"`
	Cursor  string `xml:"SyncState"`
	Folders []struct {
		ID   jmapSOAPID `xml:"FolderId"`
		Name string     `xml:"DisplayName"`
	} `xml:"Changes>Create>Folder"`
	Created []jmapSOAPID `xml:"Changes>Create>Message>ItemId"`
	Updated []jmapSOAPID `xml:"Changes>Update>Message>ItemId"`
	Deleted []jmapSOAPID `xml:"Changes>Delete>ItemId"`
	MIME    string       `xml:"Items>Message>MimeContent"`
}

func TestJMAPPersistsMembershipAcrossRestart(t *testing.T) {
	fixture := &jmapFixture{}
	upstream := httptest.NewTLSServer(fixture)
	defer upstream.Close()
	sessionURL := upstream.URL + "/.well-known/jmap"
	storePath := filepath.Join(t.TempDir(), "ewsgo.sqlite")
	var live struct {
		sync.RWMutex
		handler http.Handler
		store   *sqlite.Store
	}
	restart := func() {
		t.Helper()
		live.Lock()
		defer live.Unlock()
		if live.store != nil {
			if err := live.store.Close(); err != nil {
				t.Fatal(err)
			}
		}
		store, err := sqlite.Open(t.Context(), storePath)
		if err != nil {
			t.Fatal(err)
		}
		live.store = store
		backend, err := jmap.New(sessionURL, upstream.Client().Transport)
		if err != nil {
			t.Fatal(err)
		}
		live.handler, err = jmapEWS(t.Context(), Handler(), backend, store, sessionURL)
		if err != nil {
			t.Fatal(err)
		}
	}
	restart()
	defer func() { _ = live.store.Close() }()
	front := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		live.RLock()
		defer live.RUnlock()
		live.handler.ServeHTTP(w, r)
	}))
	defer front.Close()

	const soapNS = "http://schemas.xmlsoap.org/soap/envelope/"
	const m = "http://schemas.microsoft.com/exchange/services/2006/messages"
	const ty = "http://schemas.microsoft.com/exchange/services/2006/types"
	const base = "<t:BaseShape>AllProperties</t:BaseShape>"
	soap := func(operation, body string) jmapSOAPResponse {
		t.Helper()
		payload := fmt.Sprintf(`<s:Envelope xmlns:s="%s" xmlns:m="%s" xmlns:t="%s"><s:Body><m:%s>%s</m:%s></s:Body></s:Envelope>`,
			soapNS, m, ty, operation, body, operation)
		req, err := http.NewRequestWithContext(t.Context(), http.MethodPost, front.URL+"/EWS/Exchange.asmx", strings.NewReader(payload))
		if err != nil {
			t.Fatal(err)
		}
		req.SetBasicAuth(jmapFixtureUser, jmapFixturePass)
		res, err := front.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		if res.StatusCode != 200 {
			t.Fatalf("%s status %d", operation, res.StatusCode)
		}
		decoder := xml.NewDecoder(res.Body)
		for {
			token, err := decoder.Token()
			if err != nil {
				t.Fatalf("%s response missing: %v", operation, err)
			}
			if node, ok := token.(xml.StartElement); ok && node.Name.Local == operation+"ResponseMessage" {
				var out jmapSOAPResponse
				if err := decoder.DecodeElement(&out, &node); err != nil {
					t.Fatal(err)
				}
				return out
			}
		}
	}
	syncItems := func(folder, cursor string) jmapSOAPResponse {
		return soap("SyncFolderItems", `<m:ItemShape>`+base+`</m:ItemShape><m:SyncFolderId><t:FolderId Id="`+folder+
			`"/></m:SyncFolderId><m:SyncState>`+cursor+`</m:SyncState><m:MaxChangesReturned>50</m:MaxChangesReturned>`)
	}
	get := func(item string) jmapSOAPResponse {
		return soap("GetItem", `<m:ItemShape>`+base+`<t:IncludeMimeContent>true</t:IncludeMimeContent></m:ItemShape>
			<m:ItemIds><t:ItemId Id="`+item+`"/></m:ItemIds>`)
	}
	if reply := soap("CreateFolder", `<m:ParentFolderId><t:DistinguishedFolderId Id="msgfolderroot"/></m:ParentFolderId><m:Folders><t:Folder><t:DisplayName>Unsupported</t:DisplayName></t:Folder></m:Folders>`); reply.Code != "ErrorInvalidPropertyForOperation" {
		t.Fatalf("missing JMAP folder write: %s", reply.Code)
	}
	if reply := soap("CreateItem", `<m:Items><t:Message/></m:Items>`); reply.Code != "ErrorInvalidPropertyForOperation" {
		t.Fatalf("missing JMAP draft write: %s", reply.Code)
	}
	hierarchy := soap("SyncFolderHierarchy", "<m:FolderShape>"+base+"</m:FolderShape>")
	folders := map[string]string{}
	for _, folder := range hierarchy.Folders {
		folders[folder.Name] = folder.ID.ID
	}
	if hierarchy.Code != "NoError" || len(folders) != 2 || folders["Inbox"] == "" || folders["Project"] == "" {
		t.Fatalf("hierarchy: %#v", hierarchy)
	}
	a, b := syncItems(folders["Inbox"], ""), syncItems(folders["Project"], "")
	if a.Code != "NoError" || b.Code != "NoError" || len(a.Created) != 1 || len(b.Created) != 2 || a.Cursor == "" || b.Cursor == "" {
		t.Fatalf("initial items in both folders: %#v %#v", a, b)
	}
	aItem := a.Created[0]
	var bItem jmapSOAPID
	expectedMIME := base64.StdEncoding.EncodeToString([]byte(jmapFixtureMIME))
	for _, item := range b.Created {
		if value := get(item.ID); value.Code == "NoError" && value.MIME == expectedMIME {
			bItem = item
		}
	}
	if bItem.ID == "" || bItem.ID == aItem.ID || get(aItem.ID).MIME != expectedMIME {
		t.Fatal("expected two distinct EWS IDs for the same MIME")
	}

	// A server listing a message and then reporting it absent is an
	// incomplete snapshot. A retry with the old cursor must still work.
	fixture.mu.Lock()
	fixture.removedFromA, fixture.incomplete = true, true
	fixture.mu.Unlock()
	if failed := syncItems(folders["Inbox"], a.Cursor); failed.Code == "NoError" || failed.Cursor != "" {
		t.Fatalf("partial snapshot affected client cursor: %#v", failed)
	}
	fixture.mu.Lock()
	fixture.incomplete, fixture.losePage = false, true
	snapshots, pages := fixture.snapshotCalls, fixture.deltaPages
	fixture.mu.Unlock()
	if failed := syncItems(folders["Inbox"], a.Cursor); failed.Code == "NoError" || failed.Cursor != "" {
		t.Fatalf("lost delta page affected client cursor: %#v", failed)
	}
	fixture.mu.Lock()
	fixture.losePage = false
	fixture.mu.Unlock()
	restart()

	aNext, bNext := syncItems(folders["Inbox"], a.Cursor), syncItems(folders["Project"], b.Cursor)
	if aNext.Code != "NoError" || len(aNext.Deleted) != 1 || aNext.Deleted[0].ID != aItem.ID || len(aNext.Created)+len(aNext.Updated) != 0 {
		t.Fatalf("inbox delta: %#v", aNext)
	}
	if bNext.Code != "NoError" || len(bNext.Deleted)+len(bNext.Created)+len(bNext.Updated) != 0 {
		t.Fatalf("membership removal affected the other folder: %#v", bNext)
	}
	if get(aItem.ID).Code != "ErrorItemNotFound" || get(bItem.ID).MIME != expectedMIME {
		t.Fatal("one ID should expire and the other should still return identical MIME")
	}

	fixture.mu.Lock()
	cycled := fixture.snapshotCalls != snapshots || fixture.deltaPages < pages+5
	fixture.markRead = true
	fixture.mu.Unlock()
	if cycled {
		t.Fatal("native delta must consume both pages without any Email/query snapshot")
	}
	bLast := syncItems(folders["Project"], bNext.Cursor)
	if bLast.Code != "NoError" || len(bLast.Updated) != 1 || bLast.Updated[0].ID != bItem.ID ||
		bLast.Updated[0].ChangeKey == bItem.ChangeKey || len(bLast.Deleted)+len(bLast.Created) != 0 {
		t.Fatalf("valid metadata update: %#v", bLast)
	}
	fixture.mu.Lock()
	calls := fixture.snapshotCalls
	fixture.mu.Unlock()
	noChanges := syncItems(folders["Project"], bLast.Cursor)
	fixture.mu.Lock()
	scans := fixture.snapshotCalls
	fixture.mu.Unlock()
	if noChanges.Code != "NoError" || len(noChanges.Updated) != 0 || scans != calls {
		t.Fatal("unchanged native state should not need another enumeration")
	}

	fixture.mu.Lock()
	fixture.destroyed = true
	fixture.mu.Unlock()
	destroyed := syncItems(folders["Project"], bLast.Cursor)
	if destroyed.Code != "NoError" || len(destroyed.Deleted) != 1 || destroyed.Deleted[0].ID != bItem.ID ||
		len(destroyed.Created)+len(destroyed.Updated) != 0 || get(bItem.ID).Code != "ErrorItemNotFound" {
		t.Fatalf("native destruction must remove the last placement: %#v", destroyed)
	}

	if runner := os.Getenv("EWSGO_TEST_MAILCRAFT_RUNNER"); runner != "" {
		t.Run("MailCraft", func(t *testing.T) {
			fixture.mu.Lock()
			fixture.markRead, fixture.removedFromA, fixture.destroyed = false, false, false
			fixture.mu.Unlock()
			qualifyJMAPMailCraft(t, runner, front, fixture, restart)
		})
	}
}

func qualifyJMAPMailCraft(t *testing.T, runner string, front *httptest.Server, fixture *jmapFixture, restart func()) {
	t.Helper()
	dir := t.TempDir()
	ca := filepath.Join(dir, "ca.pem")
	mb := filepath.Join(dir, "test.mailcraft")
	if err := os.WriteFile(ca, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: front.Certificate().Raw}), 0o600); err != nil {
		t.Fatal(err)
	}
	cli := "/usr/bin/mailcraft-cli"
	run := func(args ...string) []byte {
		t.Helper()
		cmd := exec.CommandContext(t.Context(), cli, args...)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("MailCraft %s: %v\n%s", args[0], err, out)
		}
		return out
	}
	syncMailbox := func() {
		t.Helper()
		harness, err := filepath.Abs("../../harness/mailcraft")
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(t.Context(), "python3", "-c", `
import json, os, sys
from pathlib import Path
from types import SimpleNamespace
from run import cycle

runner, cli, url, user, ca, store = sys.argv[1:]
os.umask(0o077)
report = cycle(SimpleNamespace(runner=runner, cli=cli, ca=ca, mode="sync"),
               url, user, sys.stdin.readline().strip(), Path(store))
print(json.dumps(report))
sys.exit(not report["ok"] or not report["appliedPieces"])
`, runner, cli, front.URL+"/EWS/Exchange.asmx", jmapFixtureUser, ca, mb)
		cmd.Dir = harness
		cmd.Stdin = strings.NewReader(jmapFixturePass + "\n")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("MailCraft sync: %v\n%s", err, out)
		}
		t.Logf("MailCraft: %s", strings.TrimSpace(string(out)))
	}
	syncMailbox()
	check := func(folder string, expected int) int64 {
		t.Helper()
		out := run("list", mb, "--folder", folder)
		var result struct {
			Messages []struct {
				ID      int64
				Subject string
				Snippet string
			}
		}
		if err := json.Unmarshal(out, &result); err != nil || len(result.Messages) != expected {
			t.Fatalf("MailCraft expected %d messages in %s: %v\n%s", expected, folder, err, out)
		}
		for _, message := range result.Messages {
			if message.Subject == "JMAP in two folders" && strings.Contains(message.Snippet, "Synthetic JMAP body.") {
				return message.ID
			}
		}
		if expected != 0 {
			t.Fatalf("MailCraft missing expected message in %s: %s", folder, out)
		}
		return 0
	}
	check("Inbox", 1)
	projectID := check("Project", 2)
	fixture.mu.Lock()
	fixture.removedFromA = true
	fixture.mu.Unlock()
	restart()
	syncMailbox()
	check("Inbox", 0)
	if id := check("Project", 2); id == 0 || id != projectID {
		t.Fatal("MailCraft replaced the surviving message")
	}
}
