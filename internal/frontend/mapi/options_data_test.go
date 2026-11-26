// SPDX-FileCopyrightText: 2026 Defilippo srls info@defilippo.org
// SPDX-License-Identifier: AGPL-3.0-or-later

package mapi

import (
	"bytes"
	"errors"
	"testing"
)

func TestOptionsData(t *testing.T) {
	f, _ := newContentsDownloadFixture(t)
	options := []byte{ropOptionsData, 4, 0, 'S', 'M', 'T', 'P', 0, 1}
	addressTypes := []byte{0x49, 4, 0}
	want := append(ropError(ropOptionsData, 0, 0), 1, 0, 0, 0, 0)
	want = append(want, addressTypesResponse(0)...)
	if response := faiRunOK(t, f, append(bytes.Clone(options), addressTypes...)); !bytes.Equal(response, want) {
		t.Fatalf("combined OptionsData and GetAddressTypes response: %x", response)
	}

	options[1]++
	if response, _ := f.run(options); !bytes.Equal(response, ropError(ropOptionsData, 0, accessDenied)) {
		t.Fatalf("OptionsData on another logon: %x", response)
	}
	options[1], options[2] = 4, 3
	if response, _ := f.run(options); !bytes.Equal(response, ropError(ropOptionsData, 3, nullObject)) {
		t.Fatalf("OptionsData on a null handle: %x", response)
	}
	for _, invalid := range [][]byte{
		options[:len(options)-1],
		options[:len(options)-2],
		{ropOptionsData, 4, 0, 0x80, 0, 0},
	} {
		buffer := ROPBuffer{Operations: invalid, Handles: f.handles}
		if _, err := buffer.DecodeOperations(); !errors.Is(err, ErrMalformed) {
			t.Fatalf("invalid OptionsData %x passed validation: %v", invalid, err)
		}
	}
}
