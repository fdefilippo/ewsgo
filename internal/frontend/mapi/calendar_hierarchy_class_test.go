// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestCalendarClassSurvivesHierarchyFastTransferAndExclusions(t *testing.T) {
	for _, tags := range [][]uint32{nil, {0x12340003}, {0x3613001f}, {0x3613001e}, {0x36130000}, {0x36130003, 0x3613001f}} {
		f, _ := newCalendarFixture(t)
		f.transport.EnableHierarchyDownloadSpike()
		f.handles = append(f.handles, 0xffffffff, 0xffffffff)
		f.open(int(MailboxRoot))
		response, c := f.run(hierarchyExclusionOperation(4, 1, 2, tags))
		if !bytes.Equal(response, []byte{0x70, 2, 0, 0, 0, 0}) || c.errors != 0 {
			t.Fatal("Calendar hierarchy Configure")
		}
		tokens := readTestFX(t, drainFX(t, f.run, 2, 23))
		n, changes := 0, 0
		for _, v := range tokens {
			if v.tag == 0x40120003 {
				changes++
			}
			if v.tag == 0x3613001f {
				want := []byte{'I', 0, 'P', 0, 'F', 0, '.', 0, 'A', 0, 'p', 0, 'p', 0, 'o', 0, 'i', 0, 'n', 0, 't', 0, 'm', 0, 'e', 0, 'n', 0, 't', 0, 0, 0}
				if bytes.Equal(v.value, want) {
					n++
				}
			}
		}
		excluded := false
		for _, tag := range tags {
			excluded = excluded || tag>>16 == 0x3613
		}
		want := 1
		if excluded {
			want = 0
		}
		if n != want || changes == 0 {
			t.Fatal("Calendar class lost in ICS or exclusion ignored", n, want)
		}
		given, seen := testFXSets(t, tokens)
		f.run([]byte{1, 4, 2})
		f.run(hierarchyExclusionOperation(4, 1, 2, tags))
		for _, entry := range []struct {
			tag uint32
			set idSet
		}{{idsetGivenTag, given}, {cnsetSeenTag, seen}} {
			raw, _ := entry.set.marshal()
			uploadTestHierarchyState(t, f.run, 2, entry.tag, raw)
		}
		for _, token := range readTestFX(t, drainFX(t, f.run, 2, 23)) {
			if token.tag == 0x40120003 || token.tag == 0x3613001f {
				t.Fatal("unchanged hierarchy resent Calendar")
			}
		}
	}
}

func TestHierarchyClassAffectsObservationWithoutNativeRevisionChange(t *testing.T) {
	f := newHierarchyFixture(t)
	first := f.snapshot(t, MailboxRoot, hierarchyTestTime)
	// Synthetic catalog property transition verifies the projection fingerprint
	// independently of Zimbra's native revision field. No event port is present.
	for i := range f.box.page.Objects {
		if f.box.page.Objects[i].ID == "parent/opaque" {
			f.box.page.Objects[i].Properties[core.WellKnown] = core.Text("calendar")
		}
	}
	next := f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(time.Second))
	changed := 0
	for _, v := range next.folders {
		if hierarchyRow(t, first, v.fid).cn != v.cn {
			changed++
		}
	}
	if changed != 1 {
		t.Fatal("class-only projection change did not advance exactly one CN")
	}
	same := f.snapshot(t, MailboxRoot, hierarchyTestTime.Add(2*time.Second))
	for _, v := range same.folders {
		if hierarchyRow(t, next, v.fid).cn != v.cn {
			t.Fatal("identical class observation changed CN")
		}
	}

}

func uploadTestHierarchyState(t *testing.T, run func([]byte) ([]byte, storeResults), input byte, tag uint32, raw []byte) {
	t.Helper()
	begin := binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32([]byte{0x75, 4, input}, tag), uint32(len(raw)))
	chunk := append(binary.LittleEndian.AppendUint32([]byte{0x76, 4, input}, uint32(len(raw))), raw...)
	for _, op := range [][]byte{begin, chunk, {0x77, 4, input}} {
		response, c := run(op)
		if c.errors != 0 || len(response) != 6 || binary.LittleEndian.Uint32(response[2:]) != 0 {
			t.Fatal("hierarchy checkpoint upload")
		}
	}
}
