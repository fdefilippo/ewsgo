// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"encoding/binary"
	"sort"
	"time"
	"unicode/utf16"

	"ewsgo/internal/core"
)

const maxFastTransferBytes = 1 << 20 // Encoded bytes plus 16 bytes per atom.

type fastTransfer struct {
	data            []byte
	atoms           []idRange // Half-open byte spans which cannot be split internally.
	err             error
	string8CodePage uint32 // Zero emits UTF-16 text.
	limit           int    // Zero preserves the hierarchy's existing 1 MiB contract.
}

func (*fastTransfer) String() string   { return "fastTransfer{redacted}" }
func (*fastTransfer) GoString() string { return "fastTransfer{redacted}" }

func (f *fastTransfer) append(data []byte, atom bool) {
	if f.err != nil {
		return
	}
	extra := len(data)
	if atom {
		extra += 16
	}
	limit := f.limit
	if limit == 0 {
		limit = maxFastTransferBytes
	}
	if len(f.data)+16*len(f.atoms)+extra > limit {
		f.err = core.ErrUnsupported
		return
	}
	start := len(f.data)
	f.data = append(f.data, data...)
	if atom {
		f.atoms = append(f.atoms, idRange{uint64(start), uint64(len(f.data))})
	}
}
func (f *fastTransfer) word(n uint32) { f.append(binary.LittleEndian.AppendUint32(nil, n), true) }
func (f *fastTransfer) fixed(tag uint32, n uint64) {
	f.word(tag)
	f.append(binary.LittleEndian.AppendUint64(nil, n), true)
}
func (f *fastTransfer) variable(tag uint32, data []byte) {
	f.word(tag)
	f.word(uint32(len(data)))
	f.append(data, false)
}

// chunkEnd never splits an atom. Variable property bytes can be split at any
// point, as required by MS-OXCFXICS 2.2.4.1. A too-small buffer makes no progress.
func (f *fastTransfer) chunkEnd(start, maximum int) int {
	end := min(len(f.data), start+maximum)
	i := sort.Search(len(f.atoms), func(i int) bool { return int(f.atoms[i].high) > end })
	if i < len(f.atoms) && int(f.atoms[i].low) < end {
		end = int(f.atoms[i].low)
	}
	return max(start, end)
}

func validateHierarchyConfigure(c synchronizationConfigure) error {
	// Unicode and NoForeignIdentifiers only. All content flags and the
	// optional NoDeletions mode are explicitly outside this first profile.
	if c.kind != 2 || c.restrictionBytes != 0 || len(c.tags) > maxFolderPropertyTags || c.flags&1 == 0 || c.flags & ^uint16(0x0101) != 0 {
		return core.ErrUnsupported
	}
	// SendOptions Unicode must agree with SynchronizationFlags. ForceUnicode
	// is also honored. Reserved 0x20/0x40 are ignored on receipt per the spec.
	if c.sendOptions&1 == 0 || c.sendOptions & ^byte(0x69) != 0 {
		return core.ErrUnsupported
	}
	// Known content-only ExtraFlags are rejected; unknown bits MUST be ignored.
	if c.extraFlags&0x0e != 0 {
		return core.ErrUnsupported
	}
	return nil
}

func hierarchyFastTransfer(replica, mailbox [16]byte, config synchronizationConfigure, delta hierarchyDelta) (*fastTransfer, error) {
	// Mandatory and flag-controlled identification fields survive exclusions.
	// Projected classes and canonical references are optional; select
	// by property ID, including alternate/unspecified client types.
	// Never copy arbitrary native or RAM Folder properties into this stream.
	classExcluded := false
	hiddenExcluded := false
	additionalRefsExcluded := false
	var countersExcluded [3]bool
	var referenceExcluded [metadataFolderRoleCount]bool
	for _, tag := range config.tags {
		classExcluded = classExcluded || uint16(tag>>16) == 0x3613
		hiddenExcluded = hiddenExcluded || uint16(tag>>16) == folderHiddenTag>>16
		additionalRefsExcluded = additionalRefsExcluded || uint16(tag>>16) == additionalEntryIDs
		for i := range countersExcluded {
			countersExcluded[i] = countersExcluded[i] || uint16(tag>>16) == uint16(0x3601+i)
		}
		for role := metadataFolderRole(0); role < metadataFolderRoleCount; role++ {
			referenceExcluded[role] = referenceExcluded[role] || uint16(tag>>16) == uint16(role.tag()>>16)
		}
	}
	f := &fastTransfer{}
	for _, folder := range delta.changes {
		f.word(0x40120003) // IncrSyncChg marker, no value.
		f.variable(0x65e10102, folder.parentSourceKey)
		f.variable(0x65e00102, folder.sourceKey[:])
		f.fixed(0x30080040, fileTime(folder.modified))
		f.variable(0x65e20102, []byte(folder.changeKey))
		f.variable(0x65e30102, folder.predecessors)
		var name []byte
		for _, unit := range append(utf16.Encode([]rune(folder.name)), 0) {
			name = binary.LittleEndian.AppendUint16(name, unit)
		}
		f.variable(0x3001001f, name)
		if folder.hidden != nil && !hiddenExcluded {
			f.scalar(folderHiddenTag, core.Bool(*folder.hidden))
		}
		if !countersExcluded[0] {
			f.word(0x36010003)
			f.word(childFolderType(folder.search))
		}
		for i, tag := range []uint32{0x36020003, 0x36030003} {
			if folder.counts.present[i] && !countersExcluded[i+1] {
				f.word(tag)
				f.word(folder.counts.values[i])
			}
		}
		if folder.containerClass != "" && !classExcluded {
			value, ok := propertyString(folder.containerClass, 0x001f, 1252)
			if !ok {
				return nil, core.ErrInvalid
			}
			f.variable(0x3613001f, value)
		}
		for role, fid := range folder.referenceFIDs {
			if fid == 0 || referenceExcluded[role] {
				continue
			}
			if mailbox == ([16]byte{}) {
				return nil, core.ErrInvalid
			}
			f.variable(metadataFolderRole(role).tag(), marshalFolderEntryID(mailbox, replica, fid))
		}
		if folder.junkFID != 0 && !additionalRefsExcluded {
			if mailbox == ([16]byte{}) {
				return nil, core.ErrInvalid
			}
			entry := marshalFolderEntryID(mailbox, replica, folder.junkFID)
			slots, err := additionalEntryIDSlotsWithJunk(folder.additionalRefs, entry)
			if err != nil {
				return nil, core.ErrInvalid
			}
			f.word(additionalEntryIDs<<16 | 0x1102)
			f.word(uint32(len(slots)))
			for _, slot := range slots {
				f.word(uint32(len(slot)))
				f.append(slot, false)
			}
		}
		if config.extraFlags&1 != 0 {
			f.fixed(0x67480014, folder.fid)
		}
		if config.extraFlags&1 != 0 || config.flags&0x100 != 0 {
			f.fixed(0x67490014, folder.parent)
		}
	}
	if len(delta.removed) > 0 {
		var ranges []idRange
		for _, fid := range delta.removed {
			n := folderGlobalCount(fid)
			ranges = append(ranges, idRange{n, n})
		}
		raw, err := singleReplicaSet(replica, ranges).marshal()
		if err != nil {
			return nil, err
		}
		// MetaTagIdsetDeleted uses REPLID-based IDSET, unlike Given/Seen.
		deleted := append([]byte{1, 0}, raw[16:]...)
		f.word(0x40130003)
		f.variable(0x67e50102, deleted)
	}
	if err := f.state(delta.finalGiven, delta.finalSeen); err != nil {
		return nil, err
	}
	f.word(0x40140003) // IncrSyncEnd after final state.
	if f.err != nil {
		return nil, f.err
	}
	return f, nil
}

func (f *fastTransfer) state(given, seen idSet) error {
	g, err := given.marshal()
	if err != nil {
		return err
	}
	s, err := seen.marshal()
	if err != nil {
		return err
	}
	f.word(0x403a0003)
	// Given has an Integer32-looking tag but MUST be encoded as binary.
	f.variable(idsetGivenTag, g)
	f.variable(cnsetSeenTag, s)
	f.word(0x403b0003)
	return f.err
}

func stateFastTransfer(given, seen idSet) (*fastTransfer, error) {
	f := &fastTransfer{}
	if err := f.state(given, seen); err != nil {
		return nil, err
	}
	return f, nil
}

func hierarchyUploadStateFastTransfer(seen idSet) (*fastTransfer, error) {
	raw, err := seen.marshal()
	if err != nil {
		return nil, err
	}
	f := &fastTransfer{}
	f.word(0x403a0003)
	f.variable(cnsetSeenTag, raw)
	f.word(0x403b0003)
	return f, f.err
}

func fileTime(t time.Time) uint64 {
	return uint64(t.Unix()+11644473600)*10000000 + uint64(t.Nanosecond()/100)
}
