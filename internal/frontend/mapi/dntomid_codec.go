// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import "bytes"

const MaxDirectoryNames = 256

// DNToMId follows MS-OXCMAPIHTTP 2.2.5.4.1. NameValues are ASCII-z (the
// NameCount paragraph says Unicode, but the field definition specifies ASCII).
type DNToMId struct {
	Names     []string // nil means HasNames == 0; empty non-nil is a present array.
	Auxiliary []byte
}

func ParseDNToMId(data []byte) (DNToMId, error) {
	if len(data) > MaxBody {
		return DNToMId{}, ErrMalformed
	}
	r := reader{data: data}
	r.u32() // Reserved, ignored.
	flag := r.take(1)
	if len(flag) != 1 {
		return DNToMId{}, ErrMalformed
	}
	v := DNToMId{}
	if flag[0] != 0 {
		count := r.u32()
		if count > MaxDirectoryNames {
			return DNToMId{}, ErrMalformed
		}
		v.Names = make([]string, 0, count)
		for i := uint32(0); i < count; i++ {
			end := bytes.IndexByte(r.data[r.pos:], 0)
			if end < 0 || end > 4096 {
				return DNToMId{}, ErrMalformed
			}
			name := r.take(end + 1)
			for _, b := range name[:end] {
				if b < 32 || b > 127 {
					return DNToMId{}, ErrMalformed
				}
			}
			v.Names = append(v.Names, string(name[:end]))
		}
	}
	v.Auxiliary = bytes.Clone(r.blob(MaxAuxiliary))
	return v, r.done()
}

func (v DNToMId) Marshal() ([]byte, error) {
	if len(v.Names) > MaxDirectoryNames || len(v.Auxiliary) > MaxAuxiliary {
		return nil, ErrMalformed
	}
	out := append32(nil, 0)
	if v.Names == nil {
		out = append(out, 0)
	} else {
		out = append32(append(out, 1), uint32(len(v.Names)))
		for _, name := range v.Names {
			if len(name) > 4096 {
				return nil, ErrMalformed
			}
			for _, b := range []byte(name) {
				if b < 32 || b > 127 {
					return nil, ErrMalformed
				}
			}
			out = append(append(out, name...), 0)
		}
	}
	if len(out)+4+len(v.Auxiliary) > MaxBody {
		return nil, ErrMalformed
	}
	return appendBlob(out, v.Auxiliary), nil
}

func DNToMIdResponse(errorCode uint32, ids []uint32) []byte {
	out := append32(append32(nil, 0), errorCode)
	if errorCode != 0 || ids == nil {
		out = append(out, 0)
	} else {
		out = append32(append(out, 1), uint32(len(ids)))
		for _, id := range ids {
			out = append32(out, id)
		}
	}
	return append32(out, 0)
}
