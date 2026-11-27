// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/mail"
	"strings"

	"ewsgo/internal/core"
)

const nativeSent = "5"

var (
	_ core.MailSendBackend             = (*mailbox)(nil)
	_ core.MailSendByMessageIDObserver = (*mailbox)(nil)
)

func (m *mailbox) StageMailSend(ctx context.Context, intent core.SendIntent, mime []byte) (core.SendAttempt, error) {
	if !core.ValidSendIntentID(intent.ID) || intent.State != core.SendReady ||
		(intent.Input.DraftID == "") != (intent.Input.Revision == 0) {
		return nil, core.ErrInvalid
	}
	if int64(len(mime)) > m.native.client.mimeLimit {
		return nil, core.ErrLimit
	}
	message, err := mail.ReadMessage(bytes.NewReader(mime))
	if err != nil {
		return nil, core.ErrInvalid
	}
	if _, exists := message.Header["Message-Id"]; !exists {
		// Preserve any caller-supplied Message-ID and the original MIME bytes.
		// When absent, supply one so a lost response can be investigated.
		lineEnding := "\n"
		if end := bytes.IndexByte(mime, '\n'); end > 0 && mime[end-1] == '\r' {
			lineEnding = "\r\n"
		}
		mime = append([]byte("Message-ID: <"+sendInternetID(intent.ID)+">"+lineEnding), mime...)
		if int64(len(mime)) > m.native.client.mimeLimit {
			return nil, core.ErrLimit
		}
	}
	if intent.Input.DraftID != "" {
		ref, ok := parseDeltaObject(intent.Input.DraftID)
		if !ok || ref.kind != core.Message {
			return nil, core.ErrInvalid
		}
		item, err := m.native.getMessageNode(ctx, ref.ref, "1")
		if err != nil {
			return nil, semanticError(err)
		}
		draft, err := messageSummary(item)
		if err != nil {
			return nil, err
		}
		if !hasMessageRevision(draft, intent.Input.Revision) || draft.Properties[core.IsDraft] != core.Bool(true) {
			return nil, core.ErrConflict
		}
		if when := item.get("autoSendTime"); when != "" && when != "0" {
			return nil, core.ErrUnsupported
		}
	}
	upload, err := m.native.uploadDraftMIME(ctx, mime)
	if errors.Is(err, ErrLimit) {
		return nil, core.ErrLimit
	}
	if err != nil {
		return nil, semanticError(err)
	}
	return func(ctx context.Context) (core.ID, error) {
		return m.native.sendUploadedMIME(ctx, upload, intent.ID)
	}, nil
}

func (s *Session) sendUploadedMIME(ctx context.Context, upload, uid string) (core.ID, error) {
	// did would delete the draft without a revision precondition.
	message := attr(element(mailNS, "m", ""), "aid", upload)
	request := attr(attr(element(mailNS, "SendMsgRequest", "", message), "suid", uid), "noSave", "0")
	response, err := s.requestMutation(ctx, mailNS, "SendMsg", "", request)
	if err != nil {
		if errors.Is(err, core.ErrWriteNotDispatched) {
			return "", errors.Join(core.ErrSendRetryable, err)
		}
		var fault *Fault
		if errors.As(err, &fault) {
			switch fault.Code {
			case "mail.NO_SUCH_UPLOAD", "account.AUTH_FAILED", "service.AUTH_REQUIRED", "service.AUTH_EXPIRED":
				// These faults occur before SMTP submission. Staging again
				// uploads fresh MIME under the authenticated session.
				return "", errors.Join(core.ErrSendRetryable, semanticError(err))
			case "mail.MESSAGE_PARSE_ERROR":
				return "", errors.Join(core.ErrSendRejected, err)
			}
		}
		return "", err
	}
	ack := response.child(mailNS, "m")
	if len(response.Children) != 1 || ack == nil || len(ack.Children) != 0 || strings.TrimSpace(ack.Text) != "" {
		return "", ErrProtocol
	}
	id := ack.get("id")
	if id == "" {
		return "", nil // Zimbra accepts submissions without a saved copy.
	}
	if !nativeID.MatchString(id) {
		return "", ErrProtocol
	}
	return core.ID("m:" + id), nil
}

func (m *mailbox) ObserveMailSend(ctx context.Context, intent core.SendIntent) (core.SendObservation, error) {
	if !core.ValidSendIntentID(intent.ID) || intent.State != core.SendUncertain {
		return core.SendObservation{}, core.ErrInvalid
	}
	draft, draftErr := m.observeSendDraft(ctx, intent.Input)
	sent, sentErr := m.findSentSubmission(ctx, sendInternetID(intent.ID))
	return core.SendObservation{Draft: draft, SentCandidate: sent}, errors.Join(draftErr, sentErr)
}

func (m *mailbox) ObserveMailSendByMessageID(ctx context.Context, messageID string) (core.SendObservation, error) {
	if !core.ValidInternetMessageID(messageID) {
		return core.SendObservation{}, core.ErrInvalid
	}
	id, err := m.findSentSubmission(ctx, messageID)
	return core.SendObservation{SentCandidate: id}, err
}

func (m *mailbox) observeSendDraft(ctx context.Context, input core.SendInput) (core.SendDraftObservation, error) {
	if input.DraftID == "" {
		return core.SendDraftUnknown, nil
	}
	item, err := m.Get(ctx, input.DraftID)
	if errors.Is(err, core.ErrNotFound) {
		return core.SendDraftMissing, nil
	}
	if err != nil {
		return core.SendDraftUnknown, err
	}
	if item.Properties[core.MetadataRevision] == nil {
		return core.SendDraftUnknown, ErrProtocol
	}
	if hasMessageRevision(item, input.Revision) && item.Properties[core.IsDraft] == core.Bool(true) {
		return core.SendDraftUnchanged, nil
	}
	return core.SendDraftChanged, nil
}

func (m *mailbox) findSentSubmission(ctx context.Context, internetID string) (core.ID, error) {
	hits, more, err := m.native.searchMessagePage(ctx, `inid:`+nativeSent+` msgid:"`+internetID+`"`, 0, 0, 2)
	if err != nil || more || len(hits) != 1 || hits[0].parent != nativeSent {
		return "", err
	}
	id := core.ID("m:" + hits[0].id)
	mime, err := m.ReadMIME(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		return "", nil
	}
	if err != nil {
		return "", err
	}
	message, err := mail.ReadMessage(bytes.NewReader(mime))
	if err != nil {
		return "", ErrProtocol
	}
	ids := message.Header["Message-Id"]
	if len(ids) == 1 && ids[0] == "<"+internetID+">" {
		return id, nil
	}
	return "", nil
}

func sendInternetID(uid string) string {
	digest := sha256.Sum256([]byte("ewsgo-sent-message:" + uid))
	return hex.EncodeToString(digest[:16]) + "@ewsgo.invalid"
}
