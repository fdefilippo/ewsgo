// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import "context"

// MailContentReader is an optional, read-only capability for normal email in
// one own, native mail folder. Success means complete observed membership, not
// a transactional point-in-time snapshot. Errors return no usable partial view.
// Calendar events, FAI and native writes are outside this capability.
//
// Each object has its direct Parent, native revision presence, LastModifiedAt,
// complete MIME, and available header-derived To/Cc/Bcc recipients populated.
// This does not recover SMTP envelope recipients absent from the message.
// Implementations bracket MIME with matching
// native versions and verify membership again. Callers must account for the
// returned buffers' lifetime; no server-side cursor or MAPI identity is implied.
type MailContentReader interface {
	ReadMailContents(context.Context, ID) (MailContents, error)
}

// MailContentIndexer returns complete verified membership and message metadata,
// without fetching MIME. Each entry must include a backend content revision
// that changes whenever the MIME changes, and a separate metadata revision.
type MailContentIndexer interface {
	ReadMailContentIndex(context.Context, ID) (MailContents, error)
}

// VersionedMIME identifies observations for which ContentRevision identifies
// the MIME version even when the MIME itself has not been loaded.
var VersionedMIME = Key{Set: "f8b556e0-5d21-4ce8-9043-05e3d3dd7b90", Name: "versionedMIME"}

// MailFolderObserver reads the direct messages and subfolder metadata of an
// ordinary folder in the authenticated user's own mailbox. Sequence advances
// when normal messages change. Children identifies the direct subfolders and
// their metadata, independently of changes to messages in those subfolders.
// When priorSequence equals Sequence, the backend can omit MessageIDs.
// Otherwise MessageIDs must contain the complete verified direct membership.
type MailFolderObserver interface {
	ObserveMailFolder(ctx context.Context, folder ID, priorSequence *uint64) (MailFolderObservation, error)
}

// MailFolderMarkerReader reads revision and count markers for several own
// mail folders without scanning their messages. An unavailable marker is omitted.
type MailFolderMarkerReader interface {
	ReadMailFolderMarkers(context.Context, []ID) (map[ID]MailFolderMarker, error)
}

// FolderContentsObserver extends folder observation to supported groupware
// folders. The same revision and complete membership guarantees apply to
// normal items in those folders. Mail-only operations use MailFolderObserver.
type FolderContentsObserver interface {
	ObserveFolderContents(ctx context.Context, folder ID, priorSequence *uint64) (MailFolderObservation, error)
	ReadFolderContentsMarkers(context.Context, []ID) (map[ID]MailFolderMarker, error)
}

type MailFolderMarker struct {
	Sequence     uint64
	Children     [32]byte
	MessageCount uint64
}

type MailFolderObservation struct {
	Sequence     uint64
	Children     [32]byte
	MessageCount uint64
	MessageIDs   []ID
}

func (MailFolderObservation) String() string   { return "MailFolderObservation{private}" }
func (MailFolderObservation) GoString() string { return "MailFolderObservation{private}" }

// MailMessageEnvelopeReader reads a single message in the given own mail
// folder without fetching MIME or changing read state. A message moved to
// another folder is absent from this lookup.
type MailMessageEnvelopeReader interface {
	ReadMailMessageEnvelope(ctx context.Context, folder, message ID, includeSoftDeleted bool) (Object, error)
}

// MailMessageMIMEReader reads one complete bounded MIME object in the given
// own folder. It verifies folder and content revision around the MIME read and
// returns the envelope with its ContentRevision and MIME populated.
type MailMessageMIMEReader interface {
	ReadMailMessageMIME(ctx context.Context, folder, message ID, includeSoftDeleted bool) (Object, error)
}

type MailContents struct {
	Folder   ID
	Messages []Object
}

func (MailContents) String() string   { return "MailContents{private}" }
func (MailContents) GoString() string { return "MailContents{private}" }

const (
	MaxMailContentMessages   = 256
	MaxMailContentIndexItems = 1024
	MaxMailContentMIMEBytes  = 1 << 20
	MaxMailContentViewBytes  = 4 << 20
)
