// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"context"
	"encoding/binary"
	"errors"
	"slices"
	"strings"
	"sync"
	"unicode/utf16"

	"ewsgo/internal/core"
)

const maxNamedMappings = 4096

// MS-OXCDATA 2.6.1. Set is a FlatUID in wire byte order; names never enter logs.
type propertyName struct {
	Set  [16]byte
	Kind byte
	LID  uint32
	Name string
}

var psMAPI = [16]byte{0x28, 0x03, 0x02, 0, 0, 0, 0, 0, 0xc0, 0, 0, 0, 0, 0, 0, 0x46}
var psInternetHeaders = [16]byte{0x86, 0x03, 0x02, 0, 0, 0, 0, 0, 0xc0, 0, 0, 0, 0, 0, 0, 0x46}

func parsePropertyName(r *reader) (propertyName, error) {
	var name propertyName
	head := r.take(17)
	if r.err != nil {
		return name, ErrMalformed
	}
	name.Kind = head[0]
	copy(name.Set[:], head[1:])
	switch name.Kind {
	case 0:
		name.LID = r.u32()
	case 1:
		size := r.take(1)
		if len(size) != 1 || size[0] < 2 || size[0]%2 != 0 {
			return propertyName{}, ErrMalformed
		}
		wire := r.take(int(size[0]))
		if r.err != nil || binary.LittleEndian.Uint16(wire[len(wire)-2:]) != 0 {
			return propertyName{}, ErrMalformed
		}
		units := make([]uint16, len(wire)/2-1)
		for i := range units {
			units[i] = binary.LittleEndian.Uint16(wire[2*i:])
			if units[i] == 0 {
				return propertyName{}, ErrMalformed
			}
		}
		for i := 0; i < len(units); i++ {
			if utf16.IsSurrogate(rune(units[i])) {
				if units[i] < 0xd800 || units[i] > 0xdbff || i+1 == len(units) || units[i+1] < 0xdc00 || units[i+1] > 0xdfff {
					return propertyName{}, ErrMalformed
				}
				i++
			}
		}
		name.Name = string(utf16.Decode(units))
	case 0xff: // No associated name: cannot be registered or resolved.
	default:
		return propertyName{}, ErrMalformed
	}
	return name, r.err
}

// NamedPropertyStorage is scoped to one authenticated mailbox in the core.
// Register must allocate or resolve the entire batch atomically within lastID.
type NamedPropertyStorage interface {
	Load(context.Context, int) (map[uint16]core.Key, error)
	Register(context.Context, []core.Key, uint16) ([]uint16, error)
}

// The registry shares both directions of the mapping across mailbox contexts.
// With persistent storage, registration commits before publishing the IDs.
type namedPropertyRegistry struct {
	mu      sync.Mutex
	backing NamedPropertyStorage
	byKey   map[propertyName]uint16
	names   map[uint16]propertyName
}

func (s *namedPropertyRegistry) load(ctx context.Context, backing NamedPropertyStorage) error {
	bindings, err := backing.Load(ctx, maxNamedMappings)
	if err != nil {
		return err
	}
	s.backing = backing
	s.byKey = make(map[propertyName]uint16, len(bindings))
	s.names = make(map[uint16]propertyName, len(bindings))
	for id, key := range bindings {
		name, err := corePropertyName(key)
		if err != nil || name.Set == psMAPI || len(utf16.Encode([]rune(name.Name))) > 126 ||
			id < 0x8001 || id > 0x8000+maxNamedMappings || s.byKey[name] != 0 {
			return core.ErrConflict
		}
		s.publish(name, id)
	}
	return nil
}

// The caller holds mu. Only IDs assigned by this registry appear in names.
func (s *namedPropertyRegistry) registeredName(id uint16) (propertyName, bool) {
	name, found := s.names[id]
	return name, found
}

func (s *namedPropertyRegistry) ids(ctx context.Context, names []propertyName, create bool) ([]uint16, uint32) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(names) == 0 {
		ids := make([]uint16, 0, len(s.names))
		for id := range s.names {
			ids = append(ids, id)
		}
		slices.Sort(ids)
		return ids, 0
	}
	keys := make([]propertyName, len(names))
	missing := map[propertyName]bool{}
	var newNames []propertyName
	for i, name := range names {
		if name.Set == psInternetHeaders && name.Kind == 1 {
			name.Name = strings.ToLower(name.Name)
		}
		keys[i] = name
		if create && name.Set != psMAPI && name.Kind != 0xff && s.byKey[name] == 0 && !missing[name] {
			missing[name] = true
			newNames = append(newNames, name)
		}
	}
	if len(s.names)+len(missing) > maxNamedMappings {
		return nil, 0x8007000e
	}
	if len(newNames) != 0 {
		if err := s.register(ctx, newNames); err != nil {
			if errors.Is(err, core.ErrUnsupported) {
				return nil, 0x8007000e
			}
			return nil, storeError(err)
		}
	}
	ids := make([]uint16, len(keys))
	for i, key := range keys {
		if key.Set == psMAPI {
			// Numeric PS_MAPI IDs are direct, not registrations. An LID
			// that cannot fit in the 16-bit response is not mapped.
			if key.Kind == 0 && key.LID <= 0xffff {
				ids[i] = uint16(key.LID)
			}
			continue
		}
		if key.Kind == 0xff {
			continue
		}
		ids[i] = s.byKey[key]
	}
	return ids, 0
}

// The caller holds mu. Validate the complete response before updating the cache.
func (s *namedPropertyRegistry) register(ctx context.Context, names []propertyName) error {
	if s.byKey == nil {
		s.byKey = make(map[propertyName]uint16)
		s.names = make(map[uint16]propertyName)
	}
	ids := make([]uint16, len(names))
	if s.backing == nil {
		for i := range ids {
			ids[i] = uint16(0x8001 + len(s.names) + i)
		}
	} else {
		keys := make([]core.Key, len(names))
		for i, name := range names {
			keys[i] = name.coreKey()
		}
		var err error
		ids, err = s.backing.Register(ctx, keys, 0x8000+maxNamedMappings)
		if err != nil {
			return err
		}
		if len(ids) != len(names) {
			return core.ErrConflict
		}
		seen := make(map[uint16]bool, len(ids))
		for _, id := range ids {
			_, exists := s.names[id]
			if id < 0x8001 || id > 0x8000+maxNamedMappings || seen[id] || exists {
				return core.ErrConflict
			}
			seen[id] = true
		}
	}
	for i, id := range ids {
		s.publish(names[i], id)
	}
	return nil
}

func (s *namedPropertyRegistry) publish(name propertyName, id uint16) {
	s.byKey[name] = id
	s.names[id] = name
}

func (p propertyName) coreKey() core.Key {
	raw := p.Set
	standard := []byte{raw[3], raw[2], raw[1], raw[0], raw[5], raw[4], raw[7], raw[6], raw[8], raw[9], raw[10], raw[11], raw[12], raw[13], raw[14], raw[15]}
	key := core.Key{Set: formatUUID(standard)}
	if p.Kind == 0 {
		key.ID = p.LID
	} else {
		key.Name = p.Name
		key.EmptyName = p.Name == ""
	}
	return key
}
