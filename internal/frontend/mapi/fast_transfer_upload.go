// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"

	"ewsgo/internal/core"
)

// Upload belongs to one writable FAI message. Releasing the transfer handle
// ends input, while releasing the message itself discards all staged data.
// Fields are protected by the owning draft's mutex.
type faiUpload struct {
	draft    *faiDraft
	lease    *contentsLease
	data     []byte
	parsed   int
	values   faiSetRequest
	seen     map[uint16]bool
	released bool
	failed   bool
}

func (*faiUpload) String() string   { return "faiUpload{redacted}" }
func (*faiUpload) GoString() string { return "faiUpload{redacted}" }

func executeFastTransferDestination(ctx context.Context, session core.Session, objects *mailboxObjects, op Operation, handles []uint32, enabled bool) []byte {
	object, code := objects.lookup(handles[op.Input], op.Logon)
	if op.ID == ropFastTransferDestinationPutBuffer {
		var used uint16
		if code == 0 {
			if !enabled || object.download == nil {
				code = NotSupported
			} else if object.download.mailUpload != nil {
				code = faiError(object.download.mailUpload.put(ctx, op.TransferData), false)
			} else if object.download.faiUpload != nil {
				code = faiError(object.download.faiUpload.put(ctx, op.TransferData), false)
			} else {
				code = NotSupported
			}
			if code == 0 {
				used = uint16(len(op.TransferData))
			}
		}
		// Match the reference response, including counters on error.
		return binary.LittleEndian.AppendUint16(append(ropError(op.ID, op.Input, code), 0, 0, 0, 0, 1, 0, 0), used)
	}
	response := func(code uint32) []byte { return ropError(op.ID, op.Output, code) }
	if code != 0 {
		return response(code)
	}
	if op.TransferSourceOperation < 1 || op.TransferSourceOperation > 4 || op.Flags & ^byte(1) != 0 {
		return response(0x80070057)
	}
	if object.download != nil && object.download.mailImport != nil {
		if !enabled || op.Flags != 0 || op.TransferSourceOperation > 2 {
			return response(NotSupported)
		}
		handle, err := object.download.mailImport.configureUpload(ctx, session, objects, object)
		if err != nil {
			return response(faiError(err, false))
		}
		handles[op.Output] = handle
		return response(0)
	}
	if object.isLogon || object.table != nil || object.stream != nil || object.notification != nil ||
		object.download != nil && object.download.draft == nil ||
		op.TransferSourceOperation > 2 && (object.message != nil || object.attachment != nil || object.download != nil) {
		return response(NotSupported)
	}
	var draft *faiDraft
	if object.message != nil {
		draft = object.message.draft
	} else if object.download != nil {
		draft = object.download.draft
	}
	if !enabled || draft == nil || draft.privateConfig || op.Flags != 0 {
		return response(ropNotImplemented)
	}
	handle, err := draft.configureUpload(ctx, session, objects, object)
	if err != nil {
		return response(faiError(err, false))
	}
	handles[op.Output] = handle
	return response(0)
}

func (d *faiDraft) configureUpload(ctx context.Context, session core.Session, objects *mailboxObjects, object serverObject) (uint32, error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.closed || d.readOnly {
		return 0, core.ErrAccessDenied
	}
	if d.upload != nil {
		return 0, core.ErrConflict
	}
	if !objects.hasDownloadCapacity() {
		return 0, core.ErrUnsupported
	}
	if _, err := d.store.messageAnchor(ctx, session, d.root, true); err != nil {
		return 0, err
	}
	lease, err := d.store.reserveContentsMemory(2 * maxFAIObjectBytes)
	if err != nil {
		return 0, err
	}
	upload := &faiUpload{draft: d, lease: lease, data: make([]byte, 0, maxFAIObjectBytes), seen: make(map[uint16]bool)}
	child := serverObject{logon: object.logon, binding: object.binding, fid: object.fid}
	handle, err := objects.addDownload(child, &hierarchyDownload{faiUpload: upload})
	if err != nil {
		lease.release()
		return 0, err
	}
	d.upload = upload
	return handle, nil
}

func (u *faiUpload) releaseHandle() {
	if u == nil {
		return
	}
	u.draft.mu.Lock()
	defer u.draft.mu.Unlock()
	u.released = true
}

func (d *faiDraft) clearUpload() {
	if u := d.upload; u != nil {
		u.lease.release()
		u.data, u.values, u.seen = nil, nil, nil
		d.upload = nil
	}
}

func (d *faiDraft) stageUpload(ctx context.Context, session core.Session) error {
	u := d.upload
	if u == nil {
		return nil
	}
	if u.failed || u.parsed != len(u.data) {
		return core.ErrInvalid
	}
	if len(u.values) != 0 {
		if _, err := d.setLocked(ctx, session, u.values, true); err != nil {
			return err
		}
	}
	d.clearUpload()
	return nil
}

func (u *faiUpload) put(ctx context.Context, data []byte) error {
	d := u.draft
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.upload != u || d.closed || u.released || u.failed {
		return core.ErrInvalid
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if len(data) > cap(u.data)-len(u.data) {
		u.failed = true
		return core.ErrUnsupported
	}
	u.data = append(u.data, data...)
	for u.parsed < len(u.data) {
		r := reader{data: u.data[u.parsed:]}
		prop, err := readFAITransferProperty(ctx, &r, &d.store.named)
		if r.err != nil {
			return nil // The next buffer may complete this property.
		}
		if err != nil {
			u.failed = true
			return err
		}
		u.parsed += r.pos
		if prop.tag == 0x40160003 {
			// Recipients and attachments are already empty for this FAI profile.
			continue
		}
		id := uint16(prop.tag >> 16)
		if u.seen[id] || len(u.seen) == 64 {
			u.failed = true
			return core.ErrInvalid
		}
		u.seen[id] = true
		u.values = append(u.values, prop)
	}
	return nil
}

func readFAITransferProperty(ctx context.Context, r *reader, registry *namedPropertyRegistry) (faiWireProperty, error) {
	tag := r.u32()
	if r.err != nil {
		return faiWireProperty{}, core.ErrInvalid
	}
	if tag == 0x40160003 { // MetaTagFXDelProp
		target := r.u32()
		if target != 0x0e12000d && target != 0x0e13000d {
			return faiWireProperty{}, core.ErrUnsupported
		}
		return faiWireProperty{tag: tag}, nil
	}
	if !faiSetTag(tag) {
		return faiWireProperty{}, core.ErrUnsupported
	}
	var name propertyName
	var err error
	if tag>>16 >= 0x8000 {
		name, err = readTransferPropertyName(r)
		if err != nil {
			return faiWireProperty{}, err
		}
	}
	value, err := readFAITransferValue(r, uint16(tag))
	if err != nil {
		return faiWireProperty{}, err
	}
	if name != (propertyName{}) {
		ids, code := registry.ids(ctx, []propertyName{name}, false)
		if code != 0 || ids[0] < 0x8001 {
			return faiWireProperty{}, core.ErrUnsupported
		}
		tag = uint32(ids[0])<<16 | uint32(uint16(tag))
	} else if tag>>16 >= 0x8000 {
		return faiWireProperty{}, core.ErrInvalid
	}
	if tag == 0x67aa000b && value != core.Bool(true) ||
		tag == 0x001a001f && !validMessageClass(string(value.(core.Text))) {
		return faiWireProperty{}, core.ErrUnsupported
	}
	return faiWireProperty{tag, value}, nil
}

func readTransferPropertyName(r *reader) (propertyName, error) {
	var name propertyName
	descriptor := r.take(17)
	if r.err != nil {
		return name, core.ErrInvalid
	}
	copy(name.Set[:], descriptor[:16])
	name.Kind = descriptor[16]
	switch name.Kind {
	case 0:
		name.LID = r.u32()
		if r.err != nil {
			return name, core.ErrInvalid
		}
	case 1:
		var err error
		name.Name, err = createFolderString(r, 126)
		if err != nil || name.Name == "" {
			return name, core.ErrInvalid
		}
	default:
		return name, core.ErrInvalid
	}
	return name, nil
}

func readFAITransferValue(r *reader, kind uint16) (core.Value, error) {
	switch kind {
	case 0xb:
		raw := r.take(2)
		if r.err != nil {
			return nil, core.ErrInvalid
		}
		return core.Bool(binary.LittleEndian.Uint16(raw) != 0), nil
	case 0x1f, 0x102:
		size := r.u32()
		if size > maxFAIObjectBytes {
			return nil, core.ErrUnsupported
		}
		raw := r.take(int(size))
		if r.err != nil {
			return nil, core.ErrInvalid
		}
		if kind == 0x102 {
			return core.Binary(raw), nil // The owner keeps the input buffer until staging.
		}
		s := reader{data: raw}
		value, err := createFolderString(&s, maxContentsRestrictionBytes/2)
		if err != nil || s.done() != nil {
			return nil, core.ErrInvalid
		}
		return core.Text(value), nil
	default:
		value, err := readContentsValue(r, kind)
		if err != nil {
			return nil, core.ErrInvalid
		}
		return value, nil
	}
}
