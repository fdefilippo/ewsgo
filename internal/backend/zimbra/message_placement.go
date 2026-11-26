// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package zimbra

import (
	"context"
	"errors"
	"math"
	"time"

	"ewsgo/internal/core"
)

const deletedItems = "f:3"

var _ core.MailMessagePlacementWriter = (*mailbox)(nil)

func (m *mailbox) MoveMailMessage(ctx context.Context, id core.ID, expected core.Revision, destination core.ID) (core.Object, error) {
	return m.placeMailMessage(ctx, "MoveMailMessage", id, expected, destination)
}

func (m *mailbox) CopyMailMessage(ctx context.Context, id core.ID, expected core.Revision, destination core.ID) (core.Object, error) {
	return m.placeMailMessage(ctx, "CopyMailMessage", id, expected, destination)
}

func (m *mailbox) TrashMailMessage(ctx context.Context, id core.ID, expected core.Revision) (core.Object, error) {
	return m.placeMailMessage(ctx, "TrashMailMessage", id, expected, deletedItems)
}

func (m *mailbox) RestoreMailMessage(ctx context.Context, id core.ID, expected core.Revision, destination core.ID) (core.Object, error) {
	return m.placeMailMessage(ctx, "RestoreMailMessage", id, expected, destination)
}

func (m *mailbox) placeMailMessage(ctx context.Context, operation string, id core.ID, expected core.Revision, destination core.ID) (out core.Object, writeErr error) {
	started := time.Now()
	defer func() {
		core.RecordActivity(ctx, core.Activity{Layer: "backend", Operation: operation, Result: core.ActivityOutcome(writeErr), Duration: time.Since(started)})
	}()
	source, sourceOK := parseDeltaObject(id)
	target, targetOK := parseDeltaObject(destination)
	if !sourceOK || source.kind != core.Message || !targetOK || target.kind != core.Folder || expected > math.MaxInt32 ||
		operation == "RestoreMailMessage" && destination == deletedItems {
		return core.Object{}, core.ErrInvalid
	}
	tree, err := m.ownFolderTree(ctx)
	if err != nil {
		return core.Object{}, semanticError(err)
	}
	if _, err := tree.mailFolder(target.ref); err != nil {
		return core.Object{}, err
	}
	current, err := m.getMessageSummary(ctx, source.ref)
	if err != nil {
		return core.Object{}, err
	}
	parent, ok := parseDeltaObject(current.Parent)
	if !ok || parent.kind != core.Folder {
		return core.Object{}, ErrProtocol
	}
	if _, err := tree.mailFolder(parent.ref); err != nil {
		return core.Object{}, err
	}
	if !hasMessageRevision(current, expected) {
		return core.Object{}, core.ErrConflict
	}
	if operation == "RestoreMailMessage" && current.Parent != deletedItems {
		return core.Object{}, core.ErrConflict
	}
	copyMessage := operation == "CopyMailMessage"
	if !copyMessage && current.Parent == destination {
		return current, nil
	}
	if err := ctx.Err(); err != nil {
		return core.Object{}, err
	}

	ref, err := m.native.writeMessagePlacement(ctx, source.ref, target.ref, expected, copyMessage)
	if err != nil {
		return core.Object{}, err
	}
	verified, err := m.getMessageSummary(ctx, ref)
	if _, ok := verified.Properties[core.MetadataRevision]; err != nil || !ok || verified.Parent != destination {
		return core.Object{ID: core.ID("m:" + ref), Kind: core.Message}, core.ErrOutcomeUnknown
	}
	return verified, nil
}

func (s *Session) writeMessagePlacement(ctx context.Context, id, folder string, expected core.Revision, copyMessage bool) (string, error) {
	operation, actionName := "MsgAction", "move"
	if copyMessage {
		operation, actionName = "ItemAction", "copy"
	}
	action := attr(attr(attr(element(mailNS, "action", ""), "id", id), "op", actionName), "l", folder)
	if copyMessage {
		action = attr(action, "nci", "1")
	}
	response, err := s.request(ctx, mailNS, operation, "", element(mailNS, operation+"Request", "", action), modifiedConstraint(expected))
	if err != nil {
		return "", messageWriteError(err)
	}
	result := response.child(mailNS, "action")
	if len(response.Children) != 1 || result == nil || result.get("id") != id || result.get("op") != actionName {
		return "", core.ErrOutcomeUnknown
	}
	if !copyMessage {
		return id, nil
	}
	created := result.get("nci")
	if !nativeID.MatchString(created) || created == id {
		return "", core.ErrOutcomeUnknown
	}
	return created, nil
}

func messageWriteError(err error) error {
	mapped := semanticError(err)
	if errors.Is(mapped, core.ErrNotFound) || errors.Is(mapped, core.ErrAccessDenied) || errors.Is(mapped, core.ErrAuthentication) {
		return mapped
	}
	var fault *Fault
	if errors.As(err, &fault) {
		switch fault.Code {
		case "service.INVALID_REQUEST", "mail.MESSAGE_PARSE_ERROR":
			return core.ErrInvalid
		case "mail.ALREADY_EXISTS", "mail.MODIFY_CONFLICT", "mail.IMMUTABLE_OBJECT":
			return core.ErrConflict
		case "mail.NO_SUCH_UPLOAD":
			return ErrProtocol
		}
	}
	// Transport loss, malformed replies and unclassified server failures
	// leave open whether Zimbra already committed the write.
	return core.ErrOutcomeUnknown
}
