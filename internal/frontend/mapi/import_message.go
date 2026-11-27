// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"crypto/sha256"
	"encoding/binary"
	"errors"
	"maps"
	"slices"
	"time"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const (
	maxPendingMessageImports  = 32
	pendingMessageImportBytes = 2*maxHierarchyText + 256 + 25*maxFAIPCLReplicas
)

// A staging handle belongs to one Execute context. The transport serializes
// requests and handle release for that context.
type mailImportDraft struct {
	collector        *faiCollector
	request          faiImportRequest
	folder           core.ID
	special          core.SpecialFolderKind
	editKey          messageKey
	editBefore       messageVersion
	counter          uint64
	history          string
	upload           *mailImportUpload
	props            map[uint32]core.Value
	propLease        *contentsLease
	recipients       map[uint32]core.Recipient
	recipientLease   *contentsLease
	files            [][]byte
	fileBytes        int
	fileLease        *contentsLease
	pendingException *calendarExceptionDraft
	exception        *savedCalendarException
	contentRevision  core.Integer
	contentVersioned bool
	saved            bool
	closed           bool
}

func (*mailImportDraft) String() string   { return "mailImportDraft{private}" }
func (*mailImportDraft) GoString() string { return "mailImportDraft{private}" }

func (s *ramStore) newMessageImport(ctx context.Context, session core.Session, fid uint64, request faiImportRequest, collector *faiCollector) (*mailImportDraft, error) {
	if s.durable.store == nil {
		return nil, core.ErrUnsupported
	}
	box := session.Mailbox()
	var special core.SpecialFolderKind
	folder, err := s.messageAnchor(ctx, session, fid, false)
	if errors.Is(err, normalMetadataOnlyError{}) {
		if _, ok := box.(core.ContactImportWriter); ok {
			special = core.ContactsFolder
			folder, err = s.scopedMessageAnchor(ctx, session, fid, false, special)
		}
		if _, ok := box.(core.AppointmentImportWriter); ok && (errors.Is(err, normalMetadataOnlyError{}) || errors.Is(err, core.ErrNotFound)) {
			if _, readable := box.(core.AppointmentContentsReader); readable {
				special = core.CalendarFolder
				folder, err = s.scopedMessageAnchor(ctx, session, fid, false, special)
			}
		}
		if errors.Is(err, core.ErrNotFound) || errors.Is(err, normalMetadataOnlyError{}) {
			err = normalImportRejection{importNormalFolderProfile}
		}
	}
	if err != nil {
		return nil, err
	}
	if special == 0 {
		_, canAppend := box.(core.MailMessageAppender)
		_, canRead := box.(core.MailMessageMIMEReader)
		if !canAppend || !canRead {
			return nil, normalImportRejection{importNormalBackendProfile}
		}
	}
	if folder.native == "" {
		return nil, normalImportRejection{importNormalFolderProfile}
	}
	counter := ownMoveCounter(s.replica, request.source)
	s.mu.Lock()
	defer s.mu.Unlock()
	if counter == 0 {
		return nil, normalImportRejection{importNormalSourceReplica}
	}
	if special == 0 && !s.clientReservedCounterLocked(counter) {
		return nil, normalImportRejection{importNormalSourceReservation}
	}
	if s.retiredCounterLocked(counter) {
		return nil, core.ErrConflict
	}
	mid := folderID(counter)
	draft := &mailImportDraft{collector: collector, request: request, folder: folder.native, special: special, counter: counter}
	history := request.predecessors
	if key, ok := s.byMID[mid]; ok {
		v, seen := s.messages[key]
		if !seen || !v.present || v.parent != fid || key.associated || key.special != special {
			return nil, core.ErrConflict
		}
		pcl, err := parseFAIPCL(s.messagePredecessors(v))
		if err != nil {
			return nil, err
		}
		if pcl.includesXID(request.change) && pcl.includes(request.predecessors) {
			return nil, errFAIIgnore
		}
		switch key.special {
		case core.ContactsFolder:
			if _, canWrite := box.(core.ContactWriter); !canWrite {
				return nil, core.ErrUnsupported
			}
		case core.CalendarFolder:
			if _, canWrite := box.(core.AppointmentDetailsWriter); !canWrite {
				return nil, core.ErrUnsupported
			}
		default:
			// The backend appends mail atomically, but cannot replace existing MIME.
			return nil, normalImportRejection{importNormalExistingMessage}
		}
		if !history.includes(pcl) {
			return nil, core.ErrConflict
		}
		draft.editKey, draft.editBefore = key, v
	} else {
		if !s.clientReservedCounterLocked(counter) {
			return nil, normalImportRejection{importNormalSourceReservation}
		}
		if _, bound := s.byFID[mid]; bound {
			return nil, core.ErrConflict
		}
		if _, claimed := s.pendingCreations[mid]; claimed {
			return nil, core.ErrConflict
		}
	}
	history, err = mergeFAIPCL(history, faiPCL{request.change.namespace(): request.change.local()})
	if err != nil {
		return nil, err
	}
	draft.history = string(history.marshal())
	return draft, nil
}

func (d *mailImportDraft) close() {
	if d == nil {
		return
	}
	d.closed = true
	if d.upload != nil {
		d.upload.close()
	}
	d.propLease.release()
	d.props, d.propLease = nil, nil
	d.recipientLease.release()
	d.recipients, d.recipientLease = nil, nil
	d.fileLease.release()
	d.files, d.fileLease, d.fileBytes = nil, nil, 0
	d.pendingException, d.exception = nil, nil
}

type mailImportUpload struct {
	draft    *mailImportDraft
	lease    *contentsLease
	data     []byte
	released bool
	failed   bool
}

func (u *mailImportUpload) close() {
	u.lease.release()
	u.data, u.lease = nil, nil
	u.released = true
}

func (u *mailImportUpload) put(ctx context.Context, data []byte) error {
	if u.released || u.failed || u.draft.saved {
		return core.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(data) > cap(u.data)-len(u.data) {
		u.failed = true
		return core.ErrLimit
	}
	u.data = append(u.data, data...)
	return nil
}

func (d *mailImportDraft) configureUpload(ctx context.Context, session core.Session, objects *mailboxObjects, object serverObject) (uint32, error) {
	store := d.collector.state.store
	if d.saved || d.upload != nil || d.propLease != nil || d.recipients != nil || len(d.files) != 0 {
		return 0, core.ErrConflict
	}
	if err := d.collector.state.authorize(ctx, session); err != nil {
		return 0, err
	}
	if !objects.hasDownloadCapacity() {
		return 0, core.ErrUnsupported
	}
	limit := maxMessageImportBytes
	if d.special != 0 {
		limit = 2 * maxContactEditBytes
	}
	lease, err := store.reserveContentsMemory(limit)
	if err != nil {
		return 0, err
	}
	u := &mailImportUpload{draft: d, lease: lease, data: make([]byte, 0, limit)}
	child := serverObject{logon: object.logon, binding: object.binding, fid: object.fid}
	handle, err := objects.addDownload(child, &hierarchyDownload{mailUpload: u})
	if err != nil {
		u.close()
		return 0, err
	}
	d.upload = u
	return handle, nil
}

func (d *mailImportDraft) set(ctx context.Context, session core.Session, values faiSetRequest) ([]faiPropertyProblem, error) {
	if d.saved || d.upload != nil {
		return nil, core.ErrConflict
	}
	if err := d.collector.state.authorize(ctx, session); err != nil {
		return nil, err
	}
	var problems []faiPropertyProblem
	props := maps.Clone(d.props)
	if props == nil {
		props = make(map[uint32]core.Value)
	}
	for i, item := range values {
		allowed := importedMailPropertyTag(item.tag)
		if d.special != 0 {
			p, err := resolveContentsIdentity(item.tag, &d.collector.state.store.named)
			allowed = err == nil && item.value != nil &&
				(d.special == core.ContactsFolder && contactImportProperty(p) ||
					d.special == core.CalendarFolder && appointmentImportProperty(p))
		}
		if !allowed {
			problems = append(problems, faiPropertyProblem{index: uint16(i), tag: item.tag, code: NotSupported})
			continue
		}
		props[item.tag] = item.value
	}
	if len(props) == 0 {
		return problems, nil
	}
	if d.propLease == nil {
		lease, err := d.collector.state.store.reserveContentsMemory(maxMessageImportBytes)
		if err != nil {
			return nil, err
		}
		d.propLease = lease
	}
	d.props = props
	return problems, nil
}

func (d *mailImportDraft) content() ([]byte, error) {
	if d.upload != nil {
		if d.upload.failed || !d.upload.released {
			return nil, core.ErrInvalid
		}
		return d.upload.data, nil
	}
	ft := &fastTransfer{limit: maxMessageImportBytes}
	for _, tag := range slices.Sorted(maps.Keys(d.props)) {
		ft.scalar(tag, d.props[tag])
	}
	for _, file := range d.files {
		if file == nil {
			return nil, core.ErrInvalid
		}
		ft.append(file, false)
	}
	return ft.data, ft.err
}

func (d *mailImportDraft) setRecipients(ctx context.Context, session core.Session, changes []draftRecipientChange, clearAll bool) error {
	if d.special != 0 {
		return core.ErrUnsupported
	}
	if d.saved || d.upload != nil {
		return core.ErrConflict
	}
	if err := d.collector.state.authorize(ctx, session); err != nil {
		return err
	}
	rows, err := applyRecipientChanges(d.recipients, changes, clearAll)
	if err != nil {
		return err
	}
	if d.recipientLease == nil {
		lease, err := d.collector.state.store.reserveContentsMemory(maxMessageImportBytes)
		if err != nil {
			return err
		}
		d.recipientLease = lease
	}
	d.recipients = rows
	return nil
}

func (d *mailImportDraft) saveAttachment(a *draftAttachment) error {
	if d.special != 0 {
		return core.ErrUnsupported
	}
	i := int(a.number)
	if i >= len(d.files) {
		return core.ErrInvalid
	}
	room := maxMessageImportBytes - d.fileBytes + len(d.files[i])
	if room <= 0 || len(a.data) > room {
		return core.ErrLimit
	}
	if d.fileLease == nil {
		lease, err := d.collector.state.store.reserveContentsMemory(2 * maxMessageImportBytes)
		if err != nil {
			return err
		}
		d.fileLease = lease
	}
	ft := &fastTransfer{limit: room}
	ft.word(0x40000003) // NewAttach.
	ft.scalar(attachNumberTag, core.Integer(a.number))
	ft.scalar(0x37050003, core.Integer(1))
	ft.scalar(attachDataBinary, core.Binary(a.data))
	for _, value := range []struct {
		tag  uint32
		text string
	}{{0x3707001f, a.name}, {0x3704001f, a.shortName}, {0x370e001f, a.media}, {0x3712001f, a.cid}} {
		if value.text != "" {
			ft.scalar(value.tag, core.Text(value.text))
		}
	}
	ft.word(0x400e0003) // EndAttach.
	if ft.err != nil {
		return ft.err
	}
	d.fileBytes += len(ft.data) - len(d.files[i])
	d.files[i] = ft.data
	return nil
}

func (d *mailImportDraft) save(ctx context.Context, session core.Session, flags byte, resolve func(string) (string, error)) (uint64, error) {
	if _, valid := faiSaveMode(flags); !valid {
		if d.special == core.CalendarFolder {
			return 0, appointmentImportRejection{importAppointmentSaveMode, core.ErrInvalid}
		}
		return 0, core.ErrInvalid
	}
	c := d.collector
	mid := folderID(d.counter)
	if d.saved {
		return mid, nil
	}
	if d.upload == nil && len(d.props) == 0 {
		if d.special == core.CalendarFolder {
			return 0, appointmentImportRejection{importAppointmentSaveEmpty, core.ErrInvalid}
		}
		return 0, core.ErrInvalid
	}
	switch d.special {
	case core.ContactsFolder:
		return d.saveContact(ctx, session)
	case core.CalendarFolder:
		return d.saveAppointment(ctx, session)
	}
	// Charge temporary MIME encoding and native readback only while saving.
	workspace, err := c.state.store.reserveContentsPeak(ctx, 6*core.MaxMailContentMIMEBytes)
	if err != nil {
		return 0, err
	}
	defer workspace.release()
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return 0, core.ErrClosed
	}
	raw, err := d.content()
	if err != nil {
		return 0, err
	}
	mime, options, err := importedMailMIME(raw, d.recipients, resolve)
	if err != nil {
		return 0, err
	}
	if err := c.prepareNormalMutation(ctx, session); err != nil {
		return 0, err
	}
	cn, err := c.state.store.saveMailImport(ctx, session, c.state.root, d, raw, mime, options)
	if err != nil {
		return 0, err
	}
	c.acknowledgeNormal(d.counter, cn, true)
	d.saved = true
	d.close()
	return mid, nil
}

func (c *faiCollector) prepareNormalMutation(ctx context.Context, session core.Session) error {
	if err := c.prepareMutation(ctx, session, 1); err != nil {
		return err
	}
	c.state.mu.Lock()
	defer c.state.mu.Unlock()
	if len(contentsRanges(c.state.value.seen)) >= maxIDSetRanges {
		return core.ErrLimit
	}
	return nil
}

// The collector lock is held by the caller across the native write and ACK.
func (c *faiCollector) acknowledgeNormal(counter, cn uint64, created bool) {
	c.state.mu.Lock()
	seen := contentsRanges(c.state.value.seen)
	c.state.value.seen = singleReplicaSet(c.state.store.replica, append(seen, idRange{cn, cn}))
	c.state.mu.Unlock()
	if !created {
		c.changes++
	} else if !slices.Contains(c.created, counter) {
		c.created = append(c.created, counter)
		c.changes++
	}
}

func (s *ramStore) prepareMessageImportEdit(fid uint64, d *mailImportDraft) (messageVersion, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := s.byMID[folderID(d.counter)]
	current := s.messages[key]
	if key != d.editKey || current != d.editBefore || current.parent != fid || !current.present {
		return messageVersion{}, core.ErrConflict
	}
	if s.next == maxGlobalCount || s.messageRegistryBytesLocked()+len(d.history)+len(d.request.change)-
		len(current.history)-len(current.change) > s.messageRegistryLimit() {
		return messageVersion{}, core.ErrLimit
	}
	return current, nil
}

func (s *ramStore) publishMessageImportEdit(ctx context.Context, d *mailImportDraft, current messageVersion, revision core.Revision, fingerprint [32]byte) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.messages[d.editKey] != current || s.next == maxGlobalCount || s.messageRegistryBytesLocked()+
		len(d.history)+len(d.request.change)-len(current.history)-len(current.change) > s.messageRegistryLimit() {
		return 0, core.ErrOutcomeUnknown
	}
	mid := folderID(d.counter)
	current.cn = s.next + 1
	current.change, current.history, current.modified = d.request.change, d.history, d.request.modified
	current.fingerprint, current.nativeRevision = fingerprint, revision
	if err := s.commitReplica(ctx, mapireplica.Mutation{
		HighWater: current.cn, Projection: []mapireplica.Projection{s.persistedMessageProjection(mid, current)},
	}); err != nil {
		return 0, errors.Join(core.ErrOutcomeUnknown, err)
	}
	s.messages[d.editKey], s.next = current, current.cn
	return current.cn, nil
}

func (s *ramStore) saveMailImport(ctx context.Context, session core.Session, fid uint64, d *mailImportDraft, raw, mime []byte, options core.MailAppendOptions) (uint64, error) {
	s.contentsMu.Lock()
	defer s.contentsMu.Unlock()
	folder, err := s.messageAnchor(ctx, session, fid, false)
	if err != nil {
		return 0, err
	}
	if folder.native != d.folder {
		return 0, core.ErrConflict
	}
	appender, ok := session.Mailbox().(core.MailMessageAppender)
	reader, canRead := session.Mailbox().(core.MailMessageMIMEReader)
	if !ok || !canRead {
		return 0, core.ErrUnsupported
	}
	p := mapireplica.MessageImport{
		Counter: d.counter, Parent: folderGlobalCount(fid), MIME: sha256.Sum256(mime), Digest: d.requestDigest(raw),
	}
	newImport, err := s.claimMessageImport(ctx, p)
	if err != nil {
		return 0, err
	}
	if newImport {
		native, appendErr := appender.AppendMailMessage(ctx, folder.native, mime, options)
		if native.ID != "" {
			if len(native.ID) > maxHierarchyText || !validMailText(string(native.ID)) {
				return 0, core.ErrOutcomeUnknown
			}
			p.NativeID = native.ID
			if err := s.updateMessageImport(ctx, p, mapireplica.RecordMessageImport); err != nil {
				return 0, errors.Join(core.ErrOutcomeUnknown, err)
			}
		}
		if appendErr != nil {
			return 0, s.messageImportFailure(ctx, p, appendErr)
		}
		if p.NativeID == "" {
			return 0, core.ErrOutcomeUnknown
		}
	} else {
		s.mu.Lock()
		p = s.pendingMessageImports[folderID(p.Counter)]
		s.mu.Unlock()
		if p.NativeID == "" {
			return 0, core.ErrOutcomeUnknown
		}
	}
	obj, err := reader.ReadMailMessageMIME(ctx, folder.native, p.NativeID, false)
	if err != nil || obj.ID != p.NativeID || obj.Kind != core.Message || obj.Parent != folder.native ||
		sha256.Sum256(obj.MIME) != p.MIME || !validMoveMetadata(obj) ||
		obj.Properties[core.IsDraft] != core.Bool(false) || obj.Properties[core.IsRead] != core.Bool(options.Read) ||
		(obj.Properties[core.IsFlagged] == core.Bool(true)) != options.Flagged {
		return 0, errors.Join(core.ErrOutcomeUnknown, err)
	}
	fingerprint, _, err := messageFingerprint(obj)
	if err != nil {
		return 0, errors.Join(core.ErrOutcomeUnknown, err)
	}
	cn, err := s.publishMessageImport(ctx, p, d, obj.Revision, options.Read, fingerprint)
	if err != nil {
		return 0, err
	}
	d.contentRevision, d.contentVersioned = obj.Properties[core.ContentRevision].(core.Integer)
	return cn, nil
}

func (d *mailImportDraft) requestDigest(payload []byte) [32]byte {
	h := sha256.New()
	h.Write(binary.LittleEndian.AppendUint64(nil, fileTime(d.request.modified)))
	h.Write([]byte{byte(len(d.request.change))})
	h.Write([]byte(d.request.change))
	h.Write(binary.LittleEndian.AppendUint32(nil, uint32(len(d.history))))
	h.Write([]byte(d.history))
	h.Write(payload)
	var digest [32]byte
	copy(digest[:], h.Sum(nil))
	return digest
}

func (s *ramStore) claimMessageImport(ctx context.Context, p mapireplica.MessageImport) (bool, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	mid := folderID(p.Counter)
	if old, known := s.pendingMessageImports[mid]; known {
		if old.Parent != p.Parent || old.Digest != p.Digest || old.MIME != p.MIME || old.Special() != p.Special() {
			return false, core.ErrConflict
		}
		return false, nil
	}
	if _, used := s.byMID[mid]; used {
		return false, core.ErrConflict
	}
	if s.retiredCounterLocked(p.Counter) {
		return false, core.ErrConflict
	}
	if len(s.pendingMessageImports) >= maxPendingMessageImports || s.mappingCountLocked() >= s.mappingLimit() ||
		s.next > maxGlobalCount-5 || s.messageRegistryBytesLocked()+pendingMessageImportBytes > s.messageRegistryLimit() {
		return false, core.ErrLimit
	}
	if err := s.advanceReplicaLocked(ctx, mapireplica.Mutation{MessageImport: &p, ImportAction: mapireplica.ClaimMessageImport}); err != nil {
		return false, err
	}
	if s.pendingMessageImports == nil {
		s.pendingMessageImports = make(map[uint64]mapireplica.MessageImport)
	}
	s.pendingMessageImports[mid] = p
	return true, nil
}

func (s *ramStore) updateMessageImport(ctx context.Context, p mapireplica.MessageImport, action mapireplica.MessageImportAction) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.advanceReplicaLocked(ctx, mapireplica.Mutation{MessageImport: &p, ImportAction: action}); err != nil {
		return err
	}
	mid := folderID(p.Counter)
	if action == mapireplica.CancelMessageImport {
		delete(s.pendingMessageImports, mid)
	} else {
		s.pendingMessageImports[mid] = p
	}
	return nil
}

func (s *ramStore) messageImportFailure(ctx context.Context, p mapireplica.MessageImport, cause error) error {
	if p.NativeID != "" || errors.Is(cause, core.ErrOutcomeUnknown) {
		return errors.Join(core.ErrOutcomeUnknown, cause)
	}
	if !errors.Is(cause, core.ErrWriteNotDispatched) && !errors.Is(cause, core.ErrInvalid) &&
		!errors.Is(cause, core.ErrConflict) && !errors.Is(cause, core.ErrNotFound) &&
		!errors.Is(cause, core.ErrAccessDenied) && !errors.Is(cause, core.ErrAuthentication) &&
		!errors.Is(cause, core.ErrUnsupported) && !errors.Is(cause, core.ErrLimit) {
		return errors.Join(core.ErrOutcomeUnknown, cause)
	}
	commit, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Second)
	defer cancel()
	if err := s.updateMessageImport(commit, p, mapireplica.CancelMessageImport); err != nil {
		return errors.Join(core.ErrOutcomeUnknown, err)
	}
	return cause
}

func (s *ramStore) publishMessageImport(ctx context.Context, p mapireplica.MessageImport, d *mailImportDraft, revision core.Revision, read bool, fingerprint [32]byte) (uint64, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.next > maxGlobalCount-2 {
		return 0, core.ErrOutcomeUnknown
	}
	mid := folderID(p.Counter)
	version := messageVersion{
		parent: folderID(p.Parent), cn: s.next + 1, readCN: s.next + 2,
		change: d.request.change, history: d.history, fingerprint: fingerprint,
		modified: d.request.modified, present: true, read: read, nativeRevision: revision,
	}
	if err := s.commitReplica(ctx, mapireplica.Mutation{
		HighWater: version.readCN, MessageImport: &p, ImportAction: mapireplica.PublishMessageImport,
		Projection: []mapireplica.Projection{s.persistedMessageProjection(mid, version)},
	}); err != nil {
		return 0, errors.Join(core.ErrOutcomeUnknown, err)
	}
	key := messageKey{id: p.NativeID, special: p.Special()}
	s.byMID[mid], s.byMessage[key], s.messages[key], s.next = key, mid, version, version.readCN
	delete(s.pendingMessageImports, mid)
	return version.cn, nil
}

func (s *ramStore) loadMessageImports(imports []mapireplica.MessageImport) error {
	if len(imports) > maxPendingMessageImports {
		return core.ErrLimit
	}
	if len(imports) != 0 {
		s.pendingMessageImports = make(map[uint64]mapireplica.MessageImport, len(imports))
	}
	for _, p := range imports {
		mid := folderID(p.Counter)
		if _, exists := s.byMID[mid]; exists {
			return core.ErrInvalid
		}
		if _, exists := s.byFID[mid]; exists || !s.clientReservedCounterLocked(p.Counter) ||
			s.byFID[folderID(p.Parent)].native == "" {
			return core.ErrInvalid
		}
		if len(p.NativeID) > maxHierarchyText || !validMailText(string(p.NativeID)) ||
			(p.NativeID != "" && s.byMessage[messageKey{id: p.NativeID, special: p.Special()}] != 0) {
			return core.ErrInvalid
		}
		s.pendingMessageImports[mid] = p
	}
	return nil
}

func (s *ramStore) pendingImportedMessage(fid uint64, o core.Object) bool {
	var digest [32]byte
	for _, p := range s.pendingMessageImports {
		if p.Special() != 0 || folderID(p.Parent) != fid {
			continue
		}
		if p.NativeID == o.ID {
			return true
		}
		if p.NativeID != "" || len(o.MIME) == 0 {
			continue
		}
		if digest == ([32]byte{}) {
			digest = sha256.Sum256(o.MIME)
		}
		if p.MIME == digest {
			return true
		}
	}
	return false
}

// A pending append without an ID requires MIME only for as yet unmapped
// index rows. An unreadable candidate is withheld individually.
func (s *ramStore) filterIndexedImportCandidates(ctx context.Context, reader core.MailMessageMIMEReader, fid uint64, folder core.ID, objects []core.Object) ([]core.Object, error) {
	if reader == nil {
		return objects, nil // A backend without a MIME reader uses the full view.
	}
	s.mu.Lock()
	digests := make(map[[32]byte]bool)
	for _, p := range s.pendingMessageImports {
		if p.Special() == 0 && folderID(p.Parent) == fid && p.NativeID == "" {
			digests[p.MIME] = true
		}
	}
	if len(digests) == 0 {
		s.mu.Unlock()
		return objects, nil
	}
	unmapped := make(map[core.ID]bool)
	for _, object := range objects {
		if s.byMessage[messageKey{id: object.ID}] == 0 {
			unmapped[object.ID] = true
		}
	}
	s.mu.Unlock()
	visible := make([]core.Object, 0, len(objects))
	for _, object := range objects {
		if unmapped[object.ID] {
			full, err := reader.ReadMailMessageMIME(ctx, folder, object.ID, false)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			contentRevision, versioned := full.Properties[core.ContentRevision].(core.Integer)
			if err != nil || full.ID != object.ID || full.Parent != folder || full.Kind != core.Message || !versioned ||
				object.Properties[core.ContentRevision] != contentRevision || len(full.MIME) == 0 || digests[sha256.Sum256(full.MIME)] {
				continue
			}
		}
		visible = append(visible, object)
	}
	return visible, nil
}
