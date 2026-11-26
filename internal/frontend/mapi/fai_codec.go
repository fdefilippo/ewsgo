// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"sort"
	"time"

	"ewsgo/internal/core"
)

const maxFAIPCLReplicas = 64

// Change keys retain the complete opaque XID, without padding or truncation.
// Strings make these small binary identifiers immutable and comparable. They
// are never text and must not be formatted or persisted as public identifiers.
type changeXID string
type xidLocal string
type faiPCL map[[16]byte]xidLocal

func (changeXID) String() string              { return "changeXID{private}" }
func (changeXID) GoString() string            { return "changeXID{private}" }
func (xidLocal) String() string               { return "xidLocal{private}" }
func (xidLocal) GoString() string             { return "xidLocal{private}" }
func gidChangeXID(gid [22]byte) changeXID     { return changeXID(gid[:]) }
func (x changeXID) namespace() (out [16]byte) { copy(out[:], x); return }
func (x changeXID) local() xidLocal           { return xidLocal(x[16:]) }
func (x changeXID) valid() bool {
	return len(x) >= 17 && len(x) <= 24 && x.namespace() != ([16]byte{}) && x.local().valid()
}
func (v xidLocal) valid() bool {
	if len(v) < 1 || len(v) > 8 {
		return false
	}
	for i := range len(v) {
		if v[i] != 0 {
			return true
		}
	}
	return false
}

type faiImportRequest struct {
	output, flags byte
	source        [22]byte
	change        changeXID
	modified      time.Time
	predecessors  faiPCL
}
type faiWireProperty struct {
	tag   uint32
	value core.Value
}
type faiSetRequest []faiWireProperty
type faiDeleteRequest struct {
	flags   byte
	sources [][22]byte
}

func (faiPCL) String() string             { return "faiPCL{private}" }
func (faiPCL) GoString() string           { return "faiPCL{private}" }
func (faiImportRequest) String() string   { return "faiImportRequest{private}" }
func (faiImportRequest) GoString() string { return "faiImportRequest{private}" }
func (faiWireProperty) String() string    { return "faiWireProperty{private}" }
func (faiWireProperty) GoString() string  { return "faiWireProperty{private}" }
func (faiSetRequest) String() string      { return "faiSetRequest{private}" }
func (faiSetRequest) GoString() string    { return "faiSetRequest{private}" }
func (faiDeleteRequest) String() string   { return "faiDeleteRequest{private}" }
func (faiDeleteRequest) GoString() string { return "faiDeleteRequest{private}" }

func validFAIXID(x [22]byte) bool {
	return !bytes.Equal(x[:16], make([]byte, 16)) && !bytes.Equal(x[16:], make([]byte, 6))
}
func parseFAIPCL(raw []byte) (faiPCL, error) {
	if len(raw) > 25*maxFAIPCLReplicas {
		return nil, ErrUnsupported
	}
	if len(raw) == 0 {
		return nil, ErrMalformed
	}
	p := faiPCL{}
	for len(raw) > 0 {
		size := int(raw[0])
		if size < 17 || size > 24 {
			return nil, ErrUnsupported
		}
		if len(raw) < size+1 {
			return nil, ErrMalformed
		}
		x := changeXID(raw[1 : size+1])
		if !x.valid() {
			return nil, ErrMalformed
		}
		ns, local := x.namespace(), x.local()
		if previous, exists := p[ns]; exists {
			if len(previous) != len(local) {
				return nil, ErrMalformed
			}
			if local > previous {
				p[ns] = local
			}
		} else {
			p[ns] = local
		}
		if len(p) > maxFAIPCLReplicas {
			return nil, ErrUnsupported
		}
		raw = raw[size+1:]
	}
	return p, nil
}
func (p faiPCL) includes(other faiPCL) bool {
	for ns, counter := range other {
		v, ok := p[ns]
		if !ok || len(v) != len(counter) || v < counter {
			return false
		}
	}
	return true
}
func (p faiPCL) includesXID(x changeXID) bool {
	return x.valid() && p.includes(faiPCL{x.namespace(): x.local()})
}
func mergeFAIPCL(a, b faiPCL) (faiPCL, error) {
	out := faiPCL{}
	for _, p := range []faiPCL{a, b} {
		for ns, v := range p {
			if ns == ([16]byte{}) || !v.valid() {
				return nil, core.ErrInvalid
			}
			old, ok := out[ns]
			if ok && len(old) != len(v) {
				return nil, core.ErrInvalid
			}
			if !ok || v > old {
				out[ns] = v
			}
		}
	}
	if len(out) > maxFAIPCLReplicas {
		return nil, core.ErrUnsupported
	}
	return out, nil
}
func (p faiPCL) marshal() []byte {
	sets := make([][16]byte, 0, len(p))
	for ns := range p {
		sets = append(sets, ns)
	}
	sort.Slice(sets, func(i, j int) bool { return bytes.Compare(sets[i][:], sets[j][:]) < 0 })
	var raw []byte
	for _, ns := range sets {
		v := p[ns]
		raw = append(raw, byte(16+len(v)))
		raw = append(raw, ns[:]...)
		raw = append(raw, v...)
	}
	return raw
}
func parseFAIImport(r *reader, handles int) (faiImportRequest, error) {
	var out faiImportRequest
	h := r.take(4)
	if r.err != nil || int(h[0]) >= handles {
		return out, ErrMalformed
	}
	out.output, out.flags = h[0], h[1]
	if out.flags & ^byte(0x50) != 0 {
		return faiImportRequest{}, importUnknownFlags
	}
	if binary.LittleEndian.Uint16(h[2:]) != 4 {
		return faiImportRequest{}, importPropertyCount
	}
	tags := []uint32{0x65e00102, 0x30080040, 0x65e20102, 0x65e30102}
	for i, tag := range tags {
		if r.u32() != tag {
			return faiImportRequest{}, ErrMalformed
		}
		value, err := readContentsValue(r, uint16(tag))
		if err != nil {
			return faiImportRequest{}, err
		}
		switch i {
		case 0:
			raw := value.(core.Binary)
			if len(raw) != 22 {
				return faiImportRequest{}, importXIDProfile
			}
			copy(out.source[:], raw)
			if !validFAIXID(out.source) {
				return faiImportRequest{}, ErrMalformed
			}
		case 2:
			raw := value.(core.Binary)
			if len(raw) < 17 || len(raw) > 24 {
				return faiImportRequest{}, importXIDProfile
			}
			out.change = changeXID(raw)
			if !out.change.valid() {
				return faiImportRequest{}, ErrMalformed
			}
		case 1:
			out.modified = time.Time(value.(core.Timestamp))
		case 3:
			out.predecessors, err = parseFAIPCL(value.(core.Binary))
			if err != nil {
				if errors.Is(err, ErrUnsupported) {
					return faiImportRequest{}, importPCLProfile
				}
				return faiImportRequest{}, err
			}
		}
	}
	if !validFAISync(&faiSyncVersion{source: out.source, change: out.change, predecessors: out.predecessors}) {
		return faiImportRequest{}, ErrMalformed
	}
	return out, r.err
}
func faiSetTag(tag uint32) bool {
	id, kind := uint16(tag>>16), uint16(tag)
	if id >= 0x8000 {
		return kind == 0x1f || kind == 0x102 || kind == 0xb || kind == 0x40
	}
	switch tag {
	case 0x003d001f, 0x0e1d001f:
		return true
	case 0x00170003, 0x00360003, 0x0e1f000b,
		0x0e070003, 0x30080040, 0x300b0102, 0x3ffa001f, 0x685d0003, 0x36e20003:
		// Recognized on the wire; server-owned and unsupported fields are
		// reported individually by the draft, not made writable by the parser.
		return true
	case 0x001a001f, 0x0037001f, 0x3001001f, 0x65ec001f, rwRulesStreamTag, 0x7002001f, 0x7006001f, 0x0e69000b, 0x67aa000b, 0x30070040, 0x70010102, 0x7c070102, 0x7c080102, 0x70070003, 0x7c060003:
		return true
	}
	return kind != 0 && kind == associatedMetadataPropertyType(uint32(id))
}
func storeSetTag(tag uint32) bool {
	return faiSetTag(tag) || mailDraftSetTag(tag) || draftAttachmentTag(tag) ||
		tag == delegatorWantsCopyTag || tag == delegatorWantsInfoTag
}

func parseFAISet(r *reader) (faiSetRequest, error) {
	return parseAssociatedMessageSet(r, false)
}

func parseAssociatedMessageSet(r *reader, allowUnsupportedProperties bool) (faiSetRequest, error) {
	h := r.take(4)
	if r.err != nil {
		return nil, ErrMalformed
	}
	size, count := int(binary.LittleEndian.Uint16(h)), int(binary.LittleEndian.Uint16(h[2:]))
	if size < 2 {
		return nil, ErrMalformed
	}
	raw := r.take(size - 2)
	if r.err != nil {
		return nil, ErrMalformed
	}
	if count < 1 || count > 64 {
		return nil, ErrUnsupported
	}
	list := reader{data: raw}
	out := make(faiSetRequest, 0, count)
	seen := map[uint16]bool{}
	for range count {
		tag := list.u32()
		if list.err != nil {
			return nil, ErrMalformed
		}
		if seen[uint16(tag>>16)] {
			return nil, ErrMalformed
		}
		seen[uint16(tag>>16)] = true
		if !storeSetTag(tag) {
			if !allowUnsupportedProperties {
				return nil, ErrUnsupported
			}
			// Preserve the property index for the response without staging
			// values that are outside the associated-message storage schema.
			if _, err := readMailDraftSetValue(&list, tag); err != nil {
				return nil, err
			}
			out = append(out, faiWireProperty{tag: tag})
			continue
		}
		value, err := readContentsValue(&list, uint16(tag))
		if err != nil {
			return nil, err
		}
		if tag == 0x67aa000b && value != core.Bool(true) {
			return nil, ErrUnsupported
		}
		if tag == 0x001a001f && !validMessageClass(string(value.(core.Text))) {
			return nil, ErrUnsupported
		}
		out = append(out, faiWireProperty{tag, value})
	}
	if list.done() != nil {
		return nil, ErrMalformed
	}
	return out, nil
}
func parseFAIDeletes(r *reader) (faiDeleteRequest, error) {
	h := r.take(3)
	if r.err != nil {
		return faiDeleteRequest{}, ErrMalformed
	}
	out := faiDeleteRequest{flags: h[0]}
	if out.flags & ^byte(3) != 0 {
		return faiDeleteRequest{}, importUnknownFlags
	}
	if out.flags&1 != 0 {
		return faiDeleteRequest{}, importDeleteHierarchyUnsupported
	}
	if binary.LittleEndian.Uint16(h[1:]) != 1 {
		return faiDeleteRequest{}, importDeletePropertyProfile
	}
	tag := r.u32()
	if r.err != nil {
		return faiDeleteRequest{}, ErrMalformed
	}
	if tag != 0x00001102 {
		return faiDeleteRequest{}, importDeletePropertyProfile
	}
	count := r.u32()
	if r.err != nil {
		return faiDeleteRequest{}, ErrMalformed
	}
	if count < 1 || count > maxFAIObjects {
		return faiDeleteRequest{}, importDeleteCountProfile
	}
	seen := map[[22]byte]bool{}
	for range count {
		h := r.take(2)
		if r.err != nil {
			return faiDeleteRequest{}, ErrMalformed
		}
		if binary.LittleEndian.Uint16(h) != 22 {
			return faiDeleteRequest{}, importDeleteGIDProfile
		}
		raw := r.take(22)
		if r.err != nil {
			return faiDeleteRequest{}, ErrMalformed
		}
		var source [22]byte
		copy(source[:], raw)
		if !validFAIXID(source) || seen[source] {
			return faiDeleteRequest{}, ErrMalformed
		}
		seen[source] = true
		out.sources = append(out.sources, source)
	}
	return out, nil
}

func parseCollectorOpen(r *reader, handles int, hierarchy bool) (byte, bool, error) {
	h := r.take(2)
	if r.err != nil || int(h[0]) >= handles {
		return 0, false, ErrMalformed
	}
	if h[1] == 0 && !hierarchy {
		return 0, false, ErrUnsupported
	}
	return h[0], h[1] != 0, nil
}
func parseFAISave(r *reader, handles int) (input, flags byte, err error) {
	h := r.take(2)
	if r.err != nil || int(h[0]) >= handles {
		return 0, 0, ErrMalformed
	}
	var valid bool
	flags, valid = faiSaveMode(h[1])
	if !valid {
		return 0, 0, ErrUnsupported
	}
	return h[0], flags, nil
}
