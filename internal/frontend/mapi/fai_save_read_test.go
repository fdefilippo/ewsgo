// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"encoding/binary"
	"reflect"
	"testing"

	"ewsgo/internal/core"
)

func saveReadTestOperations(input byte) []byte {
	ops := append(faiSetOperation(3, faiCreateProperties()), faiSetOperation(3, syntheticFAISubject("", "Synthetic configuration"))...)
	ops = append(ops, 0x0c, 4, 1, 3, 0x0a)
	return append(ops, folderPropertiesOperation(4, input, 1, 0x67480014)...)
}

func TestFAISaveFollowedByFolderReadsAndRelease(t *testing.T) {
	f, _, store := newFAIExecuteFixture(t)
	faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
	ops := saveReadTestOperations(1)
	ops = append(ops, 1, 4, 3) // Release saved message; Folder remains alive.
	ops = append(ops, folderPropertiesOperation(4, 1, 1, 0x67480014)...)
	out, counts := f.run(ops)
	if counts.errors != 0 || len(out) != 8+8+15+15+15 || len(store.fai.records) != 1 {
		t.Fatal("Set/Set/Save/Read/Release/Read failed")
	}
	if out[16] != 0x0c || out[17] != 1 || out[22] != 3 || binary.LittleEndian.Uint64(out[23:]) == 0 {
		t.Fatal("Save response indexes/MID incorrect")
	}
	want := append([]byte{7, 1, 0, 0, 0, 0, 0}, binary.LittleEndian.AppendUint64(nil, f.fids[Inbox])...)
	if !bytes.Equal(out[31:46], want) || !bytes.Equal(out[46:], want) {
		t.Fatal("post-Save Folder property row incorrect")
	}
}

func TestFAISaveReadTailDoesNotInventMessageProperties(t *testing.T) {
	f, _, store := newFAIExecuteFixture(t)
	faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
	out, counts := f.run(saveReadTestOperations(3))
	if counts.errors != 0 || len(store.fai.records) != 1 || !bytes.Equal(out[31:], append(ropError(7, 3, 0), append32([]byte{1, 0x0a}, NotSupported)...)) {
		t.Fatal("draft property read fabricated support or lost successful Save")
	}
}

func TestFAISaveReadTailPreflightAndForbiddenSuffixHaveNoEffects(t *testing.T) {
	for _, mode := range []string{"short_output", "oversize_output", "truncated", "unknown", "set", "save", "delete", "import"} {
		t.Run(mode, func(t *testing.T) {
			f, _, store := newFAIExecuteFixture(t)
			faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
			object, _ := f.state.mailbox.object(f.handles[3], 4)
			properties, _ := core.CloneProperties(object.download.draft.properties)
			before := append([]uint32(nil), f.handles...)
			retained, next := f.transport.stores.contents.used, store.next
			ops, maximum := saveReadTestOperations(1), uint32(65535)
			switch mode {
			case "short_output":
				maximum = 32
			case "oversize_output":
				tags := make([]uint32, 17)
				for i := range tags {
					tags[i] = 0x3001001f
				}
				ops = append(ops, folderPropertiesOperation(4, 1, 1, tags...)...)
			case "truncated":
				ops = append(ops, 7, 4, 1)
			case "unknown":
				ops = append(ops, 0x04, 4, 1)
			case "set":
				ops = append(ops, faiSetOperation(3, faiCreateProperties())...)
			case "save":
				ops = append(ops, 0x0c, 4, 1, 3, 0x0a)
			case "delete":
				ops = append(ops, faiDeleteOperation(faiTestXID(1, 1))...)
			case "import":
				ops = append(ops, faiImportOperation(syntheticFAIImport())...)
			}
			body, _ := f.execute(ops, maximum)
			if len(body) == 0 || len(store.fai.records) != 0 || store.next != next || retained != f.transport.stores.contents.used || !reflect.DeepEqual(before, f.handles) || !reflect.DeepEqual(properties, object.download.draft.properties) || object.download.draft.saved {
				t.Fatal("rejected/short response batch executed prefix")
			}
		})
	}
	// Delete retains its stricter tail policy, even for a property read.
	f, _, _ := newFAIExecuteFixture(t)
	ops := append(faiDeleteOperation(faiTestXID(1, 1)), folderPropertiesOperation(4, 1, 1, 0x67480014)...)
	_, counts := f.execute(ops, 65535)
	requireImportDiagnostic(t, counts.synchronizationOptions, "commit_nonfinal_batch")
}
