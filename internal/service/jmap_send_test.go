// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package service

import (
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"ewsgo/internal/backend/jmap"
	"ewsgo/internal/core"
	"ewsgo/internal/storage/sqlite"
)

func TestJMAPSendIntentWithIncompleteAcknowledgment(t *testing.T) {
	const (
		coreURN       = "urn:ietf:params:jmap:core"
		mailURN       = "urn:ietf:params:jmap:mail"
		submissionURN = "urn:ietf:params:jmap:submission"
		user          = "jmap@example.invalid"
		input         = "From: " + user + "\r\nTo: " + user + "\r\nMessage-ID: <intent@example.invalid>\r\n\r\nSynthetic MIME.\r\n"
	)
	type obj = map[string]any
	for _, tc := range []struct {
		name, ack string
		want      error
	}{
		{name: "accepted", ack: "both"},
		{name: "lost response", ack: "loss", want: core.ErrOutcomeUnknown},
		{name: "Sent update failed", ack: "missing-copy", want: core.ErrOutcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var uploads, imports, submissions atomic.Int32
			upstream := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				login, password, ok := r.BasicAuth()
				if !ok || login != user || password != "synthetic" {
					w.WriteHeader(http.StatusUnauthorized)
					return
				}
				switch r.URL.Path {
				case "/.well-known/jmap":
					_ = json.NewEncoder(w).Encode(obj{
						"username": user, "apiUrl": "/jmap/", "uploadUrl": "/upload/{accountId}",
						"downloadUrl":     "/download/{accountId}/{blobId}/{name}?type={type}",
						"capabilities":    obj{coreURN: obj{"maxObjectsInGet": 10, "maxSizeUpload": 4096}, mailURN: obj{}, submissionURN: obj{}},
						"primaryAccounts": obj{mailURN: user, submissionURN: user},
						"accounts":        obj{user: obj{"isPersonal": true, "accountCapabilities": obj{mailURN: obj{}, submissionURN: obj{}}}},
					})
				case "/upload/" + user:
					uploads.Add(1)
					raw, err := io.ReadAll(r.Body)
					if err != nil || string(raw) != input || r.Header.Get("Content-Type") != "message/rfc822" {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					w.WriteHeader(http.StatusCreated)
					_ = json.NewEncoder(w).Encode(obj{"accountId": user, "blobId": "blob", "size": len(raw)})
				case "/jmap/":
					var request struct{ MethodCalls [][3]json.RawMessage }
					if json.NewDecoder(r.Body).Decode(&request) != nil || len(request.MethodCalls) != 1 {
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					var method string
					var args struct {
						Emails               map[string]any
						Create               map[string]struct{ IdentityID, EmailID string }
						OnSuccessUpdateEmail map[string]map[string]any
					}
					_ = json.Unmarshal(request.MethodCalls[0][0], &method)
					_ = json.Unmarshal(request.MethodCalls[0][1], &args)
					result := obj{"accountId": user, "state": "state", "notFound": []string{}}
					var afterward []any
					switch method {
					case "Mailbox/get":
						rights := obj{"mayReadItems": true, "mayAddItems": true, "mayRemoveItems": true, "maySubmit": true}
						result["list"] = []obj{
							{"id": "drafts", "name": "Drafts", "role": "drafts", "myRights": rights},
							{"id": "sent", "name": "Sent", "role": "sent", "myRights": rights},
						}
					case "Identity/get":
						result["list"] = []obj{{"id": user, "email": user}}
					case "Email/import":
						imports.Add(1)
						for key := range args.Emails {
							result["created"] = obj{key: obj{"id": "email"}}
						}
					case "EmailSubmission/set":
						submissions.Add(1)
						if tc.ack == "loss" {
							panic(http.ErrAbortHandler)
						}
						for key, job := range args.Create {
							patch := args.OnSuccessUpdateEmail["#"+key]
							if job.IdentityID != user || job.EmailID != "email" || len(patch) != 3 ||
								patch["mailboxIds/sent"] != true || patch["mailboxIds/drafts"] != nil || patch["keywords/$draft"] != nil {
								w.WriteHeader(http.StatusBadRequest)
								return
							}
							result["created"] = obj{key: obj{"id": "submission"}}
						}
						update := obj{"accountId": user, "updated": obj{"email": nil}}
						if tc.ack == "missing-copy" {
							update = obj{"accountId": user, "notUpdated": obj{"email": obj{"type": "forbidden"}}}
						}
						afterward = []any{"Email/set", update, "0"}
					default:
						w.WriteHeader(http.StatusBadRequest)
						return
					}
					responses := [][]any{{method, result, "0"}}
					if afterward != nil {
						responses = append(responses, afterward)
					}
					_ = json.NewEncoder(w).Encode(obj{"methodResponses": responses})
				default:
					http.NotFound(w, r)
				}
			}))
			defer upstream.Close()
			backend, err := jmap.New(upstream.URL+"/.well-known/jmap", upstream.Client().Transport)
			if err != nil {
				t.Fatal(err)
			}
			session, err := backend.Authenticate(t.Context(), user, "synthetic")
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close(t.Context())
			path := filepath.Join(t.TempDir(), "mailbox.sqlite")
			run := func(raw string, want error) {
				t.Helper()
				store, err := sqlite.Open(t.Context(), path)
				if err != nil {
					t.Fatal(err)
				}
				defer store.Close()
				_, err = NewMailSender(store, "test-jmap").SendMIME(t.Context(), session, "intent@example.invalid", []byte(raw))
				if !errors.Is(err, want) {
					t.Fatalf("submission result = %v, want %v", err, want)
				}
			}
			run(input, tc.want)
			run(input, tc.want)
			run(strings.Replace(input, "Synthetic", "Changed", 1), core.ErrConflict)
			if uploads.Load() != 1 || imports.Load() != 1 || submissions.Load() != 1 {
				t.Fatal("correlated retry repeated a native write")
			}
		})
	}
}
