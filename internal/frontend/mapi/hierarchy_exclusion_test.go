// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"reflect"
	"testing"
)

// Independent request fixture: client-observed flags, with synthetic exclusion
// tags. These are test data, never tags read from the owner's Outlook request.
func hierarchyExclusionOperation(logon, input, output byte, tags []uint32) []byte {
	out := []byte{0x70, logon, input, output, 2, 9, 1, 1, 0, 0, 0, 0, 0, 0}
	out = binary.LittleEndian.AppendUint16(out, uint16(len(tags)))
	for _, tag := range tags {
		out = binary.LittleEndian.AppendUint32(out, tag)
	}
	return out
}

func TestHierarchyExclusionRetainsOnlyMandatoryAndConditionalHeaders(t *testing.T) {
	f := newHierarchyDownloadFixture(t)
	raw := drainFX(t, f.run, 2, 512)
	tokens := readTestFX(t, raw)
	// Optional counters and FolderType honor property-ID exclusions; the
	// mandatory synchronization headers retain their existing contract.
	allowed := map[uint32]bool{0x65e10102: true, 0x65e00102: true, 0x30080040: true, 0x65e20102: true, 0x65e30102: true, 0x3001001f: true, 0x67480014: true, 0x67490014: true, 0x36010003: true, 0x36020003: true, 0x36030003: true, 0x3613001f: true, 0x36d20102: true, 0x36d30102: true}
	inFolder := false
	for _, token := range tokens {
		if token.tag == 0x40120003 {
			inFolder = true
			continue
		}
		if token.tag == 0x403a0003 {
			inFolder = false
		}
		if inFolder && !allowed[token.tag] {
			t.Fatal("optional folder property added without filter contract")
		}
	}
	var all []uint32
	for tag := range allowed {
		all = append(all, tag)
	}
	all = append(all, 0x3001001e, 0x00000000, 0xffff0003, 0x3001001f)
	for _, tags := range [][]uint32{nil, {0x12340003}, all, make([]uint32, 256)} {
		f.run([]byte{1, 4, 2})
		op := hierarchyExclusionOperation(4, 1, 2, tags)
		result, counts := f.run(op)
		if !bytes.Equal(result, []byte{0x70, 2, 0, 0, 0, 0}) || counts.errors != 0 {
			t.Fatal("exclusion Configure rejected")
		}
		_, _, _, reasons, _, _ := counts.synchronizationOptions.Fields()
		if !reflect.DeepEqual(reasons, []string{"empty"}) {
			t.Fatal("old diagnostic rejects supported filter")
		}
		filtered := readTestFX(t, drainFX(t, f.run, 2, 23))
		for _, token := range filtered {
			if token.tag == 0x67480014 {
				t.Fatal("FID present without Eid")
			}
		}
		// FID follows Eid; optional metadata follows exclusions by property ID.
		var expected []fxTestToken
		for _, token := range tokens {
			excluded := false
			for _, tag := range tags {
				if tag>>16 == token.tag>>16 && (token.tag == 0x36010003 || token.tag == 0x36020003 || token.tag == 0x36030003 || token.tag == 0x3613001f || token.tag == 0x36d20102 || token.tag == 0x36d30102) {
					excluded = true
				}
			}
			if token.tag != 0x67480014 && !excluded {
				expected = append(expected, token)
			}
		}
		if !reflect.DeepEqual(filtered, expected) {
			t.Fatal("filter damaged mandatory metadata, ordering or state")
		}
		given, seen := testFXSets(t, filtered)
		f.run([]byte{1, 4, 2})
		f.run(op)
		g, _ := given.marshal()
		s, _ := seen.marshal()
		for _, entry := range []struct {
			tag uint32
			raw []byte
		}{{idsetGivenTag, g}, {cnsetSeenTag, s}} {
			begin := binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32([]byte{0x75, 4, 2}, entry.tag), uint32(len(entry.raw)))
			for _, request := range [][]byte{begin, append(binary.LittleEndian.AppendUint32([]byte{0x76, 4, 2}, uint32(len(entry.raw))), entry.raw...), {0x77, 4, 2}} {
				response, _ := f.run(request)
				if binary.LittleEndian.Uint32(response[2:]) != 0 {
					t.Fatal("filtered state upload")
				}
			}
		}
		unchanged := readTestFX(t, drainFX(t, f.run, 2, 23))
		for _, token := range unchanged {
			if token.tag == 0x40120003 || token.tag == 0x40130003 {
				t.Fatal("filtered state invents delta")
			}
		}
	}
}

func TestHierarchyExclusionLimitsAndMalformedAreAtomic(t *testing.T) {
	f := newHierarchyDownloadFixture(t)
	for _, op := range [][]byte{
		hierarchyExclusionOperation(4, 1, 2, make([]uint32, 257)),
		hierarchyExclusionOperation(4, 1, 2, []uint32{0x12340003})[:19],
	} {
		_, err := (ROPBuffer{Operations: op, Handles: f.handles}).decodeStoreOperations(true, true)
		if err == nil {
			t.Fatal("limit or malformed filter accepted")
		}
		before := f.box.calls
		body, _ := f.execute(append([]byte{1, 4, 1}, op...), 65535)
		if binary.LittleEndian.Uint32(body[4:]) == 0 || f.box.calls != before {
			t.Fatal("invalid filter executed prefix")
		}
		if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 {
			t.Fatal("prefix Release executed")
		}
	}
	inclusion := hierarchyExclusionOperation(4, 1, 2, nil)
	inclusion[6] |= 0x80
	_, err := (ROPBuffer{Operations: inclusion, Handles: f.handles}).decodeStoreOperations(true, true)
	if !errors.Is(err, ErrUnsupported) {
		t.Fatal("hierarchy inclusion accepted")
	}
}
