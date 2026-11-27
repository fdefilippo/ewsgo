// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package jmap

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/mail"
	"strings"
	"time"

	"ewsgo/internal/core"
)

type setResult struct {
	AccountID  string
	Created    map[string]struct{ ID string }
	Updated    map[string]json.RawMessage
	NotCreated map[string]struct{ Type string }
	NotUpdated map[string]json.RawMessage
}

type submissionMailbox struct{ *session }

// StageMailSend resolves rights and a sender identity before the coordinator
// claims the intent. The temporary blob and native draft are created only by
// the winning attempt. A lost import or submission response stops retries.
func (s *submissionMailbox) StageMailSend(ctx context.Context, intent core.SendIntent, raw []byte) (core.SendAttempt, error) {
	if intent.Input.DraftID != "" {
		return nil, core.ErrUnsupported
	}
	if !core.ValidSendIntentID(intent.ID) || len(raw) == 0 {
		return nil, core.ErrInvalid
	}
	if int64(len(raw)) > s.maxUpload || len(raw) > core.MaxMailContentMIMEBytes {
		return nil, core.ErrLimit
	}
	message, err := mail.ReadMessage(bytes.NewReader(raw))
	if err != nil || len(message.Header["From"]) != 1 {
		return nil, core.ErrInvalid
	}
	from, err := mail.ParseAddress(message.Header.Get("From"))
	if err != nil {
		return nil, core.ErrInvalid
	}
	if len(message.Header["Sender"]) != 0 {
		sender, err := mail.ParseAddress(message.Header.Get("Sender"))
		if err != nil || len(message.Header["Sender"]) != 1 || !strings.EqualFold(sender.Address, from.Address) {
			return nil, core.ErrAccessDenied
		}
	}
	drafts, sent, err := s.submissionFolders(ctx)
	if err != nil {
		return nil, err
	}
	identity, err := s.submissionIdentity(ctx, from.Address)
	if err != nil {
		return nil, err
	}
	return func(ctx context.Context) (core.ID, error) {
		blob, err := s.uploadMessage(ctx, raw)
		if err != nil {
			// A failed temporary upload cannot enqueue mail or make a draft.
			if errors.Is(err, core.ErrLimit) || errors.Is(err, core.ErrAuthentication) || errors.Is(err, core.ErrAccessDenied) {
				return "", core.ErrSendRejected
			}
			return "", core.ErrSendRetryable
		}
		email, err := s.importDraft(ctx, intent.ID, drafts, blob)
		if err != nil {
			return "", err
		}
		return s.submitDraft(ctx, intent.ID, identity, email, drafts, sent)
	}, nil
}

func (s *session) submissionFolders(ctx context.Context) (string, string, error) {
	folders, err := s.getFolders(ctx)
	if err != nil {
		return "", "", err
	}
	var drafts, sent string
	for _, f := range folders.List {
		switch f.Role {
		case "drafts":
			if drafts != "" {
				return "", "", errProtocol
			}
			if f.Rights.Read && f.Rights.Add && f.Rights.Remove && f.Rights.Submit {
				drafts = f.ID
			}
		case "sent":
			if sent != "" {
				return "", "", errProtocol
			}
			if f.Rights.Read && f.Rights.Add {
				sent = f.ID
			}
		}
	}
	if drafts == "" || sent == "" {
		return "", "", core.ErrUnsupported
	}
	return drafts, sent, nil
}

func (s *session) submissionIdentity(ctx context.Context, from string) (string, error) {
	var result getResponse[struct{ ID, Email string }]
	if err := s.callUsing(ctx, "Identity/get", map[string]any{}, &result, true); err != nil {
		return "", err
	}
	if result.AccountID != s.account || result.List == nil || len(result.List) > 128 || len(result.NotFound) != 0 {
		return "", errProtocol
	}
	for _, identity := range result.List {
		if !validPrincipalID(identity.ID) {
			return "", errProtocol
		}
		if strings.EqualFold(identity.Email, from) {
			return identity.ID, nil
		}
	}
	return "", core.ErrAccessDenied
}

func (s *session) uploadMessage(ctx context.Context, raw []byte) (_ string, err error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: "JMAPUpload", Result: core.ActivityOutcome(err), Duration: time.Since(started)})
	}()
	data, _, err := s.client.fetch(ctx, s.auth, http.MethodPost, s.upload, raw, "application/json", "message/rfc822", maxJSONBytes)
	if err != nil {
		return "", err
	}
	var result struct {
		AccountID, BlobID string
		Size              int
	}
	if json.Unmarshal(data, &result) != nil || result.AccountID != s.account || result.Size != len(raw) || !nativeID.MatchString(result.BlobID) {
		return "", errProtocol
	}
	return result.BlobID, nil
}

func (s *session) importDraft(ctx context.Context, key, drafts, blob string) (string, error) {
	var result setResult
	err := s.call(ctx, "Email/import", map[string]any{"emails": map[string]any{
		key: map[string]any{"blobId": blob, "mailboxIds": map[string]bool{drafts: true},
			"keywords": map[string]bool{"$draft": true, "$seen": true}},
	}}, &result)
	if err != nil || result.AccountID != s.account {
		return "", core.ErrOutcomeUnknown
	}
	return result.createdID(key)
}

func (r setResult) createdID(key string) (string, error) {
	created, failed := r.Created[key], r.NotCreated[key]
	if len(r.Created) == 1 && len(r.NotCreated) == 0 && nativeID.MatchString(created.ID) {
		return created.ID, nil
	}
	if len(r.Created) == 0 && len(r.NotCreated) == 1 {
		switch failed.Type {
		case "forbidden", "invalidProperties", "notFound", "blobNotFound", "overQuota", "tooLarge",
			"invalidEmail", "invalidRecipients", "noRecipients", "tooManyRecipients", "forbiddenMailFrom",
			"forbiddenFrom", "forbiddenToSend":
			return "", core.ErrSendRejected
		}
	}
	return "", core.ErrOutcomeUnknown
}

func (s *session) submitDraft(ctx context.Context, key, identity, email, drafts, sent string) (_ core.ID, err error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: "EmailSubmission/set", Result: core.ActivityOutcome(err), Duration: time.Since(started)})
	}()
	methods, err := s.invoke(ctx, "EmailSubmission/set", map[string]any{
		"create": map[string]any{key: map[string]string{"identityId": identity, "emailId": email}},
		"onSuccessUpdateEmail": map[string]any{"#" + key: map[string]any{
			"mailboxIds/" + drafts: nil, "mailboxIds/" + sent: true, "keywords/$draft": nil,
		}},
	}, true)
	var submission setResult
	if err != nil || methods[0].decode("EmailSubmission/set", &submission) != nil || submission.AccountID != s.account {
		return "", core.ErrOutcomeUnknown
	}
	if _, err := submission.createdID(key); err != nil {
		return "", err
	}
	// The message may already be on its way if the implicit copy update
	// fails. Preserve the uncertain result and never immediately resend it.
	var copy setResult
	if len(methods) != 2 || methods[1].decode("Email/set", &copy) != nil || copy.AccountID != s.account ||
		len(copy.Updated) != 1 || copy.Updated[email] == nil || len(copy.NotUpdated) != 0 {
		return "", core.ErrOutcomeUnknown
	}
	return placementID(email, sent), nil
}

func (*submissionMailbox) ObserveMailSend(context.Context, core.SendIntent) (core.SendObservation, error) {
	return core.SendObservation{}, nil
}
