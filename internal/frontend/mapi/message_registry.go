// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"hash"
	"maps"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"ewsgo/internal/core"
	"ewsgo/internal/mailmime"
)

const maxMessageRegistryBytes = 1 << 20 // Accounted keys/version metadata only.
const pendingMessageMoveBytes = 192 + 24 + 25*maxFAIPCLReplicas

// Carries only a server policy category, never the native role or folder ID.
type normalMetadataOnlyError struct{}

func (normalMetadataOnlyError) Error() string { return "normal contents are metadata-only" }
func (normalMetadataOnlyError) Unwrap() error { return core.ErrUnsupported }

// Normal identities are native opaque IDs; FAI identities belong to EWSGo's
// local namespace. The discriminator is part of identity, never a name prefix.
type messageKey struct {
	id         core.ID
	associated bool
	special    core.SpecialFolderKind
	retired    uint64
}
type messageVersion struct {
	fingerprint        [32]byte
	parent, cn, readCN uint64
	nativeRevision     core.Revision
	modified           time.Time
	change             changeXID
	history            string
	present, read      bool
}
type messageNode struct {
	sync        *faiSyncVersion
	key         messageKey
	fingerprint [32]byte
	object      core.Object
	read        bool
	modified    time.Time
	bytes       int // Validated detached object accounting units, including overhead.
}
type messageRow struct {
	mid, parent, cn, readCN uint64
	sourceKey               [22]byte
	changeKey               changeXID
	predecessors            []byte
	associated, read        bool
	privateConfig           bool
	special                 core.SpecialFolderKind
	modified                time.Time
	object                  core.Object
}
type messageKnown struct {
	mid, parent, cn, readCN uint64
	associated, present     bool
}
type messageSnapshot struct {
	replica         [16]byte
	root, highWater uint64
	associated      bool
	messages        []messageRow
	known           []messageKnown
	retainedBytes   int // Detached objects plus row/version metadata; never logged.
}

func (messageKey) String() string        { return "messageKey{private}" }
func (messageKey) GoString() string      { return "messageKey{private}" }
func (messageNode) String() string       { return "messageNode{private}" }
func (messageNode) GoString() string     { return "messageNode{private}" }
func (messageRow) String() string        { return "messageRow{private}" }
func (messageRow) GoString() string      { return "messageRow{private}" }
func (messageSnapshot) String() string   { return "messageSnapshot{private}" }
func (messageSnapshot) GoString() string { return "messageSnapshot{private}" }

func (r messageRow) indexedMIME() bool {
	return !r.associated && !r.privateConfig && r.special == 0 && len(r.object.MIME) == 0 &&
		r.object.Properties[core.VersionedMIME] == core.Bool(true)
}

// This prerequisite produces detached observations only. No Message ROP,
// Contents context, FAI writer, native write or persistent identity is enabled.
func (s *ramStore) observeMailMessages(ctx context.Context, session core.Session, anchor uint64) (messageSnapshot, error) {
	return s.observeMailMessageSet(ctx, session, anchor, false)
}

func (s *ramStore) observeMailMessageSet(ctx context.Context, session core.Session, anchor uint64, allowIndex bool) (messageSnapshot, error) {
	if s.isFreeBusyFolder(anchor) {
		return s.observeFreeBusyMessage(ctx, session, anchor)
	}
	return s.observeMessageSet(ctx, session, anchor, false, func(ctx context.Context, box core.Mailbox, key folderKey) ([]core.Object, error) {
		outbox := key == (folderKey{role: Outbox})
		folder := key.native
		if outbox && s.hasOutboxDrafts() {
			var err error
			folder, err = s.nativeOutboxDraftsFolder(ctx, session)
			if err != nil {
				return nil, err
			}
		}
		if folder == "" {
			return nil, nil
		} // Genuine EWSGo service/RAM normal namespace.
		if index, ok := box.(core.MailContentIndexer); allowIndex && ok && !outbox && !s.needsCompleteMailObjects(anchor, folder) {
			view, err := index.ReadMailContentIndex(ctx, folder)
			if err != nil && !errors.Is(err, core.ErrUnsupported) {
				return nil, err
			}
			if err == nil {
				if view.Folder != folder {
					return nil, core.ErrInvalid
				}
				for _, object := range view.Messages {
					if len(object.MIME) != 0 || object.Properties[core.VersionedMIME] != core.Bool(true) {
						return nil, core.ErrInvalid
					}
				}
				return annotateDeferredActionTargets(ctx, box, s.filterOutboxDrafts(view.Messages, folder, false))
			}
		}
		source, ok := box.(core.MailContentReader)
		if !ok {
			return nil, core.ErrUnsupported
		}
		view, err := source.ReadMailContents(ctx, folder)
		if err != nil {
			return nil, err
		}
		if view.Folder != folder {
			return nil, core.ErrInvalid
		}
		for _, object := range view.Messages {
			if len(object.MIME) == 0 {
				return nil, core.ErrInvalid
			}
		}
		return annotateDeferredActionTargets(ctx, box, s.filterOutboxDrafts(view.Messages, folder, outbox))
	})
}

// Called under contentsMu. Pending writes can require the actual MIME digest
// or the draft recovery header before assigning a native message its MID.
func (s *ramStore) needsCompleteMailObjects(anchor uint64, folder core.ID) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, p := range s.pendingMessageImports {
		if folderID(p.Parent) == anchor {
			return true
		}
	}
	for _, p := range s.pendingMailDrafts {
		if p.dispatched && p.folder == folder {
			return true
		}
	}
	return false
}

// The source is trusted application code for one complete namespace. It is
// called after current authorization, never retained, and its output is copied.
// FAI callers must supply a complete local namespace; normal calls use the
// explicit MailContentReader. No caller-provided folder key bypasses the anchor.
func (s *ramStore) observeMessageSet(ctx context.Context, session core.Session, anchor uint64, associated bool, source func(context.Context, core.Mailbox, folderKey) ([]core.Object, error)) (messageSnapshot, error) {
	return s.observeScopedMessageSet(ctx, session, anchor, associated, 0, source)
}

func (s *ramStore) observeScopedMessageSet(ctx context.Context, session core.Session, anchor uint64, associated bool, special core.SpecialFolderKind, source func(context.Context, core.Mailbox, folderKey) ([]core.Object, error)) (messageSnapshot, error) {
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	key, err := s.scopedMessageAnchor(ctx, session, anchor, associated, special)
	if err != nil {
		return messageSnapshot{}, err
	}
	objects, err := source(ctx, session.Mailbox(), key)
	if err != nil {
		return messageSnapshot{}, err
	}
	nodes, err := scopedMessageNodes(objects, key.native, associated, special)
	if err != nil {
		return messageSnapshot{}, err
	}
	objects = nil
	trashReader, canReadTrashContacts := session.Mailbox().(core.TrashedContactReader)
	includeTrashContacts := special == 0 && !associated && canReadTrashContacts && s.contactsMetadata &&
		s.trash != "" && key.native == s.trash
	if includeTrashContacts {
		contacts, err := trashedContactNodes(ctx, trashReader, key.native)
		if err != nil {
			return messageSnapshot{}, err
		}
		if len(nodes)+len(contacts) > core.MaxMailContentIndexItems {
			return messageSnapshot{}, core.ErrLimit
		}
		budget := 0
		for _, set := range [][]messageNode{nodes, contacts} {
			for _, node := range set {
				budget += node.bytes
			}
		}
		if budget > core.MaxMailContentViewBytes {
			return messageSnapshot{}, core.ErrLimit
		}
		nodes = append(nodes, contacts...)
	}
	if associated {
		s.attachFAISync(nodes)
	}
	again, err := s.scopedMessageAnchor(ctx, session, anchor, associated, special)
	if err != nil {
		return messageSnapshot{}, err
	}
	if again != key {
		return messageSnapshot{}, core.ErrConflict
	}
	return s.commitMessages(ctx, anchor, associated, special, includeTrashContacts, nodes, nil)
}

// Authorization is side-effect free: mappings are never created as a side
// effect of checking them. Special native roles are metadata-only and rejected for Normal.
func (s *ramStore) messageAnchor(ctx context.Context, session core.Session, anchor uint64, associated bool) (folderKey, error) {
	return s.scopedMessageAnchor(ctx, session, anchor, associated, 0)
}

// Retained tables and transfer buffers can outlive the grant used to capture
// them. Include the live rights, in particular visibility of private events.
func (s *ramStore) sharedContentRights(ctx context.Context, session core.Session, fid uint64) (core.AccessRights, error) {
	s.mu.Lock()
	key := s.byFID[fid]
	s.mu.Unlock()
	if !strings.HasPrefix(string(key.native), core.SharedGroupwareFolderIDPrefix) {
		return 0, nil
	}
	if session == nil || session.Identity().Scope != s.scope || session.Mailbox() == nil || session.Mailbox().Root() != s.root {
		return 0, core.ErrAccessDenied
	}
	shares, ok := session.Mailbox().(core.SharedGroupwareFolders)
	if !ok {
		return 0, core.ErrAccessDenied
	}
	item, err := shares.ReadSharedGroupwareFolder(ctx, key.native, 0)
	if err != nil {
		return 0, err
	}
	rights, ok := item.Properties[core.SharedFolderRights].(core.Integer)
	if item.ID != key.native || !ok || core.AccessRights(rights)&core.AccessReadItems == 0 {
		return 0, core.ErrAccessDenied
	}
	return core.AccessRights(rights), nil
}

func (s *ramStore) checkSharedContentRights(ctx context.Context, session core.Session, fid uint64, expected core.AccessRights) error {
	if expected == 0 {
		return nil
	}
	rights, err := s.sharedContentRights(ctx, session, fid)
	if err != nil {
		return err
	}
	if rights != expected {
		return core.ErrAccessDenied
	}
	return nil
}

func (s *ramStore) scopedMessageAnchor(ctx context.Context, session core.Session, anchor uint64, associated bool, special core.SpecialFolderKind) (folderKey, error) {
	if err := ctx.Err(); err != nil {
		return folderKey{}, err
	}
	if session == nil || session.Identity().Scope != s.scope {
		return folderKey{}, core.ErrAccessDenied
	}
	box := session.Mailbox()
	if box == nil || box.Root() != s.root {
		return folderKey{}, core.ErrConflict
	}
	s.mu.Lock()
	key, ok := s.byFID[anchor]
	s.mu.Unlock()
	if !ok {
		return folderKey{}, core.ErrNotFound
	}
	if err := hierarchyOwner(ctx, box); err != nil {
		return folderKey{}, err
	}
	catalog, objects, err := readPrivateFolderTreeWithSpecialFolders(ctx, session, s.calendarMetadata, s.contactsMetadata, s.tasksMetadata)
	if err != nil {
		return folderKey{}, err
	}
	if catalog.scope != s.scope || catalog.folders[IPMSubtree].NativeID != s.root {
		return folderKey{}, core.ErrConflict
	}
	if s.trash != "" && key.native == s.trash && catalog.folders[DeletedItems].NativeID != key.native {
		return folderKey{}, core.ErrConflict
	}
	nodes, err := s.projectedNodes(objects)
	if err != nil {
		return folderKey{}, err
	}
	found := false
	var class string
	for _, node := range nodes {
		if node.key == key {
			found, class = true, node.containerClass
			break
		}
	}
	if !found {
		return folderKey{}, core.ErrNotFound
	}
	if special != 0 {
		role := searchCalendarRole(class)
		if class == "IPF.Contact" {
			role = core.ContactsFolder
		}
		if associated || key.native == "" || role != special {
			return folderKey{}, core.ErrNotFound
		}
	} else if !associated {
		for _, object := range objects {
			if object.ID == key.native && metadataClass(object.Properties) != "" {
				return folderKey{}, normalMetadataOnlyError{}
			}
		}
	}
	if err := hierarchyOwner(ctx, box); err != nil {
		return folderKey{}, err
	}
	if session.Identity().Scope != s.scope || session.Mailbox() == nil || session.Mailbox().Root() != s.root {
		return folderKey{}, core.ErrAccessDenied
	}
	if err := ctx.Err(); err != nil {
		return folderKey{}, err
	}
	return key, nil
}

func messageNodes(objects []core.Object, parent core.ID, associated bool) ([]messageNode, error) {
	return scopedMessageNodes(objects, parent, associated, 0)
}

func scopedMessageNodes(objects []core.Object, parent core.ID, associated bool, special core.SpecialFolderKind) ([]messageNode, error) {
	limit := core.MaxMailContentIndexItems
	if associated {
		limit = 64
	}
	if len(objects) > limit {
		return nil, core.ErrUnsupported
	}
	nodes := make([]messageNode, 0, len(objects))
	seen := map[core.ID]bool{}
	budget := 0
	for _, o := range objects {
		if o.ID == "" || len(o.ID) > maxHierarchyText || !utf8.ValidString(string(o.ID)) || strings.ContainsRune(string(o.ID), 0) || o.Kind != core.Message || o.Parent != parent || seen[o.ID] {
			return nil, core.ErrInvalid
		}
		seen[o.ID] = true
		read, ok := o.Properties[core.IsRead].(core.Bool)
		if !ok {
			return nil, core.ErrInvalid
		}
		stamp, ok := o.Properties[core.LastModifiedAt].(core.Timestamp)
		if !ok {
			return nil, core.ErrInvalid
		}
		modified := time.Time(stamp).UTC()
		if modified.Year() < 1601 || modified.Year() > 9999 {
			return nil, core.ErrInvalid
		}
		if !associated {
			ms, ok := o.Properties[core.MetadataRevision].(core.Integer)
			if !ok || ms < 0 || uint64(ms) != uint64(o.Revision) {
				return nil, core.ErrInvalid
			}
			revision, ok := o.Properties[core.ContentRevision].(core.Integer)
			if !ok || revision < 0 || special != 0 && len(o.MIME) != 0 ||
				special == 0 && len(o.MIME) == 0 && o.Properties[core.VersionedMIME] != core.Bool(true) {
				return nil, core.ErrInvalid
			}
		}
		fingerprint, bytes, err := messageFingerprint(o)
		if err != nil {
			return nil, err
		}
		if associated && bytes > 128<<10 {
			return nil, core.ErrUnsupported
		}
		budget += bytes + len(o.ID) + len(o.Parent) + 1024
		if budget > core.MaxMailContentViewBytes {
			return nil, core.ErrUnsupported
		}
		clone, err := cloneMessageObject(o)
		if err != nil {
			return nil, err
		}
		nodes = append(nodes, messageNode{key: messageKey{id: o.ID, associated: associated, special: special}, fingerprint: fingerprint, object: clone, read: bool(read), modified: modified, bytes: bytes + len(o.ID) + len(o.Parent) + 1024})
	}
	sort.Slice(nodes, func(i, j int) bool { return nodes[i].key.id < nodes[j].key.id })
	return nodes, nil
}

// Versions describe projected content. Native ms/md also change for read flags,
// so they do not independently change the content fingerprint. A read-only
// transition advances only readCN and preserves the native date of that content
// version. Real body revision, properties, recipients and attachments are hashed.
func messageFingerprint(o core.Object) ([32]byte, int, error) {
	var zero [32]byte
	if len(o.Properties) > 256 || len(o.Recipients) > 256 || len(o.Attachments) > 256 || len(o.MIME) > core.MaxMailContentMIMEBytes {
		return zero, 0, core.ErrUnsupported
	}
	h := sha256.New()
	used := 0
	put := func(data []byte) error {
		used += len(data) + 8
		if used > core.MaxMailContentViewBytes {
			return core.ErrUnsupported
		}
		var size [8]byte
		binary.LittleEndian.PutUint64(size[:], uint64(len(data)))
		h.Write(size[:])
		h.Write(data)
		return nil
	}
	keys := make([]core.Key, 0, len(o.Properties))
	for key := range o.Properties {
		if !key.Valid() {
			return zero, 0, core.ErrInvalid
		}
		keys = append(keys, key)
	}
	sort.Slice(keys, func(i, j int) bool {
		a, b := keys[i], keys[j]
		if a.Set != b.Set {
			return a.Set < b.Set
		}
		if a.ID != b.ID {
			return a.ID < b.ID
		}
		if a.EmptyName != b.EmptyName {
			return !a.EmptyName
		}
		return a.Name < b.Name
	})
	contentProperties := uint64(0)
	for _, key := range keys {
		if key != core.IsRead && key != core.MetadataRevision && key != core.LastModifiedAt {
			contentProperties++
		}
	}
	var propertyCount [8]byte
	binary.LittleEndian.PutUint64(propertyCount[:], contentProperties)
	if err := put(propertyCount[:]); err != nil {
		return zero, 0, err
	}
	for _, key := range keys {
		value := o.Properties[key]
		// Validate excluded metadata too. Values are never silently accepted as an
		// unknown core type simply because they do not affect the content version.
		if value == nil {
			return zero, 0, core.ErrInvalid
		}
		skip := key == core.IsRead || key == core.MetadataRevision || key == core.LastModifiedAt
		var target hash.Hash = h
		if skip {
			target = sha256.New()
		}
		old := h
		h = target
		var tag [4]byte
		binary.LittleEndian.PutUint32(tag[:], key.ID)
		name := key.Name
		if key.EmptyName {
			name = "\x00" // Named strings cannot contain NUL.
		}
		for _, field := range [][]byte{[]byte(key.Set), tag[:], []byte(name)} {
			if err := put(field); err != nil {
				return zero, 0, err
			}
		}
		var kind byte
		var data []byte
		switch v := value.(type) {
		case core.Bool:
			kind = 1
			data = []byte{0}
			if v {
				data[0] = 1
			}
		case core.Integer:
			kind = 2
			data = binary.LittleEndian.AppendUint64(nil, uint64(v))
		case core.Text:
			kind = 3
			if !utf8.ValidString(string(v)) {
				return zero, 0, core.ErrInvalid
			}
			data = []byte(v)
		case core.Binary:
			kind = 4
			data = v
		case core.Timestamp:
			kind = 5
			at := time.Time(v).UTC()
			if at.Year() < 1601 || at.Year() > 9999 {
				return zero, 0, core.ErrInvalid
			}
			data = []byte(at.Format(time.RFC3339Nano))
		case core.Strings:
			if len(v) > 256 {
				return zero, 0, core.ErrUnsupported
			}
			if err := put([]byte{6}); err != nil {
				return zero, 0, err
			}
			var count [8]byte
			binary.LittleEndian.PutUint64(count[:], uint64(len(v)))
			if err := put(count[:]); err != nil {
				return zero, 0, err
			}
			for _, item := range v {
				if !utf8.ValidString(item) {
					return zero, 0, core.ErrInvalid
				}
				if err := put([]byte(item)); err != nil {
					return zero, 0, err
				}
			}
			h = old
			continue
		case core.Int32s:
			if len(v) > 256 {
				return zero, 0, core.ErrUnsupported
			}
			kind = 7
			for _, item := range v {
				data = binary.LittleEndian.AppendUint32(data, uint32(item))
			}
		default:
			return zero, 0, core.ErrInvalid
		}
		if err := put([]byte{kind}); err != nil {
			return zero, 0, err
		}
		if err := put(data); err != nil {
			return zero, 0, err
		}
		h = old
	}
	mime := o.MIME
	if o.Properties[core.VersionedMIME] == core.Bool(true) {
		revision, ok := o.Properties[core.ContentRevision].(core.Integer)
		if o.Kind != core.Message || !ok || revision < 0 || used+len(o.MIME) > core.MaxMailContentViewBytes {
			return zero, 0, core.ErrInvalid
		}
		// The content revision was hashed with the properties. Account for the
		// retained raw bytes even when their version comes from the backend.
		used += len(o.MIME)
		mime = nil
	}
	if err := put(mime); err != nil {
		return zero, 0, err
	}
	// Counts frame the two collections even when individual fields are empty.
	var count [8]byte
	binary.LittleEndian.PutUint64(count[:], uint64(len(o.Recipients)))
	if err := put(count[:]); err != nil {
		return zero, 0, err
	}
	for _, r := range o.Recipients {
		if r.Kind > core.RecipientBcc {
			return zero, 0, core.ErrInvalid
		}
		if err := put([]byte{byte(r.Kind)}); err != nil {
			return zero, 0, err
		}
		for _, v := range []string{r.Address, r.DisplayName} {
			if !utf8.ValidString(v) {
				return zero, 0, core.ErrInvalid
			}
			if err := put([]byte(v)); err != nil {
				return zero, 0, err
			}
		}
	}
	binary.LittleEndian.PutUint64(count[:], uint64(len(o.Attachments)))
	if err := put(count[:]); err != nil {
		return zero, 0, err
	}
	for _, a := range o.Attachments {
		for _, v := range [][]byte{[]byte(a.ID), []byte(a.MediaType), a.Data} {
			if err := put(v); err != nil {
				return zero, 0, err
			}
		}
	}
	var sum [32]byte
	copy(sum[:], h.Sum(nil))
	return sum, used, nil
}

func cloneMessageObject(o core.Object) (core.Object, error) {
	props, err := core.CloneProperties(o.Properties)
	if err != nil {
		return core.Object{}, err
	}
	o.Properties = props
	o.MIME = append([]byte(nil), o.MIME...)
	o.Recipients = append([]core.Recipient(nil), o.Recipients...)
	out := make([]core.Attachment, len(o.Attachments))
	for i, a := range o.Attachments {
		out[i] = a
		out[i].Data = append([]byte(nil), a.Data...)
	}
	o.Attachments = out
	return o, nil
}

// Only bounded identity/version metadata is retained, never MIME, properties,
// sessions, passwords or native cursors. Any failure preserves the old registry.
func (s *ramStore) commitMessages(ctx context.Context, anchor uint64, associated bool, special core.SpecialFolderKind, includeTrashContacts bool, nodes []messageNode, previousFAI *faiNamespace) (messageSnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.byFID[anchor]; !ok {
		return messageSnapshot{}, core.ErrNotFound
	}
	needed, allocatedIDs, advances, budget := 0, 0, 0, s.messageRegistryBytesLocked()
	current := map[messageKey]bool{}
	proposed := make(map[messageKey]uint64, len(nodes))
	claimed := make(map[uint64]bool, len(nodes))
	reconciled := make(map[string]core.ID)
	outbox := s.byFID[anchor] == (folderKey{role: Outbox})
	pendingForAnchor := false
	inNamespace := func(key messageKey) bool {
		kind := s.messageKeySpecialLocked(key)
		return kind == special || includeTrashContacts && kind == core.ContactsFolder
	}
	if !associated && special == 0 {
		for _, pending := range s.pendingMailDrafts {
			pendingForAnchor = pendingForAnchor || pending.dispatched && pending.fid == anchor
		}
	}
	for _, n := range nodes {
		if !associated && n.key.special == 0 && s.byMessage[n.key] == 0 && s.pendingImportedMessage(anchor, n.object) {
			return messageSnapshot{}, core.ErrOutcomeUnknown
		}
		if !associated && n.key.special == core.CalendarFolder && s.byMessage[n.key] == 0 && s.pendingCalendarImport(anchor, n.object) {
			return messageSnapshot{}, core.ErrOutcomeUnknown
		}
		if move := s.messageMoves[s.byMessage[n.key]]; move.Source != 0 && move.Result == 0 && folderID(move.From) != anchor {
			return messageSnapshot{}, core.ErrOutcomeUnknown
		}
		if n.key.associated != associated || !inNamespace(n.key) || current[n.key] {
			return messageSnapshot{}, core.ErrInvalid
		}
		current[n.key] = true
		mid, err := s.importedMIDLocked(n)
		if err != nil {
			return messageSnapshot{}, err
		}
		if pendingForAnchor && n.object.Properties[core.IsDraft] == core.Bool(true) {
			reference := mailmime.DraftReference(n.object.MIME)
			if pending := s.pendingMailDrafts[reference]; pending.dispatched && pending.fid == anchor {
				if reconciled[reference] != "" || mid != 0 && mid != pending.mid {
					return messageSnapshot{}, core.ErrConflict
				}
				mid = pending.mid
				reconciled[reference] = n.key.id
			}
		}
		if mid != 0 {
			if claimed[mid] {
				return messageSnapshot{}, core.ErrConflict
			}
			claimed[mid] = true
			proposed[n.key] = mid
		}
		v := s.messages[n.key]
		if move := s.messageMoves[s.byMessage[n.key]]; move.Source != 0 && move.Result == 0 &&
			(v.fingerprint != n.fingerprint || v.read != n.read) {
			return messageSnapshot{}, core.ErrOutcomeUnknown
		}
		if _, mapped := s.byMessage[n.key]; !mapped {
			needed++
			if mid == 0 {
				allocatedIDs++
			}
			budget += 192 + len(n.key.id)
		}
		if !v.present || v.parent != anchor || v.fingerprint != n.fingerprint {
			advances++
		}
		if !v.present || v.read != n.read {
			advances++
		}
	}
	for key, v := range s.messages {
		if key.associated == associated && inNamespace(key) && v.present && v.parent == anchor && !current[key] {
			if move := s.messageMoves[s.byMessage[key]]; move.Source != 0 && move.Result == 0 {
				return messageSnapshot{}, core.ErrOutcomeUnknown
			}
			advances++
		}
	}
	if s.mappingCountLocked()+needed-len(reconciled) > maxRAMMappings || budget > maxMessageRegistryBytes || s.next > maxGlobalCount || uint64(allocatedIDs+advances) > maxGlobalCount-s.next {
		return messageSnapshot{}, core.ErrUnsupported
	}
	versions := make(map[messageKey]messageVersion, len(s.messages)+needed)
	byKey := make(map[messageKey]uint64, len(s.byMessage)+needed)
	byMID := make(map[uint64]messageKey, len(s.byMID)+needed)
	for key, v := range s.messages {
		versions[key] = v
	}
	for key, mid := range s.byMessage {
		byKey[key] = mid
		byMID[mid] = key
	}
	next := s.next
	result := messageSnapshot{replica: s.replica, root: anchor, associated: associated, retainedBytes: 512}
	for _, n := range nodes {
		if byKey[n.key] == 0 {
			mid := proposed[n.key]
			if mid == 0 {
				next++
				mid = folderID(next)
			}
			byKey[n.key], byMID[mid] = mid, n.key
		}
		v := versions[n.key]
		if !v.present || v.parent != anchor || v.fingerprint != n.fingerprint {
			next++
			v.cn = next
			v.modified = n.modified
			if err := s.advanceMessageHistory(&v); err != nil {
				return messageSnapshot{}, err
			}
			budget += len(v.change) + len(v.history) - len(versions[n.key].change) - len(versions[n.key].history)
		}
		if !v.present || v.read != n.read {
			next++
			v.readCN = next
		}
		v.fingerprint, v.parent, v.present, v.read = n.fingerprint, anchor, true, n.read
		v.nativeRevision = n.object.Revision
		versions[n.key] = v
		n.object.Properties[core.LastModifiedAt] = core.Timestamp(v.modified)
		row := messageRow{mid: byKey[n.key], parent: anchor, cn: v.cn, readCN: v.readCN, sourceKey: s.hierarchyXID(byKey[n.key]), changeKey: s.messageChangeKey(v), modified: v.modified, associated: associated, read: v.read, special: n.key.special, object: n.object}
		row.predecessors = s.messagePredecessors(v)
		if n.sync != nil {
			if n.sync.local {
				n.sync.change = gidChangeXID(hierarchyXID(s.replica, v.cn))
				n.sync.predecessors[n.sync.change.namespace()] = n.sync.change.local()
				n.sync.local = false
				n.sync.serverNew = false
			}
			row.sourceKey = n.sync.source
			row.changeKey = n.sync.change
			row.predecessors = n.sync.predecessors.marshal()
		}
		result.messages = append(result.messages, row)
		result.retainedBytes += n.bytes + len(row.predecessors) + 128
	}
	var removed []uint64
	for key, v := range versions {
		if key.associated == associated && inNamespace(key) && v.present && v.parent == anchor && !current[key] {
			removed = append(removed, byKey[key])
		}
	}
	sort.Slice(removed, func(i, j int) bool { return removed[i] < removed[j] })
	for _, mid := range removed {
		key := byMID[mid]
		v := versions[key]
		next++
		v.cn, v.present = next, false
		if err := s.advanceMessageHistory(&v); err != nil {
			return messageSnapshot{}, err
		}
		budget += len(v.change) + len(v.history) - len(versions[key].change) - len(versions[key].history)
		versions[key] = v
	}
	if budget > maxMessageRegistryBytes {
		return messageSnapshot{}, core.ErrUnsupported
	}
	// A newly observed absent FAI has no object or CN. Advance the allocation
	// position by a gap so a second writer cannot commit from the old state.
	if s.durable.store != nil && previousFAI != nil && next == s.next && !maps.Equal(previousFAI.absent, s.fai.absent) {
		if next == maxGlobalCount {
			return messageSnapshot{}, core.ErrUnsupported
		}
		next++
	}
	for mid, key := range byMID {
		v, observed := versions[key]
		if !observed || !inNamespace(key) {
			continue
		}
		result.known = append(result.known, messageKnown{mid: mid, parent: v.parent, cn: v.cn, readCN: v.readCN, associated: key.associated, present: v.present})
		result.retainedBytes += 64
	}
	sort.Slice(result.known, func(i, j int) bool { return result.known[i].mid < result.known[j].mid })
	result.highWater = next
	if err := ctx.Err(); err != nil {
		return messageSnapshot{}, err
	}
	if needed != 0 || next != s.next {
		if err := s.commitMessageReplica(ctx, byKey, versions, next, previousFAI, reconciled); err != nil {
			return messageSnapshot{}, err
		}
		if !associated && special == 0 && s.byFID[anchor] == (folderKey{role: DeferredAction}) {
			s.publishDeferredActionChangesLocked(result, byMID, removed)
		}
		s.byMessage, s.byMID, s.messages, s.next = byKey, byMID, versions, next
	}
	for reference, id := range reconciled {
		if outbox {
			s.linkOutboxDraftLocked(id, reference, s.pendingMailDrafts[reference])
		}
		delete(s.pendingMailDrafts, reference)
	}
	return result, nil
}

func (s *ramStore) messageKeySpecialLocked(key messageKey) core.SpecialFolderKind {
	if key.retired != 0 {
		return s.messageMoves[folderID(key.retired)].Special()
	}
	return key.special
}

func (s *ramStore) messageRegistryBytesLocked() int {
	budget := 16 * len(s.localReservations)
	for key := range s.byMessage {
		budget += 192 + len(key.id)
	}
	for _, version := range s.messages {
		budget += len(version.change) + len(version.history)
	}
	for _, move := range s.messageMoves {
		budget += 160 + len(move.NativeID)
		if move.Result == 0 {
			budget += pendingMessageMoveBytes
		}
	}
	budget += len(s.pendingMessageImports) * pendingMessageImportBytes
	return budget
}

// Called under mu. An imported own-replica GID is already a reserved server
// identity, not a request for a second MID. Only local FAI may claim it.
func (s *ramStore) importedMIDLocked(n messageNode) (uint64, error) {
	if n.sync == nil || !bytes.Equal(n.sync.source[:16], s.replica[:]) {
		return s.byMessage[n.key], nil
	}
	if !n.key.associated {
		return 0, core.ErrAccessDenied
	}
	var counter uint64
	for _, b := range n.sync.source[16:] {
		counter = counter<<8 | uint64(b)
	}
	mid := folderID(counter)
	if (!n.sync.serverNew && s.byMessage[n.key] != mid && !s.clientReservedCounterLocked(counter)) || counter == 0 || counter > s.next {
		return 0, core.ErrAccessDenied
	}
	if s.retiredCounterLocked(counter) {
		return 0, core.ErrConflict
	}
	if _, exists := s.byFID[mid]; exists {
		return 0, core.ErrConflict
	}
	if _, pending := s.pendingCreations[mid]; pending {
		return 0, core.ErrConflict
	}
	if other, exists := s.byMID[mid]; exists && other != n.key {
		return 0, core.ErrConflict
	}
	if existing := s.byMessage[n.key]; existing != 0 && existing != mid {
		return 0, core.ErrConflict
	}
	return mid, nil
}

func (messageVersion) String() string   { return "messageVersion{private}" }
func (messageVersion) GoString() string { return "messageVersion{private}" }
func (messageKnown) String() string     { return "messageKnown{private}" }
func (messageKnown) GoString() string   { return "messageKnown{private}" }
