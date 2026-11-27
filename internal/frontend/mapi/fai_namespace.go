// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/hex"
	"strings"
	"time"

	"ewsgo/internal/core"
)

const maxFAIObjects = 64
const maxFAIObjectBytes = 128 << 10
const maxFAIStoreBytes = 4 << 20

// Initially empty, exclusively owned by EWSGo. No native configuration import,
// collector, staging or operational writer is activated by this prerequisite.
type faiNamespace struct {
	records map[core.ID]faiRecord
	absent  map[[22]byte]uint64 // SourceKey -> folder; deletion before any server object existed.
	bytes   int
	lease   *contentsLease // Retained storage, to be admitted by the future writer.
}
type faiRecord struct {
	sync    *faiSyncVersion
	deleted bool
	folder  uint64
	object  core.Object
	bytes   int
}

func activeFAICount(records map[core.ID]faiRecord) int {
	count := 0
	for _, record := range records {
		if !record.deleted {
			count++
		}
	}
	return count
}

type faiObservation struct {
	snapshot messageSnapshot
	lease    *contentsLease // Detached result buffers; caller must close.
}

func (faiNamespace) String() string      { return "faiNamespace{private}" }
func (faiNamespace) GoString() string    { return "faiNamespace{private}" }
func (faiRecord) String() string         { return "faiRecord{private}" }
func (faiRecord) GoString() string       { return "faiRecord{private}" }
func (*faiObservation) String() string   { return "faiObservation{private}" }
func (*faiObservation) GoString() string { return "faiObservation{private}" }
func (o *faiObservation) close() {
	if o != nil {
		o.snapshot = messageSnapshot{}
		o.lease.release()
		o.lease = nil
	}
}

func (s *ramStore) observeFAIMessages(ctx context.Context, session core.Session, anchor uint64) (*faiObservation, error) {
	var lease *contentsLease
	snapshot, err := s.observeMessageSet(ctx, session, anchor, true, func(_ context.Context, _ core.Mailbox, key folderKey) ([]core.Object, error) {
		historyBytes, err := s.validateFAIHistory(s.fai.records, s.fai.absent)
		if err != nil {
			return nil, err
		}
		selected, activeBytes := 0, 0
		seenSources := map[[22]byte]bool{}
		var objects []core.Object
		for id, record := range s.fai.records {
			if record.object.ID != id {
				return nil, core.ErrInvalid
			}
			if record.sync != nil {
				if seenSources[record.sync.source] {
					return nil, core.ErrInvalid
				}
				seenSources[record.sync.source] = true
			}
			bytes, err := s.validateFAIRecord(record)
			if err != nil {
				return nil, err
			}
			if bytes != record.bytes {
				return nil, core.ErrInvalid
			}
			if !record.deleted {
				activeBytes += bytes
			}
			if record.folder == anchor && !record.deleted {
				if record.object.Parent != key.native {
					return nil, core.ErrInvalid
				}
				selected += bytes
				objects = append(objects, record.object) // Immutable under contentsMu.
			}
		}
		if activeFAICount(s.fai.records) > maxFAIObjects || activeBytes > maxFAIStoreBytes {
			return nil, core.ErrUnsupported
		}
		used := activeBytes + historyBytes
		if used != s.fai.bytes || s.fai.lease.size() != used {
			return nil, core.ErrInvalid
		}
		// Reserve before messageNodes clones any property/value. Store storage
		// and detached result are charged separately; errors release this lease.
		lease, err = s.reserveContentsMemory(selected)
		if err != nil {
			return nil, err
		}
		return objects, nil
	})
	if err != nil {
		lease.release()
		return nil, err
	}
	return &faiObservation{snapshot: snapshot, lease: lease}, nil
}

// Validate the complete stored object; no unknown field is silently removed.
// Streams are stored as opaque bytes, not executed or interpreted as settings.
func (s *ramStore) validateFAIRecord(record faiRecord) (int, error) {
	o := record.object
	if !validFAISync(record.sync) {
		return 0, core.ErrInvalid
	}
	if len(o.ID) != 36 || !strings.HasPrefix(string(o.ID), "fai:") {
		return 0, core.ErrInvalid
	}
	var id [16]byte
	if _, err := hex.Decode(id[:], []byte(o.ID[4:])); err != nil || strings.ToLower(string(o.ID)) != string(o.ID) {
		return 0, core.ErrInvalid
	}
	if record.deleted {
		if record.sync == nil || o.Kind != core.Message || o.Revision == 0 || len(o.Properties) != 0 || len(o.MIME) != 0 || len(o.Recipients) != 0 || len(o.Attachments) != 0 {
			return 0, core.ErrInvalid
		}
		return faiSyncBytes(record.sync) + 1024 + len(o.ID) + len(o.Parent), nil
	}
	if o.Kind != core.Message || o.Revision == 0 || len(o.MIME) != 0 || len(o.Recipients) != 0 || len(o.Attachments) != 0 || len(o.Properties) > 64 {
		return 0, core.ErrUnsupported
	}
	s.mu.Lock()
	key, known := s.byFID[record.folder]
	s.mu.Unlock()
	if !known || o.Parent != key.native {
		return 0, core.ErrInvalid
	}
	class, ok := o.Properties[core.Key{ID: 0x1a}].(core.Text)
	if !ok {
		return 0, core.ErrInvalid
	}
	if !validMessageClass(string(class)) {
		return 0, core.ErrUnsupported
	}
	if o.Properties[core.Key{ID: 0x67aa}] != core.Bool(true) {
		return 0, core.ErrInvalid
	}
	if err := validateFAISubject(o.Properties); err != nil {
		return 0, err
	}
	if _, ok := o.Properties[core.IsRead].(core.Bool); !ok {
		return 0, core.ErrInvalid
	}
	created, cok := o.Properties[core.Key{ID: 0x3007}].(core.Timestamp)
	modified, mok := o.Properties[core.LastModifiedAt].(core.Timestamp)
	if !cok || !mok || time.Time(modified).Before(time.Time(created)) {
		return 0, core.ErrInvalid
	}
	for key, value := range o.Properties {
		p, err := s.faiProperty(key, value)
		if err != nil {
			return 0, err
		}
		if !contentsValueType(p.kind, value) {
			return 0, core.ErrInvalid
		}
		// Configuration streams and named scalar extensions are retained,
		// with no server-side interpretation of XML/view payloads.
	}
	_, bytes, err := messageFingerprint(o)
	if err != nil {
		return 0, err
	}
	bytes += len(o.ID) + len(o.Parent) + 1024 + faiSyncBytes(record.sync)
	if bytes > maxFAIObjectBytes {
		return 0, core.ErrUnsupported
	}
	return bytes, nil
}

func (s *ramStore) faiProperty(key core.Key, value core.Value) (contentsProperty, error) {
	if !key.Valid() {
		return contentsProperty{}, core.ErrInvalid
	}
	if key.Set == "" {
		if kind := associatedMetadataPropertyType(key.ID); kind != 0 {
			return contentsProperty{id: uint16(key.ID), kind: kind}, nil
		}
		var kind uint16
		switch key.ID {
		case 0x1a:
			class, ok := value.(core.Text)
			if !ok || !validMessageClass(string(class)) {
				return contentsProperty{}, core.ErrUnsupported
			}
			kind = 0x1f
		case 0x37, 0x3d, 0xe1d, 0x3001, 0x65ec, 0x7002, 0x7006:
			kind = 0x1f
		case 0xe69, 0xe1f, 0x67aa:
			kind = 0xb
		case 0x3007, 0x3008:
			kind = 0x40
		case rwRulesStreamTag >> 16, 0x7001, 0x7c07, 0x7c08:
			kind = 0x102
		case 0x17, 0x36, 0x7007, 0x7c06:
			kind = 3
		default:
			return contentsProperty{}, core.ErrUnsupported
		}
		return contentsProperty{id: uint16(key.ID), kind: kind}, nil
	}
	name, err := faiNamedProperty(key)
	if err != nil {
		return contentsProperty{}, err
	}
	s.named.mu.Lock()
	registered := s.named.byKey[name] != 0
	s.named.mu.Unlock()
	if !registered {
		return contentsProperty{}, core.ErrUnsupported
	}
	// No integer-width guessing for arbitrary named values; these four types
	// have an unambiguous core-to-wire representation in the initial slice.
	var kind uint16
	switch value.(type) {
	case core.Text:
		kind = 0x1f
	case core.Binary:
		kind = 0x102
	case core.Bool:
		kind = 0xb
	case core.Timestamp:
		kind = 0x40
	default:
		return contentsProperty{}, core.ErrUnsupported
	}
	return contentsProperty{kind: kind, name: name}, nil
}

// MS-OXOCFG 2.2.9 (links) and MS-OXOSRCH 2.2.1.2 (search definitions).
// Binary values are opaque configuration; they do not grant access or
// create a search folder.
func associatedMetadataPropertyType(id uint32) uint16 {
	switch id {
	case 0x6842, 0x6844, 0x6845, 0x684b, 0x684c, 0x684d, 0x684e, 0x684f, 0x6850, 0x6854, 0x6891:
		return 0x0102
	case 0x6834, 0x683a, 0x6841, 0x6846, 0x6847, 0x6848, 0x6849, 0x684a, 0x6852, 0x6853:
		return 0x0003
	case 0x6851:
		return 0x001f
	default:
		return 0
	}
}

func faiNamedProperty(key core.Key) (propertyName, error) {
	name, err := corePropertyName(key)
	if err != nil {
		return propertyName{}, err
	}
	if name.Set == psMAPI || name.Set == ([16]byte{}) || (name.Set == psInternetHeaders && name.Name != strings.ToLower(name.Name)) {
		return propertyName{}, core.ErrUnsupported
	}
	return name, nil
}

func corePropertyName(key core.Key) (propertyName, error) {
	var raw [16]byte
	if !key.Valid() || key.Set == "" {
		return propertyName{}, core.ErrInvalid
	}
	if _, err := hex.Decode(raw[:], []byte(strings.ReplaceAll(key.Set, "-", ""))); err != nil {
		return propertyName{}, core.ErrInvalid
	}
	name := propertyName{Set: [16]byte{raw[3], raw[2], raw[1], raw[0], raw[5], raw[4], raw[7], raw[6], raw[8], raw[9], raw[10], raw[11], raw[12], raw[13], raw[14], raw[15]}, LID: key.ID, Name: key.Name}
	if key.IsName() {
		name.Kind = 1
	}
	return name, nil
}
