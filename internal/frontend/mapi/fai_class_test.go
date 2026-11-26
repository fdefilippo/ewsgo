// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"reflect"
	"strings"
	"testing"
	"unicode/utf16"

	"ewsgo/internal/core"
)

// These are specification examples and synthetic values, not client values.
var syntheticFAIClasses = []string{
	"IPM.Configuration.MRM", "IPC.MS.Outlook.AgingProperties",
	"IPM.Configuration.Synthetic", "ipm.configuration.synthetic",
	"Vendor.Custom.Configuration", "IPM.Note", strings.Repeat("X", 255),
}

func TestFAIClassCodecAcceptsProtocolValuesWithoutThreeNameAllowlist(t *testing.T) {
	for _, class := range syntheticFAIClasses {
		for _, extended := range []bool{false, true} {
			values := syntheticRAM42FAISet(extended)
			values[1].value = core.Text(class)
			wire := faiSetOperation(3, values)
			got, err := parseFAISet(&reader{data: wire[3:]})
			if err != nil || !reflect.DeepEqual(got, values) {
				t.Fatal("valid class rejected/normalized despite Associated-only writer")
			}
		}
	}
}

func classEqualityRestriction(class string) []byte {
	out := binary.LittleEndian.AppendUint32([]byte{4, 4}, 0x001a001f)
	out = binary.LittleEndian.AppendUint32(out, 0x001a001f)
	for _, ch := range utf16.Encode([]rune(class)) {
		out = binary.LittleEndian.AppendUint16(out, ch)
	}
	return append(out, 0, 0)
}

func TestFAIClassesSaveFilterFastTransferTableAndReconnect(t *testing.T) {
	for _, class := range syntheticFAIClasses {
		f, box, store := newFAIExecuteFixture(t)
		f.transport.EnableContentsTableSpike()
		faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
		values := syntheticRAM42FAISet(true)
		values[1].value = core.Text(class)
		assertFAIProblems(t, faiRunOK(t, f, faiSetOperation(3, values)), []faiPropertyProblem{{5, 0x300b0102, accessDenied}, {6, 0x3ffa001f, accessDenied}, {7, 0x685d0003, NotSupported}})
		faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 0})
		faiRunOK(t, f, []byte{1, 4, 2})
		c := fullContentsConfig()
		c.flags &^= 0x20
		faiRunOK(t, f, contentsConfigureOperation(c, classEqualityRestriction(strings.ToUpper(class))))
		matched := 0
		for _, token := range decodeTestContents(t, drainFX(t, f.run, 2, 97)) {
			if token.tag == 0x001a001f && token.value == class {
				matched++
			}
		}
		if matched != 1 {
			t.Fatal("class filter must ignore case and export original value")
		}
		faiRunOK(t, f, []byte{1, 4, 2})
		readRow := func() uint64 {
			faiRunOK(t, f, []byte{5, 4, 1, 2, 0x52})
			tags := []uint32{0x674a0014, 0x67aa000b, 0x001a001f}
			faiRunOK(t, f, tableColumnsWire(2, tags...))
			rows := decodeTableRows(t, faiRunOK(t, f, tableQueryWire(2, 0, 1, 8)), tags)
			// The Unicode table cell includes its terminator in the existing
			// 510-byte limit; FastTransfer above retains the full 255 bytes.
			want := class
			if len(want) > 254 {
				want = want[:254]
			}
			if len(rows) != 1 || rows[0][1] != true || rows[0][2] != want {
				t.Fatal("FAI Table lost Associated discriminator/class")
			}
			return rows[0][0].(uint64)
		}
		mid := readRow()
		f.state.mailbox.close()
		f.state.mailbox = newMailboxObjects(f.transport.stores)
		f.handles = []uint32{0xffffffff, 0xffffffff, 0xffffffff, 0xffffffff, 0xffffffff}
		faiRunOK(t, f, logonOperation(f.identity.LegacyDN, 4, 0))
		f.open(int(Inbox))
		if readRow() != mid || len(store.fai.records) != 1 || box.sourceCalls != 0 {
			t.Fatal("reconnect changed FAI or FAI accessed native contents")
		}
		f.state.mailbox.close()
	}
}

func TestMessageClassGrammarBoundaries(t *testing.T) {
	for b := 0; b <= 255; b++ {
		class := "X" + string([]byte{byte(b)}) + "Y"
		if validMessageClass(class) != (b >= 0x20 && b <= 0x7f) {
			t.Fatal("message class ASCII boundary")
		}
	}
	for _, class := range []string{"X", " ", ".X", "X..Y", strings.Repeat("X", 255)} {
		if !validMessageClass(class) {
			t.Fatal("additional undocumented class restriction")
		}
	}
}

func TestFAIClassInvalidValuesPreserveDraftAndNamespace(t *testing.T) {
	f, _ := messageFixture(t)
	d := newFAIDraftForTest(t, f, syntheticFAIImport())
	before, _ := core.CloneProperties(d.properties)
	for _, class := range []string{"", "IPM.Configuration.", "IPM.\x1f", "IPM.\x80", "IPM.é", strings.Repeat("X", 256)} {
		values := faiSetRequest{{0x00170003, core.Integer(1)}, {0x001a001f, core.Text(class)}}
		if _, err := d.set(context.Background(), f.session, values); !errors.Is(err, core.ErrUnsupported) || !reflect.DeepEqual(before, d.properties) {
			t.Fatal("invalid class changed draft")
		}
		r := faiTestRecord(f.store, d.root, 1)
		r.object.Properties[core.Key{ID: 0x1a}] = core.Text(class)
		if _, err := f.store.validateFAIRecord(r); err == nil {
			t.Fatal("invalid class accepted into namespace")
		}
		wire := faiSetOperation(3, values)
		if got, err := parseFAISet(&reader{data: wire[3:]}); err == nil || got != nil {
			t.Fatal("invalid class accepted by wire parser")
		}
	}
	if bytes.Contains([]byte(d.String()), []byte("Synthetic")) {
		t.Fatal("draft formatter exposed configuration")
	}
}
