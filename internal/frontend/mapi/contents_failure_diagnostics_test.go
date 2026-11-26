// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"

	"ewsgo/internal/core"
)

func TestContentsFailureDiagnosticsPreserveWireAndCleanup(t *testing.T) {
	for _, tc := range []struct {
		name, stage string
		block       int
		source      bool
	}{
		{"normal_budget", "normal_memory", maxStoreContentsMemory, false},
		{"projection_budget", "projection_memory", 7 << 20, false},
		{"source_unsupported", "normal_capture", 0, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var first []byte
			for _, trace := range []bool{false, true} {
				f, box := newContentsDownloadFixture(t)
				f.transport.synchronizationOptionsTrace = trace
				configureContentsTest(t, f, fullContentsConfig())
				object, _ := f.state.mailbox.object(f.handles[2], 4)
				store := object.binding.store
				if tc.name == "projection_budget" {
					box.objects = nil
					for i := range 3 {
						message := messageTestObject(core.ID(fmt.Sprintf("large-fixture-%d", i)), store.byFID[f.fids[Inbox]].native)
						message.MIME = []byte("Subject: synthetic\r\n\r\n" + strings.Repeat("x", 512<<10))
						box.objects = append(box.objects, message)
					}
				}
				baseline := store.allocator.contents.used
				if tc.block != 0 {
					size := tc.block
					if size == maxStoreContentsMemory {
						size -= baseline
					}
					lease, err := store.reserveContentsMemory(size)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(lease.release)
					baseline += size
				}
				if tc.source {
					box.sourceErr = core.ErrUnsupported
				}
				wire, counts := f.run(hierarchyBufferOperation(4, 2, 4096))
				if binary.LittleEndian.Uint32(wire[2:]) != NotSupported || counts.errors != 1 {
					t.Fatal("expected a real unsupported response")
				}
				if first != nil && !bytes.Equal(first, wire) {
					t.Fatal("trace changed response")
				}
				first = wire
				_, _, _, labels, _ := counts.execution.Fields()
				if slices.Contains(labels, "contents_failure:"+tc.stage) != trace || slices.Contains(labels, "contents_failure:memory_budget") != (trace && tc.block != 0) {
					t.Fatal("wrong failure stage or invented budget cause", labels)
				}
				if store.allocator.contents.used != baseline || object.download.contents.transfer != nil || object.download.contents.ticket != nil || object.download.contents.cursor != 0 || len(store.contentsProofs) != 0 {
					t.Fatal("failed preparation leaked memory or published a stream/checkpoint")
				}
				// The failure is latched, with identical bytes and no repeated capture.
				reads := box.sourceCalls
				again, counts := f.run(append(hierarchyBufferOperation(4, 2, 4096), 1, 4, 2))
				_, _, _, labels, _ = counts.execution.Fields()
				if !bytes.Equal(again, wire) || box.sourceCalls != reads || slices.Contains(labels, "contents_failure:"+tc.stage) != trace {
					t.Fatal("latched failure or later Release lost the original stage", labels)
				}
			}
		})
	}
}

func TestContentsFailureDiagnosticClosedVocabularyAndRedaction(t *testing.T) {
	secret := errors.New("private-native-id-and-content")
	for stage := contentsFailureStage(1); int(stage) < len(contentsFailureLabels); stage++ {
		err := contentsPreparationError{stage, secret}
		if !errors.Is(err, secret) {
			t.Fatal("lost original error")
		}
		got := inspectContentsFailure(err, contentsFailureEncoding)
		if got.stage != stage || got.budget {
			t.Fatal("stage changed")
		}
		for _, text := range []string{fmt.Sprint(err), fmt.Sprintf("%#v", err), fmt.Sprint(got), fmt.Sprintf("%#v", got)} {
			if strings.Contains(text, secret.Error()) {
				t.Fatal("cause escaped redaction")
			}
		}
	}
	if got := inspectContentsFailure(contentsPreparationError{contentsFailureFAICapture, contentsBudgetError{}}, contentsFailureEncoding); got.stage != contentsFailureFAICapture || !got.budget {
		t.Fatal("typed quota cause not preserved")
	}
	if !errors.Is(contentsBudgetError{}, core.ErrUnsupported) {
		t.Fatal("wire error contract changed")
	}
	f, _ := newContentsDownloadFixture(t)
	f.transport.EnableSynchronizationOptionsTrace()
	configureContentsTest(t, f, fullContentsConfig())
	_, counts := f.run(hierarchyBufferOperation(4, 2, 4096))
	if counts.execution.contentsFailureStages != 0 || counts.execution.contentsBudgetFailure {
		t.Fatal("success invented a failure")
	}
	// Invalid/released handles must not inspect another object's failure.
	f.run([]byte{1, 4, 2})
	_, counts = f.run(hierarchyBufferOperation(4, 2, 4096))
	if counts.execution.contentsFailureStages != 0 || counts.execution.contentsBudgetFailure {
		t.Fatal("invalid handle invented a stage")
	}
}
