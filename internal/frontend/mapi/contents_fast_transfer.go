// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"sort"
	"strings"
	"time"
	"unicode/utf16"
	"unicode/utf8"

	"ewsgo/internal/core"
	"ewsgo/internal/mapireplica"
)

const (
	maxContentsFastTransferBytes        = 4 << 20
	cnsetSeenFAITag              uint32 = 0x67da0102
	cnsetReadTag                 uint32 = 0x67d20102
)

type contentsSyncState struct{ given, seen, seenFAI, read idSet }
type contentsChange struct {
	row     messageRow
	payload projectedMessage
}
type contentsDelta struct {
	changes                         []contentsChange
	deleted, noLonger, read, unread []uint64
	retired                         []idRange
	final                           contentsSyncState
}
type contentsTransfer struct {
	stream                           *fastTransfer
	lease                            *contentsLease
	sequence                         *contentsSequence
	normalChanges, associatedChanges bool // Presence only, for opt-in completion diagnostics.
}

func (contentsSyncState) String() string   { return "contentsSyncState{private}" }
func (contentsSyncState) GoString() string { return "contentsSyncState{private}" }
func (contentsChange) String() string      { return "contentsChange{private}" }
func (contentsChange) GoString() string    { return "contentsChange{private}" }
func (contentsDelta) String() string       { return "contentsDelta{private}" }
func (contentsDelta) GoString() string     { return "contentsDelta{private}" }
func (*contentsTransfer) String() string   { return "contentsTransfer{private}" }
func (*contentsTransfer) GoString() string { return "contentsTransfer{private}" }
func (t *contentsTransfer) close() {
	if t != nil {
		t.stream = nil
		t.lease.release()
		t.lease = nil
		if t.sequence != nil {
			t.sequence.prepared.close()
			t.sequence = nil
		}
	}
}

func (s *ramStore) projectContentsPayload(row messageRow, identity core.Identity) (projectedMessage, error) {
	if row.privateConfig {
		return projectFreeBusyMessage(row), nil
	}
	if row.special == core.ContactsFolder {
		return projectContact(row), nil
	}
	if row.special != 0 {
		return projectCalendarSearchItem(row)
	}
	if !row.associated {
		p, err := projectReadableMailContents(row.object)
		if err == nil {
			p.bytes += s.projectMeetingDelivery(p.values, row.object, identity)
			if p.bytes > maxMailProjectionBytes {
				return projectedMessage{}, core.ErrUnsupported
			}
		}
		return p, err
	}
	if _, err := s.validateFAIRecord(faiRecord{folder: row.parent, object: row.object}); err != nil {
		return projectedMessage{}, err
	}
	p := projectedMessage{values: contentsValues{}, bytes: 1024}
	for key, value := range row.object.Properties {
		property, err := s.faiProperty(key, value)
		if err != nil {
			return projectedMessage{}, err
		}
		if b, ok := value.(core.Binary); ok {
			value = append(core.Binary(nil), b...)
		}
		p.values[property] = value
	}
	_, used, err := messageFingerprint(row.object)
	if err != nil {
		return projectedMessage{}, err
	}
	p.size, p.bytes = used, used+1024
	// The initial FAI profile has no attachments or transport state.
	// mfFAI is server-derived; mfRead is ignored for associated messages.
	// Do not synthesize a delivery date from creation/modification time.
	putContentsValue(p.values, 0x0e070003, core.Integer(0x40))
	return p, nil
}

func (s *ramStore) projectContentsRow(row messageRow, identity core.Identity) (contentsChange, error) {
	p, err := s.projectContentsPayload(row, identity)
	if err != nil {
		return contentsChange{}, err
	}
	addContentsRowMetadata(&p, row)
	if s.isQueuedSubmission(row.mid) {
		tag := fixedContentsProperty(0x0e070003)
		if flags, ok := p.values[tag].(core.Integer); ok {
			p.values[tag] = flags | 4
		}
	}
	return contentsChange{row: row, payload: p}, nil
}

func addContentsRowMetadata(p *projectedMessage, row messageRow) {
	p.bytes += len(row.predecessors) + 256
	putContentsValue(p.values, 0x65e00102, core.Binary(append([]byte(nil), row.sourceKey[:]...)))
	putContentsValue(p.values, 0x30080040, core.Timestamp(row.modified))
	putContentsValue(p.values, 0x65e20102, core.Binary([]byte(row.changeKey)))
	putContentsValue(p.values, 0x65e30102, core.Binary(append([]byte(nil), row.predecessors[:]...)))
	putContentsValue(p.values, 0x67aa000b, core.Bool(row.associated))
}

// Only encoding is performed here. The download layer must authorize before
// and after capture, bind/evaluate restrictions, and own the projection lease.
// An encoding error releases its lease and exposes no partial stream.
func (s *ramStore) contentsFastTransfer(ctx context.Context, c synchronizationConfigure, selection contentsSelection, delta contentsDelta) (*contentsTransfer, error) {
	if err := validateContentsTransfer(c, delta); err != nil {
		return nil, err
	}
	lease, err := s.reserveContentsPeak(ctx, maxContentsFastTransferBytes)
	if err != nil {
		return nil, err
	}
	f := &fastTransfer{limit: maxContentsFastTransferBytes}
	result := &contentsTransfer{stream: f, lease: lease}
	fail := func(err error) (*contentsTransfer, error) { result.close(); return nil, err }
	changes := orderedContentsChanges(c, delta.changes)
	if c.flags&0x8000 != 0 {
		if err := writeContentsProgress(f, changes); err != nil {
			return fail(err)
		}
	}
	seen := map[uint64]bool{}
	for _, change := range changes {
		if err := ctx.Err(); err != nil {
			return fail(err)
		}
		if seen[change.row.mid] {
			return fail(core.ErrInvalid)
		}
		seen[change.row.mid] = true
		if err := s.writeContentsChange(f, c, selection, change); err != nil {
			return fail(err)
		}
		if change.row.associated {
			result.associatedChanges = true
		} else {
			result.normalChanges = true
		}
	}
	if err := s.writeContentsTail(f, c, delta); err != nil {
		return fail(err)
	}
	if err := ctx.Err(); err != nil {
		return fail(err)
	}
	if f.err != nil {
		return fail(f.err)
	}
	// Encoding reserves the maximum before allocating. A retained immutable
	// stream owns only its encoded bytes and atom accounting units afterwards;
	// leaving the peak reservation live starves other concurrent downloads.
	if err := lease.shrink(len(f.data) + 16*len(f.atoms)); err != nil {
		return fail(err)
	}
	return result, nil
}

func validateContentsTransfer(c synchronizationConfigure, delta contentsDelta) error {
	if err := validateContentsConfigure(c); err != nil {
		return err
	}
	if len(delta.changes) > core.MaxMailContentIndexItems+maxFAIObjects {
		return core.ErrUnsupported
	}
	// Contradictory categories cannot be emitted even if each IDSET encodes.
	classified := map[uint64]byte{}
	for group, ids := range [][]uint64{delta.deleted, delta.noLonger, delta.read, delta.unread} {
		if len(ids) > mapireplica.MaxBindings {
			return core.ErrUnsupported
		}
		for _, id := range ids {
			if old := classified[id]; old != 0 && old != byte(group+1) {
				return core.ErrInvalid
			}
			classified[id] = byte(group + 1)
		}
	}
	return nil
}

func orderedContentsChanges(c synchronizationConfigure, source []contentsChange) []contentsChange {
	changes := append([]contentsChange(nil), source...)
	if c.extraFlags&8 != 0 {
		sort.SliceStable(changes, func(i, j int) bool {
			at := func(change contentsChange) time.Time {
				if date, ok := change.payload.values[fixedContentsProperty(0x0e060040)].(core.Timestamp); ok {
					return time.Time(date)
				}
				return change.row.modified
			}
			a, b := at(changes[i]), at(changes[j])
			if a.Equal(b) {
				return changes[i].row.mid < changes[j].row.mid
			}
			return a.After(b)
		})
	}
	return changes
}

func writeContentsProgress(f *fastTransfer, changes []contentsChange) error {
	var raw [32]byte
	for _, change := range changes {
		if change.payload.size < 0 || change.payload.size > maxMailProjectionBytes {
			return core.ErrInvalid
		}
		count, size := 4, 8
		if !change.row.associated {
			count, size = 16, 24
		}
		binary.LittleEndian.PutUint32(raw[count:], binary.LittleEndian.Uint32(raw[count:])+1)
		binary.LittleEndian.PutUint64(raw[size:], binary.LittleEndian.Uint64(raw[size:])+uint64(change.payload.size))
	}
	f.word(0x4074000b)
	f.variable(0x00000102, raw[:])
	return f.err
}

func (s *ramStore) writeContentsChange(f *fastTransfer, c synchronizationConfigure, selection contentsSelection, change contentsChange) error {
	row, p := change.row, change.payload
	if row.mid == 0 || uint16(row.mid) != 1 || folderGlobalCount(row.mid) == 0 || row.cn == 0 || row.cn > maxGlobalCount || row.modified.Year() < 1601 || row.modified.Year() > 9999 || p.size < 0 || p.size > maxMailProjectionBytes || row.associated && c.flags&0x10 == 0 || !row.associated && c.flags&0x20 == 0 {
		return core.ErrInvalid
	}
	progress := c.flags&0x8000 != 0
	if progress {
		f.word(0x4075000b)
		f.scalar(0x00000003, core.Integer(p.size))
		f.scalar(0x0000000b, core.Bool(row.associated))
	}
	f.word(0x40120003)
	sourceKey := row.sourceKey
	if c.flags&0x100 != 0 {
		sourceKey = s.hierarchyXID(row.mid)
	}
	f.variable(0x65e00102, sourceKey[:])
	f.fixed(0x30080040, fileTime(row.modified))
	f.variable(0x65e20102, []byte(row.changeKey))
	f.variable(0x65e30102, row.predecessors[:])
	f.scalar(0x67aa000b, core.Bool(row.associated))
	if c.extraFlags&1 != 0 {
		f.fixed(0x674a0014, row.mid)
	}
	if c.extraFlags&2 != 0 {
		f.scalar(0x0e080003, core.Integer(p.size))
	}
	if c.extraFlags&4 != 0 {
		f.fixed(0x67a40014, folderID(row.cn))
	}
	f.word(0x40150003)
	if err := s.contentsPropertyList(f, p.values, func(property contentsProperty) bool {
		if contentsHeaderProperty(property.id) {
			return false
		}
		html := fixedContentsProperty(0x10130102)
		if property.id == messageBodyID && selection.includes(html, row.associated) {
			if c.flags&0x2000 != 0 && p.values[html] != nil {
				return false
			}
			// Outlook can exclude plain text while accepting HTML. Preserve
			// the only body for text-only mail in this exclusion mode.
			if !row.associated && !selection.onlySpecified && p.values[html] == nil {
				return true
			}
		}
		return selection.includes(property, row.associated)
	}); err != nil {
		return err
	}
	if err := s.writeFastTransferMessageChildren(f, p, func(tag uint32) bool {
		return selection.includes(fixedContentsProperty(tag), row.associated)
	}); err != nil {
		return err
	}
	return f.err
}

func (s *ramStore) writeContentsTail(f *fastTransfer, c synchronizationConfigure, delta contentsDelta) error {
	if len(delta.deleted) > 0 || len(delta.retired) > 0 || len(delta.noLonger) > 0 {
		if c.flags&2 != 0 || len(delta.noLonger) > 0 && c.flags&4 != 0 {
			return core.ErrInvalid
		}
		f.word(0x40130003)
		if err := f.messageIDRanges(s.replica, 0x67e50102, delta.deleted, delta.retired); err != nil {
			return err
		}
		if err := f.messageIDSet(s.replica, 0x40210102, delta.noLonger); err != nil {
			return err
		}
	}
	if len(delta.read) > 0 || len(delta.unread) > 0 {
		if c.flags&8 == 0 {
			return core.ErrInvalid
		}
		f.word(0x402f0003)
		if err := f.messageIDSet(s.replica, 0x402d0102, delta.read); err != nil {
			return err
		}
		if err := f.messageIDSet(s.replica, 0x402e0102, delta.unread); err != nil {
			return err
		}
	}
	if err := f.contentsState(delta.final); err != nil {
		return err
	}
	f.word(0x40140003)
	return f.err
}

func validateContentsConfigure(c synchronizationConfigure) error {
	// RecoverMode/PartialItem advertise client capabilities. This profile
	// always sends full changes and fails the entire stream on an error.
	if c.kind != 1 || c.flags&1 == 0 || c.flags & ^uint16(0xe1bf) != 0 || len(c.tags) > maxFolderPropertyTags || c.sendOptions&1 == 0 || c.sendOptions & ^byte(0x7d) != 0 {
		return core.ErrUnsupported
	}
	// Unknown ExtraFlags and reserved SendOptions 0x20/0x40 are ignored.
	return nil
}

func contentsHeaderProperty(id uint16) bool {
	switch id {
	case 0x65e0, 0x3008, 0x65e2, 0x65e3, 0x67aa, 0x674a, 0x0e08, 0x67a4:
		return true
	}
	return false
}
func (s *ramStore) writeFastTransferMessageChildren(f *fastTransfer, p projectedMessage, include func(uint32) bool) error {
	for _, sub := range []struct {
		tag, start, end uint32
		rows            []contentsValues
	}{
		{0x0e12000d, 0x40030003, 0x40040003, p.recipients},
		{0x0e13000d, 0x40000003, 0x400e0003, p.attachments},
	} {
		if !include(sub.tag) {
			continue
		}
		f.scalar(0x40160003, core.Integer(sub.tag))
		for index, values := range sub.rows {
			f.word(sub.start)
			// MS-OXCFXICS 2.2.4.3.23 requires Rowid first after
			// StartRecip, just as AttachNumber precedes attachment data.
			identityTag := uint32(0x30000003)
			if sub.tag == 0x0e13000d {
				identityTag = 0x0e210003
			}
			n, ok := values[fixedContentsProperty(identityTag)].(core.Integer)
			if !ok {
				return core.ErrInvalid
			}
			f.scalar(identityTag, n)
			if err := s.contentsPropertyList(f, values, func(property contentsProperty) bool { return property.id != uint16(identityTag>>16) }); err != nil {
				return err
			}
			if sub.tag == 0x0e13000d {
				if message, ok := p.embedded[index]; ok {
					f.word(0x40010003) // StartEmbed.
					if err := s.contentsPropertyList(f, message.values, nil); err != nil {
						return err
					}
					if err := s.writeFastTransferMessageChildren(f, message, func(uint32) bool { return true }); err != nil {
						return err
					}
					f.word(0x40020003) // EndEmbed.
				}
			}
			f.word(sub.end)
		}
	}
	return f.err
}

func (s *ramStore) contentsPropertyList(f *fastTransfer, values contentsValues, include func(contentsProperty) bool) error {
	return s.writeFastTransferProperties(f, values, include, nil)
}

func (s *ramStore) writeFastTransferProperties(f *fastTransfer, values contentsValues, include func(contentsProperty) bool, selectTag func(uint32) bool) error {
	type propertyValue struct {
		property contentsProperty
		tag      uint32
		value    core.Value
	}
	properties := make([]propertyValue, 0, len(values))
	for property, value := range values {
		if property.id >= 0x6600 && property.id <= 0x67ff {
			continue
		} // MS-OXCFXICS 3.2.5.12.
		if include != nil && !include(property) {
			continue
		}
		id := property.id
		if property.name != (propertyName{}) {
			if id != 0 {
				return core.ErrInvalid
			}
			s.named.mu.Lock()
			id = s.named.byKey[property.name]
			s.named.mu.Unlock()
			if id < 0x8001 {
				return core.ErrUnsupported
			}
		} else if id == 0 || id >= 0x8000 {
			return core.ErrInvalid
		}
		if !contentsValueType(property.kind, value) {
			return core.ErrInvalid
		}
		if selectTag != nil && !selectTag(uint32(id)<<16|uint32(property.kind)) {
			continue
		}
		kind := property.kind
		if kind == 0x001f && f.string8CodePage != 0 {
			kind = 0x001e
		}
		properties = append(properties, propertyValue{property, uint32(id)<<16 | uint32(kind), value})
	}
	sort.Slice(properties, func(i, j int) bool { return properties[i].tag < properties[j].tag })
	for _, p := range properties {
		if p.property.name != (propertyName{}) {
			name := p.property.name
			definition := binary.LittleEndian.AppendUint32(nil, p.tag)
			definition = append(definition, name.Set[:]...)
			definition = append(definition, name.Kind)
			switch name.Kind {
			case 0:
				definition = binary.LittleEndian.AppendUint32(definition, name.LID)
			case 1:
				if name.Name == "" || !utf8.ValidString(name.Name) || strings.ContainsRune(name.Name, 0) || len(utf16.Encode([]rune(name.Name))) > 126 {
					return core.ErrInvalid
				}
				definition = append(definition, contentsUnicode(name.Name)...)
			default:
				return core.ErrInvalid
			}
			f.append(definition, true) // The entire named propDef is a single atom.
		} else {
			f.word(p.tag)
		}
		f.scalarValue(p.tag, p.value)
	}
	return f.err
}

func contentsUnicode(text string) []byte {
	var data []byte
	for _, unit := range utf16.Encode([]rune(text)) {
		data = binary.LittleEndian.AppendUint16(data, unit)
	}
	return append(data, 0, 0)
}
func (f *fastTransfer) scalar(tag uint32, value core.Value) { f.word(tag); f.scalarValue(tag, value) }
func (f *fastTransfer) scalarValue(tag uint32, value core.Value) {
	kind := uint16(tag)
	if kind == 0x001e && f.string8CodePage != 0 {
		text, ok := value.(core.Text)
		if !ok {
			f.err = core.ErrInvalid
			return
		}
		data, ok := propertyString8(string(text), f.string8CodePage)
		if !ok {
			f.err = core.ErrUnsupported
			return
		}
		f.word(uint32(len(data)))
		f.append(data, false)
		return
	}
	if !contentsValueType(kind, value) {
		f.err = core.ErrInvalid
		return
	}
	switch v := value.(type) {
	case core.Integer:
		switch uint16(tag) {
		case 2:
			f.append(binary.LittleEndian.AppendUint16(nil, uint16(v)), true)
		case 3:
			f.append(binary.LittleEndian.AppendUint32(nil, uint32(v)), true)
		case 0x14:
			f.append(binary.LittleEndian.AppendUint64(nil, uint64(v)), true)
		default:
			f.err = core.ErrUnsupported
		}
	case core.Bool:
		var b byte
		if v {
			b = 1
		}
		f.append([]byte{b, 0}, true)
	case core.Timestamp:
		f.append(binary.LittleEndian.AppendUint64(nil, fileTime(time.Time(v))), true)
	case core.Text:
		data := contentsUnicode(string(v))
		f.word(uint32(len(data)))
		f.append(data, false)
	case core.Binary:
		f.word(uint32(len(v)))
		f.append(v, false)
	case core.Int32s:
		f.word(uint32(len(v)))
		for _, item := range v {
			f.word(uint32(item))
		}
	default:
		f.err = core.ErrUnsupported
	}
}
func (f *fastTransfer) messageIDSet(replica [16]byte, tag uint32, ids []uint64) error {
	return f.messageIDRanges(replica, tag, ids, nil)
}

func (f *fastTransfer) messageIDRanges(replica [16]byte, tag uint32, ids []uint64, retired []idRange) error {
	if len(ids) == 0 && len(retired) == 0 {
		return nil
	}
	if len(ids) > mapireplica.MaxBindings {
		return core.ErrUnsupported
	}
	ranges := make([]idRange, 0, len(ids)+len(retired))
	ranges = append(ranges, retired...)
	for _, id := range ids {
		if id == 0 || uint16(id) != 1 {
			return core.ErrInvalid
		}
		n := folderGlobalCount(id)
		if n == 0 {
			return core.ErrInvalid
		}
		ranges = append(ranges, idRange{n, n})
	}
	raw, err := singleReplicaSet(replica, ranges).marshal()
	if err != nil {
		return err
	}
	f.variable(tag, append([]byte{1, 0}, raw[16:]...)) // REPLID-based IDSET.
	return f.err
}
func (f *fastTransfer) contentsState(state contentsSyncState) error {
	f.word(0x403a0003)
	for _, item := range []struct {
		tag uint32
		set idSet
	}{{idsetGivenTag, state.given}, {cnsetSeenTag, state.seen}, {cnsetSeenFAITag, state.seenFAI}, {cnsetReadTag, state.read}} {
		raw, err := item.set.marshal()
		if err != nil {
			return err
		}
		f.variable(item.tag, raw)
	}
	f.word(0x403b0003)
	return f.err
}
