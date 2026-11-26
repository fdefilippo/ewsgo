// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"ewsgo/internal/core"
)

const testUploadID = "12345678-1234-1234-1234-123456789abc:abcdef98-1234-1234-1234-123456789abc"

func TestAppendMailMessageRequiresVerifiedOrdinaryMIME(t *testing.T) {
	mime := []byte("From: lab@example.invalid\r\nSubject: Synthetic import\r\n\r\nExact MIME\r\n")
	for _, tc := range []struct {
		name          string
		folder        core.ID
		read          bool
		uploads, adds int32
		wantID        core.ID
		wantErr       error
	}{
		{"unread mail", "f:2", false, 1, 1, "m:9", nil},
		{"read mail", "f:2", true, 1, 1, "m:9", nil},
		{"nonmail folder", "f:10", false, 0, 0, "", core.ErrUnsupported},
		{"foreign folder", "f:other:2", false, 0, 0, "", core.ErrInvalid},
		{"malformed MIME", "f:2", false, 0, 0, "", core.ErrInvalid},
		{"lost SOAP reply", "f:2", false, 1, 1, "", core.ErrOutcomeUnknown},
		{"MIME changed", "f:2", false, 1, 1, "m:9", core.ErrOutcomeUnknown},
		{"moved after append", "f:2", false, 1, 1, "m:9", core.ErrOutcomeUnknown},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var uploads, adds, summaries atomic.Int32
			fallback := fakeZimbra(t)
			flag := "u"
			if tc.read {
				flag = ""
			}
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				switch r.URL.Path {
				case "/service/upload":
					uploads.Add(1)
					token, err := r.Cookie("ZM_AUTH_TOKEN")
					raw, readErr := io.ReadAll(r.Body)
					if r.Method != http.MethodPost || r.URL.RawQuery != "fmt=raw" || err != nil || token.Value != fixtureToken ||
						r.Header.Get("Content-Type") != "message/rfc822; name=message.eml" || readErr != nil || !bytes.Equal(raw, mime) {
						t.Error("unexpected upload request")
					}
					_, _ = io.WriteString(w, "200,'null','"+testUploadID+"'\n")
					return
				case "/service/home/~/":
					if r.URL.Query().Get("id") != "9" {
						t.Error("MIME readback selected another message")
					}
					_, _ = w.Write(mime)
					if tc.name == "MIME changed" {
						_, _ = io.WriteString(w, "concurrent edit")
					}
					return
				}
				req := readMessageRequest(t, r)
				switch req.XMLName.Local {
				case "GetFolderRequest":
					if req.get("tr") != "0" {
						t.Error("mount traversal enabled")
					}
					envelope(w, `<GetFolderResponse xmlns="urn:zimbraMail"><folder id="1"><folder id="2" l="1" view="message"/><folder id="10" l="1" view="appointment"/></folder></GetFolderResponse>`)
				case "AddMsgRequest":
					adds.Add(1)
					m := req.child(mailNS, "m")
					if m == nil || m.get("aid") != testUploadID || m.get("l") != "2" ||
						m.get("f") != flag || m.get("noICal") != "1" || req.get("filterSent") != "0" {
						t.Error("unexpected AddMsg request")
					}
					if tc.name == "lost SOAP reply" {
						w.WriteHeader(http.StatusBadGateway)
						return
					}
					envelope(w, `<AddMsgResponse xmlns="urn:zimbraMail"><m id="9" l="2"/></AddMsgResponse>`)
				case "GetMsgRequest":
					checkMessageSelector(t, req)
					folder := "2"
					if summaries.Add(1) == 2 && tc.name == "moved after append" {
						folder = "5"
					}
					envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="9" l="`+folder+`" f="`+flag+`" ms="7" rev="3"/></GetMsgResponse>`)
				default:
					fallback(w, r)
				}
			}, Config{})
			box := &mailbox{native: login(t, client)}
			input := mime
			if tc.name == "malformed MIME" {
				input = []byte("empty header block expected")
			}
			got, err := box.AppendMailMessage(t.Context(), tc.folder, input, core.MailAppendOptions{Read: tc.read})
			if !errors.Is(err, tc.wantErr) || got.ID != tc.wantID || uploads.Load() != tc.uploads || adds.Load() != tc.adds {
				t.Fatalf("ID %q error %v uploads %d SOAP adds %d", got.ID, err, uploads.Load(), adds.Load())
			}
			if err == nil && (got.Parent != tc.folder || got.Properties[core.IsDraft] != core.Bool(false) || got.Properties[core.IsRead] != core.Bool(tc.read)) {
				t.Fatal("written message metadata differs")
			}
		})
	}
}

func TestSaveMailDraftValidationAndUncertainOutcome(t *testing.T) {
	mime := []byte("From: lab@example.invalid\r\nSubject: Synthetic draft\r\n\r\nTest body\r\n")
	const draft = `<m id="9" l="6" f="d" ms="7" rev="3"/>`
	tests := []struct {
		name, before, outcome string
		id, wantID            core.ID
		sendUploads, writes   int32
		wantErr               error
	}{
		{name: "create", sendUploads: 1, writes: 1, wantID: "m:9"},
		{name: "update", id: "m:9", before: draft, sendUploads: 1, writes: 1, wantID: "m:9"},
		{name: "expired upload credentials", outcome: "upload denied", sendUploads: 1, wantErr: core.ErrAuthentication},
		{name: "upload too large", outcome: "upload too large", sendUploads: 1, wantErr: ErrLimit},
		{name: "malformed upload ID", outcome: "bad upload", sendUploads: 1, wantErr: ErrProtocol},
		{name: "invalid MIME", outcome: "bad MIME", wantErr: core.ErrInvalid},
		{name: "ordinary message", id: "m:9", before: `<m id="9" l="6" ms="7" rev="3"/>`, wantErr: core.ErrConflict},
		{name: "another mailbox", id: "m:other:9", wantErr: core.ErrInvalid},
		{name: "scheduled draft", id: "m:9", before: `<m id="9" l="6" f="d" ms="7" rev="3" autoSendTime="1795060000000"/>`, wantErr: core.ErrUnsupported},
		{name: "stale revision before upload", id: "m:9", before: `<m id="9" l="6" f="d" ms="8" rev="3"/>`, wantErr: core.ErrConflict},
		{name: "native conflict during save", id: "m:9", before: draft, outcome: "conflict", sendUploads: 1, writes: 1, wantErr: core.ErrConflict},
		{name: "lost acknowledgment", outcome: "lost acknowledgment", sendUploads: 1, writes: 1, wantErr: core.ErrOutcomeUnknown},
		{name: "missing after acknowledgment", outcome: "missing after write", sendUploads: 1, writes: 1, wantID: "m:9", wantErr: core.ErrOutcomeUnknown},
		{name: "concurrent content after acknowledgment", outcome: "replaced after write", sendUploads: 1, writes: 1, wantID: "m:9", wantErr: core.ErrOutcomeUnknown},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var uploads, saves atomic.Int32
			fallback := fakeZimbra(t)
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/service/upload" {
					uploads.Add(1)
					token, err := r.Cookie("ZM_AUTH_TOKEN")
					raw, readErr := io.ReadAll(r.Body)
					if r.Method != http.MethodPost || r.URL.RawQuery != "fmt=raw" || err != nil || token.Value != fixtureToken ||
						r.Header.Get("Content-Type") != "message/rfc822; name=draft.eml" || readErr != nil || !bytes.Equal(raw, mime) {
						t.Error("upload altered the MIME or used an unexpected request")
					}
					switch tt.outcome {
					case "upload denied":
						_, _ = io.WriteString(w, "401,'null'\n")
					case "upload too large":
						_, _ = io.WriteString(w, "413,'null'\n")
					case "bad upload":
						_, _ = io.WriteString(w, "200,'null','not-a-valid-upload-id'\n")
					default:
						_, _ = io.WriteString(w, "200,'null','"+testUploadID+"'\n")
					}
					return
				}
				req := readMessageRequest(t, r)
				switch req.XMLName.Local {
				case "GetMsgRequest":
					checkMessageSelector(t, req)
					item := tt.before
					if saves.Load() != 0 && tt.outcome == "missing after write" {
						fault(w, "mail.NO_SUCH_MSG")
						return
					}
					if saves.Load() != 0 {
						item = `<m id="9" l="6" f="d" ms="8" rev="4"/>`
						if tt.outcome == "replaced after write" {
							item = `<m id="9" l="6" f="d" ms="9" rev="5"/>`
						}
					}
					envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail">`+item+`</GetMsgResponse>`)
				case "SaveDraftRequest":
					saves.Add(1)
					msg := req.child(mailNS, "m")
					expectedID := strings.TrimPrefix(string(tt.id), "m:")
					if msg == nil || msg.get("id") != expectedID || msg.get("aid") != testUploadID || len(msg.Children) != 0 ||
						msg.get("autoSendTime") != "" || req.get("wantModSeq") != "1" {
						t.Error("unexpected SaveDraft selector")
					}
					var expectedToken string
					if tt.id != "" {
						expectedToken = "7"
					}
					checkSOAPModifiedConstraint(t, r, expectedToken)
					switch tt.outcome {
					case "conflict":
						fault(w, "mail.MODIFY_CONFLICT")
					case "lost acknowledgment":
						// A proxy error cannot establish whether Zimbra committed.
						w.WriteHeader(http.StatusBadGateway)
					default:
						envelope(w, `<SaveDraftResponse xmlns="urn:zimbraMail"><m id="9" rev="4"/></SaveDraftResponse>`)
					}
				default:
					fallback(w, r)
				}
			}, Config{})
			box := &mailbox{native: login(t, client)}
			input := mime
			if tt.outcome == "bad MIME" {
				input = []byte("this has no message headers or delimiter")
			}
			var expected core.Revision
			if tt.id != "" {
				expected = 7
			}
			got, err := box.SaveMailDraft(t.Context(), tt.id, expected, input)
			if !errors.Is(err, tt.wantErr) || got.ID != tt.wantID {
				t.Fatalf("got ID %q with %v, want %q with %v", got.ID, err, tt.wantID, tt.wantErr)
			}
			if uploads.Load() != tt.sendUploads || saves.Load() != tt.writes {
				t.Fatalf("unexpected number of uploads (%d) or saves (%d)", uploads.Load(), saves.Load())
			}
			if err == nil && (got.Parent != nativeDrafts || got.Properties[core.ContentRevision] != core.Integer(4) || got.Revision != 8) {
				t.Fatal("save did not return the verified draft")
			}
		})
	}
}

func TestSetMailFlagNoopAndUnknown(t *testing.T) {
	for _, tt := range []struct {
		name, outcome string
		read          bool
		wantWrites    int32
		wantErr       error
	}{
		{name: "already unread"},
		{name: "set read", read: true, wantWrites: 1},
		{name: "native conflict", outcome: "conflict", read: true, wantWrites: 1, wantErr: core.ErrConflict},
		{name: "readback sees unmodified state", outcome: "unchanged", read: true, wantWrites: 1, wantErr: core.ErrOutcomeUnknown},
	} {
		t.Run(tt.name, func(t *testing.T) {
			var writes atomic.Int32
			fallback := fakeZimbra(t)
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				req := readMessageRequest(t, r)
				switch req.XMLName.Local {
				case "GetMsgRequest":
					checkMessageSelector(t, req)
					item := `<m id="9" l="2" f="uf" ms="7" rev="3"/>`
					if writes.Load() > 0 && tt.outcome == "" {
						item = `<m id="9" l="2" f="f" ms="8" rev="3"/>`
					}
					envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail">`+item+`</GetMsgResponse>`)
				case "MsgActionRequest":
					writes.Add(1)
					action := req.child(mailNS, "action")
					if action == nil || action.get("id") != "9" || action.get("op") != "read" {
						t.Error("unexpected flag write selector")
					}
					checkSOAPModifiedConstraint(t, r, "7")
					if tt.outcome == "conflict" {
						fault(w, "mail.MODIFY_CONFLICT")
					} else {
						envelope(w, `<MsgActionResponse xmlns="urn:zimbraMail"><action id="9" op="read"/></MsgActionResponse>`)
					}
				default:
					fallback(w, r)
				}
			}, Config{})
			box := &mailbox{native: login(t, client)}
			got, err := box.SetMailFlag(t.Context(), "m:9", 7, core.MailFlagRead, tt.read)
			if !errors.Is(err, tt.wantErr) || writes.Load() != tt.wantWrites {
				t.Fatalf("got %v and %d writes, want %v and %d", err, writes.Load(), tt.wantErr, tt.wantWrites)
			}
			if tt.wantErr == core.ErrOutcomeUnknown && got.ID != "m:9" {
				t.Fatal("uncertain readback lost the acknowledged ID")
			}
			if err == nil && (got.Properties[core.IsRead] != core.Bool(tt.read) || got.Properties[core.IsFlagged] != core.Bool(true) ||
				got.Properties[core.ContentRevision] != core.Integer(3)) {
				t.Fatal("did not preserve unrelated metadata")
			}
		})
	}
}

func checkSOAPModifiedConstraint(t *testing.T, r *http.Request, token string) {
	t.Helper()
	raw, err := io.ReadAll(r.Body)
	root, parseErr := parseXML(raw)
	change := root.find(contextNS, "change")
	if err != nil || parseErr != nil || token != "" && (change == nil || change.get("type") != "mod" || change.get("token") != token) ||
		token == "" && change != nil {
		t.Error("unexpected SOAP revision constraint")
	}
}
