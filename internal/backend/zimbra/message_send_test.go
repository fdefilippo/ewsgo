// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sync/atomic"
	"testing"

	"ewsgo/internal/core"
)

const testSendUID = "abcdefghijklmnopqrstuvwxyz"
const testSendMessageID = "fa40a7e9ae0bbe5253a8102ede6451db@ewsgo.invalid"

func TestSendDialFailureCanBeRetried(t *testing.T) {
	client := testClient(t, fakeZimbra(t), Config{})
	session := login(t, client)
	client.http.CloseIdleConnections()
	client.http.Transport = &http.Transport{
		DialContext: func(context.Context, string, string) (net.Conn, error) {
			return nil, errors.New("fixture dial failure")
		},
	}
	if _, err := session.sendUploadedMIME(t.Context(), testUploadID, testSendUID); !errors.Is(err, core.ErrSendRetryable) || !errors.Is(err, core.ErrWriteNotDispatched) {
		t.Fatalf("dial failure: %v", err)
	}
}

func TestStageMailSendUsesOnlyUploadedMIME(t *testing.T) {
	const mime = "To: self@example.invalid\r\nSubject: Fixture\r\n\r\nBody\r\n"
	for _, tc := range []struct {
		name            string
		header          string
		draftRevision   core.Revision
		response, fault string
		wantID          core.ID
		stageErr        error
		sendErr         error
	}{
		{name: "acknowledgment with Sent copy", draftRevision: 7, response: `<m id="42"/>`, wantID: "m:42"},
		{name: "acknowledgment without copy", header: "Message-ID: <original@example.invalid>\r\n", response: `<m/>`},
		{name: "missing upload", fault: "mail.NO_SUCH_UPLOAD", sendErr: core.ErrSendRetryable},
		{name: "expired authentication", fault: "service.AUTH_EXPIRED", sendErr: core.ErrSendRetryable},
		{name: "authentication required", fault: "service.AUTH_REQUIRED", sendErr: core.ErrSendRetryable},
		{name: "authentication failed", fault: "account.AUTH_FAILED", sendErr: core.ErrSendRetryable},
		{name: "malformed MIME", fault: "mail.MESSAGE_PARSE_ERROR", sendErr: core.ErrSendRejected},
		{name: "SMTP failure may follow acceptance or partial send", fault: "mail.SEND_FAILURE", sendErr: ErrProtocol},
		{name: "proxy lost response", sendErr: ErrProtocol},
		{name: "invalid identifier", response: `<m id="other:42"/>`, sendErr: ErrProtocol},
		{name: "missing acknowledgment", response: `<unexpected/>`, sendErr: ErrProtocol},
		{name: "stale draft", draftRevision: 6, stageErr: core.ErrConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var uploads, sends atomic.Int32
			fallback := fakeZimbra(t)
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path == "/service/upload" {
					uploads.Add(1)
					content, err := io.ReadAll(r.Body)
					prefix := tc.header
					if prefix == "" {
						prefix = "Message-ID: <" + testSendMessageID + ">\r\n"
					}
					if err != nil || !bytes.Equal(content, []byte(prefix+mime)) {
						t.Error("uploaded MIME differed from the expected bytes")
					}
					_, _ = io.WriteString(w, "200,'null','"+testUploadID+"'\n")
					return
				}
				request := readMessageRequest(t, r)
				switch request.XMLName.Local {
				case "GetMsgRequest":
					envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="9" l="6" f="d" ms="7" rev="3"/></GetMsgResponse>`)
				case "SendMsgRequest":
					sends.Add(1)
					message := request.child(mailNS, "m")
					if request.get("suid") != testSendUID || request.get("noSave") != "0" ||
						message == nil || message.get("did") != "" || message.get("aid") != testUploadID {
						t.Error("unexpected send request or source-draft deletion requested")
					}
					switch {
					case tc.fault != "":
						fault(w, tc.fault)
					case tc.response != "":
						envelope(w, `<SendMsgResponse xmlns="urn:zimbraMail">`+tc.response+`</SendMsgResponse>`)
					default:
						w.WriteHeader(http.StatusBadGateway)
					}
				default:
					fallback(w, r)
				}
			}, Config{})
			box := &mailbox{native: login(t, client)}
			intent := core.SendIntent{ID: testSendUID, State: core.SendReady}
			if tc.draftRevision != 0 {
				intent.Input.DraftID, intent.Input.Revision = "m:9", tc.draftRevision
			}
			attempt, err := box.StageMailSend(t.Context(), intent, []byte(tc.header+mime))
			if !errors.Is(err, tc.stageErr) || sends.Load() != 0 {
				t.Fatalf("stage: %v, send count %d", err, sends.Load())
			}
			if tc.stageErr != nil {
				if attempt != nil || uploads.Load() != 0 {
					t.Fatal("stale draft permitted preparation")
				}
				return
			}
			id, err := attempt(t.Context())
			if !errors.Is(err, tc.sendErr) || id != tc.wantID || uploads.Load() != 1 || sends.Load() != 1 {
				t.Fatalf("submitted: %q, %v, uploads %d, sends %d", id, err, uploads.Load(), sends.Load())
			}
			if errors.Is(err, core.ErrWriteNotDispatched) {
				t.Fatal("a request received by the server was classified as not dispatched")
			}
		})
	}
}

func TestObserveMailSendRequiresUniqueExactSentEvidence(t *testing.T) {
	const oneHit = `<m id="42" sf="42" l="5"/>`
	for _, tc := range []struct {
		name, hits, mimeID string
		want               core.ID
	}{
		{name: "indexed and verified", hits: oneHit, mimeID: testSendMessageID, want: "m:42"},
		{name: "absent"},
		{name: "duplicate", hits: oneHit + `<m id="43" sf="43" l="5"/>`},
		{name: "wrong folder", hits: `<m id="42" sf="42" l="2"/>`},
		{name: "wrong Message-ID", hits: oneHit, mimeID: "different@example.invalid"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fallback := fakeZimbra(t)
			client := testClient(t, func(w http.ResponseWriter, r *http.Request) {
				if r.Method == http.MethodGet {
					if tc.mimeID == "" {
						t.Error("read MIME without an unambiguous search hit")
					}
					_, _ = io.WriteString(w, "Message-ID: <"+tc.mimeID+">\r\n\r\nFixture\r\n")
					return
				}
				request := readMessageRequest(t, r)
				switch request.XMLName.Local {
				case "GetMsgRequest":
					envelope(w, `<GetMsgResponse xmlns="urn:zimbraMail"><m id="9" l="6" f="d" ms="8" rev="4"/></GetMsgResponse>`)
				case "SearchRequest":
					query := request.child(mailNS, "query")
					if query == nil || query.Text != `inid:5 msgid:"`+testSendMessageID+`"` || request.get("limit") != "2" {
						t.Error("unexpected Sent search")
					}
					envelope(w, `<SearchResponse xmlns="urn:zimbraMail" sortBy="idAsc" offset="0" more="0">`+tc.hits+`</SearchResponse>`)
				case "SendMsgRequest":
					t.Error("observation triggered a new send")
				default:
					fallback(w, r)
				}
			}, Config{})
			box := &mailbox{native: login(t, client)}
			intent := core.SendIntent{ID: testSendUID, State: core.SendUncertain, Input: core.SendInput{DraftID: "m:9", Revision: 7}}
			got, err := box.ObserveMailSend(t.Context(), intent)
			if err != nil || got.SentCandidate != tc.want || got.Draft != core.SendDraftChanged {
				t.Fatalf("observation: %+v, %v", got, err)
			}
		})
	}
}
