// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

// Package mapireplica defines the persisted state shared by the MAPI
// frontend and its storage adapter.
package mapireplica

import (
	"context"
	"strings"
	"time"

	"ewsgo/internal/core"
)

// MaxBindings bounds a full persistent mailbox snapshot. Bindings survive
// object removal so an old client ID can never identify a different object.
const MaxBindings = 16_384

const (
	MaxPermissionMembers = 1024
	MaxFAITombstones     = 1024
	MaxFAITombstoneBytes = 1 << 20
)

type Store interface {
	LoadMAPIReplica(ctx context.Context, mailboxID string) (Snapshot, error)
	CommitMAPIReplica(ctx context.Context, mailboxID string, expected Version, mutation Mutation) error
	ReserveMAPIReplicaIDs(ctx context.Context, mailboxID string, expected Version, count uint32) (IDRange, error)
	ValidateMAPIContentsProof(ctx context.Context, mailboxID string, epoch [16]byte, digest [32]byte) error
	PublishMAPIContentsProof(ctx context.Context, mailboxID string, epoch [16]byte, digest [32]byte) error
	LoadMAPIUploadProofs(ctx context.Context, mailboxID string, epoch [16]byte, root uint64) ([]UploadProof, error)
	PublishMAPIUploadProof(ctx context.Context, mailboxID string, epoch [16]byte, proof UploadProof) error
	BindMAPIPermissionMembers(ctx context.Context, mailboxID string, accounts []string) (map[string]uint64, error)
	MAPIPermissionMember(ctx context.Context, mailboxID string, id uint64) (string, error)
}

// Version is an allocation position. Epoch is a GUID in MAPI wire order.
type Version struct {
	Epoch     [16]byte
	HighWater uint64
}

type Namespace uint8

const (
	Folder Namespace = iota + 1
	Message
	AssociatedMessage
	RecoverableMessage
)

type Binding struct {
	Namespace Namespace
	Key       []byte
	Counter   uint64
}

// Projection is the last observed version. Removed objects retain a version
// so a later delta can acknowledge them.
type Projection struct {
	Counter, Parent          uint64
	ChangeNumber, ReadNumber uint64
	Fingerprint              [32]byte
	NativeRevision           core.Revision
	Modified                 time.Time
	ChangeKey, Predecessors  []byte
	Present, Read            bool
}

type IDRange struct {
	First, Last uint64
}

type Snapshot struct {
	Version         Version
	Bindings        []Binding
	Reserved        []IDRange
	Projection      []Projection
	FAI             FAIState
	Drafts          []DraftPlacement
	Spooler         []SpoolerEntry
	FolderValues    []FolderValue
	FolderCreations []FolderCreation
	ForeignReplicas [][16]byte
	MessageMoves    []MessageMove
	MessageImports  []MessageImport
}

// Bindings allocate above the previous high-water mark. ReservedBindings
// consume IDs issued to an offline client. StagedBindings consume IDs issued
// by the server for objects whose creation has yet to be committed.
type Mutation struct {
	HighWater            uint64
	Bindings             []Binding
	ReservedBindings     []Binding
	StagedBindings       []Binding
	Stage                Namespace
	Projection           []Projection
	FAI                  *FAIChanges
	Drafts               []DraftPlacement
	ForgetDrafts         []uint64
	Spooler              *SpoolerEntry
	ForgetSpooler        uint64
	ResolveSpooler       uint64
	FolderValues         []FolderValue
	FolderCreation       *FolderCreation
	ForgetFolderCreation uint64
	ForeignReplica       [16]byte
	MessageMove          *MessageMove
	CancelMessageMove    uint64
	MessageImport        *MessageImport
	ImportAction         MessageImportAction
}

// SpoolerEntry binds one queued native draft revision to its original MAPI folder.
type SpoolerEntry struct {
	Counter, Folder uint64
	Revision        core.Revision
	State           SpoolerState
}

const MaxSpoolerEntries = 256

type SpoolerState uint8

const (
	SpoolerQueued SpoolerState = iota + 1
	SpoolerDispatching
	SpoolerAccepted
)

type MessageImport struct {
	Counter, Parent      uint64
	Digest, MIME         [32]byte
	NativeID             core.ID
	ContactReference     [16]byte
	AppointmentReference [16]byte
}

func (p MessageImport) Special() core.SpecialFolderKind {
	if p.AppointmentReference != ([16]byte{}) {
		return core.CalendarFolder
	}
	if p.ContactReference != ([16]byte{}) {
		return core.ContactsFolder
	}
	return 0
}

type MessageImportAction uint8

const (
	ClaimMessageImport MessageImportAction = iota + 1
	RecordMessageImport
	CancelMessageImport
	PublishMessageImport
)

// MessageMove claims a client-issued destination counter before moving a
// message. A nonzero Result is the committed destination change number.
// The request digest covers the source and destination identities and history.
type MessageMove struct {
	Source, Destination uint64
	From, To            uint64
	NativeID            core.ID
	Revision            core.Revision
	Request             [32]byte
	Result              uint64
	NewerClient         bool
}

// Special identifies the native typed namespace. Contact IDs carry the same
// core discriminator in their persisted import, move, and active binding.
func (m MessageMove) Special() core.SpecialFolderKind {
	if strings.HasPrefix(string(m.NativeID), core.ContactIDPrefix) {
		return core.ContactsFolder
	}
	return 0
}

// RetiredMessageKey keeps the source identity available for ICS deletion
// after transferring its native binding to the destination counter.
func RetiredMessageKey(counter uint64) []byte {
	return []byte{0xfe, byte(counter >> 40), byte(counter >> 32), byte(counter >> 24),
		byte(counter >> 16), byte(counter >> 8), byte(counter)}
}

// FolderCreation claims a client-issued FID before native folder creation.
// A native identity is recorded only when the backend returns it.
type FolderCreation struct {
	Counter          uint64
	Parent, NativeID core.ID
	Name             string
	ChangeKey        []byte
}

// UploadProof relates client SeenFAI, Seen and Read sets before and after
// a completed collector transfer. A zero optional digest leaves that set
// unchanged. Created lists the only IDs this transfer may add to Given.
type UploadProof struct {
	Root                             uint64
	Before, ReadBefore, NormalBefore []byte
	After, ReadAfter, NormalAfter    [32]byte
	Created                          []uint64
}

type FolderValueKind uint8

const (
	FolderDefinition FolderValueKind = iota + 1
	FolderProperties
	FolderSearch
	FolderOST
	FolderRetiredMIDs
)

// A nil or empty value deletes the extension of that kind for the FID.
type FolderValue struct {
	Counter uint64
	Kind    FolderValueKind
	Data    []byte
}

// DraftPlacement ties a server-issued MID to a native draft creation, or to
// its acknowledged Outbox placement. An empty NativeID denotes a dispatched
// creation whose native identity still needs to be reconciled.
type DraftPlacement struct {
	Counter, Folder    uint64
	Reference          string
	NativeFolder       core.ID
	NativeID, CopyOf   core.ID
	SubmissionRevision core.Revision
	TrashPending       bool
}

type FAIObject struct {
	Counter       uint64
	Source        [22]byte
	Properties    core.Properties
	CacheBytes    int
	PrivateConfig bool
}

type FAITombstone struct {
	Source [22]byte
	Folder uint64
}

type FAIState struct {
	Objects    []FAIObject
	Tombstones []FAITombstone
}

type FAIChanges struct {
	Objects          []FAIObject
	Tombstones       []FAITombstone
	ForgetObjects    []uint64
	ForgetTombstones [][22]byte
}
