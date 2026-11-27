// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"ewsgo/internal/core"
)

var errFAIIgnore = errors.New("FAI version already represented")

type faiSyncVersion struct {
	source       [22]byte
	change       changeXID
	predecessors faiPCL
	local        bool // Set only on a private candidate; resolved by registry commit.
	serverNew    bool // Source allocated by CreateMessage, before its first save.
}

func (*faiSyncVersion) String() string   { return "faiSyncVersion{private}" }
func (*faiSyncVersion) GoString() string { return "faiSyncVersion{private}" }
func faiSyncBytes(v *faiSyncVersion) int {
	if v == nil {
		return 0
	}
	return 512 + 64*len(v.predecessors)
}
func cloneFAISync(v *faiSyncVersion) *faiSyncVersion {
	if v == nil {
		return nil
	}
	out := *v
	out.predecessors = make(faiPCL, len(v.predecessors))
	for k, c := range v.predecessors {
		out.predecessors[k] = c
	}
	return &out
}

// Revision describes every committed local FAI save, including metadata-only
// imports. Native mail's fingerprint/read-state behavior is unchanged.
func (s *ramStore) attachFAISync(nodes []messageNode) {
	for i := range nodes {
		n := &nodes[i]
		record, ok := s.fai.records[n.key.id]
		if !ok {
			continue
		}
		n.sync = record.sync
		h := sha256.New()
		h.Write(n.fingerprint[:])
		h.Write(binary.LittleEndian.AppendUint64(nil, uint64(record.object.Revision)))
		copy(n.fingerprint[:], h.Sum(nil))
	}
}

type faiDraft struct {
	acknowledgedCN                 uint64 // Last committed version supplied by this handle.
	mu                             sync.Mutex
	store                          *ramStore
	root                           uint64
	request                        faiImportRequest
	id                             core.ID
	base                           core.Revision
	newMID                         uint64
	properties                     core.Properties
	lease                          *contentsLease // One retained property set and its metadata.
	upload                         *faiUpload
	saved, dirty, readOnly, closed bool
	privateConfig                  bool
}

func faiIDForSource(source [22]byte) core.ID {
	hash := sha256.Sum256(source[:])
	return core.ID("fai:" + hex.EncodeToString(hash[:16]))
}

func (*faiDraft) String() string   { return "faiDraft{private}" }
func (*faiDraft) GoString() string { return "faiDraft{private}" }
func (d *faiDraft) close() {
	if d == nil {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	d.closed = true
	d.properties = nil
	d.request = faiImportRequest{}
	d.clearUpload()
	d.lease.release()
	d.lease = nil
}
func (s *ramStore) faiSource(record faiRecord) [22]byte {
	if record.sync != nil {
		return record.sync.source
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hierarchyXID(s.byMessage[messageKey{id: record.object.ID, associated: true}])
}

// Called with contentsMu. A SourceKey never grants access to a native message.
func (s *ramStore) findFAISource(root uint64, source [22]byte) (core.ID, faiRecord, error) {
	if folder, exists := s.fai.absent[source]; exists {
		if folder != root {
			return "", faiRecord{}, core.ErrAccessDenied
		}
		// Sentinel only: no message, MID or change number has been created.
		return "", faiRecord{deleted: true, folder: root}, nil
	}
	for id, r := range s.fai.records {
		if s.faiSource(r) == source {
			if r.folder != root {
				return "", faiRecord{}, core.ErrAccessDenied
			}
			return id, r, nil
		}
	}
	if bytes.Equal(source[:16], s.replica[:]) {
		// New own-replica GIDs must belong to a range actually issued through
		// GetLocalReplicaIds, rather than a native object's or change's counter.
		var counter uint64
		for _, b := range source[16:] {
			counter = counter<<8 | uint64(b)
		}
		s.mu.Lock()
		key, known := s.byMID[folderID(counter)]
		s.mu.Unlock()
		if known {
			if !key.associated {
				return "", faiRecord{}, core.ErrAccessDenied
			}
			r, exists := s.fai.records[key.id]
			if !exists || r.folder != root {
				return "", faiRecord{}, core.ErrAccessDenied
			}
			return key.id, r, nil
		}
		s.mu.Lock()
		reserved := s.clientReservedCounterLocked(counter) && !s.retiredCounterLocked(counter)
		s.mu.Unlock()
		if !reserved {
			return "", faiRecord{}, core.ErrAccessDenied
		}
	}
	return "", faiRecord{}, core.ErrNotFound
}
func (s *ramStore) newFAIDraft(ctx context.Context, session core.Session, root uint64, request faiImportRequest) (*faiDraft, error) {
	if request.flags&0x10 == 0 || request.flags & ^byte(0x50) != 0 || !validFAIXID(request.source) || !request.change.valid() || !request.predecessors.includesXID(request.change) {
		return nil, core.ErrInvalid
	}
	if !validFAISync(&faiSyncVersion{source: request.source, change: request.change, predecessors: request.predecessors}) || request.modified.Year() < 1601 || request.modified.Year() > 9999 {
		return nil, core.ErrInvalid
	}
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	if err := s.validateFAIXIDWidths(&faiSyncVersion{source: request.source, change: request.change, predecessors: request.predecessors}); err != nil {
		return nil, err
	}
	if local, ok := request.predecessors[s.replica]; ok {
		var counter uint64
		for _, b := range []byte(local) {
			counter = counter<<8 | uint64(b)
		}
		s.mu.Lock()
		future := counter > s.next
		s.mu.Unlock()
		if future {
			return nil, core.ErrResync
		}
	}
	if _, err := s.messageAnchor(ctx, session, root, true); err != nil {
		return nil, err
	}
	id, record, err := s.findFAISource(root, request.source)
	if err != nil && err != core.ErrNotFound {
		return nil, err
	}
	if record.deleted {
		return nil, core.ErrConflict
	}
	if record.sync != nil {
		// NoForeignIdentifiers can expose the internal GID instead of the
		// stored SourceKey. Resolving that alias does not rewrite lineage.
		request.source = record.sync.source
	}
	if err == nil && record.sync != nil {
		incoming, current := request.predecessors, record.sync.predecessors
		if current.includes(incoming) {
			return nil, errFAIIgnore
		} // No stage for ignored old/equal versions.
		if !incoming.includes(current) && request.flags&0x40 != 0 {
			return nil, core.ErrConflict
		}
	}
	if id == "" {
		if activeFAICount(s.fai.records) >= maxFAIObjects {
			return nil, core.ErrUnsupported
		}
		id = faiIDForSource(request.source)
		if _, exists := s.fai.records[id]; exists {
			return nil, core.ErrConflict
		}
	}
	lease, err := s.reserveContentsMemory(maxFAIObjectBytes)
	if err != nil {
		return nil, err
	}
	if err := ctx.Err(); err != nil {
		lease.release()
		return nil, err
	}
	request.predecessors, _ = mergeFAIPCL(request.predecessors, nil)
	return &faiDraft{store: s, root: root, request: request, id: id, base: record.object.Revision, properties: core.Properties{core.Key{ID: 0x67aa}: core.Bool(true), core.IsRead: core.Bool(false), core.LastModifiedAt: core.Timestamp(request.modified)}, lease: lease, dirty: true}, nil
}
func faiCoreProperty(p contentsProperty) core.Key {
	if p.name == (propertyName{}) {
		return core.Key{ID: uint32(p.id)}
	}
	return p.name.coreKey()
}

// An explicitly opened FAI edits the committed revision using the same
// isolated staging and conflict checks as an imported FAI after its first save.
func (s *ramStore) editOpenedFAI(ctx context.Context, row messageRow) (*faiDraft, error) {
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	record, exists := s.fai.records[row.object.ID]
	if !exists || record.deleted || record.folder != row.parent {
		return nil, core.ErrNotFound
	}
	if record.object.Revision != row.object.Revision {
		return nil, core.ErrConflict
	}
	pcl := faiPCL{row.changeKey.namespace(): row.changeKey.local()}
	if record.sync != nil {
		pcl, _ = mergeFAIPCL(record.sync.predecessors, nil)
	}
	lease, err := s.reserveContentsMemory(maxFAIObjectBytes)
	if err != nil {
		return nil, err
	}
	return &faiDraft{
		store: s, root: row.parent, id: row.object.ID, base: row.object.Revision,
		request:    faiImportRequest{source: row.sourceKey, change: row.changeKey, predecessors: pcl},
		properties: row.object.Properties, lease: lease, saved: true,
	}, nil
}
func (d *faiDraft) set(ctx context.Context, session core.Session, values faiSetRequest) ([]faiPropertyProblem, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.setLocked(ctx, session, values, false)
}

func (d *faiDraft) setLocked(ctx context.Context, session core.Session, values faiSetRequest, transfer bool) ([]faiPropertyProblem, error) {
	if d.closed {
		return nil, core.ErrClosed
	}
	if d.readOnly {
		return nil, core.ErrAccessDenied
	}
	if d.privateConfig {
		return d.setFreeBusyProperties(ctx, session, values)
	}
	if _, err := d.store.messageAnchor(ctx, session, d.root, true); err != nil {
		return nil, err
	}
	// The staged set and a detached candidate can coexist during an edit.
	workspace, err := d.store.reserveContentsMemory(2 * maxFAIObjectBytes)
	if err != nil {
		return nil, err
	}
	defer workspace.release()
	next, err := core.CloneProperties(d.properties)
	if err != nil {
		return nil, err
	}
	var problems []faiPropertyProblem
	changed := false
	for index, v := range values {
		if !transfer && v.value == nil && !storeSetTag(v.tag) {
			problems = append(problems, faiPropertyProblem{uint16(index), v.tag, NotSupported})
			continue
		}
		if !faiSetTag(v.tag) || !contentsValueType(uint16(v.tag), v.value) {
			return nil, core.ErrUnsupported
		}
		retain, code := faiSetDisposition(v, d.properties)
		if code != 0 {
			if transfer {
				switch code {
				case accessDenied:
					// The destination owns these values.
					continue
				case NotSupported:
					return nil, core.ErrUnsupported
				default:
					return nil, core.ErrInvalid
				}
			}
			problems = append(problems, faiPropertyProblem{uint16(index), v.tag, code})
		}
		if !retain {
			continue
		}
		p, err := resolveContentsIdentity(v.tag, &d.store.named)
		if err != nil {
			return nil, err
		}
		key := faiCoreProperty(p)
		actual, err := d.store.faiProperty(key, v.value)
		if err != nil || actual != p {
			return nil, core.ErrUnsupported
		}
		one, err := core.CloneProperties(core.Properties{key: v.value})
		if err != nil {
			return nil, err
		}
		next[key] = one[key]
		changed = true
	}
	if err := applyFAISubject(next, values); err != nil {
		return nil, err
	}
	if len(next) > 64 {
		return nil, core.ErrUnsupported
	}
	_, size, err := messageFingerprint(core.Object{Properties: next})
	if err != nil {
		return nil, err
	}
	if size+4096 > maxFAIObjectBytes {
		return nil, core.ErrUnsupported
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if changed {
		d.properties = next
		d.dirty = true
	}
	return problems, nil
}
func (d *faiDraft) save(ctx context.Context, session core.Session, flags byte) (messageRow, error) {
	row, _, _, err := d.saveWithCreation(ctx, session, flags)
	return row, err
}
func (d *faiDraft) saveWithCreation(ctx context.Context, session core.Session, flags byte) (messageRow, bool, bool, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed {
		return messageRow{}, false, false, core.ErrClosed
	}
	var valid bool
	flags, valid = faiSaveMode(flags)
	if !valid {
		return messageRow{}, false, false, core.ErrUnsupported
	}
	if err := d.stageUpload(ctx, session); err != nil {
		return messageRow{}, false, false, err
	}
	if d.privateConfig {
		row, created, err := d.saveFreeBusyMessage(ctx, session, flags)
		return row, created, false, err
	}
	s := d.store
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	key, err := s.messageAnchor(ctx, session, d.root, true)
	if err != nil {
		return messageRow{}, false, false, err
	}
	current, exists := s.fai.records[d.id]
	if folder, deleted := s.fai.absent[d.request.source]; deleted {
		if folder != d.root {
			return messageRow{}, false, false, core.ErrAccessDenied
		}
		return messageRow{}, false, false, core.ErrConflict
	}
	if current.deleted {
		return messageRow{}, false, false, core.ErrConflict
	}
	if d.saved && !d.dirty {
		workspace, err := s.reserveContentsMemory(maxFAIObjectBytes)
		if err != nil {
			return messageRow{}, false, false, err
		}
		defer workspace.release()
		row, err := s.readFAIRowLocked(ctx, d.root, d.id)
		if err == nil {
			d.finishSave(flags)
		}
		return row, false, row.cn != d.acknowledgedCN, err
	}
	if d.readOnly {
		return messageRow{}, false, false, core.ErrAccessDenied
	}
	if exists && current.object.Revision != d.base && flags != 4 {
		return messageRow{}, false, false, core.ErrConflict
	}
	if !exists && d.base != 0 {
		return messageRow{}, false, false, core.ErrConflict
	}
	if !exists && activeFAICount(s.fai.records) >= maxFAIObjects {
		return messageRow{}, false, false, core.ErrUnsupported
	}
	// Reserve the candidate and detached copy before a save can publish changes.
	workspace, err := s.reserveContentsMemory(2 * maxFAIObjectBytes)
	if err != nil {
		return messageRow{}, false, false, err
	}
	defer workspace.release()
	props, err := core.CloneProperties(d.properties)
	if err != nil {
		return messageRow{}, false, false, err
	}
	incoming := &faiSyncVersion{source: d.request.source, change: d.request.change, predecessors: d.request.predecessors}
	incoming = cloneFAISync(incoming)
	if d.saved || d.newMID != 0 {
		props[core.LastModifiedAt] = core.Timestamp(time.Now().UTC())
		incoming.local = true
		incoming.serverNew = !d.saved
	}
	record := faiRecord{folder: d.root, sync: incoming, object: core.Object{ID: d.id, Parent: key.native, Kind: core.Message, Revision: current.object.Revision + 1, Properties: props}}
	serverWins := false
	if exists && current.sync != nil {
		oldPCL, newPCL := current.sync.predecessors, incoming.predecessors
		oldIncludes, newIncludes := oldPCL.includes(newPCL), newPCL.includes(oldPCL)
		if !d.saved && flags != 4 && oldIncludes {
			return messageRow{}, false, false, core.ErrConflict
		}
		conflict := !oldIncludes && !newIncludes
		if !d.saved && conflict && d.request.flags&0x40 != 0 && flags != 4 {
			return messageRow{}, false, false, core.ErrConflict
		}
		merged, err := mergeFAIPCL(oldPCL, newPCL)
		if err != nil {
			return messageRow{}, false, false, err
		}
		// FAI conflicts have no conflict-message attachments. Preserve the winner
		// and merge both histories using MS-OXCFXICS Last Writer Wins.
		if !d.saved && conflict && flags != 4 {
			oldDate := time.Time(current.object.Properties[core.LastModifiedAt].(core.Timestamp))
			if oldDate.After(d.request.modified) || oldDate.Equal(d.request.modified) && current.sync.change[:16] > incoming.change[:16] {
				serverWins = true
				record = current
				record.object.Revision++
				record.sync = cloneFAISync(current.sync)
			}
		}
		record.sync.predecessors = merged
	}
	if record.sync.local {
		if _, exists := record.sync.predecessors[s.replica]; !exists {
			if len(record.sync.predecessors) >= maxFAIPCLReplicas {
				return messageRow{}, false, false, core.ErrUnsupported
			}
			// Reserve the namespace slot before quota validation. Registry commit
			// replaces this private placeholder with the allocated change counter.
			record.sync.predecessors[s.replica] = xidLocal("\x00\x00\x00\x00\x00\x01")
		}
	}
	if err = s.validateFAIXIDWidths(record.sync); err != nil {
		return messageRow{}, false, false, err
	}
	if _, err = s.validateFAIRecord(record); err != nil {
		return messageRow{}, false, false, err
	}
	row, err := s.commitFAIRecord(ctx, session, d.root, d.id, record)
	if err != nil {
		return messageRow{}, false, false, err
	}
	committed := s.fai.records[d.id]
	d.base = committed.object.Revision
	d.saved, d.dirty = true, false
	// Copy before the next property edit; committed records remain immutable.
	d.properties, _ = core.CloneProperties(committed.object.Properties)
	d.request.change = committed.sync.change
	d.request.predecessors, _ = mergeFAIPCL(committed.sync.predecessors, nil)
	d.request.modified = time.Time(committed.object.Properties[core.LastModifiedAt].(core.Timestamp))
	d.finishSave(flags)
	d.acknowledgedCN = 0
	if !serverWins {
		d.acknowledgedCN = row.cn
	}
	return row, !exists, serverWins, nil
}
func (s *ramStore) faiRowLocked(ctx context.Context, root uint64, id core.ID, previous *faiNamespace) (messageRow, error) {
	var objects []core.Object
	for _, r := range s.fai.records {
		if r.folder == root && !r.deleted {
			objects = append(objects, r.object)
		}
	}
	nodes, err := messageNodes(objects, s.byFID[root].native, true)
	if err != nil {
		return messageRow{}, err
	}
	s.attachFAISync(nodes)
	snapshot, err := s.commitMessages(ctx, root, true, 0, false, nodes, previous)
	if err != nil {
		return messageRow{}, err
	}
	for _, row := range snapshot.messages {
		if row.object.ID == id {
			return row, nil
		}
	}
	return messageRow{}, core.ErrNotFound
}

// Called with contentsMu. Reserve the entire candidate namespace and detached
// validation view before cloning. A failed registry commit rolls back namespace.
func (s *ramStore) commitFAIRecord(ctx context.Context, session core.Session, root uint64, id core.ID, record faiRecord) (messageRow, error) {
	records := make(map[core.ID]faiRecord, len(s.fai.records)+1)
	for k, r := range s.fai.records {
		records[k] = r
	}
	records[id] = record
	row, err := s.commitFAIRecords(ctx, session, root, records, id)
	return row, err
}
func (s *ramStore) commitFAIRecords(ctx context.Context, session core.Session, root uint64, records map[core.ID]faiRecord, want core.ID) (messageRow, error) {
	return s.commitFAINamespace(ctx, session, root, records, s.fai.absent, want)
}
func (s *ramStore) commitFAINamespace(ctx context.Context, session core.Session, root uint64, records map[core.ID]faiRecord, absent map[[22]byte]uint64, want core.ID) (messageRow, error) {
	if want != "" {
		r, exists := records[want]
		if !exists || r.deleted || r.folder != root {
			return messageRow{}, core.ErrNotFound
		}
	}
	total, err := s.validateFAIHistory(records, absent)
	if err != nil {
		return messageRow{}, err
	}
	activeBytes := 0
	seenSources := map[[22]byte]bool{}
	for id, r := range records {
		if r.object.ID != id {
			return messageRow{}, core.ErrInvalid
		}
		if r.sync != nil {
			if seenSources[r.sync.source] {
				return messageRow{}, core.ErrInvalid
			}
			seenSources[r.sync.source] = true
		}
		n, err := s.validateFAIRecord(r)
		if err != nil {
			return messageRow{}, err
		}
		r.bytes = n
		records[id] = r
		if !r.deleted {
			activeBytes += n
		}
	}
	if activeFAICount(records) > maxFAIObjects || activeBytes > maxFAIStoreBytes {
		return messageRow{}, core.ErrUnsupported
	}
	total += activeBytes
	lease, err := s.reserveContentsMemory(total)
	if err != nil {
		return messageRow{}, err
	}
	scratch, err := s.reserveContentsMemory(maxFAIStoreBytes)
	if err != nil {
		lease.release()
		return messageRow{}, err
	}
	defer scratch.release()
	if _, err = s.messageAnchor(ctx, session, root, true); err != nil {
		lease.release()
		return messageRow{}, err
	}
	// Only the changed record is detached from the caller. Existing records are
	// immutable and safely shared with the old namespace during the transaction.
	for id, r := range records {
		if old, ok := s.fai.records[id]; !ok || old.object.Revision != r.object.Revision {
			r.object, err = cloneMessageObject(r.object)
			if err != nil {
				lease.release()
				return messageRow{}, err
			}
			r.sync = cloneFAISync(r.sync)
			records[id] = r
		}
	}
	old := s.fai
	s.fai = faiNamespace{records: records, absent: absent, bytes: total, lease: lease}
	row, err := s.faiRowLocked(ctx, root, want, &old)
	if err != nil && !(want == "" && err == core.ErrNotFound) {
		s.fai = old
		lease.release()
		return messageRow{}, err
	}
	s.publishFAIChanges(root, old.records, records)
	old.lease.release()
	return row, nil
}
func (s *ramStore) deleteFAI(ctx context.Context, session core.Session, root uint64, request faiDeleteRequest) error {
	_, err := s.deleteFAIChanges(ctx, session, root, request)
	return err
}
func (s *ramStore) deleteFAIChanges(ctx context.Context, session core.Session, root uint64, request faiDeleteRequest) ([]uint64, error) {
	return s.deleteContentsChanges(ctx, session, root, request, nil, nil)
}

func (s *ramStore) deleteContentsChanges(ctx context.Context, session core.Session, root uint64, request faiDeleteRequest, native core.MailMessageSoftDeleter, contacts core.ContactSoftDeleter) ([]uint64, error) {
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	if request.flags & ^byte(2) != 0 || len(request.sources) < 1 || len(request.sources) > maxFAIObjects {
		return nil, core.ErrUnsupported
	}
	anchor, err := s.messageAnchor(ctx, session, root, true)
	if err != nil {
		return nil, err
	}
	var normal []uint64
	var personal []core.ID
	hasAssociated := false
	records := map[core.ID]faiRecord{}
	absent := make(map[[22]byte]uint64, len(s.fai.absent))
	for source, folder := range s.fai.absent {
		absent[source] = folder
	}
	var targets []core.ID
	for id, r := range s.fai.records {
		records[id] = r
	}
	for _, source := range request.sources {
		if !validFAIXID(source) {
			return nil, core.ErrInvalid
		}
		if counter := ownMoveCounter(s.replica, source); counter != 0 {
			mid := folderID(counter)
			s.mu.Lock()
			key, known := s.byMID[mid]
			version := s.messages[key]
			s.mu.Unlock()
			if known && !key.associated {
				switch {
				case key.special == core.ContactsFolder:
					if contacts == nil {
						return nil, core.ErrUnsupported
					}
					if version.parent != root {
						return nil, core.ErrConflict
					}
					personal = append(personal, key.id)
					continue
				case key.special != 0 && native != nil:
					return nil, core.ErrUnsupported
				case native != nil && request.flags == 0:
					normal = append(normal, mid)
					continue
				}
			}
		}
		hasAssociated = true
		id, r, err := s.findFAISource(root, source)
		if errors.Is(err, core.ErrNotFound) {
			absent[source] = root
			continue
		}
		if err != nil {
			return nil, err
		}
		if id != "" {
			targets = append(targets, id)
		}
		if r.deleted {
			continue
		}
		if r.sync == nil {
			return nil, core.ErrUnsupported
		}
		r.deleted = true
		r.object.Revision++
		r.object.Properties = nil
		r.sync = cloneFAISync(r.sync)
		records[id] = r
	}
	if len(personal) > 0 {
		if len(normal) != 0 {
			return nil, core.ErrConflict
		}
		if _, err := s.validateFAIHistory(records, absent); err != nil {
			return nil, err
		}
		if err := s.softDeleteContacts(ctx, session, root, personal, contacts); err != nil {
			return nil, err
		}
	}
	if len(normal) > 0 {
		folder, err := s.messageAnchor(ctx, session, root, false)
		if err != nil {
			return nil, err
		}
		if folder != anchor || folder.native == "" {
			return nil, core.ErrUnsupported
		}
		if _, err := s.validateFAIHistory(records, absent); err != nil {
			return nil, err
		}
		box := session.Mailbox()
		targets := make([]core.Object, 0, len(normal))
		for _, mid := range normal {
			current, err := s.messagePlacementSource(ctx, box, folder.native, mid)
			if errors.Is(err, core.ErrNotFound) {
				// A retransmission can name a message that already left this folder.
				continue
			}
			if err != nil {
				return nil, err
			}
			targets = append(targets, current)
		}
		for _, target := range targets {
			if err := native.SoftDeleteMailMessage(ctx, target.ID, target.Revision); err != nil {
				return nil, err
			}
		}
	}
	if !hasAssociated {
		return nil, nil
	}
	if _, err := s.commitFAINamespace(ctx, session, root, records, absent, ""); err != nil {
		return nil, err
	}
	var counters []uint64
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, id := range targets {
		counters = append(counters, s.messages[messageKey{id: id, associated: true}].cn)
	}
	return counters, nil
}

func (s *ramStore) readFAIRowLocked(ctx context.Context, root uint64, id core.ID) (messageRow, error) {
	r, exists := s.fai.records[id]
	if !exists || r.deleted || r.folder != root {
		return messageRow{}, core.ErrNotFound
	}
	s.mu.Lock()
	mid := s.byMessage[messageKey{id: id, associated: true}]
	v := s.messages[messageKey{id: id, associated: true}]
	s.mu.Unlock()
	if mid == 0 || !v.present {
		return messageRow{}, core.ErrResync
	}
	object, err := cloneMessageObject(r.object)
	if err != nil {
		return messageRow{}, err
	}
	if err = ctx.Err(); err != nil {
		return messageRow{}, err
	}
	row := messageRow{mid: mid, parent: root, cn: v.cn, readCN: v.readCN, associated: true, read: v.read, modified: v.modified, object: object}
	if r.sync == nil {
		row.sourceKey = s.hierarchyXID(mid)
		row.changeKey = gidChangeXID(hierarchyXID(s.replica, v.cn))
		row.predecessors = append([]byte{22}, []byte(row.changeKey)...)
	} else {
		row.sourceKey = r.sync.source
		row.changeKey = r.sync.change
		row.predecessors = r.sync.predecessors.marshal()
	}
	return row, nil
}
func validFAISync(v *faiSyncVersion) bool {
	if v == nil {
		return true
	}
	if !validFAIXID(v.source) || !v.change.valid() || len(v.predecessors) < 1 || len(v.predecessors) > maxFAIPCLReplicas || !v.predecessors.includesXID(v.change) {
		return false
	}
	for set, counter := range v.predecessors {
		if set == gidChangeXID(v.source).namespace() && len(counter) != 6 {
			return false
		}
		if set == ([16]byte{}) || !counter.valid() {
			return false
		}
	}
	return true
}

// contentsMu must be held. A namespace has one LocalId width throughout this
// RAM store, including retained tombstones. Drafts do not claim a namespace;
// Save checks again so concurrent staged imports cannot change its width.
func (s *ramStore) validateFAIXIDWidths(incoming *faiSyncVersion) error {
	if incoming == nil || !validFAISync(incoming) {
		return core.ErrInvalid
	}
	// Only learn the incoming namespaces (at most 66), then compare against
	// committed versions. Their mutual consistency was checked at their Saves.
	widths := map[[16]byte]int{s.replica: 6, gidChangeXID(incoming.source).namespace(): 6}
	for ns, local := range incoming.predecessors {
		if old, exists := widths[ns]; exists && old != len(local) {
			return core.ErrInvalid
		}
		widths[ns] = len(local)
	}
	matches := func(ns [16]byte, width int) bool { old, exists := widths[ns]; return !exists || old == width }
	for source := range s.fai.absent {
		if !matches(gidChangeXID(source).namespace(), 6) {
			return core.ErrInvalid
		}
	}
	for _, record := range s.fai.records {
		v := record.sync
		if v == nil {
			continue
		}
		if !validFAISync(v) || !matches(gidChangeXID(v.source).namespace(), 6) {
			return core.ErrInvalid
		}
		for ns, local := range v.predecessors {
			if !matches(ns, len(local)) {
				return core.ErrInvalid
			}
		}
	}
	return nil
}
