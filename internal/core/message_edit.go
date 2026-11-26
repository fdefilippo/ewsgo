// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"encoding/hex"
	"time"
)

// DraftReferenceHeader is reserved for the opaque reference to persistent
// draft properties. Its value is a randomly generated lowercase hex string.
const DraftReferenceHeader = "X-Ewsgo-Draft-Reference"

func ValidDraftReference(value string) bool {
	if len(value) != 32 {
		return false
	}
	var decoded [16]byte
	n, err := hex.Decode(decoded[:], []byte(value))
	return err == nil && n == len(decoded) && value == hex.EncodeToString(decoded[:])
}

// MailDraftReferenceFinder resolves references only inside the selected
// folder of the authenticated mailbox. Matches must be exact and live drafts.
type MailDraftReferenceFinder interface {
	FindMailDraftsByReference(ctx context.Context, folder ID, reference string) ([]Object, error)
}

// MailDraftWriter saves a complete RFC 5322/MIME draft. Empty ID and zero
// Expected create a draft. Updating uses the current metadata revision.
// An acknowledged ID can be returned with ErrOutcomeUnknown if readback fails.
type MailDraftWriter interface {
	SaveMailDraft(ctx context.Context, id ID, expected Revision, mime []byte) (Object, error)
}

// MailMessageAppender adds ordinary mail to a native folder without sending
// it. The caller owns replay: an uncertain result must be reconciled before
// attempting another append. An acknowledged ID can accompany ErrOutcomeUnknown.
type MailMessageAppender interface {
	AppendMailMessage(ctx context.Context, folder ID, mime []byte, options MailAppendOptions) (Object, error)
}

// MailAppendOptions holds the marks applied with the new message.
type MailAppendOptions struct {
	Read, Flagged bool
}

type MailFlag uint8

const (
	MailFlagRead MailFlag = iota + 1
	MailFlagMarked
)

// MailFlagWriter updates one flag, leaving the MIME and other flags intact.
// Expected is the metadata revision observed by the caller.
type MailFlagWriter interface {
	SetMailFlag(ctx context.Context, id ID, expected Revision, flag MailFlag, value bool) (Object, error)
}

// MailReadWriter marks a message read with the caller's receipt policy.
// A backend that cannot honor the policy must return ErrUnsupported.
// A failure after a native effect can be ErrOutcomeUnknown.
type MailReadWriter interface {
	MarkMailRead(ctx context.Context, id ID, expected Revision, suppressReceipt bool) (Object, error)
}

// MailMarks specifies a subset of message metadata. A nil Categories field
// leaves tags unchanged and an empty but non-nil field clears all tags.
type MailMarks struct {
	Read       *bool
	FollowUp   *MailFollowUp
	Importance *Integer
	Categories Strings
}

func (m MailMarks) Empty() bool {
	return m.Read == nil && m.FollowUp == nil && m.Importance == nil && m.Categories == nil
}

// MailMarksWriter updates marks starting at the given revision. A backend that uses
// several native writes reports ErrOutcomeUnknown if one fails after another applies.
type MailMarksWriter interface {
	SetMailMarks(ctx context.Context, id ID, expected Revision, marks MailMarks) (Object, error)
}

type FollowUpStatus uint8

const (
	FollowUpNone FollowUpStatus = iota
	FollowUpMarked
	FollowUpComplete
)

type MailFollowUp struct {
	Status               FollowUpStatus
	Start, Due, Complete time.Time
}

func (f MailFollowUp) Valid() bool {
	switch f.Status {
	case FollowUpNone:
		return f.Start.IsZero() && f.Due.IsZero() && f.Complete.IsZero()
	case FollowUpMarked:
		return f.Complete.IsZero() && (f.Start.IsZero() || f.Due.IsZero() || !f.Due.Before(f.Start))
	case FollowUpComplete:
		return f.Start.IsZero() && f.Due.IsZero()
	default:
		return false
	}
}

func MailFollowUpOf(p Properties) MailFollowUp {
	readTime := func(key Key) time.Time {
		value, _ := p[key].(Timestamp)
		return time.Time(value)
	}
	if p[IsFlagged] == Bool(true) {
		return MailFollowUp{Status: FollowUpMarked, Start: readTime(FollowUpStart), Due: readTime(FollowUpDue)}
	}
	if p[FollowUpCompleted] == Bool(true) {
		return MailFollowUp{Status: FollowUpComplete, Complete: readTime(FollowUpCompleteAt)}
	}
	return MailFollowUp{}
}

var (
	FollowUpStart      = Key{Set: "f8b556e0-5d21-4ce8-9043-05e3d3dd7b90", Name: "followUpStart"}
	FollowUpDue        = Key{Set: "f8b556e0-5d21-4ce8-9043-05e3d3dd7b90", Name: "followUpDue"}
	FollowUpCompleted  = Key{Set: "f8b556e0-5d21-4ce8-9043-05e3d3dd7b90", Name: "followUpCompleted"}
	FollowUpCompleteAt = Key{Set: "f8b556e0-5d21-4ce8-9043-05e3d3dd7b90", Name: "followUpCompleteAt"}
)

var IsDraft = Key{Set: "f8b556e0-5d21-4ce8-9043-05e3d3dd7b90", Name: "isDraft"}
var IsFlagged = Key{Set: "f8b556e0-5d21-4ce8-9043-05e3d3dd7b90", Name: "isFlagged"}
