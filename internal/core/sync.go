// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import "context"

// FolderSync is an optional provisional capability. Its cursor belongs to the
// provider and selected folder; a frontend must map it to its own cursor space.
type FolderSync interface {
	SyncFolder(context.Context, ID, Cursor, int) (SyncPage, error)
}
type SyncPage struct {
	Objects []Object
	Deleted []ID
	Next    Cursor
	More    bool
}

// ReadStateWriter deliberately promises no compare-and-swap or multi-property
// atomic commit. Providers unable to satisfy Edit's contract must not fake it.
type ReadStateWriter interface {
	SetRead(context.Context, ID, bool) (Object, error)
}
type MIMEReader interface {
	ReadMIME(context.Context, ID) ([]byte, error)
}

// MailAttachmentReader lists descriptors and reads one decoded MIME attachment.
// An attachment ID is opaque and belongs to its message and content version.
type MailAttachmentReader interface {
	ListMailAttachments(context.Context, ID) ([]MailAttachment, error)
	ReadMailAttachment(context.Context, ID, string) (Attachment, error)
}

type MailAttachment struct {
	ID, Name, MediaType, ContentID string
	Inline                         bool
}

var Subject = Key{ID: 0x0037}
var Preview = Key{ID: 0x3fd9}
var MessageID = Key{ID: 0x1035}
var MessageSize = Key{ID: 0x0e08}
var ReceivedAt = Key{ID: 0x0e06}
var MailCategories = Key{Set: "f8b556e0-5d21-4ce8-9043-05e3d3dd7b90", Name: "categories"}
var HasAttachments = Key{ID: 0x0e1b}
var ConversationID = Key{Set: "f8b556e0-5d21-4ce8-9043-05e3d3dd7b90", Name: "conversationID"}
var MessageImportance = Key{ID: 0x0017}
var FromAddress = Key{ID: 0x0c1f}
var ContentRevision = Key{Set: "f8b556e0-5d21-4ce8-9043-05e3d3dd7b90", Name: "contentRevision"}

// Optional source metadata. Property presence distinguishes a supplied zero
// sequence from absence; Object.Revision alone cannot express that distinction.
var MetadataRevision = Key{Set: "f8b556e0-5d21-4ce8-9043-05e3d3dd7b90", Name: "metadataRevision"}
var LastModifiedAt = Key{ID: 0x3008}
