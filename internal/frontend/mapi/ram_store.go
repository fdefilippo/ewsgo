// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"container/list"
	"context"
	"crypto/rand"
	"encoding/binary"
	"errors"
	"strings"
	"sync"
	"sync/atomic"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const maxRAMStores = 64
const maxRAMMappings = 4096

// ramStores belongs to one SessionTransport/installation. Entries never get
// evicted and counters never get reused in this process. Quotas fail closed.
// No backend session, credentials or token is retained here.
type ramStores struct {
	contents         contentsMemory
	contactsMetadata bool
	tasksMetadata    bool
	draftsReference  bool
	privateConfig    bool
	calendarMetadata bool // Immutable after configuring the transport.
	mu               sync.Mutex
	entries          map[core.Scope]*ramStore
	namedStorage     func(context.Context, core.Scope) (NamedPropertyStorage, error)
	replicaStorage   func(context.Context, core.Scope) (boundReplica, error)
	nextHandle       atomic.Uint64
	transfers        atomic.Int64 // Bounded across all EMSMDB contexts of this transport.
}

type folderKey struct {
	native  core.ID
	role    SpecialFolderRole
	dynamic uint64
}

type ramStore struct {
	foreignReplicas       [][16]byte // Guarded by mu; index + 2 is the nonreused REPLID.
	foreignReplicaLease   *contentsLease
	permissionMembers     map[core.Scope]uint64 // Guarded by mu; dedicated, nonreused MemberIds.
	permissionMemberLease *contentsLease        // Permanent RAM registry charged to the shared ledger.
	localReservations     []idRange             // Own-replica client GIDs reserved by GetLocalReplicaIds.
	retiredMIDs           map[uint64][]idRange  // Abandoned reservations indexed by folder, guarded by mu.
	retiredRangeCount     int
	retiredMIDUnion       []idRange
	allocator             *ramStores
	fai                   faiNamespace // Guarded by contentsMu. No operational writer yet.
	calendarMetadata      bool
	contactsMetadata      bool
	tasksMetadata         bool
	draftsReference       bool
	privateConfig         bool
	localFreeBusy         messageRow // Guarded by mu.
	localFreeBusyLease    *contentsLease
	mu                    sync.Mutex
	streamLocks           streamLocks
	contentsMu            sync.Mutex // Serializes complete message observations and future FAI transactions.
	folderAggregatesMu    sync.Mutex // Serializes verified mail folder observations.
	folderAggregates      map[core.ID]folderAggregateState
	contentsProofsMu      sync.Mutex // Never held across backend I/O.
	contentsProofs        map[[32]byte]*contentsStateProof
	contentsProofOrder    list.List // Oldest completed checkpoint first, guarded by contentsProofsMu.
	faiStatePermits       map[*faiStatePermit]bool
	byMessage             map[messageKey]uint64
	byMID                 map[uint64]messageKey
	embeddedMIDs          map[[32]byte]uint64 // Guarded by mu. Embedded IDs are not standalone messages.
	messages              map[messageKey]messageVersion
	messageMoves          map[uint64]mapireplica.MessageMove // Guarded by mu and serialized by contentsMu.
	unboundMoves          int
	pendingMessageImports map[uint64]mapireplica.MessageImport
	recoverableMIDs       map[core.ID]uint64 // Separate from active message observations; guarded by mu.
	recoverableByMID      map[uint64]recoverableReference
	recoverableBytes      int
	recoverableMIDLease   *contentsLease
	hierarchyMu           sync.Mutex // Serializes native reads and commits; never acquired under mu.
	hierarchy             map[folderKey]hierarchyVersion
	pendingCreations      map[uint64]hierarchyCreation // Guarded by mu and serialized by hierarchyMu.
	unboundCreations      int                          // Reserved mapping slots without an acknowledged native ID.
	pendingMailDrafts     map[string]pendingMailDraft  // Guarded by mu, indexed by the private MIME reference.
	spoolerMu             sync.Mutex                   // Serializes native sends and the initial enqueue.
	spoolerQueue          map[uint64]spooledDraft      // Guarded by mu, keyed by original MID.
	outboxDrafts          map[core.ID]outboxDraft      // Guarded by mu, one entry per bound native message.
	scope                 core.Scope
	root                  core.ID
	inbox                 core.ID
	sent                  core.ID
	trash                 core.ID
	replica               [16]byte
	categoryStateKey      [32]byte // Process-local key for opaque table collapse state.
	byKey                 map[folderKey]uint64
	byFID                 map[uint64]folderKey
	next                  uint64
	durable               boundReplica
	named                 namedPropertyRegistry
	ost                   map[uint64][]byte
	metadata              map[uint64]folderMetadata
	metadataBytes         int
	dynamic               map[uint64]ramFolder
	dynamicBytes          int
	localNotifications    localNotificationJournal // Guarded by mu.
	receiveInbox          uint64                   // Receive-folder projection, guarded by mu.
	receiveInboxSince     uint64
	receiveRootSince      uint64
}

type storeBinding struct {
	store       *ramStore
	fids        [specialFolderCount]uint64
	mailboxGUID [16]byte
}

func newRAMStores() *ramStores { return &ramStores{entries: make(map[core.Scope]*ramStore)} }

func (s *ramStores) bind(ctx context.Context, catalog privateFolderCatalog) (storeBinding, error) {
	if catalog.scope.Backend == "" || catalog.scope.Account == "" || catalog.folders[IPMSubtree].NativeID == "" {
		return storeBinding{}, core.ErrInvalid
	}
	keys := make([]folderKey, specialFolderCount)
	seen := map[folderKey]bool{}
	for i, entry := range catalog.folders {
		native := entry.Role == IPMSubtree || entry.Role == Inbox || entry.Role == SentItems || entry.Role == DeletedItems
		if entry.Role != SpecialFolderRole(i) || native != (entry.NativeID != "") {
			return storeBinding{}, core.ErrInvalid
		}
		key := folderKey{role: entry.Role}
		if native {
			key = folderKey{native: entry.NativeID}
		}
		if seen[key] {
			return storeBinding{}, core.ErrConflict
		}
		seen[key] = true
		keys[i] = key
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	store := s.entries[catalog.scope]
	if store == nil {
		if len(s.entries) >= maxRAMStores {
			return storeBinding{}, core.ErrUnsupported
		}
		store = &ramStore{allocator: s, scope: catalog.scope, root: catalog.folders[IPMSubtree].NativeID, inbox: catalog.folders[Inbox].NativeID, sent: catalog.folders[SentItems].NativeID, trash: catalog.folders[DeletedItems].NativeID, byKey: map[folderKey]uint64{}, byFID: map[uint64]folderKey{}, calendarMetadata: s.calendarMetadata, contactsMetadata: s.contactsMetadata, tasksMetadata: s.tasksMetadata, draftsReference: s.draftsReference, privateConfig: s.privateConfig}
		_, _ = rand.Read(store.replica[:])
		_, _ = rand.Read(store.categoryStateKey[:])
		store.replica[7] = store.replica[7]&0x0f | 0x40 // UUIDv4 in GUID wire order.
		store.replica[8] = store.replica[8]&0x3f | 0x80
		if s.namedStorage != nil {
			backing, err := s.namedStorage(ctx, catalog.scope)
			if err != nil {
				return storeBinding{}, err
			}
			if err := store.named.load(ctx, backing); err != nil {
				return storeBinding{}, err
			}
		}
		if s.replicaStorage != nil {
			backing, err := s.replicaStorage(ctx, catalog.scope)
			if err != nil {
				return storeBinding{}, err
			}
			if err := store.loadReplica(ctx, backing); err != nil {
				return storeBinding{}, err
			}
		}
	} else if store.root != catalog.folders[IPMSubtree].NativeID {
		return storeBinding{}, core.ErrConflict
	}
	ids, err := store.assign(ctx, keys)
	if err != nil {
		if s.entries[catalog.scope] == nil {
			store.releaseReplicaLeases()
		}
		return storeBinding{}, err
	}
	s.entries[catalog.scope] = store
	binding := storeBinding{store: store}
	copy(binding.fids[:], ids)
	return binding, nil
}

func (s *ramStore) assign(ctx context.Context, keys []folderKey) ([]uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	needed := map[folderKey]bool{}
	for _, key := range keys {
		if _, exists := s.byKey[key]; !exists {
			needed[key] = true
		}
	}
	if s.mappingCountLocked()+len(needed) > maxRAMMappings || s.next+uint64(len(needed)) >= 1<<48 {
		return nil, core.ErrUnsupported
	}
	ids := make([]uint64, len(keys))
	newIDs := make(map[folderKey]uint64, len(needed))
	next := s.next
	for i, key := range keys {
		id := s.byKey[key]
		if id == 0 {
			id = newIDs[key]
			if id == 0 {
				next++
				id = folderID(next)
				newIDs[key] = id
			}
		}
		ids[i] = id
	}
	if s.durable.store != nil && next != s.next {
		mutation := mapireplica.Mutation{HighWater: next}
		for key, fid := range newIDs {
			binding, err := persistedFolderBinding(key, fid)
			if err != nil {
				return nil, err
			}
			mutation.Bindings = append(mutation.Bindings, binding)
		}
		if err := s.commitReplica(ctx, mutation); err != nil {
			return nil, err
		}
	}
	for key, fid := range newIDs {
		s.byKey[key], s.byFID[fid] = fid, key
	}
	s.next = next
	return ids, nil
}

func (s *ramStore) mappingCountLocked() int {
	return len(s.byKey) + len(s.byMessage) + len(s.embeddedMIDs) + len(s.recoverableMIDs) + s.unboundCreations + s.unboundMoves +
		len(s.pendingMailDrafts) + len(s.pendingMessageImports)
}

type storeFolder struct {
	fid, parent   uint64
	native        core.ID
	role          SpecialFolderRole
	properties    core.Properties
	comment       string
	dynamic       bool
	search        bool
	childCount    int
	childrenKnown bool
	aggregate     folderAggregateValues
	aggregateCode uint32
}

func (storeFolder) String() string   { return "storeFolder{redacted}" }
func (storeFolder) GoString() string { return "storeFolder{redacted}" }

// folder checks current native access even for service objects. A mapping is
// never proof that a native folder still exists or remains readable.
func (s *ramStore) folder(ctx context.Context, session core.Session, fid uint64) (storeFolder, error) {
	if session == nil || session.Identity().Scope != s.scope {
		return storeFolder{}, core.ErrAccessDenied
	}
	s.mu.Lock()
	key, exists := s.byFID[fid]
	s.mu.Unlock()
	if !exists {
		return storeFolder{}, core.ErrNotFound
	}
	box := session.Mailbox()
	if box == nil || box.Root() != s.root {
		return storeFolder{}, core.ErrConflict
	}
	root, err := box.Get(ctx, s.root)
	if err != nil {
		return storeFolder{}, err
	}
	if root.Kind != core.Folder || root.ID != s.root {
		return storeFolder{}, core.ErrInvalid
	}
	if key.dynamic != 0 {
		return s.dynamicFolder(ctx, session, fid, key)
	}
	out := storeFolder{fid: fid, native: key.native, role: key.role}
	parent := folderKey{role: MailboxRoot}
	if key.native != "" {
		object := root
		if key.native != s.root {
			if strings.HasPrefix(string(key.native), core.SharedGroupwareFolderIDPrefix) {
				object, err = s.metadataFolderByID(ctx, session, key.native)
			} else {
				object, err = box.Get(ctx, key.native)
				if (s.calendarMetadata || s.contactsMetadata || s.tasksMetadata) && errors.Is(err, core.ErrNotFound) {
					object, err = s.metadataFolderByID(ctx, session, key.native)
				}
			}
			if err != nil {
				return storeFolder{}, err
			}
			if object.Parent == "" || object.Parent == object.ID {
				return storeFolder{}, core.ErrInvalid
			}
			parent = folderKey{native: object.Parent}
		}
		if object.ID != key.native || object.Kind != core.Folder {
			return storeFolder{}, core.ErrInvalid
		}
		out.properties, err = core.CloneProperties(object.Properties)
		if err != nil {
			return storeFolder{}, err
		}
	} else {
		service, ok := serviceFolderForRole(key.role)
		if !ok || key.role == freeBusyFolderRole && !s.privateConfig {
			return storeFolder{}, core.ErrInvalid
		}
		if service.class != "" {
			if err := hierarchyOwner(ctx, box); err != nil {
				return storeFolder{}, err
			}
		}
		out.properties = emptyNormalFolderProperties(service.name)
		if key.role == freeBusyFolderRole && s.freeBusyMessage().mid != 0 {
			out.properties[core.FolderItemCount] = core.Integer(1)
			out.properties[core.FolderUnreadCount] = core.Integer(1)
		}
		if key.role == MailboxRoot {
			return out, nil
		}
		parent = service.parent(s.root)
	}
	ids, err := s.assign(ctx, []folderKey{parent})
	if err != nil {
		return storeFolder{}, err
	}
	out.parent = ids[0]
	return out, nil
}

// Handle numbers are unique for this transport process, including across
// contexts/accounts. Released values are never recycled into other objects.
func (s *ramStores) handle() (uint32, error) {
	n := s.nextHandle.Add(1)
	if n >= 0xffffffff {
		return 0, core.ErrUnsupported
	}
	return uint32(n), nil
}

// A FID is REPLID (LE16) followed by GLOBCNT (MSB first).
// Keep the aggregate uint64 opaque to every caller.
func folderID(count uint64) uint64 {
	counter := globalCount(count)
	wire := [8]byte{1, 0}
	copy(wire[2:], counter[:])
	return binary.LittleEndian.Uint64(wire[:])
}

// These roles have no slot in the 13-FID private Logon response.
const (
	journalFolderRole SpecialFolderRole = specialFolderCount + iota
	notesFolderRole
	freeBusyFolderRole
	serviceFolderRoleCount
)

type serviceFolder struct {
	name, class  string
	inIPMSubtree bool
}

var serviceFolders = [serviceFolderRoleCount]serviceFolder{
	MailboxRoot:        {name: "Mailbox Root"},
	DeferredAction:     {name: "Deferred Action"},
	SpoolerQueue:       {name: "Spooler Queue"},
	Outbox:             {name: "Outbox", inIPMSubtree: true},
	CommonViews:        {name: "Common Views"},
	Schedule:           {name: "Schedule"},
	Search:             {name: "Search"},
	Views:              {name: "Views"},
	Shortcuts:          {name: "Shortcuts"},
	journalFolderRole:  {name: "Journal", class: "IPF.Journal", inIPMSubtree: true},
	notesFolderRole:    {name: "Notes", class: "IPF.StickyNote", inIPMSubtree: true},
	freeBusyFolderRole: {name: "Freebusy Data", class: "IPF.Note"},
}

func serviceFolderForRole(role SpecialFolderRole) (serviceFolder, bool) {
	if role >= serviceFolderRoleCount {
		return serviceFolder{}, false
	}
	folder := serviceFolders[role]
	return folder, folder.name != ""
}

func (f serviceFolder) parent(root core.ID) folderKey {
	if f.inIPMSubtree {
		return folderKey{native: root}
	}
	return folderKey{role: MailboxRoot}
}

func serviceFolderName(role SpecialFolderRole) string {
	folder, _ := serviceFolderForRole(role)
	return folder.name
}
