// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"reflect"
	"strings"
	"testing"

	"ewsgo/internal/core"
)

func TestFolderOpenDiagnosticClassifiesActualResultsAndPreservesWire(t *testing.T) {
	for _, tc := range []struct{ name, target, result string }{
		{"native", "native", "success"}, {"service", "service", "success"},
		{"native_missing", "native", "not_found"}, {"native_denied", "native", "access_denied"},
		{"unmapped", "unmapped", "not_found"}, {"reserved", "reserved_local", "not_found"},
		{"other_replica", "other_replica", "not_found"}, {"zero", "zero", "not_found"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var baseline []byte
			for _, unknown := range []bool{false, true} {
				for _, options := range []bool{false, true} {
					f := newOSTFixture(t)
					f.transport.unknownPropertyTrace, f.transport.synchronizationOptionsTrace = unknown, options
					s := f.state.mailbox.objects[f.handles[0]].binding.store
					fid := f.fids[Inbox]
					switch tc.name {
					case "service":
						fid = f.fids[Outbox]
					case "native_missing":
						f.box.getErr = core.ErrNotFound
					case "native_denied":
						f.box.getErr = core.ErrAccessDenied
					case "unmapped":
						fid = folderID(maxGlobalCount)
					case "reserved":
						s.localReservations = []idRange{{999, 1001}}
						fid = folderID(1000)
					case "other_replica":
						fid = fid&^uint64(0xffff) | 2
					case "zero":
						fid = 0
					}
					reply, counts := f.execute(folderOperation(fid, 4, 0, 1), 65535)
					if baseline == nil {
						baseline = reply
					} else if !bytes.Equal(reply, baseline) {
						t.Fatal("diagnostic changed complete response or handles")
					}
					want := []string{"none"}
					if unknown && options {
						want = []string{"target:" + tc.target, "result:" + tc.result}
					}
					if !reflect.DeepEqual(counts.execution.FolderOpenProfiles(), want) {
						t.Fatal("target/result classification", counts.execution.FolderOpenProfiles(), want)
					}
					if text := fmt.Sprintf("%v %#v", counts.execution, counts.execution); strings.Contains(text, "native") || strings.Contains(text, "target") {
						t.Fatal("default formatting exposes diagnostic state")
					}
				}
			}
		})
	}
}

func TestFolderOpenDiagnosticMappingFamiliesAndInvalidInput(t *testing.T) {
	f := newOSTFixture(t)
	s := f.state.mailbox.objects[f.handles[0]].binding.store
	fid := folderID(1234)
	op := Operation{ID: 2, Logon: 4, Input: 0, Folder: fid}
	for _, family := range []string{"dynamic", "message", "unmapped"} {
		delete(s.byFID, fid)
		delete(s.byMID, fid)
		switch family {
		case "dynamic":
			s.byFID[fid] = folderKey{dynamic: 15}
		case "message":
			s.byMID = map[uint64]messageKey{fid: {}}
		}
		c := storeResults{folderOpenEnabled: true}
		c.recordFolderOpen(f.state.mailbox, op, f.handles, ropError(2, 1, PropertyNotFound))
		if !reflect.DeepEqual(c.execution.FolderOpenProfiles(), []string{"target:" + family, "result:not_found"}) {
			t.Fatal("family classification", c.execution.FolderOpenProfiles())
		}
	}
	c := storeResults{folderOpenEnabled: true}
	op.Logon++
	c.recordFolderOpen(f.state.mailbox, op, f.handles, ropError(2, 1, accessDenied))
	if !reflect.DeepEqual(c.execution.FolderOpenProfiles(), []string{"input:unavailable", "result:access_denied"}) {
		t.Fatal("cross-logon target metadata exposed")
	}
}

func TestFolderOpenDiagnosticRequiresExecutionAndCompleteRequest(t *testing.T) {
	f := newOSTFixture(t)
	f.transport.EnableUnknownPropertyTrace()
	f.transport.EnableSynchronizationOptionsTrace()
	valid := folderOperation(f.fids[Inbox], 4, 0, 1)
	for _, bad := range [][]byte{valid[:len(valid)-1], append(bytes.Clone(valid), 0xff, 4, 0), append([]byte{0xff, 4, 0}, valid...)} {
		_, c := f.execute(bad, 65535)
		if !reflect.DeepEqual(c.execution.FolderOpenProfiles(), []string{"none"}) {
			t.Fatal("diagnostic from unexecuted batch")
		}
	}
	_, c := f.execute(valid, 1)
	if !reflect.DeepEqual(c.execution.FolderOpenProfiles(), []string{"none"}) {
		t.Fatal("diagnostic before response-space preflight")
	}
	// An error response and the target label must come from a real ROP result.
	response, _ := f.run(valid)
	if len(response) < 6 || binary.LittleEndian.Uint32(response[2:]) != 0 {
		t.Fatal("valid operation changed by prior rejected batches")
	}
}
