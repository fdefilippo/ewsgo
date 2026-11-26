// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"
	"time"

	"ewsgo/internal/core"
)

func TestCalendarReferenceSurvivesInboxHierarchyAndFilters(t *testing.T) {
	for _, tags := range [][]uint32{nil, {0x3613001f}, {0x36d00102}, {0x36d00000}, {0x36d0001f, 0x36d00102}} {
		f, _ := newCalendarFixture(t)
		entry := calendarEntry(t, f)
		f.transport.EnableHierarchyDownloadSpike()
		f.handles = append(f.handles, 0xffffffff, 0xffffffff)
		f.open(int(MailboxRoot))
		_, c := f.run(hierarchyExclusionOperation(4, 1, 2, tags))
		if c.errors != 0 {
			t.Fatal("Configure")
		}
		tokens := readTestFX(t, drainFX(t, f.run, 2, 23))
		var source []byte
		matches := 0
		inbox := binary.LittleEndian.AppendUint64(nil, f.fids[Inbox])
		for _, token := range tokens {
			if token.tag == 0x40120003 {
				source = nil
			}
			if token.tag == 0x65e00102 {
				source = token.value
			}
			if token.tag == 0x36d00102 {
				if len(source) != 22 || !bytes.Equal(source[16:], inbox[2:]) || !bytes.Equal(token.value, entry) {
					t.Fatal("reference differs from canonical Inbox EntryID or wrong source")
				}
				matches++
			}
		}
		want := 1
		for _, tag := range tags {
			if tag>>16 == 0x36d0 {
				want = 0
			}
		}
		if matches != want {
			t.Fatal("canonical reference lost in hierarchy or filter ignored", matches, want)
		}
		given, seen := testFXSets(t, tokens)
		f.run([]byte{1, 4, 2})
		f.run(hierarchyExclusionOperation(4, 1, 2, tags))
		for _, v := range []struct {
			tag uint32
			set idSet
		}{{idsetGivenTag, given}, {cnsetSeenTag, seen}} {
			raw, _ := v.set.marshal()
			uploadTestHierarchyState(t, f.run, 2, v.tag, raw)
		}
		for _, token := range readTestFX(t, drainFX(t, f.run, 2, 23)) {
			if token.tag == 0x40120003 || token.tag == 0x36d00102 {
				t.Fatal("unchanged reference resent")
			}
		}
	}
}

func TestHierarchyReferenceDependencyChangesOnlyWhenCalendarIdentityChanges(t *testing.T) {
	f, b := newCalendarFixture(t)
	calendarEntry(t, f)
	o, _ := f.state.mailbox.object(f.handles[0], 4)
	s := o.binding.store
	inbox := s.byFID[f.fids[Inbox]]
	root := folderKey{role: MailboxRoot}
	scan := func(at time.Time) {
		t.Helper()
		if _, err := s.observeHierarchy(context.Background(), f.session, f.fids[MailboxRoot], at); err != nil {
			t.Fatal(err)
		}
	}
	scan(hierarchyTestTime)
	firstInbox, firstRoot := s.hierarchy[inbox].cn, s.hierarchy[root].cn
	b.calendar.Properties[core.DisplayName] = core.Text("Renamed Calendar")
	scan(hierarchyTestTime.Add(time.Second))
	if s.hierarchy[inbox].cn != firstInbox || s.hierarchy[root].cn != firstRoot {
		t.Fatal("target rename changed reference CN")
	}
	b.calendar.ID = "replacement-calendar"
	scan(hierarchyTestTime.Add(2 * time.Second))
	if s.hierarchy[inbox].cn == firstInbox || s.hierarchy[root].cn == firstRoot {
		t.Fatal("target replacement did not advance reference CN")
	}
	nextInbox, nextRoot := s.hierarchy[inbox].cn, s.hierarchy[root].cn
	scan(hierarchyTestTime.Add(3 * time.Second))
	if s.hierarchy[inbox].cn != nextInbox || s.hierarchy[root].cn != nextRoot {
		t.Fatal("identical reference scan changed CN")
	}
	b.calendarErr = core.ErrNotFound
	scan(hierarchyTestTime.Add(4 * time.Second))
	if s.hierarchy[inbox].cn == nextInbox || s.hierarchy[root].cn == nextRoot {
		t.Fatal("target removal did not advance reference CN")
	}
}

func TestCalendarHierarchyReferenceAbsentTargetAndDefaultOff(t *testing.T) {
	for _, enabled := range []bool{false, true} {
		f, b := newCalendarFixture(t)
		if enabled {
			b.calendarErr = core.ErrNotFound
		} else {
			for _, store := range f.transport.stores.entries {
				store.calendarMetadata = false
			}
		}
		f.transport.EnableHierarchyDownloadSpike()
		f.handles = append(f.handles, 0xffffffff, 0xffffffff)
		f.open(int(MailboxRoot))
		f.run(hierarchyConfigureOperation(4, 1, 2))
		for _, token := range readTestFX(t, drainFX(t, f.run, 2, 23)) {
			if token.tag == 0x36d00102 {
				t.Fatal("unavailable or disabled Calendar reference fabricated")
			}
		}
	}
}
