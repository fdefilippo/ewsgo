// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestRejectedTableDiagnosticFollowsValidatedPrefix(t *testing.T) {
	for _, id := range []byte{4, 5, 0x3e} {
		for flags := 0; flags < 256; flags++ {
			f, box := newContentsDownloadFixture(t)
			f.transport.EnableContentsTableSpike()
			f.transport.EnableSynchronizationOptionsTrace()
			// Release is decoded but cannot take effect when a later ROP fails.
			request := []byte{1, 4, 1, id, 4, 1, 2, byte(flags)}
			b := ROPBuffer{Operations: request, Handles: f.handles}
			_, err := b.decodeTableOperations(true, true, true, true, false, false, false, false, false, true, true, true)
			if err == nil {
				continue
			}
			var u unsupportedOperation
			if !errors.As(err, &u) || u.label != publicROPLabels[id] {
				t.Fatal("lost actual rejected operation")
			}
			_, counts := f.execute(request, 65535)
			want := inspectTableFlags(request[3:], len(f.handles))
			if !reflect.DeepEqual(counts.execution.RejectedTableFlags(), want) || counts.unsupported != publicROPLabels[id] {
				t.Fatal("wrong rejected Table flags")
			}
			if _, code := f.state.mailbox.object(f.handles[1], 4); code != 0 || box.sourceCalls != 0 || f.transport.stores.contents.used != 0 {
				t.Fatal("rejected batch executed prefix or read source")
			}
			wire, _ := b.Marshal()
			if first := InspectROP(wire); first.First != "release" || len(first.Flags) != 0 {
				t.Fatal("rejected metadata contaminated first-ROP inspection")
			}
		}
	}
}

func TestRejectedTableDiagnosticRequiresValidatedHeaderAndTrace(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	f.transport.EnableContentsTableSpike()
	f.transport.EnableSynchronizationOptionsTrace()
	for _, bad := range [][]byte{
		{5, 4, 1}, {5, 4, 1, 2}, {5, 4, 9, 2, 0}, {5, 4, 1, 9, 0},
		// Stop at an earlier unsupported operation; do not scan for Table bytes.
		{0x13, 4, 1, 5, 4, 1, 2, 0},
	} {
		_, c := f.execute(bad, 65535)
		if !reflect.DeepEqual(c.execution.RejectedTableFlags(), []string{"none"}) {
			t.Fatal("incomplete or unreachable header leaked flags")
		}
	}
	_, c := f.execute([]byte{5, 4, 1, 2, 0x22}, 65535)
	if !strings.Contains(strings.Join(c.execution.RejectedTableFlags(), " "), "table_soft_deletes") {
		t.Fatal("missing parsed rejection")
	}
	if strings.Contains(fmt.Sprintf("%v %#v", c.execution, c.execution.rejectedTable), "34") {
		t.Fatal("raw flag in default formatter")
	}
	f.transport.synchronizationOptionsTrace = false
	_, c = f.execute([]byte{5, 4, 1, 2, 0x22}, 65535)
	if !reflect.DeepEqual(c.execution.RejectedTableFlags(), []string{"none"}) {
		t.Fatal("diagnostic enabled without trace")
	}
}
