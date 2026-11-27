// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"bytes"
	"context"
	"errors"
	"math"
	"net/http"
	"net/mail"
	"regexp"
	"strconv"
	"time"

	"ewsgo/internal/core"
)

const nativeDrafts = "f:6"
const zimbraUploadUUID = `[0-9a-fA-F]{8}(?:-[0-9a-fA-F]{4}){3}-[0-9a-fA-F]{12}`

var (
	_ core.MailDraftWriter     = (*mailbox)(nil)
	_ core.MailFlagWriter      = (*mailbox)(nil)
	_ core.MailMessageAppender = (*mailbox)(nil)

	// Zimbra's plain upload reply is a status, a null request ID and
	// one upload UUID, optionally prefixed by the mailbox server UUID.
	draftUploadReply = regexp.MustCompile(`^([0-9]{3}),'null'(?:,'((?:` + zimbraUploadUUID + `:)?` + zimbraUploadUUID + `)')?$`)
)

func (m *mailbox) SaveMailDraft(ctx context.Context, id core.ID, expected core.Revision, mime []byte) (out core.Object, writeErr error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: "SaveMailDraft", Result: core.ActivityOutcome(writeErr), Duration: time.Since(started)})
	}()
	if len(mime) == 0 || id == "" && expected != 0 || expected > math.MaxInt32 {
		return core.Object{}, core.ErrInvalid
	}
	if int64(len(mime)) > m.native.client.mimeLimit {
		return core.Object{}, errors.Join(core.ErrLimit, ErrLimit)
	}
	if _, err := mail.ReadMessage(bytes.NewReader(mime)); err != nil {
		return core.Object{}, core.ErrInvalid
	}

	parent := core.ID(nativeDrafts)
	var ref string
	var change []node
	var previousContent core.Integer
	if id != "" {
		source, ok := parseDeltaObject(id)
		if !ok || source.kind != core.Message {
			return core.Object{}, core.ErrInvalid
		}
		ref = source.ref
		item, err := m.native.getMessageNode(ctx, ref, "1")
		if err != nil {
			return core.Object{}, semanticError(err)
		}
		current, err := messageSummary(item)
		if err != nil {
			return core.Object{}, err
		}
		if !hasMessageRevision(current, expected) || current.Properties[core.IsDraft] != core.Bool(true) {
			return core.Object{}, core.ErrConflict
		}
		if item.get("autoSendTime") != "" && item.get("autoSendTime") != "0" {
			return core.Object{}, core.ErrUnsupported
		}
		previousContent, ok = current.Properties[core.ContentRevision].(core.Integer)
		if !ok {
			return core.Object{}, ErrProtocol
		}
		parent = current.Parent
		change = []node{modifiedConstraint(expected)}
	}

	upload, err := m.native.uploadDraftMIME(ctx, mime)
	if errors.Is(err, ErrLimit) {
		return core.Object{}, errors.Join(core.ErrLimit, ErrLimit)
	}
	if err != nil {
		return core.Object{}, semanticError(err)
	}
	if err := ctx.Err(); err != nil {
		return core.Object{}, err
	}
	msg := attr(element(mailNS, "m", ""), "aid", upload)
	if ref != "" {
		msg = attr(msg, "id", ref)
	}
	response, err := m.native.request(ctx, mailNS, "SaveDraft", "",
		attr(element(mailNS, "SaveDraftRequest", "", msg), "wantModSeq", "1"), change...)
	if err != nil {
		return core.Object{}, messageWriteError(err)
	}
	saved := response.child(mailNS, "m")
	if len(response.Children) != 1 || saved == nil || !nativeID.MatchString(saved.get("id")) || ref != "" && saved.get("id") != ref {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	ref = saved.get("id")
	acknowledged := core.Object{ID: core.ID("m:" + ref), Kind: core.Message}
	version, err := optionalFolderNumber(*saved, "rev")
	if err != nil || version == nil || id != "" && core.Integer(*version) <= previousContent {
		return acknowledged, core.ErrOutcomeUnknown
	}
	verified, err := m.getMessageSummary(ctx, ref)
	if err != nil || verified.Parent != parent || verified.Properties[core.IsDraft] != core.Bool(true) ||
		verified.Properties[core.ContentRevision] != core.Integer(*version) || verified.Properties[core.MetadataRevision] == nil ||
		id != "" && verified.Revision <= expected {
		return acknowledged, core.ErrOutcomeUnknown
	}
	return verified, nil
}

func (m *mailbox) AppendMailMessage(ctx context.Context, folder core.ID, mime []byte, options core.MailAppendOptions) (_ core.Object, writeErr error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: "AppendMailMessage", Result: core.ActivityOutcome(writeErr), Duration: time.Since(started)})
	}()
	target, ok := parseDeltaObject(folder)
	if !ok || target.kind != core.Folder || len(mime) == 0 {
		return core.Object{}, core.ErrInvalid
	}
	if int64(len(mime)) > m.native.client.mimeLimit {
		return core.Object{}, errors.Join(core.ErrLimit, ErrLimit)
	}
	if _, err := mail.ReadMessage(bytes.NewReader(mime)); err != nil {
		return core.Object{}, core.ErrInvalid
	}
	if err := m.mailContentFolder(ctx, target.ref); err != nil {
		return core.Object{}, errors.Join(core.ErrWriteNotDispatched, semanticError(err))
	}
	upload, err := m.native.uploadMessageMIME(ctx, mime, "UploadMailMIME", "message.eml")
	if errors.Is(err, ErrLimit) {
		return core.Object{}, errors.Join(core.ErrLimit, ErrLimit)
	}
	if err != nil {
		return core.Object{}, errors.Join(core.ErrWriteNotDispatched, semanticError(err))
	}
	msg := attr(attr(attr(element(mailNS, "m", ""), "l", target.ref), "aid", upload), "noICal", "1")
	var flags string
	if !options.Read {
		flags += "u"
	}
	if options.Flagged {
		flags += "f"
	}
	if flags != "" {
		msg = attr(msg, "f", flags)
	}
	response, err := m.native.requestMutation(ctx, mailNS, "AddMsg", "",
		attr(element(mailNS, "AddMsgRequest", "", msg), "filterSent", "0"))
	if err != nil {
		return core.Object{}, messageWriteError(err)
	}
	added := response.child(mailNS, "m")
	if len(response.Children) != 1 || added == nil || !nativeID.MatchString(added.get("id")) {
		return core.Object{}, core.ErrOutcomeUnknown
	}
	ref := added.get("id")
	id := core.ID("m:" + ref)
	acknowledged := core.Object{ID: id, Kind: core.Message}
	before, err := m.getMessageSummary(ctx, ref)
	contentVersion, versioned := before.Properties[core.ContentRevision].(core.Integer)
	if err != nil || before.ID != id || before.Parent != folder || !versioned {
		return acknowledged, core.ErrOutcomeUnknown
	}
	stored, err := m.ReadMIME(ctx, id)
	if err != nil || !bytes.Equal(mime, stored) {
		return acknowledged, core.ErrOutcomeUnknown
	}
	verified, err := m.getMessageSummary(ctx, ref)
	if err != nil || verified.ID != id || verified.Parent != folder || verified.Properties[core.ContentRevision] != contentVersion ||
		verified.Properties[core.MetadataRevision] == nil || verified.Properties[core.IsDraft] != core.Bool(false) ||
		verified.Properties[core.IsRead] != core.Bool(options.Read) || (verified.Properties[core.IsFlagged] == core.Bool(true)) != options.Flagged {
		return acknowledged, core.ErrOutcomeUnknown
	}
	return verified, nil
}

func (s *Session) uploadDraftMIME(ctx context.Context, mime []byte) (string, error) {
	return s.uploadMessageMIME(ctx, mime, "UploadDraftMIME", "draft.eml")
}

func (s *Session) uploadMessageMIME(ctx context.Context, mime []byte, activity, filename string) (_ string, uploadErr error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: activity, Result: core.ActivityOutcome(semanticError(uploadErr)), Duration: time.Since(started)})
	}()
	token, err := s.credential()
	if err != nil {
		return "", err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, s.client.origin+"/service/upload?fmt=raw", bytes.NewReader(mime))
	if err != nil {
		return "", ErrProtocol
	}
	req.Header.Set("Content-Type", "message/rfc822; name="+filename)
	req.AddCookie(&http.Cookie{Name: "ZM_AUTH_TOKEN", Value: token})
	reply, status, err := s.client.exchange(ctx, req, 1024)
	if err != nil {
		return "", err
	}
	if status != http.StatusOK {
		return "", draftUploadStatus(status)
	}
	parts := draftUploadReply.FindSubmatch(bytes.TrimSpace(reply))
	if parts == nil {
		return "", ErrProtocol
	}
	code, _ := strconv.Atoi(string(parts[1]))
	if code != http.StatusOK || len(parts[2]) == 0 {
		return "", draftUploadStatus(code)
	}
	return string(parts[2]), nil
}

func draftUploadStatus(status int) error {
	switch status {
	case http.StatusUnauthorized:
		return ErrExpired
	case http.StatusForbidden:
		return ErrDenied
	case http.StatusRequestEntityTooLarge:
		return ErrLimit
	default:
		return ErrProtocol
	}
}

func (m *mailbox) SetMailFlag(ctx context.Context, id core.ID, expected core.Revision, flag core.MailFlag, value bool) (out core.Object, writeErr error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: "SetMailFlag", Result: core.ActivityOutcome(writeErr), Duration: time.Since(started)})
	}()
	source, ok := parseDeltaObject(id)
	if !ok || source.kind != core.Message || expected > math.MaxInt32 {
		return core.Object{}, core.ErrInvalid
	}
	var action string
	var key core.Key
	switch flag {
	case core.MailFlagRead:
		action, key = "read", core.IsRead
	case core.MailFlagMarked:
		action, key = "flag", core.IsFlagged
	default:
		return core.Object{}, core.ErrUnsupported
	}
	current, err := m.getMessageSummary(ctx, source.ref)
	if err != nil {
		return core.Object{}, err
	}
	if !hasMessageRevision(current, expected) {
		return core.Object{}, core.ErrConflict
	}
	if current.Properties[key] == core.Bool(value) {
		return current, nil
	}
	if !value {
		action = "!" + action
	}
	if err := ctx.Err(); err != nil {
		return core.Object{}, err
	}
	if err := m.native.messageAction(ctx, source.ref, action, "", modifiedConstraint(expected)); err != nil {
		return core.Object{}, messageWriteError(err)
	}
	verified, err := m.getMessageSummary(ctx, source.ref)
	if err != nil || verified.Revision <= expected || verified.Properties[key] != core.Bool(value) {
		return core.Object{ID: id, Kind: core.Message}, core.ErrOutcomeUnknown
	}
	return verified, nil
}

func modifiedConstraint(revision core.Revision) node {
	return attr(attr(element(contextNS, "change", ""), "type", "mod"), "token", strconv.FormatUint(uint64(revision), 10))
}

func hasMessageRevision(object core.Object, expected core.Revision) bool {
	return object.Properties[core.MetadataRevision] != nil && object.Revision == expected
}
