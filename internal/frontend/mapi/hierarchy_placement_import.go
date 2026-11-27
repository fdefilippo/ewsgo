// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"strings"
	"time"
	"unicode/utf8"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

var (
	errMissingHierarchyParent = errors.New("hierarchy import parent not found")
	errHierarchyCycle         = errors.New("hierarchy import would create a cycle")
)

func (t *SessionTransport) EnableHierarchyImport() { t.hierarchyImport = true }

type hierarchyImportRefusal uint8

const (
	hierarchyImportCollector hierarchyImportRefusal = iota + 1
	hierarchyImportWriter
	hierarchyImportReplica
	hierarchyImportVirtualSource
	hierarchyImportUnreservedSource
	hierarchyImportClass
	hierarchyImportParentClass
	hierarchyImportExtraProperty
	hierarchyImportDistinguished
	hierarchyImportLimit
	hierarchyImportOther
)

func (hierarchyImportRefusal) Error() string { return "hierarchy import not supported" }
func (hierarchyImportRefusal) Unwrap() error { return core.ErrUnsupported }

func (r hierarchyImportRefusal) label() string {
	names := [...]string{"", "hierarchy_import_collector", "hierarchy_import_writer",
		"hierarchy_import_replica", "hierarchy_import_virtual_source", "hierarchy_import_unreserved_source", "hierarchy_import_class",
		"hierarchy_import_parent_class", "hierarchy_import_property", "hierarchy_import_distinguished",
		"hierarchy_import_limit", "hierarchy_import_other"}
	if int(r) >= len(names) {
		return names[hierarchyImportOther]
	}
	return names[r]
}

func executeHierarchyImport(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32) ([]byte, hierarchyImportRefusal) {
	fail := func(code uint32) []byte { return ropError(op.ID, op.Input, code) }
	object, code := objects.object(handles[op.Input], op.Logon)
	if code != 0 {
		return fail(code), 0
	}
	download := object.download
	if download == nil || !download.hierarchyCollector || download.state == nil || op.HierarchyImport == nil {
		return fail(NotSupported), hierarchyImportCollector
	}
	switch err := download.state.importMailFolderPlacement(ctx, session, *op.HierarchyImport); err {
	case nil:
	case errMissingHierarchyParent:
		return fail(0x80040803), 0 // NoParentFolder: client can retry after importing the parent.
	case errHierarchyCycle:
		return fail(0x80040804), 0
	default:
		code := faiError(err, false)
		var reason hierarchyImportRefusal
		if code == NotSupported && !errors.As(err, &reason) {
			reason = hierarchyImportOther
		}
		return fail(code), reason
	}
	// MS-OXCROPS 2.2.13.4.2: the successful response includes a zero FID.
	return binary.LittleEndian.AppendUint64(fail(0), 0), 0
}

func (s *hierarchyState) importMailFolderPlacement(ctx context.Context, session core.Session, request hierarchyImport) error {
	if err := validateHierarchyPlacement(request); err != nil {
		return err
	}
	propertyProfile := validateHierarchyImportProperties(request.properties)
	store := s.store
	if session == nil || session.Identity().Scope != store.scope {
		return core.ErrAccessDenied
	}
	box := session.Mailbox()
	if box == nil || box.Root() != store.root {
		return core.ErrAccessDenied
	}
	writer, ok := box.(core.MailFolderHierarchyWriter)
	if !ok {
		return hierarchyImportWriter
	}
	if [16]byte(request.source[:16]) != store.replica {
		return hierarchyImportReplica
	}
	fid := binary.LittleEndian.Uint64(append([]byte{1, 0}, request.source[16:]...))
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.pending != nil || s.started {
		return core.ErrConflict
	}
	store.hierarchyMu.Lock()
	defer store.hierarchyMu.Unlock()
	store.mu.Lock()
	key, mapped := store.byFID[fid]
	creation, creating := store.pendingCreations[fid]
	counter := uint64(0)
	for _, b := range request.source[16:] {
		counter = counter<<8 | uint64(b)
	}
	reserved := store.clientReservedCounterLocked(counter) && !store.retiredCounterLocked(counter)
	latest := globalCount(store.next)
	store.mu.Unlock()
	if fid == s.root {
		return hierarchyImportVirtualSource
	}
	if !mapped && !reserved {
		return hierarchyImportUnreservedSource
	}
	incoming, err := hierarchyImportHistory(request, store.replica, latest)
	if err != nil {
		return err
	}
	now := time.Now().UTC().Truncate(100 * time.Nanosecond)
	snapshot, err := store.observeHierarchyLocked(ctx, session, s.root, now)
	if err != nil {
		return err
	}
	rows := make(map[uint64]hierarchyFolder, len(snapshot.folders))
	for _, row := range snapshot.folders {
		rows[row.fid] = row
	}
	current, exists := rows[fid]
	if !exists && mapped && !creating {
		return core.ErrAccessDenied
	}
	var previous faiPCL
	if exists {
		previous, err = parseFAIPCL(current.predecessors)
		if err != nil || !previous.includesXID(current.changeKey) {
			return core.ErrInvalid
		}
		// Compare the currently authorized source before checking its write
		// capabilities. Replay neither changes placement nor imports properties.
		if previous.includes(incoming) {
			return errFAIIgnore
		}
	}
	if mapped && (key.native == "" || key.native == store.root) {
		return hierarchyImportVirtualSource
	}
	if current.containerClass != "" && current.containerClass != "IPF.Note" {
		return hierarchyImportClass
	}
	parent := s.root
	if request.parent != ([22]byte{}) {
		if [16]byte(request.parent[:16]) != store.replica {
			return core.ErrAccessDenied
		}
		parent = binary.LittleEndian.Uint64(append([]byte{1, 0}, request.parent[16:]...))
	}
	for at := parent; at != s.root; {
		if at == fid {
			return errHierarchyCycle
		}
		row, exists := rows[at]
		if !exists {
			store.mu.Lock()
			_, mapped := store.byFID[at]
			store.mu.Unlock()
			if !mapped {
				return errMissingHierarchyParent
			}
			return core.ErrAccessDenied
		}
		at = row.parent
	}
	store.mu.Lock()
	parentKey := store.byFID[parent]
	base := store.hierarchy[key]
	metadata := store.metadata[fid]
	store.mu.Unlock()
	if parentKey.native == "" || parent != s.root && rows[parent].containerClass != "" && rows[parent].containerClass != "IPF.Note" {
		return hierarchyImportParentClass
	}
	placement := core.MailFolderPlacement{Parent: parentKey.native, Name: request.name}
	if !mapped || creating {
		if propertyProfile != nil {
			return propertyProfile
		}
		return s.importNewMailFolder(ctx, box, writer, fid, placement, request.change, incoming, snapshot.budget, now, creation, creating)
	}
	// Rejected imports have no need for a second native folder lookup, after
	// the authorized snapshot has classified the source and checked for replay.
	if current.distinguished {
		return hierarchyImportDistinguished
	}
	if propertyProfile != nil {
		return propertyProfile
	}
	before, err := box.Get(ctx, key.native)
	if err != nil {
		return err
	}
	if before.Properties[core.WellKnown] != nil {
		return hierarchyImportDistinguished
	}
	oldFingerprint, err := importFolderFingerprint(before, key.native, store, metadata)
	if err != nil {
		return err
	}
	if oldFingerprint != base.fingerprint {
		return core.ErrConflict
	}

	history, err := mergeFAIPCL(previous, incoming)
	if err != nil {
		return err
	}
	conflict := !incoming.includes(previous)
	if conflict && !time.Time(request.modified).After(current.modified) {
		// MS-OXCFXICS 3.1.5.6.2.2 keeps the server folder on a tie.
		placement = core.MailFolderPlacement{Parent: before.Parent, Name: current.name}
	}
	rawHistory := history.marshal()
	historySize := len(rawHistory)
	if conflict && history[store.replica] == "" {
		if len(history) >= maxFAIPCLReplicas {
			return hierarchyImportLimit
		}
		historySize += 23 // SizedXid for the newly allocated server change key.
	}
	if snapshot.budget+2*(historySize+max(len(request.change), 22)-len(base.history)-len(base.change))+3*max(0, len(placement.Name)-len(current.name)) > maxHierarchyBytes {
		return hierarchyImportLimit
	}
	if err := hierarchyOwner(ctx, box); err != nil {
		return err
	}
	store.mu.Lock()
	cn, seen, err := s.reserveHierarchyCNLocked(!conflict)
	store.mu.Unlock()
	if err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	fingerprint := oldFingerprint
	if before.Parent != placement.Parent || current.name != placement.Name {
		after, err := writer.UpdateMailFolder(ctx, before, placement)
		if err != nil {
			return err
		}
		if after.Parent != placement.Parent || after.Properties[core.DisplayName] != core.Text(placement.Name) {
			return core.ErrConflict
		}
		fingerprint, err = importFolderFingerprint(after, key.native, store, metadata)
		if err != nil {
			return err
		}
	}
	store.mu.Lock()
	defer store.mu.Unlock()
	if store.durable.store != nil {
		cn, seen, err = s.reserveHierarchyCNLocked(!conflict)
		if err != nil {
			return err
		}
	}
	change := request.change
	if conflict {
		// Give the resolution its own key. Outlook must download this version
		// and both histories, including when the native folder was unchanged.
		change = gidChangeXID(hierarchyXID(store.replica, cn))
		history[store.replica] = change.local()
		rawHistory = history.marshal()
	}
	if now.Before(base.observed) {
		now = base.observed
	}
	version := hierarchyVersion{
		fingerprint: fingerprint, parent: store.byKey[folderKey{native: placement.Parent}],
		cn: cn, observed: now, present: true, change: change, history: string(rawHistory),
	}
	if err := store.commitImportedFolder(ctx, key, fid, version, false); err != nil {
		return err
	}
	s.seen = seen
	return nil
}

func (s *ramStore) commitImportedFolder(ctx context.Context, key folderKey, fid uint64, version hierarchyVersion, creation bool) error {
	if s.durable.store != nil {
		mutation := mapireplica.Mutation{
			HighWater: version.cn, Projection: []mapireplica.Projection{folderProjection(fid, version)},
		}
		if creation {
			mutation.ForgetFolderCreation = folderGlobalCount(fid)
		}
		if err := s.commitReplica(ctx, mutation); err != nil {
			return err
		}
		s.next = version.cn
	}
	s.hierarchy[key] = version
	return nil
}

// Preflight the next change number and Seen range before native I/O. The
// durable replica allocates the final CN together with the verified version.
func (s *hierarchyState) reserveHierarchyCNLocked(reportSeen bool) (uint64, idSet, error) {
	store := s.store
	if store.next >= maxGlobalCount {
		return 0, idSet{}, hierarchyImportLimit
	}
	cn := store.next + 1
	seen := s.seen
	if reportSeen {
		seen = singleReplicaSet(store.replica, mergeIDRanges(append(contentsRanges(seen), idRange{cn, cn})))
		if _, err := seen.marshal(); err != nil {
			return 0, idSet{}, err
		}
	}
	if store.durable.store == nil {
		store.next = cn
	}
	return cn, seen, nil
}

func validateHierarchyPlacement(request hierarchyImport) error {
	if !validFAIXID(request.source) || !request.change.valid() || request.name == "" ||
		!utf8.ValidString(request.name) || utf8.RuneCountInString(request.name) > 256 ||
		strings.ContainsAny(request.name, "\x00\r\n/") {
		return core.ErrInvalid
	}
	if year := time.Time(request.modified).Year(); year < 1601 || year > 9999 {
		return core.ErrInvalid
	}
	return nil
}

func validateHierarchyImportProperties(properties []hierarchyImportProperty) error {
	for _, property := range properties {
		switch property.tag {
		case 0x65e10102, 0x65e00102, 0x30080040, 0x65e20102, 0x65e30102, 0x3001001f:
			// MS-OXCFXICS 3.2.5.9.4.3: HierarchyValues wins.
		case 0x30070040:
			// MS-OXCFOLD 2.2.2.2.1: CreationTime is server-owned.
		case 0x3613001e:
			if string(property.wire) != "IPF.Note\x00" {
				return hierarchyImportClass
			}
		case 0x3613001f:
			r := reader{data: property.wire}
			value, err := readContentsValue(&r, 0x001f)
			if err != nil || r.pos != len(property.wire) || value != core.Text("IPF.Note") {
				return hierarchyImportClass
			}
		default:
			return hierarchyImportExtraProperty
		}
	}
	return nil
}

func hierarchyImportHistory(request hierarchyImport, replica [16]byte, latest [6]byte) (faiPCL, error) {
	history := faiPCL{}
	if len(request.predecessors) != 0 {
		var err error
		history, err = parseFAIPCL(request.predecessors)
		if err != nil {
			return nil, err
		}
	}
	history, err := mergeFAIPCL(history, faiPCL{request.change.namespace(): request.change.local()})
	if err != nil {
		return nil, err
	}
	if local, exists := history[replica]; exists && (len(local) != 6 || string(local) > string(latest[:])) {
		return nil, core.ErrResync
	}
	return history, nil
}

func importFolderFingerprint(object core.Object, id core.ID, store *ramStore, metadata folderMetadata) ([32]byte, error) {
	if object.ID != id || object.Kind != core.Folder {
		return [32]byte{}, core.ErrInvalid
	}
	node, err := nativeHierarchyNode(object, store.root, store.draftsReference)
	if err != nil {
		return [32]byte{}, err
	}
	if len(metadata) != 0 {
		node.fingerprint = metadataFingerprint(metadata, node.fingerprint)
	}
	return node.fingerprint, nil
}
