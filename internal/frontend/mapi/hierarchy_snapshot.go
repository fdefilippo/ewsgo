// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"ewsgo/internal/core"
)

const (
	maxHierarchyBytes = 1 << 20 // Accounted data, not a promise about Go heap RSS.
	maxHierarchyText  = 4096
)

// This is a headless prerequisite, not an ICS handle or a wire response.
// Snapshots are detached values. A future download must reauthorize every read.
type hierarchySnapshot struct {
	replica   [16]byte
	root      uint64
	highWater uint64
	budget    int
	folders   []hierarchyFolder // Descendants only, parents before children.
	known     []uint64          // Includes mappings no longer in the projection.
}

type hierarchyFolder struct {
	counts          folderCounters
	native          core.ID
	fid, parent     uint64
	sourceKey       [22]byte
	parentSourceKey []byte // Empty for direct children of the synchronization root.
	changeKey       changeXID
	predecessors    []byte
	cn              uint64
	creator         uint32
	modified        time.Time // EWSGo observation time; never a native fallback.
	name            string
	containerClass  string // Classes declared by the projection.
	search          bool
	hidden          *bool // Present when set in the MAPI folder metadata.
	referenceFIDs   [metadataFolderRoleCount]uint64
	junkFID         uint64
	additionalRefs  []byte
}

type hierarchyVersion struct {
	fingerprint [32]byte
	cn          uint64
	creator     uint32
	parent      uint64
	observed    time.Time
	change      changeXID
	history     string // Immutable canonical PCL bytes.
	present     bool
}

type hierarchyNode struct {
	counts         folderCounters
	key, parent    folderKey
	name           string
	containerClass string
	search         bool
	hidden         *bool
	references     [metadataFolderRoleCount]folderKey
	junk           folderKey
	additionalRefs []byte
	fingerprint    [32]byte
	creator        uint32
}

func (hierarchySnapshot) String() string   { return "hierarchySnapshot{redacted}" }
func (hierarchySnapshot) GoString() string { return "hierarchySnapshot{redacted}" }
func (hierarchyFolder) String() string     { return "hierarchyFolder{redacted}" }
func (hierarchyFolder) GoString() string   { return "hierarchyFolder{redacted}" }
func (hierarchyNode) String() string       { return "hierarchyNode{redacted}" }
func (hierarchyNode) GoString() string     { return "hierarchyNode{redacted}" }

// observeHierarchy serializes the native reads as well as the commit, so an
// older in-flight query cannot overwrite a newer observation. No store mutex
// is held across native I/O. It retains no session, token or native object.
// observed is supplied by the projection clock, not obtained from the backend.
func (s *ramStore) observeHierarchy(ctx context.Context, session core.Session, anchor uint64, observed time.Time) (hierarchySnapshot, error) {
	s.hierarchyMu.Lock()
	defer s.hierarchyMu.Unlock()
	return s.observeHierarchyLocked(ctx, session, anchor, observed)
}

func (s *ramStore) observeHierarchyLocked(ctx context.Context, session core.Session, anchor uint64, observed time.Time) (hierarchySnapshot, error) {
	if err := ctx.Err(); err != nil {
		return hierarchySnapshot{}, err
	}
	if session == nil || session.Identity().Scope != s.scope {
		return hierarchySnapshot{}, core.ErrAccessDenied
	}
	box := session.Mailbox()
	if box == nil || box.Root() != s.root {
		return hierarchySnapshot{}, core.ErrConflict
	}
	s.mu.Lock()
	anchorKey, exists := s.byFID[anchor]
	s.mu.Unlock()
	if !exists {
		return hierarchySnapshot{}, core.ErrNotFound
	}
	if err := hierarchyOwner(ctx, box); err != nil {
		return hierarchySnapshot{}, err
	}
	root, err := box.Get(ctx, s.root)
	if err != nil {
		return hierarchySnapshot{}, err
	}
	if root.ID != s.root || root.Kind != core.Folder {
		return hierarchySnapshot{}, core.ErrInvalid
	}
	catalog, objects, err := readPrivateFolderTreeWithSpecialFolders(ctx, session, s.calendarMetadata, s.contactsMetadata, s.tasksMetadata)
	if err != nil {
		return hierarchySnapshot{}, err
	}
	if catalog.scope != s.scope || catalog.folders[IPMSubtree].NativeID != s.root {
		return hierarchySnapshot{}, core.ErrConflict
	}
	nodes, err := s.projectedNodes(objects)
	if err != nil {
		return hierarchySnapshot{}, err
	}
	for role := metadataFolderRole(0); role < metadataFolderRoleCount; role++ {
		if s.metadataRoleEnabled(role) {
			if err := projectHierarchySpecialReferences(nodes, objects, catalog.folders[Inbox].NativeID, s.root, role); err != nil {
				return hierarchySnapshot{}, err
			}
		}
	}
	if err := s.projectHierarchyJunk(nodes, objects, catalog.folders[Inbox].NativeID); err != nil {
		return hierarchySnapshot{}, err
	}
	found := false
	for _, n := range nodes {
		found = found || n.key == anchorKey
	}
	if !found {
		return hierarchySnapshot{}, core.ErrNotFound
	}
	// A successful query is not a permanent access grant. Recheck after reading
	// and validating it, including cancellation, before changing any RAM state.
	if err := hierarchyOwner(ctx, box); err != nil {
		return hierarchySnapshot{}, err
	}
	currentBox := session.Mailbox()
	if session.Identity().Scope != s.scope || currentBox == nil || currentBox.Root() != s.root {
		return hierarchySnapshot{}, core.ErrAccessDenied
	}
	observed = observed.UTC().Truncate(100 * time.Nanosecond)
	if observed.Year() < 1601 || observed.Year() > 9999 {
		return hierarchySnapshot{}, core.ErrInvalid
	}
	return s.commitHierarchy(ctx, nodes, anchorKey, observed)
}

func hierarchyOwner(ctx context.Context, box core.Mailbox) error {
	owner, err := mailboxOwner(ctx, box)
	if err != nil {
		return err
	}
	if !owner {
		return core.ErrAccessDenied
	}
	return ctx.Err()
}

func mailboxOwner(ctx context.Context, box core.Mailbox) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if reader, ok := box.(core.MailboxOwnerReader); ok {
		return reader.MailboxOwner(ctx)
	}
	reader, ok := box.(core.MailboxStateReader)
	if !ok {
		return false, core.ErrUnsupported
	}
	state, err := reader.MailboxState(ctx)
	return state.Owner, err
}

// hierarchyNodes only retains the metadata that can affect this projection.
// Native revision/date presence contributes to the fingerprint, whereas counts
// and changes to contained messages do not independently advance a folder CN.
func hierarchyNodes(objects []core.Object, root core.ID, dynamic ...ramFolder) ([]hierarchyNode, error) {
	return hierarchyNodesForStore(objects, root, false, false, dynamic...)
}
func hierarchyNodesForStore(objects []core.Object, root core.ID, drafts, privateConfig bool, dynamic ...ramFolder) ([]hierarchyNode, error) {
	byKey := make(map[folderKey]hierarchyNode, len(objects)+11)
	for _, object := range objects {
		n, err := nativeHierarchyNode(object, root, drafts)
		if err != nil {
			return nil, err
		}
		byKey[n.key] = n
	}
	for role, folder := range serviceFolders {
		if folder.name == "" || SpecialFolderRole(role) == freeBusyFolderRole && !privateConfig {
			continue
		}
		n := hierarchyNode{
			key: folderKey{role: SpecialFolderRole(role)}, parent: folder.parent(root),
			name: folder.name, containerClass: folder.class,
			counts: folderCounters{present: [2]bool{true, true}},
		}
		n.fingerprint = hierarchyFingerprint(n, 0, false, "")
		byKey[n.key] = n
	}
	// Add reachable dynamic children in parent order; an absent native ancestor
	// hides its whole RAM branch without granting access through retained IDs.
	for pass := 0; pass < maxDynamicDepth; pass++ {
		for _, folder := range dynamic {
			key := folderKey{dynamic: folder.id}
			if _, ok := byKey[key]; ok {
				continue
			}
			if _, ok := byKey[folder.parent]; !ok {
				continue
			}
			n := hierarchyNode{key: key, parent: folder.parent, name: folder.name, search: folder.search,
				counts: folderCounters{present: [2]bool{true, true}}, creator: folder.creator}
			n.fingerprint = hierarchyFingerprint(n, 0, false, folder.comment)
			byKey[key] = n
		}
	}
	children := make(map[folderKey][]folderKey)
	for key, n := range byKey {
		if key == (folderKey{role: MailboxRoot}) {
			continue
		}
		if _, ok := byKey[n.parent]; !ok {
			return nil, core.ErrInvalid
		}
		children[n.parent] = append(children[n.parent], key)
	}
	for parent := range children {
		sort.Slice(children[parent], func(i, j int) bool {
			a, b := children[parent][i], children[parent][j]
			if a.native != b.native {
				return a.native < b.native
			}
			if a.role != b.role {
				return a.role < b.role
			}
			return a.dynamic < b.dynamic
		})
	}
	// Iterative traversal also bounds call-stack use for a 1000-deep tree.
	queue := []folderKey{{role: MailboxRoot}}
	out := make([]hierarchyNode, 0, len(byKey))
	for len(queue) != 0 {
		key := queue[0]
		queue = queue[1:]
		out = append(out, byKey[key])
		queue = append(queue, children[key]...)
		if len(out) > len(byKey) {
			return nil, core.ErrInvalid
		}
	}
	if len(out) != len(byKey) {
		return nil, core.ErrInvalid
	}
	return out, nil
}

func nativeHierarchyNode(object core.Object, root core.ID, drafts bool) (hierarchyNode, error) {
	if len(object.ID) > maxHierarchyText || len(object.Parent) > maxHierarchyText {
		return hierarchyNode{}, core.ErrUnsupported
	}
	name, ok := object.Properties[core.DisplayName].(core.Text)
	if !ok || !utf8.ValidString(string(name)) || strings.ContainsRune(string(name), 0) {
		return hierarchyNode{}, core.ErrInvalid
	}
	if len(name) > maxHierarchyText {
		return hierarchyNode{}, core.ErrUnsupported
	}
	n := hierarchyNode{key: folderKey{native: object.ID}, parent: folderKey{native: object.Parent}, name: string(name)}
	counts, err := readFolderCounters(object.Properties)
	if err != nil {
		return hierarchyNode{}, err
	}
	n.counts = counts
	n.containerClass = nativeFolderClass(object.Properties, drafts)
	if object.ID == root {
		n.parent = folderKey{role: MailboxRoot}
	}
	revision, hasRevision := object.Properties[core.MetadataRevision]
	if hasRevision {
		value, valid := revision.(core.Integer)
		if !valid || value < 0 || uint64(value) != uint64(object.Revision) {
			return hierarchyNode{}, core.ErrInvalid
		}
	}
	date, hasDate := object.Properties[core.LastModifiedAt]
	stamp := ""
	if hasDate {
		value, valid := date.(core.Timestamp)
		if !valid {
			return hierarchyNode{}, core.ErrInvalid
		}
		t := time.Time(value).UTC()
		if t.Year() < 1601 || t.Year() > 9999 {
			return hierarchyNode{}, core.ErrInvalid
		}
		stamp = t.Format(time.RFC3339Nano)
	}
	n.fingerprint = hierarchyFingerprint(n, uint64(object.Revision), hasRevision, stamp)
	return n, nil
}

func hierarchyFingerprint(n hierarchyNode, revision uint64, present bool, date string) [32]byte {
	// Length prefixes avoid concatenation ambiguities for opaque IDs and names.
	data := []byte{byte(n.parent.role), 0}
	if present {
		data[1] = 1
	}
	data = binary.LittleEndian.AppendUint64(data, revision)
	if n.parent.dynamic != 0 {
		data = binary.LittleEndian.AppendUint64(data, n.parent.dynamic)
	}
	for _, field := range []string{string(n.parent.native), n.name, date, n.containerClass} {
		data = binary.LittleEndian.AppendUint32(data, uint32(len(field)))
		data = append(data, field...)
	}
	if n.search {
		data = append(data, searchFolderType)
	}
	return sha256.Sum256(data)
}

// Resolve references from the same authorized projection as their source folders.
// A target replacement/removal changes the source CN even if its revision does not.
func projectHierarchySpecialReferences(nodes []hierarchyNode, objects []core.Object, inbox, root core.ID, role metadataFolderRole) error {
	target := folderKey{role: role.serviceRole()}
	if target.role == MailboxRoot {
		for _, object := range objects {
			if object.Properties[core.WellKnown] != core.Text(role.wellKnown()) {
				continue
			}
			if object.ID == root || object.Parent == "" || object.Parent == object.ID {
				return core.ErrInvalid
			}
			if target.native != "" {
				return core.ErrConflict
			}
			target.native = object.ID
		}
	}
	for i := range nodes {
		n := &nodes[i]
		if n.key != (folderKey{role: MailboxRoot}) && n.key != (folderKey{native: inbox}) {
			continue
		}
		n.references[role] = target
		data := append([]byte(role.wellKnown()+"-reference-v1\x00"), n.fingerprint[:]...)
		if target.role != MailboxRoot {
			data = append(data, byte(target.role))
		}
		data = binary.LittleEndian.AppendUint32(data, uint32(len(target.native)))
		n.fingerprint = sha256.Sum256(append(data, target.native...))
	}
	return nil
}

func (s *ramStore) projectHierarchyJunk(nodes []hierarchyNode, objects []core.Object, inbox core.ID) error {
	junk, err := nativeJunkFolder(objects, s.root)
	if err != nil || junk == "" {
		return err
	}
	key := folderKey{native: inbox}
	s.mu.Lock()
	saved := s.metadata[s.byKey[key]][additionalEntryIDs]
	s.mu.Unlock()
	// The client may write an old Junk EntryID. Only a change to the native
	// role or a client-owned slot advances the synchronization version.
	refs, err := replaceJunkReference(saved, nil)
	if err != nil {
		return core.ErrInvalid
	}
	for i := range nodes {
		n := &nodes[i]
		if n.key != key {
			continue
		}
		n.junk = folderKey{native: junk}
		n.additionalRefs = saved
		data := append([]byte("junk-reference-v1\x00"), n.fingerprint[:]...)
		data = binary.LittleEndian.AppendUint32(data, uint32(len(junk)))
		data = append(data, junk...)
		n.fingerprint = sha256.Sum256(append(data, refs...))
	}
	return nil
}

// commitHierarchy stages counters, mappings, versions and the detached result
// together. Every failure leaves all four unchanged. No eviction or ID reuse.
func (s *ramStore) commitHierarchy(ctx context.Context, nodes []hierarchyNode, anchor folderKey, observed time.Time) (hierarchySnapshot, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	current := make(map[folderKey]bool, len(nodes))
	needed, changed, budget := 0, 0, 0
	for key := range s.byKey {
		budget += 160 + len(key.native)
	}
	for _, v := range s.hierarchy {
		// Retained predecessor data, including removed objects, and the
		// detached copy that can be emitted for an observed folder.
		budget += 2 * (len(v.change) + len(v.history))
	}
	for _, n := range nodes {
		current[n.key] = true
		if _, ok := s.byKey[n.key]; !ok {
			needed++
			budget += 160 + len(n.key.native)
		}
		previous := s.hierarchy[n.key]
		if !previous.present || previous.fingerprint != n.fingerprint {
			changed++
		}
		// Includes bounded UTF-16 output size as well as the retained UTF-8 name.
		budget += 384 + 3*(len(n.name)+len(n.containerClass))
		budget += len(n.additionalRefs)
		for _, target := range n.references {
			if target != (folderKey{}) {
				budget += len(target.native) + 54
			}
		}
	}
	for key, version := range s.hierarchy {
		if version.present && !current[key] {
			changed++
		}
	}
	for _, n := range nodes {
		if n.junk != (folderKey{}) && !current[n.junk] {
			return hierarchySnapshot{}, core.ErrInvalid
		}
		for _, target := range n.references {
			if target != (folderKey{}) && !current[target] {
				return hierarchySnapshot{}, core.ErrInvalid
			}
		}
	}
	if s.mappingCountLocked()+needed > maxRAMMappings || budget > maxHierarchyBytes || s.next > maxGlobalCount || uint64(needed+changed) > maxGlobalCount-s.next {
		return hierarchySnapshot{}, core.ErrUnsupported
	}
	byKey, byFID := make(map[folderKey]uint64, len(s.byKey)+needed), make(map[uint64]folderKey, len(s.byFID)+needed)
	versions := make(map[folderKey]hierarchyVersion, len(s.hierarchy)+needed)
	for key, fid := range s.byKey {
		byKey[key], byFID[fid] = fid, key
	}
	for key, v := range s.hierarchy {
		versions[key] = v
	}
	next := s.next
	for _, n := range nodes {
		if byKey[n.key] == 0 {
			next++
			fid := folderID(next)
			byKey[n.key], byFID[fid] = fid, n.key
		}
		v := versions[n.key]
		if !v.present || v.fingerprint != n.fingerprint {
			next++
			// Clock corrections cannot move an existing object's observed date
			// backwards; the strictly increasing CN remains the version clock.
			at := observed
			if at.Before(v.observed) {
				at = v.observed
			}
			change := gidChangeXID(hierarchyXID(s.replica, next))
			list := faiPCL{s.replica: change.local()}
			if v.history != "" {
				previous, err := parseFAIPCL([]byte(v.history))
				if err != nil || !previous.includesXID(v.change) {
					return hierarchySnapshot{}, core.ErrInvalid
				}
				list, err = mergeFAIPCL(previous, list)
				if err != nil {
					return hierarchySnapshot{}, err
				}
			}
			history := string(list.marshal())
			budget += 2 * (len(change) + len(history) - len(v.change) - len(v.history))
			var creator uint32
			if v.cn == 0 {
				creator = n.creator
			}
			v = hierarchyVersion{fingerprint: n.fingerprint, cn: next, creator: creator, observed: at, change: change, history: history, present: true}
		}
		versions[n.key] = v
	}
	for _, n := range nodes {
		v := versions[n.key]
		if n.key != (folderKey{role: MailboxRoot}) {
			v.parent = byKey[n.parent]
		}
		versions[n.key] = v
	}
	if budget > maxHierarchyBytes {
		return hierarchySnapshot{}, core.ErrUnsupported
	}
	// Sort tombstones as well, so identical input never depends on map iteration.
	var removed []uint64
	for key, v := range versions {
		if v.present && !current[key] {
			removed = append(removed, byKey[key])
		}
	}
	sort.Slice(removed, func(i, j int) bool { return removed[i] < removed[j] })
	for _, fid := range removed {
		key := byFID[fid]
		v := versions[key]
		next++
		v.cn, v.present = next, false
		versions[key] = v
	}
	result := hierarchySnapshot{replica: s.replica, root: byKey[anchor], highWater: next, budget: budget}
	descendants := map[folderKey]bool{anchor: true}
	for _, n := range nodes {
		if n.key == anchor || !descendants[n.parent] {
			continue
		}
		descendants[n.key] = true
		fid, parent, v := byKey[n.key], byKey[n.parent], versions[n.key]
		f := hierarchyFolder{native: n.key.native, fid: fid, parent: parent, sourceKey: s.hierarchyXID(fid), changeKey: v.change, predecessors: []byte(v.history), cn: v.cn, creator: v.creator, modified: v.observed, name: n.name, containerClass: n.containerClass, search: n.search, hidden: n.hidden, counts: n.counts}
		if n.junk.native != "" {
			f.junkFID, f.additionalRefs = byKey[n.junk], n.additionalRefs
		}
		for role, target := range n.references {
			if target != (folderKey{}) {
				f.referenceFIDs[role] = byKey[target]
			}
		}
		if n.parent != anchor {
			key := s.hierarchyXID(parent)
			f.parentSourceKey = append([]byte(nil), key[:]...)
		}
		result.folders = append(result.folders, f)
	}
	for fid := range byFID {
		result.known = append(result.known, fid)
	}
	sort.Slice(result.known, func(i, j int) bool { return result.known[i] < result.known[j] })
	if err := ctx.Err(); err != nil {
		return hierarchySnapshot{}, err
	}
	if err := s.commitHierarchyReplica(ctx, byKey, versions, next); err != nil {
		return hierarchySnapshot{}, err
	}
	s.byKey, s.byFID, s.hierarchy, s.next = byKey, byFID, versions, next
	return result, nil
}

func hierarchyXID(replica [16]byte, count uint64) (out [22]byte) {
	copy(out[:16], replica[:])
	counter := globalCount(count)
	copy(out[16:], counter[:])
	return out
}

func (s *ramStore) hierarchyXID(fid uint64) (out [22]byte) {
	copy(out[:16], s.replica[:])
	wire := binary.LittleEndian.AppendUint64(nil, fid)
	copy(out[16:], wire[2:])
	return out
}
