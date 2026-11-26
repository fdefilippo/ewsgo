// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"slices"
	"testing"
)

func TestTransferStateFailureDistinguishesExecuteBranches(t *testing.T) {
	for _, scenario := range []struct {
		name   string
		setup  func(*testing.T, *ostFixture) byte
		code   uint32
		labels []string
	}{
		{
			name:  "folder instead of synchronization context",
			setup: func(_ *testing.T, _ *ostFixture) byte { return 1 },
			code:  NotSupported, labels: []string{"transfer_state_wrong_object"},
		},
		{
			name: "state upload pending",
			setup: func(t *testing.T, f *ostFixture) byte {
				configureContentsTest(t, f, fullContentsConfig())
				begin := binary.LittleEndian.AppendUint32([]byte{0x75, 4, 2}, cnsetSeenTag)
				faiRunOK(t, f, binary.LittleEndian.AppendUint32(begin, 1))
				return 2
			},
			code: NotSupported, labels: []string{"transfer_state_pending_state", "transfer_state_source_contents"},
		},
		{
			name: "memory quota exhausted",
			setup: func(t *testing.T, f *ostFixture) byte {
				configureContentsTest(t, f, fullContentsConfig())
				object, code := f.state.mailbox.object(f.handles[2], 4)
				if code != 0 {
					t.Fatal("missing Contents handle")
				}
				lease, err := object.binding.store.reserveContentsMemory(maxStoreContentsMemory - contentsStateMemory)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(lease.release)
				return 2
			},
			code: NotSupported, labels: []string{"transfer_state_memory", "transfer_state_source_contents"},
		},
		{
			name: "mailbox authorization revoked",
			setup: func(t *testing.T, f *ostFixture) byte {
				configureContentsTest(t, f, fullContentsConfig())
				f.box.state.Owner = false
				return 2
			},
			code: accessDenied, labels: []string{"transfer_state_authorization", "transfer_state_source_contents"},
		},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			for _, trace := range []bool{false, true} {
				f, _ := newContentsDownloadFixture(t)
				input := scenario.setup(t, f)
				if trace {
					f.transport.EnableSynchronizationOptionsTrace()
				}
				ops := append([]byte{0x82, 4, input, 3}, hierarchyBufferOperation(4, 3, 4096)...)
				wire, counts := f.run(ops)
				want := append(ropError(0x82, 3, scenario.code), contentsBufferResponse(3, nullObject, 0, nil, 0, 0)...)
				if !bytes.Equal(wire, want) || counts.errors != 2 || f.handles[3] != 0xffffffff {
					t.Fatalf("GetTransferState/GetBuffer response = %x, want %x (trace=%t)", wire, want, trace)
				}
				wantLabels := []string{"none"}
				if trace {
					wantLabels = scenario.labels
					_, _, _, buffers, _ := counts.execution.Fields()
					if !slices.Equal(buffers, []string{"unknown:error"}) {
						t.Fatal("GetBuffer classification changed", buffers)
					}
				}
				_, _, _, labels, _, _ := counts.synchronizationOptions.Fields()
				if !slices.Equal(labels, wantLabels) {
					t.Fatalf("diagnostic labels = %v, want %v", labels, wantLabels)
				}
			}
		})
	}
}

func TestTransferStateFirstFailureSurvivesLaterConfigure(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	f.transport.EnableSynchronizationOptionsTrace()
	ops := append([]byte{0x82, 4, 1, 3}, contentsConfigureOperation(fullContentsConfig(), nil)...)
	ops = append(ops, 0x82, 4, 3, 3)
	_, counts := f.run(ops)
	_, _, _, labels, _, _ := counts.synchronizationOptions.Fields()
	if counts.errors != 2 || !slices.Equal(labels, []string{"transfer_state_wrong_object"}) {
		t.Fatal("first failure lost after Configure or a later GetTransferState", labels)
	}
}
