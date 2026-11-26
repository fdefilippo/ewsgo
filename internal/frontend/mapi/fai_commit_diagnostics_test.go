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

func TestFAICommitTailDiagnosticAndNoPrefixEffects(t *testing.T) {
	for _, commit := range []struct {
		name string
		wire []byte
	}{
		{"save", []byte{0x0c, 4, 1, 3, 0x0a}},
		{"delete", faiDeleteOperation(faiTestXID(1, 1))},
	} {
		for _, tail := range []struct {
			name string
			wire []byte
		}{
			{"RopGetAddressTypes", []byte{0x49, 4, 0}},
			{"RopSynchronizationGetTransferState", []byte{0x82, 4, 2, 4}},
			{"RopSaveChangesMessage", []byte{0x0c, 4, 1, 3, 0x0a}},
			{"RopSetProperties", faiSetOperation(3, faiCreateProperties())},
		} {
			t.Run(commit.name+"/"+tail.name, func(t *testing.T) {
				f, box, store := newFAIExecuteFixture(t)
				faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
				object, _ := f.state.mailbox.object(f.handles[3], 4)
				properties, _ := core.CloneProperties(object.download.draft.properties)
				handles := append([]uint32(nil), f.handles...)
				allocated, retained := f.state.mailbox.allocated, f.transport.stores.contents.used
				ops := append(faiSetOperation(3, faiCreateProperties()), commit.wire...)
				ops = append(ops, 1, 4, 1) // Release remains permitted after commit.
				ops = append(ops, tail.wire...)
				body, counts := f.execute(ops, 65535)
				if !bytes.Equal(body, executeError(NotSupported, 65535)) || counts.unsupported != tail.name {
					t.Fatal("commit policy lost public tail label or changed wire response")
				}
				requireImportDiagnostic(t, counts.synchronizationOptions, "commit_nonfinal_batch")
				if !reflect.DeepEqual(handles, f.handles) || allocated != f.state.mailbox.allocated || retained != f.transport.stores.contents.used || len(store.fai.records) != 0 || box.sourceCalls != 0 {
					t.Fatal("diagnostic rejection executed prefix or retained memory")
				}
				if object.download.draft.closed || !reflect.DeepEqual(properties, object.download.draft.properties) || object.download.draft.saved {
					t.Fatal("rejected commit changed draft")
				}
			})
		}
	}
}

func TestFAICommitDiagnosticOnlyAfterCompleteDecoding(t *testing.T) {
	f, _, _ := newFAIExecuteFixture(t)
	for _, tail := range [][]byte{{0x49, 4}, {0x04, 4, 1}, {0xfd, 4, 0}} {
		ops := append([]byte{0x0c, 4, 1, 3, 0x0a}, tail...)
		body, counts := f.execute(ops, 65535)
		if binary.LittleEndian.Uint32(body[4:]) == 0 || counts.synchronizationOptions.commitNonfinal {
			t.Fatal("malformed or undecodable tail promoted to commit policy")
		}
	}
	// The diagnostic does not retain operations, indexes, flags or properties.
	d := SynchronizationOptionsDiagnostic{commitNonfinal: true}
	text := fmt.Sprintf("%v %+v %#v", d, d, d)
	if strings.Count(text, "SynchronizationOptionsDiagnostic{redacted}") != 3 {
		t.Fatal("diagnostic formatting exposed state")
	}
}
