// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package core

import (
	"context"
	"time"
)

const (
	MaxAvailabilityMailboxes = 100
	MaxAvailabilityWindow    = 62 * 24 * time.Hour
)

type AvailabilityState uint8

const (
	AvailabilityFree AvailabilityState = iota
	AvailabilityTentative
	AvailabilityBusy
	AvailabilityOutOfOffice
	AvailabilityNoData
)

type AvailabilityInterval struct {
	Start, End time.Time
	State      AvailabilityState
}

// AvailabilityReader queries with the authenticated user's rights. Results
// preserve address order. Intervals for each address are ordered, disjoint,
// and cover the query window, including explicit intervals of unknown status.
type AvailabilityReader interface {
	ReadAvailability(ctx context.Context, addresses []string, start, end time.Time) ([][]AvailabilityInterval, error)
}
