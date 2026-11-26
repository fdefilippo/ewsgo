// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"

	"ewsgo/internal/core"
)

// The caller has already verified the wire size and bounded the count.
func decodeDistinctMessageIDs(wire []byte) ([]uint64, error) {
	count := len(wire) / 8
	seen := make(map[uint64]bool, count)
	mids := make([]uint64, 0, count)
	for len(wire) != 0 {
		mid := binary.LittleEndian.Uint64(wire)
		if seen[mid] {
			return nil, ErrMalformed
		}
		seen[mid] = true
		mids = append(mids, mid)
		wire = wire[8:]
	}
	return mids, nil
}

func executeMessageBatch(ctx context.Context, mids []uint64, apply func(uint64) error) (code uint32, partial bool) {
	var failed uint32
	var completed bool
	for _, mid := range mids {
		if err := ctx.Err(); err != nil {
			return storeError(err), true
		}
		err := apply(mid)
		if errors.Is(err, core.ErrOutcomeUnknown) {
			// Reconcile the native state before attempting another mutation.
			return storeError(err), true
		}
		if err == nil {
			completed = true
		} else if failed == 0 {
			failed = storeError(err)
		}
	}
	if completed {
		return 0, failed != 0
	}
	return failed, failed != 0
}
