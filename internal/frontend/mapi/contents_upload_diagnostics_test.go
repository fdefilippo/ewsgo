// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"ewsgo/internal/core"
)

func TestContentsUploadFailureReasonsAndAtomicity(t *testing.T) {
	for _, name := range []string{"parse", "replica", "counter", "given_limit", "given_class", "given_unknown", "sequence", "authorize", "cancelled", "closed", "unknown_property"} {
		t.Run(name, func(t *testing.T) {
			f, _ := messageFixture(t)
			row := messageObserve(t, f, Inbox).messages[0]
			c := testContentsStateConfig()
			if name == "given_class" {
				c.frame.flags &^= 0x20
			}
			s := contentsStateForTest(t, f, c)
			ctx := context.Background()
			tag, stage, kind, cause := uint32(cnsetSeenTag), uploadParse, byte(1), error(ErrMalformed)
			var raw []byte
			switch name {
			case "parse":
				raw = []byte{1}
			case "replica":
				tag, stage, kind, cause = cnsetSeenFAITag, uploadReplica, 2, core.ErrResync
				raw, _ = singleReplicaSet([16]byte{99}, []idRange{{1, 1}}).marshal()
			case "counter":
				stage, kind, cause = uploadCounter, 2, core.ErrResync
				raw, _ = singleReplicaSet(f.store.replica, []idRange{{maxGlobalCount, maxGlobalCount}}).marshal()
			case "given_limit":
				tag, stage, kind, cause = idsetGivenTag, uploadGivenLimit, 2, core.ErrResync
				raw, _ = singleReplicaSet(f.store.replica, []idRange{{1, 2}}).marshal()
			case "given_class":
				tag, stage, kind, cause = idsetGivenTag, uploadGivenClass, 2, core.ErrResync
				n := folderGlobalCount(row.mid)
				raw, _ = singleReplicaSet(f.store.replica, []idRange{{n, n}}).marshal()
			case "given_unknown":
				tag, stage, kind, cause = idsetGivenTag, uploadGivenUnknown, 2, core.ErrResync
				n := folderGlobalCount(f.binding.fids[Inbox])
				raw, _ = singleReplicaSet(f.store.replica, []idRange{{n, n}}).marshal()
			case "sequence":
				tag, stage, kind, cause = cnsetReadTag, uploadSequence, 3, core.ErrConflict
			case "authorize":
				stage, kind, cause = uploadAuthorize, 4, core.ErrAccessDenied
			case "cancelled":
				stage, kind, cause = uploadAuthorize, 7, context.Canceled
			case "closed":
				stage, kind, cause = uploadLifecycle, 8, core.ErrClosed
			case "unknown_property":
				tag, stage, kind, cause = 0xdead0102, uploadSequence, 6, core.ErrUnsupported
			}
			op := hierarchyUpload{kind: 0x77}
			if name == "unknown_property" {
				op = hierarchyUpload{kind: 0x75, property: tag}
			} else {
				size := uint32(len(raw))
				if name == "sequence" {
					size = 1
				}
				if err := s.upload(ctx, f.session, hierarchyUpload{kind: 0x75, property: tag, size: size}); err != nil {
					t.Fatal(err)
				}
				if len(raw) > 0 {
					if err := s.upload(ctx, f.session, hierarchyUpload{kind: 0x76, size: uint32(len(raw)), data: raw}); err != nil {
						t.Fatal(err)
					}
				}
			}
			if name == "authorize" {
				f.box.state.Owner = false
			}
			if name == "cancelled" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			if name == "closed" {
				s.close()
			}
			before, pending := cloneContentsState(s.value), s.pending
			err := s.upload(ctx, f.session, op)
			want := contentsUploadFailure{stage: stage, property: contentsUploadProperty(tag), kind: kind}
			if name == "closed" {
				want.property = contentsUploadProperty(0) // close discarded pending state.
			}
			if !errors.Is(err, cause) || inspectContentsUploadFailure(err) != want || hierarchyError(err) != hierarchyError(cause) {
				t.Fatalf("wrong cause/stage/status: %v, %v", err, inspectContentsUploadFailure(err).labels())
			}
			if !reflect.DeepEqual(before, s.value) || s.pending != pending {
				t.Fatal("failed upload changed committed or pending state")
			}
		})
	}
}

func uploadDiagnosticWire(tag uint32, raw []byte, size uint32) []byte {
	ops := binary.LittleEndian.AppendUint32(binary.LittleEndian.AppendUint32([]byte{0x75, 4, 2}, tag), size)
	if len(raw) > 0 {
		ops = append(ops, binary.LittleEndian.AppendUint32([]byte{0x76, 4, 2}, uint32(len(raw)))...)
		ops = append(ops, raw...)
	}
	return append(ops, 0x77, 4, 2)
}

func TestContentsUploadDiagnosticGateWireAndFirstFailure(t *testing.T) {
	var off []byte
	for _, enabled := range []bool{false, true} {
		f, _ := newContentsDownloadFixture(t)
		if enabled {
			f.transport.EnableSynchronizationOptionsTrace()
		}
		ops := contentsConfigureOperation(fullContentsConfig(), nil)
		ops = append(ops, uploadDiagnosticWire(cnsetSeenTag, []byte{1}, 1)...)
		// A later failing Configure replaces its option metadata; the original
		// upload failure must survive both it and the following upload error.
		bad := fullContentsConfig()
		bad.flags &^= 0x20
		ops = append(ops, contentsConfigureOperation(bad, []byte{6, 1, 3, 0, 0x45, 0x23, 8, 0, 0, 0})...)
		ops = append(ops, uploadDiagnosticWire(cnsetReadTag, nil, 1)...)
		wire, counts := f.run(ops)
		if !enabled {
			off = wire
			if counts.synchronizationOptions.uploadFailure.kind != 0 {
				t.Fatal("disabled diagnostic retained failure")
			}
		} else {
			if !bytes.Equal(off, wire) {
				t.Fatal("diagnostic changed protocol response")
			}
			want := []string{"state_upload_parse", "state_property_seen", "state_error_malformed"}
			if !reflect.DeepEqual(counts.synchronizationOptions.uploadFailure.labels(), want) {
				t.Fatal("lost first upload failure")
			}
			_, _, _, reasons, _, _ := counts.synchronizationOptions.Fields()
			for _, reason := range want {
				if !strings.Contains(strings.Join(reasons, ","), reason) {
					t.Fatal("missing exported label")
				}
			}
		}
	}
	for _, enabled := range []bool{false, true} {
		f, _ := newContentsDownloadFixture(t)
		if enabled {
			f.transport.EnableSynchronizationOptionsTrace()
		}
		ops := append(contentsConfigureOperation(fullContentsConfig(), nil), uploadDiagnosticWire(cnsetSeenTag, nil, 0)...)
		_, counts := f.run(ops)
		if counts.errors != 0 || counts.synchronizationOptions.uploadFailure.kind != 0 {
			t.Fatal("successful empty state marked failed")
		}
	}
}

func TestContentsUploadFailureFormattingAndClosedVocabulary(t *testing.T) {
	private := errors.New("PRIVATE-client-payload-and-identifiers")
	err := &contentsUploadError{cause: private, failure: contentsUploadFailure{stage: 255, property: 255, kind: 255}}
	if !errors.Is(err, private) {
		t.Fatal("error chain changed")
	}
	if strings.Contains(fmt.Sprintf("%v %+v %#v %v %#v", err, err, err, err.failure, err.failure), "PRIVATE") {
		t.Fatal("private error leaked")
	}
	if !reflect.DeepEqual(err.failure.labels(), []string{"state_upload_other", "state_property_other", "state_error_other"}) {
		t.Fatal("unbounded labels")
	}
	d := SynchronizationOptionsDiagnostic{uploadFailure: contentsUploadFailure{stage: uploadSequence, property: 4, kind: 3}}
	flags, _, _, reasons, restriction, tags := d.Fields()
	if !reflect.DeepEqual(flags, []string{"none"}) || !reflect.DeepEqual(reasons, d.uploadFailure.labels()) || restriction != "none" || tags != "none" {
		t.Fatal("upload-only diagnostic falsely claims Configure")
	}
}

func TestContentsUploadCollectorFailureAndHierarchySeparation(t *testing.T) {
	f, _, _ := newFAIExecuteFixture(t)
	f.transport.EnableSynchronizationOptionsTrace()
	wire, counts := f.run(uploadDiagnosticWire(cnsetSeenFAITag, []byte{1}, 1))
	if len(wire) != 18 || binary.LittleEndian.Uint32(wire[14:]) != faiError(ErrMalformed, false) || !reflect.DeepEqual(counts.synchronizationOptions.uploadFailure.labels(), []string{"state_upload_parse", "state_property_seen_fai", "state_error_malformed"}) {
		t.Fatal("collector lost failure metadata or changed its status mapping")
	}
	h := newHierarchyCollectorFixture(t)
	h.transport.EnableSynchronizationOptionsTrace()
	_, hierarchyCounts := h.run(uploadDiagnosticWire(cnsetSeenTag, []byte{1}, 1))
	if hierarchyCounts.errors == 0 || hierarchyCounts.synchronizationOptions.uploadFailure.kind != 0 {
		t.Fatal("hierarchy failure mislabeled as Contents/FAI")
	}
}
