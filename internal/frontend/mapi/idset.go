// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"sort"
)

const (
	maxIDSetBytes    = 64 << 10
	maxIDSetRanges   = 4096
	maxIDSetReplicas = 64
)

type idRange struct{ low, high uint64 }
type replicaSet struct {
	replica [16]byte
	ranges  []idRange
}
type idSet struct{ replicas []replicaSet }

func (s idSet) containsSet(before idSet) bool {
	for _, replica := range before.replicas {
		var ranges []idRange
		for _, current := range s.replicas {
			if current.replica == replica.replica {
				ranges = current.ranges
				break
			}
		}
		at := 0
		for _, wanted := range replica.ranges {
			for at < len(ranges) && ranges[at].high < wanted.low {
				at++
			}
			if at == len(ranges) || ranges[at].low > wanted.low || ranges[at].high < wanted.high {
				return false
			}
		}
	}
	return true
}

func (idSet) String() string   { return "idSet{redacted}" }
func (idSet) GoString() string { return "idSet{redacted}" }

// parseIDSet decodes REPLGUID/GLOBSET pairs. Work and allocation depend on
// encoded bytes/commands and ranges, never on a 48-bit range's cardinality.
func parseIDSet(data []byte) (idSet, error) {
	if len(data) > maxIDSetBytes {
		return idSet{}, ErrUnsupported
	}
	r := reader{data: data}
	result := idSet{}
	total := 0
	for r.pos < len(data) {
		if len(result.replicas) >= maxIDSetReplicas {
			return idSet{}, ErrUnsupported
		}
		raw := r.take(16)
		if r.err != nil {
			return idSet{}, ErrMalformed
		}
		var replica [16]byte
		copy(replica[:], raw)
		if len(result.replicas) > 0 && bytes.Compare(result.replicas[len(result.replicas)-1].replica[:], replica[:]) >= 0 {
			return idSet{}, ErrMalformed
		}
		ranges, err := parseGlobset(&r, &total)
		if err != nil {
			return idSet{}, err
		}
		result.replicas = append(result.replicas, replicaSet{replica: replica, ranges: mergeIDRanges(ranges)})
	}
	return result, nil
}

func parseGlobset(r *reader, total *int) ([]idRange, error) {
	var prefix [6]byte
	var blocks [6]int
	used, depth := 0, 0
	var ranges []idRange
	add := func(low, high uint64) error {
		if low > high {
			return ErrMalformed
		}
		if *total >= maxIDSetRanges {
			return ErrUnsupported
		}
		*total = *total + 1
		ranges = append(ranges, idRange{low, high})
		return nil
	}
	count := func(tail []byte) uint64 {
		var value uint64
		for _, b := range prefix[:used] {
			value = value<<8 | uint64(b)
		}
		for _, b := range tail {
			value = value<<8 | uint64(b)
		}
		return value
	}
	for r.pos < len(r.data) {
		command := r.take(1)[0]
		switch {
		case command == 0:
			// End terminates this replica's set; unused common bytes do not
			// become a value. A final Pop is not required by the format.
			return ranges, nil
		case command >= 1 && command <= 6:
			n := int(command)
			if used+n > 6 {
				return nil, ErrMalformed
			}
			tail := r.take(n)
			if r.err != nil {
				return nil, ErrMalformed
			}
			if used+n == 6 {
				value := count(tail)
				if err := add(value, value); err != nil {
					return nil, err
				}
				// Full singleton Push auto-pops only this last block.
			} else {
				copy(prefix[used:], tail)
				blocks[depth] = n
				depth++
				used += n
			}
		case command == 0x50:
			if depth == 0 {
				return nil, ErrMalformed
			}
			depth--
			used -= blocks[depth]
		case command == 0x52:
			n := 6 - used
			low := r.take(n)
			high := r.take(n)
			if r.err != nil {
				return nil, ErrMalformed
			}
			if err := add(count(low), count(high)); err != nil {
				return nil, err
			}
		case command == 0x42:
			if used != 5 {
				return nil, ErrMalformed
			}
			fields := r.take(2)
			if r.err != nil {
				return nil, ErrMalformed
			}
			base, mask := int(fields[0]), fields[1]
			low, high := base, base
			flush := func() error { return add(count([]byte{byte(low)}), count([]byte{byte(high)})) }
			for bit := 0; bit < 8; bit++ {
				if mask&(1<<bit) == 0 {
					continue
				}
				value := base + bit + 1
				if value > 255 {
					return nil, ErrMalformed
				}
				if value == high+1 {
					high = value
				} else {
					if err := flush(); err != nil {
						return nil, err
					}
					low, high = value, value
				}
			}
			if err := flush(); err != nil {
				return nil, err
			}
		default:
			return nil, ErrMalformed
		}
	}
	return nil, ErrMalformed // Every GLOBSET needs an End command.
}

func mergeIDRanges(input []idRange) []idRange {
	if len(input) == 0 {
		return nil
	}
	ranges := append([]idRange(nil), input...)
	sort.Slice(ranges, func(i, j int) bool {
		if ranges[i].low != ranges[j].low {
			return ranges[i].low < ranges[j].low
		}
		return ranges[i].high < ranges[j].high
	})
	out := ranges[:0]
	for _, r := range ranges {
		if len(out) > 0 && r.low <= out[len(out)-1].high+1 {
			if r.high > out[len(out)-1].high {
				out[len(out)-1].high = r.high
			}
		} else {
			out = append(out, r)
		}
	}
	return out
}

func (set idSet) contains(replica [16]byte, n uint64) bool {
	for _, s := range set.replicas {
		if s.replica != replica {
			continue
		}
		i := sort.Search(len(s.ranges), func(i int) bool { return s.ranges[i].high >= n })
		return i < len(s.ranges) && s.ranges[i].low <= n
	}
	return false
}

// The encoder emits unprefixed Range commands or full singleton Push commands.
// It compresses large consecutive sets without an input-dependent expansion.
func (set idSet) marshal() ([]byte, error) {
	if len(set.replicas) > maxIDSetReplicas {
		return nil, ErrUnsupported
	}
	var out []byte
	total := 0
	for i, s := range set.replicas {
		if i > 0 && bytes.Compare(set.replicas[i-1].replica[:], s.replica[:]) >= 0 {
			return nil, ErrMalformed
		}
		if len(s.ranges) == 0 {
			continue
		}
		out = append(out, s.replica[:]...)
		for j, r := range s.ranges {
			if r.low > r.high || r.high > maxGlobalCount || (j > 0 && r.low <= s.ranges[j-1].high+1) {
				return nil, ErrMalformed
			}
			total++
			if total > maxIDSetRanges {
				return nil, ErrUnsupported
			}
			low, high := globalCount(r.low), globalCount(r.high)
			if r.low == r.high {
				out = append(out, 6)
				out = append(out, low[:]...)
			} else {
				out = append(out, 0x52)
				out = append(out, low[:]...)
				out = append(out, high[:]...)
			}
			if len(out) >= maxIDSetBytes {
				return nil, ErrUnsupported
			}
		}
		out = append(out, 0)
	}
	if len(out) > maxIDSetBytes {
		return nil, ErrUnsupported
	}
	return out, nil
}

func singleReplicaSet(replica [16]byte, ranges []idRange) idSet {
	if len(ranges) == 0 {
		return idSet{}
	}
	return idSet{replicas: []replicaSet{{replica: replica, ranges: mergeIDRanges(ranges)}}}
}
