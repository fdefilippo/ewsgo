// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"slices"
	"strings"
	"testing"

	"ewsgo/internal/core"
)

func TestContentsCompletionSummariesFromActualStreams(t *testing.T) {
	for _, tc := range []struct {
		name                          string
		role                          SpecialFolderRole
		normal, associated, populated bool
		labels                        []string
	}{
		{"inbox", Inbox, true, false, true, []string{"inbox_normal_nonempty:done"}},
		{"inbox_empty", Inbox, true, false, false, []string{"inbox_normal_empty:done"}},
		{"sent", SentItems, true, false, true, []string{"other_normal_nonempty:done"}},
		{"sent_empty", SentItems, true, false, false, []string{"other_normal_empty:done"}},
		{"fai", Inbox, false, true, true, []string{"associated_nonempty:done"}},
		{"fai_empty", Inbox, false, true, false, []string{"associated_empty:done"}},
		{"mixed", Inbox, true, true, true, []string{"inbox_normal_nonempty:done", "associated_nonempty:done"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, box := newContentsDownloadFixture(t)
			object, _ := f.state.mailbox.object(f.handles[1], 4)
			store := object.binding.store
			fid := f.fids[tc.role]
			if tc.role != Inbox {
				f.run(folderOperation(fid, 4, 0, 1))
			}
			box.objects = nil
			if tc.normal && tc.populated {
				box.objects = []core.Object{messageTestObject("synthetic-message", store.byFID[fid].native)}
			}
			if tc.associated && tc.populated {
				seedFAI(t, store, faiTestRecord(store, fid, 1))
			}
			c := fullContentsConfig()
			c.flags &^= 0x30
			if tc.normal {
				c.flags |= 0x20
			}
			if tc.associated {
				c.flags |= 0x10
			}
			// The same process, data and initial state must produce identical wire
			// with tracing off/on and exactly the same number of source reads.
			var previous []byte
			previousReads := -1
			for _, trace := range []bool{false, true} {
				f.transport.synchronizationOptionsTrace = trace
				configureContentsTest(t, f, c)
				calls := box.sourceCalls
				var raw []byte
				completed := false
				for range 10000 {
					wire, counts := f.run(hierarchyBufferOperation(4, 2, 73))
					if counts.errors != 0 || len(wire) < 15 {
						t.Fatal("GetBuffer failed")
					}
					raw = append(raw, wire[15:]...)
					_, _, _, labels, _ := counts.execution.Fields()
					done := binary.LittleEndian.Uint16(wire[6:]) == 3
					for _, label := range tc.labels {
						if slices.Contains(labels, label) != (trace && done) {
							t.Fatal("completion reported before Done or missing", labels)
						}
					}
					if done {
						completed = true
						break
					}
				}
				if !completed {
					t.Fatal("stream did not complete")
				}
				reads := box.sourceCalls - calls
				if trace && (!bytes.Equal(previous, raw) || previousReads != reads) {
					t.Fatal("diagnostic changed wire or source reads")
				}
				previous, previousReads = raw, reads
				f.run([]byte{1, 4, 2})
			}
		})
	}
}

func TestContentsCompletionSurvivesReleaseInSameBatch(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	f.transport.EnableSynchronizationOptionsTrace()
	configureContentsTest(t, f, fullContentsConfig())
	op := append(hierarchyBufferOperation(4, 2, 4096), 1, 4, 2)
	_, counts := f.run(op)
	_, _, _, labels, _ := counts.execution.Fields()
	if !slices.Contains(labels, "inbox_normal_nonempty:done") || !slices.Contains(labels, "associated_empty:done") {
		t.Fatal("later Release hid a completed source", labels)
	}
	if _, code := f.state.mailbox.object(f.handles[2], 4); code != invalidObject {
		t.Fatal("diagnostic interfered with Release")
	}
}

func TestContentsCompletionStateErrorsReleaseAndVocabulary(t *testing.T) {
	f, box := newContentsDownloadFixture(t)
	f.transport.EnableSynchronizationOptionsTrace()
	configureContentsTest(t, f, fullContentsConfig())
	box.sourceErr = core.ErrAccessDenied
	_, counts := f.run(hierarchyBufferOperation(4, 2, 4096))
	if counts.execution.contentsCompletions != 0 {
		t.Fatal("failed source reported a completion")
	}
	box.sourceErr = nil
	f.run([]byte{1, 4, 2})
	configureContentsTest(t, f, fullContentsConfig())
	drainFX(t, f.run, 2, 4096)
	f.run([]byte{0x82, 4, 2, 3})
	_, counts = f.run(hierarchyBufferOperation(4, 3, 4096))
	if counts.execution.contentsCompletions != 0 {
		t.Fatal("state-only export reported messages")
	}
	f.run([]byte{1, 4, 2})
	_, counts = f.run(hierarchyBufferOperation(4, 2, 4096))
	if counts.execution.contentsCompletions != 0 {
		t.Fatal("released source reported completion")
	}
	for bits := 0; bits < 2048; bits++ {
		d := ExecutionDiagnostic{contentsCompletions: uint16(bits)}
		_, _, _, labels, _ := d.Fields()
		if len(labels) > 11 {
			t.Fatal("unbounded vocabulary")
		}
		for _, s := range labels {
			if s != "none" && s != "other" && !slices.Contains([]string{
				"inbox_normal_empty:done", "inbox_normal_nonempty:done", "other_normal_empty:done", "other_normal_nonempty:done",
				"associated_empty:done", "associated_nonempty:done", "contacts_normal_empty:done", "contacts_normal_nonempty:done",
				"calendar_normal_empty:done", "calendar_normal_nonempty:done",
			}, s) {
				t.Fatal("non-public label")
			}
		}
		if strings.Contains(fmt.Sprintf("%v %+v %#v", d, d, d), "nonempty") {
			t.Fatal("unredacted formatting")
		}
	}
}
