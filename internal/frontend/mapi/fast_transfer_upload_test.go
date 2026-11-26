// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"context"
	"encoding/binary"
	"testing"

	"ewsgo/internal/core"
)

func faiUploadBuffer(slot byte, data []byte) []byte {
	raw := binary.LittleEndian.AppendUint16([]byte{ropFastTransferDestinationPutBuffer, 4, slot}, uint16(len(data)))
	return append(raw, data...)
}

func TestFastTransferDestinationStagesAndSavesFAI(t *testing.T) {
	f, _, store := newFAIExecuteFixture(t)
	t.Cleanup(f.state.mailbox.close)
	f.handles = append(f.handles, 0xffffffff)
	faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
	faiRunOK(t, f, []byte{ropFastTransferDestinationConfigure, 4, 3, 5, 1, 0})
	name := propertyName{Set: [16]byte{6, 7, 8}, Kind: 1, Name: "Synthetic view setting"}
	ids, code := store.named.ids(context.Background(), []propertyName{name}, true)
	if code != 0 || ids[0] == 0x8123 {
		t.Fatal("named property fixture")
	}
	stream := &fastTransfer{limit: maxFAIObjectBytes}
	blob := bytes.Repeat([]byte{1, 2, 3, 4}, 17<<10)
	for _, property := range faiCreateProperties() {
		stream.scalar(property.tag, property.value)
	}
	stream.scalar(0x0e1f000b, core.Bool(true))
	stream.scalar(0x40160003, core.Integer(0x0e12000d))
	stream.word(0x81230102)
	stream.append(name.Set[:], true)
	stream.append([]byte{1}, true)
	stream.append(contentsUnicode(name.Name), true)
	stream.scalarValue(0x81230102, core.Binary(blob))
	if stream.err != nil {
		t.Fatal(stream.err)
	}
	// Split inside the first property tag and across a >64 KiB value.
	for _, chunk := range [][]byte{stream.data[:3], stream.data[3:40000], stream.data[40000:]} {
		raw := faiRunOK(t, f, faiUploadBuffer(5, chunk))
		if len(raw) != fastTransferPutBufferResponseSize || binary.LittleEndian.Uint16(raw[13:]) != uint16(len(chunk)) {
			t.Fatal("upload response framing or UsedSize")
		}
	}
	if len(store.fai.records) != 0 {
		t.Fatal("buffer write published a FAI before SaveChanges")
	}
	faiRunOK(t, f, []byte{1, 4, 5})
	faiRunOK(t, f, []byte{0x0c, 4, 4, 3, 0})
	if len(store.fai.records) != 1 {
		t.Fatal("SaveChanges did not publish exactly one FAI")
	}
	for _, record := range store.fai.records {
		p := record.object.Properties
		named, _ := p[name.coreKey()].(core.Binary)
		if p[core.Key{ID: 0x0e1f}] != core.Bool(true) || p[core.Key{ID: 0x7006}] != core.Text("Synthetic view") ||
			!bytes.Equal(named, blob) {
			t.Fatal("stored properties did not match the upload")
		}
	}
	f.state.mailbox.close()
	if f.transport.stores.transfers.Load() != 0 || f.transport.stores.contents.used != store.fai.lease.size() {
		t.Fatal("upload leaked transfer or memory quota")
	}
}

func TestFastTransferDestinationNeverSavesBrokenUpload(t *testing.T) {
	for _, tc := range []struct {
		name     string
		bad      []byte
		putError bool
	}{
		{"partial binary", []byte{2, 1, 1, 0x70, 5, 0, 0, 0, 1}, false},
		{"recipient marker", []byte{3, 0, 3, 0x40}, true},
		{"malformed unicode", []byte{0x1f, 0, 6, 0x70, 2, 0, 0, 0, 0x41, 0}, true},
		{"unsupported property", []byte{3, 0, 0x34, 0x12, 1, 0, 0, 0}, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, _, store := newFAIExecuteFixture(t)
			t.Cleanup(f.state.mailbox.close)
			f.handles = append(f.handles, 0xffffffff)
			faiRunOK(t, f, faiImportOperation(syntheticFAIImport()))
			faiRunOK(t, f, []byte{ropFastTransferDestinationConfigure, 4, 3, 5, 2, 0})
			stream := &fastTransfer{limit: maxFAIObjectBytes}
			for _, property := range faiCreateProperties()[:2] {
				stream.scalar(property.tag, property.value)
			}
			stream.data = append(stream.data, tc.bad...)
			raw, counts := f.run(faiUploadBuffer(5, stream.data))
			if len(raw) != fastTransferPutBufferResponseSize || (counts.errors != 0) != tc.putError ||
				tc.putError && binary.LittleEndian.Uint16(raw[13:]) != 0 {
				t.Fatal("unexpected PutBuffer result")
			}
			faiRunOK(t, f, []byte{1, 4, 5})
			_, counts = f.run([]byte{0x0c, 4, 4, 3, 0})
			if counts.errors != 1 || len(store.fai.records) != 0 {
				t.Fatal("an incomplete or rejected upload reached the FAI store")
			}
			f.state.mailbox.close()
			if f.transport.stores.transfers.Load() != 0 || f.transport.stores.contents.used != 0 {
				t.Fatal("aborted upload leaked quota")
			}
		})
	}
}
