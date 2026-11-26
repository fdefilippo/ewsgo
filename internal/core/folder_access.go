// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"time"
)

// FolderAccessReader returns a complete observed ACL or explicitly unknown
// data. It does not expose protocol bitmasks, grant passwords or access keys.
// A token identifies this observation, not a database revision or durable ID.
// Callers must revalidate before using a retained snapshot; expiry is independent
// of changes to the token. Implementations do not promise transactional reads.
type FolderAccessReader interface {
	ReadFolderAccess(context.Context, ID) (FolderAccess, error)
}

// FolderAccessWriter applies one grant change in the scope of an observed
// complete ACL. It guards the target folder during the native mutation and
// returns a fresh observation. An uncertain result requires reconciliation.
// A zero expiry preserves the expiry of an existing grant or lets the backend
// apply its default to a new grant. A nonzero expiry must be representable.
type FolderAccessWriter interface {
	SetFolderAccessGrant(ctx context.Context, expected FolderAccess, grant AccessGrant) (FolderAccess, error)
	RemoveFolderAccessGrant(ctx context.Context, expected FolderAccess, kind AccessSubjectKind, subject string) (FolderAccess, error)
}

// FolderUserGrantsWriter replaces all user grants of a complete, explicit,
// user-only ACL in one native transaction. The retained grants must carry
// their observed expiry. A new grant uses the native default expiry. An
// empty list clears the ACL. Unsupported ACLs are rejected before writing.
type FolderUserGrantsWriter interface {
	ReplaceFolderUserGrants(ctx context.Context, expected FolderAccess, grants []AccessGrant) (FolderAccess, error)
}

type AccessCompleteness uint8

const (
	AccessUnknown AccessCompleteness = iota
	AccessComplete
)

type AccessProvenance uint8

const (
	AccessUnspecified AccessProvenance = iota
	AccessExplicit
	AccessInherited
	AccessServicePolicy
)

type AccessSubjectKind uint8

const (
	AccessSubjectUnknown AccessSubjectKind = iota
	AccessUser
	AccessGroup
	AccessDomain
	AccessCOS
	AccessAuthenticated
	AccessPublic
	AccessGuest
	AccessKey
)

type AccessRights uint32

const (
	AccessReadItems AccessRights = 1 << iota
	AccessCreateItems
	AccessEditOwned
	AccessEditAny
	AccessDeleteOwned
	AccessDeleteAny
	AccessCreateSubfolders
	AccessVisible
	AccessAdminister
	AccessPrivate
	AccessWorkflow
	AccessFreeBusySimple
	AccessFreeBusyDetailed
)
const MaxFolderAccessGrants = 256

// AccessComposition describes backend-specific rules applied after grants are
// united. User grants include the union of authenticated and public grants;
// authenticated includes public. Other subject types must not be discarded.
type AccessComposition uint8

const (
	AccessUnion                AccessComposition = iota
	AccessReadCreateSubfolders                   // ReadItems + CreateItems also permit creating subfolders.
)

type AccessGrant struct {
	Kind      AccessSubjectKind
	Subject   string // Native principal identity; never a grant password or key.
	Rights    AccessRights
	ExpiresAt time.Time // Exclusive expiry instant; zero means no expiry.
}
type FolderAccess struct {
	Composition    AccessComposition
	Scope          Scope
	Folder, Source ID
	Token          [32]byte
	Completeness   AccessCompleteness
	Provenance     AccessProvenance
	CanViewList    bool
	// EffectiveRights contains confirmed caller actions for this reader; it is
	// not an exhaustive authorization contract for other backend operations.
	EffectiveRights AccessRights
	ObservedAt      time.Time
	Grants          []AccessGrant
}

func (FolderAccess) String() string   { return "FolderAccess{private}" }
func (FolderAccess) GoString() string { return "FolderAccess{private}" }
func (AccessGrant) String() string    { return "AccessGrant{private}" }
func (AccessGrant) GoString() string  { return "AccessGrant{private}" }
