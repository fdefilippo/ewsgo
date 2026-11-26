// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import "encoding/binary"

const maxTableColumns = 64
const tableSoftDeletes = 0x20

// A synchronous Table profile. Invalid flags and unrecognized column types
// are rejected while decoding the entire batch.
type tableRequest struct {
	flags, direction, origin byte
	count                    uint16
	seek                     int32
	wantMoved                bool
	numerator, denominator   uint32
	columns                  []uint32
	sort                     *tableSortRequest
	restriction              *contentsRestriction
	bookmark                 []byte
	rowID                    uint64
	instance                 uint32
	restrictionID            [32]byte
	limit                    int // Execution-only response allowance, assigned before effects.
	rejection                tableProfileRejection
	bindingRejected          bool // Set only at the failed execution-time binding.
}

func (*tableRequest) String() string   { return "tableRequest{private}" }
func (*tableRequest) GoString() string { return "tableRequest{private}" }

func parseTableOperation(r *reader, op *Operation, handles int) error {
	q := &tableRequest{}
	op.Table = q
	switch op.ID {
	case 0x14, 0x4f:
		return parseTableRestriction(r, op.ID, q)
	case 0x13:
		var err error
		q.sort, err = parseTableSort(r)
		return err
	case 0x04, 0x05, 0x21, 0x3e, 0x3f:
		h := r.take(2)
		if r.err != nil {
			return r.err
		}
		op.Output, q.flags = h[0], h[1]
		if int(op.Output) >= handles {
			return ErrMalformed
		}
		if op.ID == 0x3f {
			// The request size is known regardless of TableFlags. Validate
			// those bits on Execute so a rules error is local to this ROP.
			break
		}
		if op.ID == 0x3e {
			if q.flags & ^byte(2) != 0 {
				return ErrUnsupported
			}
			break
		}
		// NoNotifications selects a detached snapshot. Without it, the view is
		// refreshed and TableChanged notifications are delivered on Execute.
		// UseUnicode controls unspecified string
		// columns; explicit String8 and Unicode columns retain their types.
		allowed := byte(0x52)
		switch op.ID {
		case 0x05:
			allowed |= tableSoftDeletes
			if q.flags&0x02 != 0 && q.flags&tableSoftDeletes != 0 {
				return ErrUnsupported
			}
		case 0x21:
			allowed = 0x40
		case 0x04:
			allowed = 0x54 | tableSoftDeletes | tableSuppressNotifications
		}
		if q.flags & ^allowed != 0 {
			return ErrUnsupported
		}
	case 0x12:
		h := r.take(3)
		if r.err != nil {
			return r.err
		}
		q.flags = h[0]
		n := int(binary.LittleEndian.Uint16(h[1:]))
		if n == 0 || n > maxTableColumns {
			q.rejection = tableProfileRejection{reason: tableColumnCount}
			return ErrUnsupported
		}
		wire := r.take(n * 4)
		if r.err != nil {
			return r.err
		}
		if q.flags & ^byte(1) != 0 {
			q.rejection = tableProfileRejection{reason: tableColumnFlags, flags: q.flags}
			return ErrUnsupported
		}
		q.columns = make([]uint32, n)
		for i := range q.columns {
			tag := binary.LittleEndian.Uint32(wire[i*4:])
			switch uint16(tag) {
			case 0x02, 0x03, 0x04, 0x05, 0x06, 0x07, 0x0b, 0x0d, 0x14,
				0x1e, 0x1f, 0x40, 0x48, 0xfb, 0xfd, 0xfe, 0x102,
				0x1002, 0x1003, 0x1004, 0x1005, 0x1006, 0x1007, 0x1014,
				0x101e, 0x101f, 0x1040, 0x1048, 0x1102, 0x301e, 0x301f:
				// MS-OXCDATA property types are valid column descriptors.
				// Missing values are reported by the row encoder per cell.
			default:
				q.rejection = tableProfileRejection{reason: tableColumnType, tag: tag}
				return ErrUnsupported
			}
			q.columns[i] = tag
		}
	case 0x15:
		h := r.take(4)
		if r.err != nil {
			return r.err
		}
		q.flags, q.direction, q.count = h[0], h[1], binary.LittleEndian.Uint16(h[2:])
		if q.direction > 1 {
			return ErrMalformed
		}
		// EnablePackedBuffers allows extra response buffers; MS-OXCRPC
		// 3.1.4.2.1.2.2 permits returning only the initial real row buffer.
		if q.flags & ^byte(3) != 0 {
			q.rejection = tableProfileRejection{reason: tableQueryFlags, flags: q.flags}
			return ErrUnsupported
		}
		if q.count == 0 {
			q.rejection = tableProfileRejection{reason: tableQueryCount, flags: q.flags}
			return ErrUnsupported
		}
	case 0x18:
		h := r.take(6)
		if r.err != nil {
			return r.err
		}
		q.origin, q.seek = h[0], int32(binary.LittleEndian.Uint32(h[1:]))
		if h[5] > 1 {
			return ErrMalformed
		}
		if q.origin > 2 {
			return ErrUnsupported
		}
	case 0x19, 0x89:
		size := r.take(2)
		if r.err != nil {
			return r.err
		}
		q.bookmark = r.take(int(binary.LittleEndian.Uint16(size)))
		if op.ID == 0x19 {
			h := r.take(5)
			if r.err != nil {
				return r.err
			}
			if h[4] > 1 {
				return ErrMalformed
			}
			q.seek, q.wantMoved = int32(binary.LittleEndian.Uint32(h)), h[4] == 1
		}
	case 0x1a:
		h := r.take(8)
		if r.err != nil {
			return r.err
		}
		q.numerator, q.denominator = binary.LittleEndian.Uint32(h), binary.LittleEndian.Uint32(h[4:])
	case 0x59:
		h := r.take(10)
		if r.err != nil {
			return r.err
		}
		q.count, q.rowID = binary.LittleEndian.Uint16(h), binary.LittleEndian.Uint64(h[2:])
	case 0x5a:
		h := r.take(8)
		if r.err != nil {
			return r.err
		}
		q.rowID = binary.LittleEndian.Uint64(h)
	case 0x6b:
		h := r.take(12)
		if r.err != nil {
			return r.err
		}
		q.rowID, q.instance = binary.LittleEndian.Uint64(h), binary.LittleEndian.Uint32(h[8:])
	case 0x6c:
		size := r.take(2)
		if r.err != nil {
			return r.err
		}
		q.bookmark = r.take(int(binary.LittleEndian.Uint16(size))) // Opaque collapse state.
	}
	return r.err
}
