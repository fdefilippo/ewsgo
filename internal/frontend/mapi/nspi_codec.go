// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

// STAT is the 36-byte table state from MS-OXNSPI 2.2.8. Storing a client's
// requested position does not assert that the referenced table exists.
type STAT struct {
	SortType, ContainerID, CurrentRec                       uint32
	Delta                                                   int32
	NumPos, TotalRecs, CodePage, TemplateLocale, SortLocale uint32
}

type Bind struct {
	Flags     uint32
	State     *STAT
	Auxiliary []byte
}

func ParseBind(data []byte) (Bind, error) {
	if len(data) > MaxBody {
		return Bind{}, ErrMalformed
	}
	r := reader{data: data}
	v := Bind{Flags: r.u32()}
	hasState := r.take(1)
	if len(hasState) != 1 {
		return Bind{}, ErrMalformed
	}
	if hasState[0] != 0 {
		v.State = &STAT{SortType: r.u32(), ContainerID: r.u32(), CurrentRec: r.u32(), Delta: int32(r.u32()), NumPos: r.u32(), TotalRecs: r.u32(), CodePage: r.u32(), TemplateLocale: r.u32(), SortLocale: r.u32()}
	}
	v.Auxiliary = append([]byte(nil), r.blob(MaxAuxiliary)...)
	return v, r.done()
}

func (v Bind) Marshal() ([]byte, error) {
	if len(v.Auxiliary) > MaxAuxiliary {
		return nil, ErrMalformed
	}
	out := append32(nil, v.Flags)
	if v.State == nil {
		out = append(out, 0)
	} else {
		out = append(out, 1)
		for _, value := range []uint32{v.State.SortType, v.State.ContainerID, v.State.CurrentRec, uint32(v.State.Delta), v.State.NumPos, v.State.TotalRecs, v.State.CodePage, v.State.TemplateLocale, v.State.SortLocale} {
			out = append32(out, value)
		}
	}
	return appendBlob(out, v.Auxiliary), nil
}

func BindResponse(errorCode uint32, serverGUID [16]byte) []byte {
	out := append32(append32(nil, 0), errorCode)
	out = append(out, serverGUID[:]...)
	return append32(out, 0)
}

func parseUnbind(data []byte) error {
	if len(data) > MaxBody {
		return ErrMalformed
	}
	r := reader{data: data}
	r.u32() // Reserved, ignored per MS-OXCMAPIHTTP 2.2.5.2.1.
	r.blob(MaxAuxiliary)
	return r.done()
}

// ServerGUID identifies this gateway's NSPI service, not an account or a
// fictitious Zimbra GAL server. GUID integer fields use little-endian wire order.
func (n Namespace) ServerGUID() [16]byte {
	u := n.uuid
	return [16]byte{u[3], u[2], u[1], u[0], u[5], u[4], u[7], u[6], u[8], u[9], u[10], u[11], u[12], u[13], u[14], u[15]}
}
